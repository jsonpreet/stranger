package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/stranger/control-plane/internal/deployer"
	"github.com/stranger/control-plane/internal/queue"
	"github.com/stranger/control-plane/internal/secrets"
	"github.com/stranger/control-plane/internal/store"
	"github.com/stranger/control-plane/types"
)

var projectNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-_ ]{1,62}$`)
var secretKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

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

	logProxyClient := &http.Client{}

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
				CreatedAt:          time.Now().UTC(),
			}

			if err := db.CreateProject(project); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			_ = db.CreateAuditEvent(types.AuditEvent{
				ID:           uuid.NewString(),
				Action:       "project.created",
				Actor:        actorFromRequest(r),
				ResourceType: "project",
				ResourceID:   project.ID,
				Metadata: fmt.Sprintf(
					`{"template":%q,"dockerfile_location":%q,"base_directory":%q}`,
					project.Template,
					project.DockerfileLocation,
					project.BaseDirectory,
				),
				CreatedAt: time.Now().UTC(),
			})

			writeJSON(w, http.StatusCreated, project)
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}))

	mux.HandleFunc("/projects/", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
		projectID, secretKey, ok := parseProjectSecretPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}

		if _, err := db.GetProject(projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "Project not found", http.StatusNotFound)
				return
			}
			http.Error(w, "DB error", http.StatusInternalServerError)
			return
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

	mux.HandleFunc("/deploy", withAPIAuth(apiToken, func(w http.ResponseWriter, r *http.Request) {
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

		writeJSON(w, http.StatusOK, map[string]string{
			"status": req.Status,
			"job_id": req.JobID,
		})
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

func validateCreateProjectRequest(req types.CreateProjectRequest) error {
	name := strings.TrimSpace(req.Name)
	repo := strings.TrimSpace(req.RepoURL)
	template := strings.ToLower(strings.TrimSpace(req.Template))

	if !projectNamePattern.MatchString(name) {
		return fmt.Errorf("invalid name: use 2-63 chars, letters/numbers/spaces/dash/underscore")
	}

	if err := validateRepoURL(repo); err != nil {
		return err
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

func parseProjectSecretPath(path string) (projectID, secretKey string, ok bool) {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 3 && parts[0] == "projects" && parts[2] == "secrets" {
		projectID = strings.TrimSpace(parts[1])
		return projectID, "", projectID != ""
	}
	if len(parts) == 4 && parts[0] == "projects" && parts[2] == "secrets" {
		projectID = strings.TrimSpace(parts[1])
		decodedKey, err := url.PathUnescape(parts[3])
		if err != nil {
			return "", "", false
		}
		secretKey = strings.TrimSpace(decodedKey)
		return projectID, secretKey, projectID != "" && secretKey != ""
	}
	return "", "", false
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
