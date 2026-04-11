package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stranger/control-plane/internal/deployer"
	"github.com/stranger/control-plane/internal/secrets"
	"github.com/stranger/control-plane/internal/store"
	"github.com/stranger/control-plane/types"
)

type Dispatcher struct {
	store        *store.Store
	keyring      *secrets.Keyring
	agentURL     string
	agentToken   string
	pollInterval time.Duration
	client       *http.Client
}

func NewDispatcher(store *store.Store, keyring *secrets.Keyring, agentURL, agentToken string) *Dispatcher {
	return &Dispatcher{
		store:        store,
		keyring:      keyring,
		agentURL:     strings.TrimRight(agentURL, "/"),
		agentToken:   agentToken,
		pollInterval: 500 * time.Millisecond,
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (d *Dispatcher) Start(ctx context.Context) {
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	slog.Info("Deploy dispatcher started", "poll_interval", d.pollInterval.String())

	for {
		select {
		case <-ctx.Done():
			slog.Info("Deploy dispatcher stopping")
			return
		case <-ticker.C:
			if err := d.dispatchOnce(ctx); err != nil {
				slog.Error("Dispatcher iteration failed", "error", err)
			}
		}
	}
}

// StartWatchdog launches a background goroutine that marks stale jobs as failed.
func (d *Dispatcher) StartWatchdog(ctx context.Context) {
	go d.runWatchdog(ctx)
}

func (d *Dispatcher) runWatchdog(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	slog.Info("Stale job watchdog started")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, err := d.store.MarkStaleJobsFailed(15 * time.Minute)
			if err != nil {
				slog.Error("Watchdog: failed to mark stale jobs", "error", err)
			} else if count > 0 {
				slog.Warn("Watchdog: marked stale jobs as failed", "count", count)
			}
		}
	}
}

func (d *Dispatcher) dispatchOnce(ctx context.Context) error {
	job, err := d.store.ClaimNextDeployJob()
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}

	slog.Info("Dispatching deploy job", "job_id", job.ID, "project_id", job.ProjectID, "attempt", job.AttemptCount)
	return d.processJob(ctx, *job)
}

func (d *Dispatcher) processJob(ctx context.Context, job types.DeployJob) error {
	project, err := d.store.GetProject(job.ProjectID)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("project lookup failed: %v", err))
	}

	// Stack project: multi-service pipeline path
	if project.StackConfig != "" && deployer.IsStackTemplate(project.Template) {
		return d.processStackJob(ctx, job, project)
	}

	// Single-container path (existing logic)
	plan, err := deployer.GeneratePlan(project)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("plan generation failed: %v", err))
	}
	plan.JobID = job.ID

	// Rollback: use the prebuilt image tag instead of building from source
	if job.PrebuiltImageTag != "" {
		plan.Build.PrebuiltImageTag = job.PrebuiltImageTag
	}

	secretValues, err := d.loadSecretEnv(job.ProjectID)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("secret decryption failed: %v", err))
	}
	for key, value := range secretValues {
		plan.Runtime.Env[key] = value
	}

	payload, err := json.Marshal(plan)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("plan serialization failed: %v", err))
	}

	// Dispatch with retry: 3 attempts, backoff 2s → 4s → 8s
	backoffs := []time.Duration{0, 2 * time.Second, 4 * time.Second}
	var resp *http.Response
	var doErr error

	for attempt, backoff := range backoffs {
		if attempt > 0 {
			slog.Info("Retrying agent dispatch", "job_id", job.ID, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return d.failJob(job.ID, "dispatch cancelled during retry")
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.agentURL+"/deploy", bytes.NewReader(payload))
		if err != nil {
			return d.failJob(job.ID, fmt.Sprintf("agent request build failed: %v", err))
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-Token", d.agentToken)

		resp, doErr = d.client.Do(req)
		if doErr != nil {
			slog.Warn("Agent dispatch attempt failed (network)", "job_id", job.ID, "attempt", attempt+1, "error", doErr)
			continue // network error: retry
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// 4xx: permanent failure, don't retry
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return d.failJob(job.ID, fmt.Sprintf("agent rejected deploy (%d): %s", resp.StatusCode, strings.TrimSpace(string(body))))
		}

		if resp.StatusCode >= 500 {
			// 5xx: transient, retry
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			doErr = fmt.Errorf("agent server error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
			resp = nil
			slog.Warn("Agent dispatch 5xx, will retry", "job_id", job.ID, "attempt", attempt+1)
			continue
		}

		break // success (2xx)
	}

	if doErr != nil || resp == nil {
		return d.failJob(job.ID, fmt.Sprintf("agent dispatch failed after %d attempts: %v", len(backoffs), doErr))
	}
	defer resp.Body.Close()

	if err := d.store.MarkDeployJobDispatched(job.ID, plan.ID); err != nil {
		return err
	}

	_ = d.store.CreateAuditEvent(types.AuditEvent{
		ID:           uuid.NewString(),
		Action:       "deploy.dispatched",
		Actor:        "system:dispatcher",
		ResourceType: "deploy_job",
		ResourceID:   job.ID,
		Metadata:     fmt.Sprintf(`{"plan_id":%q,"project_id":%q}`, plan.ID, job.ProjectID),
	})

	slog.Info("Deploy job dispatched", "job_id", job.ID, "plan_id", plan.ID, "project_id", job.ProjectID)
	return nil
}

// processStackJob handles multi-service (pipeline) deployments.
func (d *Dispatcher) processStackJob(ctx context.Context, job types.DeployJob, project types.Project) error {
	slog.Info("Dispatching stack deploy job", "job_id", job.ID, "project_id", job.ProjectID)

	plan, exposedSecrets, err := deployer.GenerateStackPlan(project)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("stack plan generation failed: %v", err))
	}
	plan.JobID = job.ID

	// Persist generated passwords as encrypted project secrets
	for key, value := range exposedSecrets {
		ciphertext, encErr := d.keyring.Encrypt(value)
		if encErr != nil {
			slog.Warn("Failed to encrypt stack secret", "key", key, "error", encErr)
			continue
		}
		if _, err := d.store.UpsertProjectSecret(project.ID, key, ciphertext); err != nil {
			slog.Warn("Failed to store stack secret", "key", key, "error", err)
		}
	}

	payload, err := json.Marshal(plan)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("stack plan serialization failed: %v", err))
	}

	// Dispatch to /deploy/stack
	backoffs := []time.Duration{0, 2 * time.Second, 4 * time.Second}
	var resp *http.Response
	var doErr error

	for attempt, backoff := range backoffs {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return d.failJob(job.ID, "dispatch cancelled during retry")
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.agentURL+"/deploy/stack", bytes.NewReader(payload))
		if err != nil {
			return d.failJob(job.ID, fmt.Sprintf("stack agent request build failed: %v", err))
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-Token", d.agentToken)

		resp, doErr = d.client.Do(req)
		if doErr != nil {
			slog.Warn("Stack agent dispatch attempt failed", "job_id", job.ID, "attempt", attempt+1, "error", doErr)
			continue
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return d.failJob(job.ID, fmt.Sprintf("agent rejected stack deploy (%d): %s", resp.StatusCode, strings.TrimSpace(string(body))))
		}
		if resp.StatusCode >= 500 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			doErr = fmt.Errorf("stack agent 5xx (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
			resp = nil
			continue
		}
		break
	}

	if doErr != nil || resp == nil {
		return d.failJob(job.ID, fmt.Sprintf("stack agent dispatch failed after %d attempts: %v", len(backoffs), doErr))
	}
	defer resp.Body.Close()

	if err := d.store.MarkDeployJobDispatched(job.ID, plan.ID); err != nil {
		return err
	}

	_ = d.store.CreateAuditEvent(types.AuditEvent{
		ID:           uuid.NewString(),
		Action:       "deploy.stack.dispatched",
		Actor:        "system:dispatcher",
		ResourceType: "deploy_job",
		ResourceID:   job.ID,
		Metadata:     fmt.Sprintf(`{"plan_id":%q,"project_id":%q}`, plan.ID, project.ID),
	})

	slog.Info("Stack deploy job dispatched", "job_id", job.ID, "plan_id", plan.ID)
	return nil
}

func (d *Dispatcher) failJob(jobID, reason string) error {
	if err := d.store.MarkDeployJobFailed(jobID, reason); err != nil {
		return err
	}

	_ = d.store.CreateAuditEvent(types.AuditEvent{
		ID:           uuid.NewString(),
		Action:       "deploy.failed",
		Actor:        "system:dispatcher",
		ResourceType: "deploy_job",
		ResourceID:   jobID,
		Metadata:     fmt.Sprintf(`{"reason":%q}`, reason),
	})

	slog.Error("Deploy job failed", "job_id", jobID, "reason", reason)
	return nil
}

func (d *Dispatcher) loadSecretEnv(projectID string) (map[string]string, error) {
	secretsList, err := d.store.ListProjectSecretCiphertexts(projectID)
	if err != nil {
		return nil, err
	}

	envValues := make(map[string]string, len(secretsList))
	for _, item := range secretsList {
		decrypted, err := d.keyring.Decrypt(item.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("key %s: %w", item.Key, err)
		}

		if decrypted.NeedsRotation(d.keyring.PrimaryKeyID()) {
			rotatedCiphertext, err := d.keyring.Encrypt(decrypted.Plaintext)
			if err != nil {
				return nil, fmt.Errorf("key %s rotation failed: %w", item.Key, err)
			}
			if err := d.store.UpdateProjectSecretCiphertext(projectID, item.Key, rotatedCiphertext); err != nil {
				slog.Warn("Failed to persist rotated secret", "project_id", projectID, "key", item.Key, "error", err)
			}
		}

		envValues[item.Key] = decrypted.Plaintext
	}
	return envValues, nil
}
