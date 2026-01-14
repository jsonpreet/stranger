package types

type DeployPlan struct {
	ID        string            `json:"id"`
	Template  string            `json:"template"`
	Build     BuildSpec         `json:"build"`
	Runtime   RuntimeSpec       `json:"runtime"`
	Router    RouterSpec        `json:"router"`
}

type BuildSpec struct {
	RepoURL    string `json:"repo_url"`
	Dockerfile string `json:"dockerfile"` // relative path, e.g., "./Dockerfile"
}

type RuntimeSpec struct {
	Port int               `json:"port"`
	Env  map[string]string `json:"env"`
}

type RouterSpec struct {
	Domain string `json:"domain"`
}
