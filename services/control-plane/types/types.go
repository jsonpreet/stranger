package types

import "time"

type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	RepoURL   string    `json:"repo_url"`
	Template  string    `json:"template"`
	CreatedAt time.Time `json:"created_at"`
}

type DeployRequest struct {
	ProjectID string `json:"project_id"`
}
