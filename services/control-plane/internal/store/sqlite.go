package store

import (
	"database/sql"
	"log/slog"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stranger/control-plane/types"
)

type Store struct {
	db *sql.DB
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	if err := initSchema(db); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS projects (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		repo_url TEXT NOT NULL,
		template TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := db.Exec(schema)
	return err
}

func (s *Store) CreateProject(p types.Project) error {
	_, err := s.db.Exec("INSERT INTO projects (id, name, repo_url, template, created_at) VALUES (?, ?, ?, ?, ?)",
		p.ID, p.Name, p.RepoURL, p.Template, p.CreatedAt)
	return err
}

func (s *Store) ListProjects() ([]types.Project, error) {
	rows, err := s.db.Query("SELECT id, name, repo_url, template, created_at FROM projects ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []types.Project
	for rows.Next() {
		var p types.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.RepoURL, &p.Template, &p.CreatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}
