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
	Port               int       `json:"port,omitempty"`
	CustomDomain       string    `json:"custom_domain,omitempty"`
	MemoryLimit        string    `json:"memory_limit,omitempty"`
	CPULimit           string    `json:"cpu_limit,omitempty"`
	NotifyURL          string    `json:"notify_url,omitempty"`
	// StackConfig holds the JSON-encoded stack definition for multi-service apps.
	// Empty for single-container projects.
	StackConfig        string    `json:"stack_config,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type CreateProjectRequest struct {
	Name               string `json:"name"`
	RepoURL            string `json:"repo_url"`
	Template           string `json:"template"`
	DockerfileLocation string `json:"dockerfile_location,omitempty"`
	BaseDirectory      string `json:"base_directory,omitempty"`
	Port               int    `json:"port,omitempty"`
	CustomDomain       string `json:"custom_domain,omitempty"`
	MemoryLimit        string `json:"memory_limit,omitempty"`
	CPULimit           string `json:"cpu_limit,omitempty"`
	NotifyURL          string `json:"notify_url,omitempty"`
	// StackConfig is the JSON-encoded stack definition sent from the marketplace.
	StackConfig        string `json:"stack_config,omitempty"`
	// InitialSecrets are stored as encrypted project secrets immediately on project creation.
	// Stack apps use this to persist admin credentials (ADMIN_EMAIL, ADMIN_PASSWORD, etc.)
	// so the welcome email and credential display work without a first deploy.
	InitialSecrets     map[string]string `json:"initial_secrets,omitempty"`
}

type UpdateProjectSettingsRequest struct {
	CustomDomain string `json:"custom_domain"`
	MemoryLimit  string `json:"memory_limit"`
	CPULimit     string `json:"cpu_limit"`
	NotifyURL    string `json:"notify_url"`
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

type RollbackRequest struct {
	ProjectID string `json:"project_id"`
}

type DeployCallbackRequest struct {
	JobID     string `json:"job_id"`
	PlanID    string `json:"plan_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Domain    string `json:"domain,omitempty"`
	HostPort  string `json:"host_port,omitempty"`
	ImageTag  string `json:"image_tag,omitempty"`
}

type DeployJob struct {
	ID               string     `json:"id"`
	ProjectID        string     `json:"project_id"`
	Status           string     `json:"status"`
	PlanID           string     `json:"plan_id,omitempty"`
	Error            string     `json:"error,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	AttemptCount     int        `json:"attempt_count"`
	ImageTag         string     `json:"image_tag,omitempty"`
	PrebuiltImageTag string     `json:"prebuilt_image_tag,omitempty"`
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
	RepoURL          string `json:"repo_url"`
	Dockerfile       string `json:"dockerfile"`
	BaseDirectory    string `json:"base_directory,omitempty"`
	CacheKey         string `json:"cache_key,omitempty"`
	PrebuiltImageTag string `json:"prebuilt_image_tag,omitempty"`
	// BuildMode: "" = docker, "nixpacks" = nixpacks CLI
	BuildMode        string `json:"build_mode,omitempty"`
	// DockerImage: when set, pull this image from Docker Hub instead of building.
	DockerImage      string `json:"docker_image,omitempty"`
}

// ─── Stack / Pipeline types ───────────────────────────────────────────────────

// StackDeployPlan is sent to the agent for multi-container (stack) deployments.
type StackDeployPlan struct {
	ID        string         `json:"id"`
	JobID     string         `json:"job_id"`
	ProjectID string         `json:"project_id"`
	// Network is the Docker bridge network name shared by all services.
	Network   string         `json:"network"`
	Services  []StackService `json:"services"`
	Router    RouterSpec     `json:"router"`
}

// StackService describes one container in a stack.
type StackService struct {
	Key           string            `json:"key"`           // logical name, e.g. "db", "app"
	ContainerName string            `json:"container_name"` // e.g. "stack-<projectID>-db"
	DockerImage   string            `json:"docker_image"`   // e.g. "mysql:8.0"
	Port          int               `json:"port"`
	Internal      bool              `json:"internal"`      // true = no Caddy route
	DependsOn     []string          `json:"depends_on,omitempty"`
	Env           map[string]string `json:"env"`
	Volume        string            `json:"volume,omitempty"` // named Docker volume
}

type RuntimeSpec struct {
	Port        int               `json:"port"`
	Env         map[string]string `json:"env"`
	MemoryLimit string            `json:"memory_limit,omitempty"`
	CPULimit    string            `json:"cpu_limit,omitempty"`
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
