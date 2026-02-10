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
}
