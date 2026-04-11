package types

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
	Dockerfile       string `json:"dockerfile"`               // relative path, e.g., "Dockerfile"
	BaseDirectory    string `json:"base_directory,omitempty"` // relative path, e.g., "." or "apps/web"
	CacheKey         string `json:"cache_key,omitempty"`
	PrebuiltImageTag string `json:"prebuilt_image_tag,omitempty"`
	// BuildMode: "" = docker, "nixpacks" = nixpacks CLI
	BuildMode        string `json:"build_mode,omitempty"`
	// DockerImage: when set, pull this image from Docker Hub instead of building.
	DockerImage      string `json:"docker_image,omitempty"`
}

// ─── Stack / Pipeline types ─────────────────────────────────────────────────

// StackDeployPlan is sent to the agent for multi-container (stack) deployments.
type StackDeployPlan struct {
	ID        string         `json:"id"`
	JobID     string         `json:"job_id"`
	ProjectID string         `json:"project_id"`
	Network   string         `json:"network"`
	Services  []StackService `json:"services"`
	Router    RouterSpec     `json:"router"`
}

// StackService describes one container in a stack.
type StackService struct {
	Key           string            `json:"key"`
	ContainerName string            `json:"container_name"`
	DockerImage   string            `json:"docker_image"`
	Port          int               `json:"port"`
	Internal      bool              `json:"internal"`
	DependsOn     []string          `json:"depends_on,omitempty"`
	Env           map[string]string `json:"env"`
	Volume        string            `json:"volume,omitempty"`
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

const (
	DeployCallbackStatusRunning   = "running"
	DeployCallbackStatusSucceeded = "succeeded"
	DeployCallbackStatusFailed    = "failed"
)

type DeployCallback struct {
	JobID    string `json:"job_id"`
	PlanID   string `json:"plan_id"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Domain   string `json:"domain,omitempty"`
	HostPort string `json:"host_port,omitempty"`
	ImageTag string `json:"image_tag,omitempty"`
}
