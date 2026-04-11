package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/stranger/control-plane/internal/deployer"
	"github.com/stranger/control-plane/internal/mailer"
	"github.com/stranger/control-plane/internal/queue"
	"github.com/stranger/control-plane/internal/secrets"
	"github.com/stranger/control-plane/internal/store"
	"github.com/stranger/control-plane/types"
	"golang.org/x/time/rate"
)

var projectNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-_ ]{1,62}$`)
var secretKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// agentStatus tracks the last heartbeat from the agent.
var (
	agentMu       sync.Mutex
	agentLastSeen time.Time
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	apiToken := strings.TrimSpace(os.Getenv("STRANGER_API_TOKEN"))
	if apiToken == "" {
		slog.Error("Missing required env var", "name", "STRANGER_API_TOKEN")
		os.Exit(1)
	}

	agentToken := strings.TrimSpace(os.Getenv("STRANGER_AGENT_TOKEN"))
	if agentToken == "" {
		slog.Error("Missing required env var", "name", "STRANGER_AGENT_TOKEN")
		os.Exit(1)
	}
	secretsKey := strings.TrimSpace(os.Getenv("STRANGER_SECRETS_KEY"))
	secretsKeys := strings.TrimSpace(os.Getenv("STRANGER_SECRETS_KEYS"))
	secretsPrimaryKeyID := strings.TrimSpace(os.Getenv("STRANGER_SECRETS_PRIMARY_KEY_ID"))
	webhookSecret := strings.TrimSpace(os.Getenv("STRANGER_WEBHOOK_SECRET"))

	agentURL := envOrDefault("STRANGER_AGENT_URL", "http://localhost:3000")
	listenAddr := envOrDefault("STRANGER_LISTEN_ADDR", ":8080")
	dbPath := envOrDefault("STRANGER_DB_PATH", "stranger.db")

	db, err := store.NewStore(dbPath)
	if err != nil {
		slog.Error("Failed to init db", "error", err)
		os.Exit(1)
	}
	secretKeyring, err := secrets.NewKeyringFromConfig(secretsKeys, secretsPrimaryKeyID, secretsKey)
	if err != nil {
		slog.Error("Failed to initialize secrets keyring", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dispatcher := queue.NewDispatcher(db, secretKeyring, agentURL, agentToken)
	go dispatcher.Start(ctx)
	dispatcher.StartWatchdog(ctx)

	go startDailyCleanup(ctx, db)

	// Mailer: optional SMTP configuration for welcome emails after stack deploys.
	// If SMTP_HOST is empty, emails are silently skipped.
	mailerSvc := mailer.New(mailer.Config{
		Host:     strings.TrimSpace(os.Getenv("SMTP_HOST")),
		Port:     atoiOrDefault(strings.TrimSpace(os.Getenv("SMTP_PORT")), 587),
		Username: strings.TrimSpace(os.Getenv("SMTP_USER")),
		Password: strings.TrimSpace(os.Getenv("SMTP_PASS")),
		From:     envOrDefault("SMTP_FROM", "Stranger <no-reply@stranger.local>"),
	})
	if mailerSvc.Enabled() {
		slog.Info("Mailer: SMTP configured", "host", os.Getenv("SMTP_HOST"), "from", os.Getenv("SMTP_FROM"))
	} else {
		slog.Info("Mailer: SMTP not configured — welcome emails will be skipped")
	}

	logProxyClient := &http.Client{}
	agentProxyClient := &http.Client{Timeout: 65 * time.Second}

	// Rate limiters: 10 req/min per IP, burst 5
	deployRateLimiter := newIPRateLimiter()
	webhookRateLimiter := newIPRateLimiter()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/projects", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			projects, err := db.ListProjects()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, projects)
			return
		}

		if r.Method == http.MethodPost {
			var req types.CreateProjectRequest
			if err := decodeJSON(r, &req); err != nil {
				http.Error(w, "Invalid body", http.StatusBadRequest)
				return
			}
			if err := validateCreateProjectRequest(req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			project := types.Project{
				ID:                 uuid.NewString(),
				Name:               strings.TrimSpace(req.Name),
				RepoURL:            strings.TrimSpace(req.RepoURL),
				Template:           strings.ToLower(strings.TrimSpace(req.Template)),
				DockerfileLocation: normalizeDockerfileLocation(req.DockerfileLocation),
				BaseDirectory:      normalizeBaseDirectory(req.BaseDirectory),
				Port:               req.Port,
				CustomDomain:       strings.TrimSpace(req.CustomDomain),
				MemoryLimit:        strings.TrimSpace(req.MemoryLimit),
				CPULimit:           strings.TrimSpace(req.CPULimit),
				NotifyURL:          strings.TrimSpace(req.NotifyURL),
				StackConfig:        strings.TrimSpace(req.StackConfig),
				CreatedAt:          time.Now().UTC(),
			}

			if err := db.CreateProject(project); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// Store initial secrets (e.g. admin credentials for stack apps).
			for key, plaintext := range req.InitialSecrets {
				key = strings.TrimSpace(key)
				if key == "" || !secretKeyPattern.MatchString(key) {
					continue
				}
				ciphertext, encErr := secretKeyring.Encrypt(strings.TrimSpace(plaintext))
				if encErr != nil {
					slog.Warn("Failed to encrypt initial secret", "key", key, "error", encErr)
					continue
				}
				if _, err := db.UpsertProjectSecret(project.ID, key, ciphertext); err != nil {
					slog.Warn("Failed to store initial secret", "key", key, "error", err)
				}
			}

			_ = db.CreateAuditEvent(types.AuditEvent{
				ID:           uuid.NewString(),
				Action:       "project.created",
				Actor:        actorFromRequest(r),
				ResourceType: "project",
				ResourceID:   project.ID,
				Metadata: fmt.Sprintf(
					`{"template":%q,"dockerfile_location":%q,"base_directory":%q,"port":%d,"is_stack":%t}`,
					project.Template,
					project.DockerfileLocation,
					project.BaseDirectory,
					project.Port,
					project.StackConfig != "",
				),
				CreatedAt: time.Now().UTC(),
			})

			writeJSON(w, http.StatusCreated, project)
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}))

	mux.HandleFunc("/projects/", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		trimmedPath := strings.Trim(r.URL.Path, "/")
		parts := strings.Split(trimmedPath, "/")
		// parts[0]="projects", parts[1]=projectID, parts[2]=segment, parts[3]=key
		if len(parts) < 2 || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		projectID := strings.TrimSpace(parts[1])

		project, err := db.GetProject(projectID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "Project not found", http.StatusNotFound)
				return
			}
			http.Error(w, "DB error", http.StatusInternalServerError)
			return
		}

		// GET or DELETE /projects/{id}
		if len(parts) == 2 {
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, project)
			case http.MethodDelete:
				// Derive the Caddy domain the same way deployer does
				domain := project.CustomDomain
				if domain == "" {
					if label, err := sanitizeLabelForDomain(project.Name); err == nil {
						domain = label + ".localhost"
					}
				}
				// Best-effort: tell agent to stop container + remove route
				go callAgentTeardown(agentURL, agentToken, project.ID, domain)

				if err := db.DeleteProject(projectID); err != nil {
					http.Error(w, "Failed to delete project", http.StatusInternalServerError)
					return
				}
				_ = db.CreateAuditEvent(types.AuditEvent{
					ID:           uuid.NewString(),
					Action:       "project.deleted",
					Actor:        actorFromRequest(r),
					ResourceType: "project",
					ResourceID:   projectID,
					CreatedAt:    time.Now().UTC(),
				})
				writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "id": projectID})
			default:
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		segment := parts[2]

		// PUT /projects/{id}/settings
		if segment == "settings" && len(parts) == 3 {
			if r.Method != http.MethodPut {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Initialize with current values for partial-update behaviour
			req := types.UpdateProjectSettingsRequest{
				CustomDomain: project.CustomDomain,
				MemoryLimit:  project.MemoryLimit,
				CPULimit:     project.CPULimit,
				NotifyURL:    project.NotifyURL,
			}
			if err := decodeJSON(r, &req); err != nil {
				http.Error(w, "Invalid body", http.StatusBadRequest)
				return
			}
			if err := db.UpdateProjectSettings(projectID, strings.TrimSpace(req.CustomDomain), strings.TrimSpace(req.MemoryLimit), strings.TrimSpace(req.CPULimit), strings.TrimSpace(req.NotifyURL)); err != nil {
				http.Error(w, "Failed to update settings", http.StatusInternalServerError)
				return
			}
			updated, _ := db.GetProject(projectID)
			writeJSON(w, http.StatusOK, updated)
			return
		}

		// /projects/{id}/container
		if segment == "container" && len(parts) == 3 {
			if r.Method == http.MethodGet {
				agentReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
					strings.TrimRight(agentURL, "/")+"/container/status?project_id="+projectID,
					nil,
				)
				if err != nil {
					http.Error(w, "Failed to build agent request", http.StatusInternalServerError)
					return
				}
				agentReq.Header.Set("X-Agent-Token", agentToken)

				resp, err := agentProxyClient.Do(agentReq)
				if err != nil {
					http.Error(w, "Agent unreachable: "+err.Error(), http.StatusBadGateway)
					return
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(body)
				return
			}

			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var creq struct {
				Action string `json:"action"`
			}
			if err := decodeJSON(r, &creq); err != nil {
				http.Error(w, "Invalid body", http.StatusBadRequest)
				return
			}
			action := strings.ToLower(strings.TrimSpace(creq.Action))
			if action != "start" && action != "stop" && action != "restart" {
				http.Error(w, "action must be start, stop, or restart", http.StatusBadRequest)
				return
			}

			// Forward to agent
			agentBody, _ := json.Marshal(map[string]string{
				"project_id": projectID,
				"action":     action,
			})
			agentReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
				strings.TrimRight(agentURL, "/")+"/container/action",
				bytes.NewReader(agentBody),
			)
			if err != nil {
				http.Error(w, "Failed to build agent request", http.StatusInternalServerError)
				return
			}
			agentReq.Header.Set("Content-Type", "application/json")
			agentReq.Header.Set("Authorization", "Bearer "+agentToken)

			resp, err := agentProxyClient.Do(agentReq)
			if err != nil {
				http.Error(w, "Agent unreachable: "+err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				http.Error(w, string(body), resp.StatusCode)
				return
			}

			_ = db.CreateAuditEvent(types.AuditEvent{
				ID:           uuid.NewString(),
				Action:       "project.container." + action,
				Actor:        actorFromRequest(r),
				ResourceType: "project",
				ResourceID:   projectID,
				Metadata:     `{"action":"` + action + `"}`,
				CreatedAt:    time.Now().UTC(),
			})

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}

		// /projects/{id}/secrets[/{key}]
		if segment != "secrets" {
			http.NotFound(w, r)
			return
		}

		secretKey := ""
		if len(parts) == 4 {
			decodedKey, err := url.PathUnescape(parts[3])
			if err != nil {
				http.NotFound(w, r)
				return
			}
			secretKey = strings.TrimSpace(decodedKey)
		}

		if r.Method == http.MethodGet && secretKey == "" {
			projectSecrets, err := db.ListProjectSecrets(projectID)
			if err != nil {
				http.Error(w, "Failed to list secrets", http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, projectSecrets)
			return
		}

		if r.Method == http.MethodPut && secretKey != "" {
			if err := validateSecretKey(secretKey); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			var req types.UpsertProjectSecretRequest
			if err := decodeJSON(r, &req); err != nil {
				http.Error(w, "Invalid body", http.StatusBadRequest)
				return
			}
			if err := validateSecretValue(req.Value); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			encryptedValue, err := secretKeyring.Encrypt(req.Value)
			if err != nil {
				http.Error(w, "Failed to encrypt secret", http.StatusInternalServerError)
				return
			}

			secret, err := db.UpsertProjectSecret(projectID, secretKey, encryptedValue)
			if err != nil {
				http.Error(w, "Failed to save secret", http.StatusInternalServerError)
				return
			}

			_ = db.CreateAuditEvent(types.AuditEvent{
				ID:           uuid.NewString(),
				Action:       "project.secret.upserted",
				Actor:        actorFromRequest(r),
				ResourceType: "project",
				ResourceID:   projectID,
				Metadata:     fmt.Sprintf(`{"key":%q}`, secretKey),
				CreatedAt:    time.Now().UTC(),
			})

			writeJSON(w, http.StatusOK, secret)
			return
		}

		if r.Method == http.MethodDelete && secretKey != "" {
			if err := validateSecretKey(secretKey); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			deleted, err := db.DeleteProjectSecret(projectID, secretKey)
			if err != nil {
				http.Error(w, "Failed to delete secret", http.StatusInternalServerError)
				return
			}
			if !deleted {
				http.Error(w, "Secret not found", http.StatusNotFound)
				return
			}

			_ = db.CreateAuditEvent(types.AuditEvent{
				ID:           uuid.NewString(),
				Action:       "project.secret.deleted",
				Actor:        actorFromRequest(r),
				ResourceType: "project",
				ResourceID:   projectID,
				Metadata:     fmt.Sprintf(`{"key":%q}`, secretKey),
				CreatedAt:    time.Now().UTC(),
			})

			writeJSON(w, http.StatusOK, map[string]string{
				"status": "deleted",
				"key":    secretKey,
			})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}))

	mux.HandleFunc("/detect-template", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		repoURL := strings.TrimSpace(r.URL.Query().Get("repo_url"))
		if repoURL == "" {
			http.Error(w, "Missing repo_url", http.StatusBadRequest)
			return
		}
		if err := validateRepoURL(repoURL); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		template := deployer.DetectTemplate(repoURL)
		writeJSON(w, http.StatusOK, map[string]string{
			"template": template,
			"repo_url": repoURL,
		})
	}))

	mux.HandleFunc("/admin/secrets/rotate", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req types.RotateSecretsRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}

		req.ProjectID = strings.TrimSpace(req.ProjectID)
		if req.ProjectID != "" {
			if _, err := db.GetProject(req.ProjectID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.Error(w, "Project not found", http.StatusNotFound)
					return
				}
				http.Error(w, "DB error", http.StatusInternalServerError)
				return
			}
		}

		result, err := rotateSecretsAtRest(db, secretKeyring, req.ProjectID, req.DryRun)
		if err != nil {
			http.Error(w, "Failed to rotate secrets", http.StatusInternalServerError)
			return
		}

		_ = db.CreateAuditEvent(types.AuditEvent{
			ID:           uuid.NewString(),
			Action:       "secrets.rotated",
			Actor:        actorFromRequest(r),
			ResourceType: "secrets",
			ResourceID:   result.ProjectID,
			Metadata:     fmt.Sprintf(`{"dry_run":%t,"rotated":%d,"failed":%d,"primary_key_id":%q}`, result.DryRun, result.Rotated, result.Failed, result.PrimaryKeyID),
			CreatedAt:    time.Now().UTC(),
		})

		writeJSON(w, http.StatusOK, result)
	}))

	mux.HandleFunc("/deploy", withRateLimit(deployRateLimiter, withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req types.DeployRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		req.ProjectID = strings.TrimSpace(req.ProjectID)
		if req.ProjectID == "" {
			http.Error(w, "Missing project_id", http.StatusBadRequest)
			return
		}

		if _, err := db.GetProject(req.ProjectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "Project not found", http.StatusNotFound)
				return
			}
			http.Error(w, "DB error", http.StatusInternalServerError)
			return
		}

		now := time.Now().UTC()
		job := types.DeployJob{
			ID:           uuid.NewString(),
			ProjectID:    req.ProjectID,
			Status:       types.DeployJobStatusQueued,
			CreatedAt:    now,
			UpdatedAt:    now,
			AttemptCount: 0,
		}
		if err := db.CreateDeployJob(job); err != nil {
			http.Error(w, "Failed to create deploy job", http.StatusInternalServerError)
			return
		}

		_ = db.CreateAuditEvent(types.AuditEvent{
			ID:           uuid.NewString(),
			Action:       "deploy.queued",
			Actor:        actorFromRequest(r),
			ResourceType: "deploy_job",
			ResourceID:   job.ID,
			Metadata:     fmt.Sprintf(`{"project_id":%q}`, req.ProjectID),
			CreatedAt:    now,
		})

		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "queued",
			"job_id": job.ID,
		})
	})))

	mux.HandleFunc("/deploy/rollback", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req types.RollbackRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
		req.ProjectID = strings.TrimSpace(req.ProjectID)
		if req.ProjectID == "" {
			http.Error(w, "Missing project_id", http.StatusBadRequest)
			return
		}

		if _, err := db.GetProject(req.ProjectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "Project not found", http.StatusNotFound)
				return
			}
			http.Error(w, "DB error", http.StatusInternalServerError)
			return
		}

		jobs, err := db.GetLastTwoSuccessfulDeployJobs(req.ProjectID)
		if err != nil {
			http.Error(w, "Failed to fetch deploy history", http.StatusInternalServerError)
			return
		}
		if len(jobs) < 2 {
			http.Error(w, "Not enough successful deploys to rollback (need at least 2)", http.StatusBadRequest)
			return
		}

		prevJob := jobs[1] // second most recent succeeded job
		imageTag := prevJob.ImageTag
		if imageTag == "" {
			// Derive from plan ID as fallback
			imageTag = fmt.Sprintf("stranger-app-%s:latest", prevJob.PlanID)
		}

		now := time.Now().UTC()
		rollbackJob := types.DeployJob{
			ID:               uuid.NewString(),
			ProjectID:        req.ProjectID,
			Status:           types.DeployJobStatusQueued,
			PrebuiltImageTag: imageTag,
			CreatedAt:        now,
			UpdatedAt:        now,
			AttemptCount:     0,
		}
		if err := db.CreateDeployJob(rollbackJob); err != nil {
			http.Error(w, "Failed to create rollback job", http.StatusInternalServerError)
			return
		}

		_ = db.CreateAuditEvent(types.AuditEvent{
			ID:           uuid.NewString(),
			Action:       "deploy.rollback.queued",
			Actor:        actorFromRequest(r),
			ResourceType: "deploy_job",
			ResourceID:   rollbackJob.ID,
			Metadata:     fmt.Sprintf(`{"project_id":%q,"image_tag":%q,"prev_job_id":%q}`, req.ProjectID, imageTag, prevJob.ID),
			CreatedAt:    now,
		})

		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "queued",
			"job_id": rollbackJob.ID,
		})
	}))

	mux.HandleFunc("/deploy/jobs", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
		limit := parseLimit(r.URL.Query().Get("limit"), 50)

		jobs, err := db.ListDeployJobs(projectID, limit)
		if err != nil {
			http.Error(w, "Failed to list deploy jobs", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, jobs)
	}))

	mux.HandleFunc("/deploy/jobs/", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		jobID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/deploy/jobs/"))
		if jobID == "" {
			http.Error(w, "Missing job id", http.StatusBadRequest)
			return
		}

		job, err := db.GetDeployJob(jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "Deploy job not found", http.StatusNotFound)
				return
			}
			http.Error(w, "Failed to fetch deploy job", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, job)
	}))

	mux.HandleFunc("/internal/deploy/callback", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req types.DeployCallbackRequest
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}

		req.JobID = strings.TrimSpace(req.JobID)
		req.PlanID = strings.TrimSpace(req.PlanID)
		req.Status = strings.ToLower(strings.TrimSpace(req.Status))
		req.Error = strings.TrimSpace(req.Error)
		req.Domain = strings.TrimSpace(req.Domain)
		req.HostPort = strings.TrimSpace(req.HostPort)
		req.ImageTag = strings.TrimSpace(req.ImageTag)

		if req.JobID == "" {
			http.Error(w, "Missing job_id", http.StatusBadRequest)
			return
		}

		job, err := db.GetDeployJob(req.JobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "Deploy job not found", http.StatusNotFound)
				return
			}
			http.Error(w, "Failed to fetch deploy job", http.StatusInternalServerError)
			return
		}

		finalStatus := job.Status == types.DeployJobStatusSucceeded || job.Status == types.DeployJobStatusFailed
		if finalStatus {
			writeJSON(w, http.StatusOK, map[string]string{
				"status": job.Status,
				"job_id": req.JobID,
			})
			return
		}

		var action string
		switch req.Status {
		case types.DeployJobStatusRunning:
			if err := db.MarkDeployJobRunning(req.JobID); err != nil {
				http.Error(w, "Failed to mark deploy job running", http.StatusInternalServerError)
				return
			}
			action = "deploy.running"
		case types.DeployJobStatusSucceeded:
			if err := db.MarkDeployJobSucceeded(req.JobID, req.PlanID); err != nil {
				http.Error(w, "Failed to mark deploy job succeeded", http.StatusInternalServerError)
				return
			}
			if req.ImageTag != "" {
				if err := db.UpdateDeployJobImageTag(req.JobID, req.ImageTag); err != nil {
					slog.Warn("Failed to store image tag", "job_id", req.JobID, "error", err)
				}
			}
			action = "deploy.succeeded"
		case types.DeployJobStatusFailed:
			if req.Error == "" {
				req.Error = "deploy execution failed"
			}
			if err := db.MarkDeployJobFailed(req.JobID, req.Error); err != nil {
				http.Error(w, "Failed to mark deploy job failed", http.StatusInternalServerError)
				return
			}
			action = "deploy.failed"
		default:
			http.Error(w, "Invalid status", http.StatusBadRequest)
			return
		}

		metadata := fmt.Sprintf(`{"plan_id":%q,"status":%q,"domain":%q,"host_port":%q,"error":%q}`,
			req.PlanID, req.Status, req.Domain, req.HostPort, req.Error)
		_ = db.CreateAuditEvent(types.AuditEvent{
			ID:           uuid.NewString(),
			Action:       action,
			Actor:        "system:agent",
			ResourceType: "deploy_job",
			ResourceID:   req.JobID,
			Metadata:     metadata,
			CreatedAt:    time.Now().UTC(),
		})

		// Fire notify webhook for terminal states
		if req.Status == types.DeployJobStatusSucceeded || req.Status == types.DeployJobStatusFailed {
			if project, err := db.GetProject(job.ProjectID); err == nil && project.NotifyURL != "" {
				go fireNotifyWebhook(project.NotifyURL, req.JobID, job.ProjectID, req.Status, req.Domain, req.Error)
			}
		}

		// Send welcome email for successful stack deploys
		if req.Status == types.DeployJobStatusSucceeded && mailerSvc.Enabled() {
			go func(jobID, projectID, domain string) {
				project, err := db.GetProject(projectID)
				if err != nil || project.StackConfig == "" {
					return // not a stack project
				}

				// Load all decrypted secrets for this project
				ciphertexts, err := db.ListProjectSecretCiphertexts(projectID)
				if err != nil {
					slog.Warn("Mailer: failed to load project secrets", "project_id", projectID, "error", err)
					return
				}
				plainSecrets := make(map[string]string, len(ciphertexts))
				for _, c := range ciphertexts {
					if result, decErr := secretKeyring.Decrypt(c.Ciphertext); decErr == nil {
						plainSecrets[c.Key] = result.Plaintext
					}
				}

				adminEmail := mailer.ResolveSecret(plainSecrets, "ADMIN_EMAIL", "WORDPRESS_ADMIN_EMAIL")
				if adminEmail == "" {
					return // no email to send to
				}

				siteURL := "http://" + domain
				username := mailer.ResolveSecret(plainSecrets, "ADMIN_USER", "WORDPRESS_ADMIN_USER", "ADMIN_EMAIL")
				password := mailer.ResolveSecret(plainSecrets, "ADMIN_PASSWORD", "WORDPRESS_ADMIN_PASSWORD")

				_ = mailerSvc.SendStackWelcome(adminEmail, project.Name, project.Template, siteURL, username, password)
			}(req.JobID, job.ProjectID, req.Domain)
		}

		writeJSON(w, http.StatusOK, map[string]string{
			"status": req.Status,
			"job_id": req.JobID,
		})
	}))

	mux.HandleFunc("/internal/agent/heartbeat", withAgentAuth(agentToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		agentMu.Lock()
		agentLastSeen = time.Now().UTC()
		agentMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	mux.HandleFunc("/internal/agent/status", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		agentMu.Lock()
		lastSeen := agentLastSeen
		agentMu.Unlock()

		online := !lastSeen.IsZero() && time.Since(lastSeen) < 60*time.Second
		payload := map[string]any{
			"last_seen": lastSeen,
			"online":    online,
		}
		if dockerStatus, err := fetchAgentDockerStatus(r.Context(), agentProxyClient, agentURL, agentToken); err == nil {
			payload["docker"] = dockerStatus
		} else {
			payload["docker"] = map[string]any{
				"running": false,
				"error":   err.Error(),
			}
		}
		writeJSON(w, http.StatusOK, payload)
	}))

	mux.HandleFunc("/internal/agent/docker", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}
		payload, statusCode, err := proxyAgentDockerAction(r.Context(), agentProxyClient, agentURL, agentToken, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write(payload)
	}))

	mux.HandleFunc("/logs", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		projectID := r.URL.Query().Get("project_id")
		if projectID == "" {
			http.Error(w, "Missing project_id", http.StatusBadRequest)
			return
		}

		targetURL := fmt.Sprintf("%s/logs?project_id=%s", strings.TrimRight(agentURL, "/"), url.QueryEscape(projectID))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
		if err != nil {
			http.Error(w, "Failed to build request", http.StatusInternalServerError)
			return
		}
		req.Header.Set("X-Agent-Token", agentToken)

		resp, err := logProxyClient.Do(req)
		if err != nil {
			http.Error(w, "Failed to connect to agent", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= http.StatusBadRequest {
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(resp.Body, 4096))
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Transfer-Encoding", "chunked")

		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		_, _ = io.Copy(w, resp.Body)
	}))

	// GitHub webhook endpoint — uses HMAC signature instead of API token auth
	mux.HandleFunc("/webhooks/github", withRateLimit(webhookRateLimiter, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}

		// Validate HMAC signature if secret is configured
		if webhookSecret != "" {
			sig := r.Header.Get("X-Hub-Signature-256")
			if !validateGitHubSignature(webhookSecret, body, sig) {
				http.Error(w, "Invalid signature", http.StatusUnauthorized)
				return
			}
		}

		// Only process push events
		if r.Header.Get("X-GitHub-Event") != "push" {
			writeJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": "not a push event"})
			return
		}

		var payload githubPushPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}

		// Only process pushes to main or master
		if payload.Ref != "refs/heads/main" && payload.Ref != "refs/heads/master" {
			writeJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": "not main/master branch"})
			return
		}

		// Find matching project by repo URL
		projects, err := db.ListProjects()
		if err != nil {
			http.Error(w, "Failed to list projects", http.StatusInternalServerError)
			return
		}

		var matchedProject *types.Project
		for i := range projects {
			p := &projects[i]
			if strings.EqualFold(p.RepoURL, payload.Repository.CloneURL) ||
				strings.EqualFold(p.RepoURL, payload.Repository.SSHURL) {
				matchedProject = p
				break
			}
		}

		if matchedProject == nil {
			writeJSON(w, http.StatusOK, map[string]any{"queued": false, "reason": "no matching project found"})
			return
		}

		now := time.Now().UTC()
		job := types.DeployJob{
			ID:           uuid.NewString(),
			ProjectID:    matchedProject.ID,
			Status:       types.DeployJobStatusQueued,
			CreatedAt:    now,
			UpdatedAt:    now,
			AttemptCount: 0,
		}
		if err := db.CreateDeployJob(job); err != nil {
			http.Error(w, "Failed to create deploy job", http.StatusInternalServerError)
			return
		}

		_ = db.CreateAuditEvent(types.AuditEvent{
			ID:           uuid.NewString(),
			Action:       "deploy.queued",
			Actor:        "webhook:github",
			ResourceType: "deploy_job",
			ResourceID:   job.ID,
			Metadata:     fmt.Sprintf(`{"project_id":%q,"ref":%q}`, matchedProject.ID, payload.Ref),
			CreatedAt:    now,
		})

		slog.Info("GitHub webhook queued deploy", "project_id", matchedProject.ID, "ref", payload.Ref, "job_id", job.ID)
		writeJSON(w, http.StatusOK, map[string]any{"queued": true, "job_id": job.ID})
	}))

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("Control Plane starting", "addr", listenAddr)

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

// --- Rate limiting ---

type ipRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

func newIPRateLimiter() *ipRateLimiter {
	return &ipRateLimiter{
		limiters: make(map[string]*rate.Limiter),
	}
}

func (rl *ipRateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	l, ok := rl.limiters[ip]
	if !ok {
		// 10 requests per minute, burst of 5
		l = rate.NewLimiter(rate.Limit(10.0/60), 5)
		rl.limiters[ip] = l
	}
	return l
}

func withRateLimit(rl *ipRateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.getLimiter(ip).Allow() {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- Daily cleanup goroutine ---

func startDailyCleanup(ctx context.Context, db *store.Store) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := db.PurgeOldTerminalJobs(30 * 24 * time.Hour); err != nil {
				slog.Error("Cleanup: failed to purge old jobs", "error", err)
			} else {
				slog.Info("Cleanup: purged old deploy jobs", "count", n)
			}
			if n, err := db.PurgeOldAuditEvents(30 * 24 * time.Hour); err != nil {
				slog.Error("Cleanup: failed to purge old audit events", "error", err)
			} else {
				slog.Info("Cleanup: purged old audit events", "count", n)
			}
		}
	}
}

// --- GitHub webhook helpers ---

type githubPushPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
	} `json:"repository"`
}

func validateGitHubSignature(secret string, body []byte, signature string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// --- Auth middleware ---

func withAPIAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestToken := bearerToken(r.Header.Get("Authorization"))
		if !secureEquals(requestToken, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func withAgentAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestToken := strings.TrimSpace(r.Header.Get("X-Agent-Token"))
		if !secureEquals(requestToken, token) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func secureEquals(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func bearerToken(headerValue string) string {
	headerValue = strings.TrimSpace(headerValue)
	if headerValue == "" {
		return ""
	}
	parts := strings.SplitN(headerValue, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// --- JSON helpers ---

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

// --- Validation helpers ---

func validateCreateProjectRequest(req types.CreateProjectRequest) error {
	name := strings.TrimSpace(req.Name)
	repo := strings.TrimSpace(req.RepoURL)
	template := strings.ToLower(strings.TrimSpace(req.Template))
	isStack := strings.TrimSpace(req.StackConfig) != ""

	if !projectNamePattern.MatchString(name) {
		return fmt.Errorf("invalid name: use 2-63 chars, letters/numbers/spaces/dash/underscore")
	}

	// Stack apps (e.g. WordPress, Ghost) have no source repo.
	if !isStack {
		if err := validateRepoURL(repo); err != nil {
			return err
		}
	}

	if !deployer.IsSupportedTemplate(template) {
		return fmt.Errorf("unsupported template: %s", template)
	}

	if _, err := validateDockerfileLocation(req.DockerfileLocation); err != nil {
		return err
	}

	if _, err := validateBaseDirectory(req.BaseDirectory); err != nil {
		return err
	}

	if req.Port < 0 || req.Port > 65535 {
		return fmt.Errorf("port must be empty/default or between 1 and 65535")
	}

	return nil
}

func validateRepoURL(repoURL string) error {
	if repoURL == "" {
		return fmt.Errorf("repo_url is required")
	}
	if strings.HasPrefix(repoURL, "/") || strings.HasPrefix(repoURL, "./") || strings.HasPrefix(repoURL, "../") {
		return nil
	}

	parsed, err := url.Parse(repoURL)
	if err != nil || parsed.Scheme == "" {
		return fmt.Errorf("repo_url must be an absolute path or a valid URL")
	}

	switch parsed.Scheme {
	case "http", "https", "ssh", "git", "file":
		return nil
	default:
		return fmt.Errorf("unsupported repo_url scheme: %s", parsed.Scheme)
	}
}

func validateSecretKey(secretKey string) error {
	if !secretKeyPattern.MatchString(secretKey) {
		return fmt.Errorf("invalid secret key: use ENV style keys (A-Z, 0-9, _), max 128 chars")
	}
	return nil
}

func validateSecretValue(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("secret value cannot be empty")
	}
	if len(value) > 65535 {
		return fmt.Errorf("secret value too large")
	}
	return nil
}

func rotateSecretsAtRest(db *store.Store, keyring *secrets.Keyring, projectID string, dryRun bool) (types.RotateSecretsResponse, error) {
	records, err := db.ListSecretRecords(projectID)
	if err != nil {
		return types.RotateSecretsResponse{}, err
	}

	result := types.RotateSecretsResponse{
		ProjectID:    projectID,
		DryRun:       dryRun,
		PrimaryKeyID: keyring.PrimaryKeyID(),
		Total:        len(records),
	}

	for _, record := range records {
		decrypted, err := keyring.Decrypt(record.Ciphertext)
		if err != nil {
			result.Failed++
			if len(result.Errors) < 10 {
				result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: decrypt failed", record.ProjectID, record.Key))
			}
			continue
		}

		if !decrypted.NeedsRotation(keyring.PrimaryKeyID()) {
			result.Skipped++
			continue
		}

		rotatedCiphertext, err := keyring.Encrypt(decrypted.Plaintext)
		if err != nil {
			result.Failed++
			if len(result.Errors) < 10 {
				result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: encrypt failed", record.ProjectID, record.Key))
			}
			continue
		}

		if !dryRun {
			if err := db.UpdateProjectSecretCiphertext(record.ProjectID, record.Key, rotatedCiphertext); err != nil {
				result.Failed++
				if len(result.Errors) < 10 {
					result.Errors = append(result.Errors, fmt.Sprintf("%s/%s: persist failed", record.ProjectID, record.Key))
				}
				continue
			}
		}

		result.Rotated++
	}

	return result, nil
}

func parseLimit(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if parsed <= 0 || parsed > 200 {
		return fallback
	}
	return parsed
}

func actorFromRequest(r *http.Request) string {
	actor := strings.TrimSpace(r.Header.Get("X-Actor"))
	if actor != "" {
		return actor
	}
	return "api-token"
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func atoiOrDefault(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return fallback
}

func fetchAgentDockerStatus(ctx context.Context, client *http.Client, agentURL, agentToken string) (map[string]any, error) {
	targetURL := strings.TrimRight(agentURL, "/") + "/system/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Agent-Token", agentToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("agent docker status failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func proxyAgentDockerAction(ctx context.Context, client *http.Client, agentURL, agentToken string, body []byte) ([]byte, int, error) {
	targetURL := strings.TrimRight(agentURL, "/") + "/system/docker"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Agent-Token", agentToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return nil, 0, err
	}
	return payload, resp.StatusCode, nil
}

func validateDockerfileLocation(value string) (string, error) {
	return normalizeRelativePath(value, "Dockerfile", "dockerfile_location")
}

func validateBaseDirectory(value string) (string, error) {
	return normalizeRelativePath(value, ".", "base_directory")
}

func normalizeDockerfileLocation(value string) string {
	location, err := validateDockerfileLocation(value)
	if err != nil {
		return "Dockerfile"
	}
	return location
}

func normalizeBaseDirectory(value string) string {
	baseDir, err := validateBaseDirectory(value)
	if err != nil {
		return "."
	}
	return baseDir
}

// sanitizeLabelForDomain converts a project name to a DNS-safe label (same logic as deployer).
func sanitizeLabelForDomain(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	replacer := strings.NewReplacer(" ", "-", "_", "-", ".", "-")
	normalized = replacer.Replace(normalized)
	normalized = strings.Trim(normalized, "-")
	if len(normalized) > 63 {
		normalized = strings.Trim(normalized[:63], "-")
	}
	if normalized == "" {
		return "", fmt.Errorf("empty label")
	}
	return normalized, nil
}

// callAgentTeardown asks the agent to stop the container and remove the Caddy route.
// Runs async (called in goroutine); errors are logged only.
func callAgentTeardown(agentURL, agentToken, projectID, domain string) {
	u := strings.TrimRight(agentURL, "/") + "/deploy/" + projectID
	if domain != "" {
		u += "?domain=" + url.QueryEscape(domain)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		slog.Warn("Agent teardown: build request failed", "project_id", projectID, "error", err)
		return
	}
	req.Header.Set("X-Agent-Token", agentToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		slog.Warn("Agent teardown: request failed", "project_id", projectID, "error", err)
		return
	}
	resp.Body.Close()
	slog.Info("Agent teardown completed", "project_id", projectID, "domain", domain, "http_status", resp.StatusCode)
}

// fireNotifyWebhook sends a JSON POST to notifyURL with the deploy result.
// Called asynchronously — errors are only logged.
func fireNotifyWebhook(notifyURL, jobID, projectID, status, domain, errMsg string) {
	payload := map[string]string{
		"job_id":     jobID,
		"project_id": projectID,
		"status":     status,
		"domain":     domain,
		"error":      errMsg,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, notifyURL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("Notify webhook: failed to build request", "url", notifyURL, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("Notify webhook: request failed", "url", notifyURL, "error", err)
		return
	}
	resp.Body.Close()
	slog.Info("Notify webhook fired", "url", notifyURL, "http_status", resp.StatusCode)
}

func normalizeRelativePath(value, fallback, fieldName string) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if trimmed == "" {
		trimmed = fallback
	}

	clean := path.Clean(strings.TrimPrefix(trimmed, "./"))
	if fallback == "." && clean == "" {
		clean = "."
	}
	if clean == "" || clean == "/" {
		clean = fallback
	}

	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("%s must be a relative path", fieldName)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s cannot escape repository root", fieldName)
	}

	if fallback == "Dockerfile" && clean == "." {
		clean = fallback
	}

	return clean, nil
}
