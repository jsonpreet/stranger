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

	plan, err := deployer.GeneratePlan(project)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("plan generation failed: %v", err))
	}
	plan.JobID = job.ID
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.agentURL+"/deploy", bytes.NewReader(payload))
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("agent request build failed: %v", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", d.agentToken)

	resp, err := d.client.Do(req)
	if err != nil {
		return d.failJob(job.ID, fmt.Sprintf("agent dispatch failed: %v", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return d.failJob(job.ID, fmt.Sprintf("agent rejected deploy (%d): %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}

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
