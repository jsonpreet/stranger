package types

import "time"

const (
	DeployJobStatusQueued      = "queued"
	DeployJobStatusDispatching = "dispatching"
	DeployJobStatusDispatched  = "dispatched"
	DeployJobStatusRunning     = "running"
	DeployJobStatusSucceeded   = "succeeded"
	DeployJobStatusFailed      = "failed"
)

type Project struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	RepoURL            string    `json:"repo_url"`
	Template           string    `json:"template"`
	DockerfileLocation string    `json:"dockerfile_location"`
	BaseDirectory      string    `json:"base_directory"`
	CreatedAt          time.Time `json:"created_at"`
}

type CreateProjectRequest struct {
	Name               string `json:"name"`
	RepoURL            string `json:"repo_url"`
	Template           string `json:"template"`
	DockerfileLocation string `json:"dockerfile_location,omitempty"`
	BaseDirectory      string `json:"base_directory,omitempty"`
}

type UpsertProjectSecretRequest struct {
	Value string `json:"value"`
}

type RotateSecretsRequest struct {
	ProjectID string `json:"project_id,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

type RotateSecretsResponse struct {
	ProjectID    string   `json:"project_id,omitempty"`
	DryRun       bool     `json:"dry_run"`
	PrimaryKeyID string   `json:"primary_key_id"`
	Total        int      `json:"total"`
	Rotated      int      `json:"rotated"`
	Skipped      int      `json:"skipped"`
	Failed       int      `json:"failed"`
	Errors       []string `json:"errors,omitempty"`
}

type DeployRequest struct {
	ProjectID string `json:"project_id"`
}

type DeployCallbackRequest struct {
	JobID    string `json:"job_id"`
	PlanID   string `json:"plan_id"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Domain   string `json:"domain,omitempty"`
	HostPort string `json:"host_port,omitempty"`
}

type DeployJob struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"project_id"`
	Status       string     `json:"status"`
	PlanID       string     `json:"plan_id,omitempty"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	AttemptCount int        `json:"attempt_count"`
}

type DeployPlan struct {
	ID        string      `json:"id"`
	JobID     string      `json:"job_id"`
	ProjectID string      `json:"project_id"`
	Template  string      `json:"template"`
	Build     BuildSpec   `json:"build"`
	Runtime   RuntimeSpec `json:"runtime"`
	Router    RouterSpec  `json:"router"`
}

type BuildSpec struct {
	RepoURL       string `json:"repo_url"`
	Dockerfile    string `json:"dockerfile"`               // relative path, e.g., "Dockerfile"
	BaseDirectory string `json:"base_directory,omitempty"` // relative path, e.g., "." or "apps/web"
}

type RuntimeSpec struct {
	Port int               `json:"port"`
	Env  map[string]string `json:"env"`
}

type RouterSpec struct {
	Domain string `json:"domain"`
}

type AuditEvent struct {
	ID           string    `json:"id"`
	Action       string    `json:"action"`
	Actor        string    `json:"actor"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Metadata     string    `json:"metadata,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type ProjectSecret struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
