package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stranger/control-plane/types"
)

type Store struct {
	db *sql.DB
}

type ProjectSecretCiphertext struct {
	Key        string
	Ciphertext string
}

type SecretRecord struct {
	ProjectID  string
	Key        string
	Ciphertext string
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;`); err != nil {
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
		dockerfile_location TEXT NOT NULL DEFAULT 'Dockerfile',
		base_directory TEXT NOT NULL DEFAULT '.',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS deploy_jobs (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		status TEXT NOT NULL,
		plan_id TEXT,
		error TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		started_at DATETIME,
		finished_at DATETIME,
		attempt_count INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_deploy_jobs_status_created_at ON deploy_jobs(status, created_at);
	CREATE INDEX IF NOT EXISTS idx_deploy_jobs_project_created_at ON deploy_jobs(project_id, created_at DESC);

	CREATE TABLE IF NOT EXISTS audit_events (
		id TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		actor TEXT NOT NULL,
		resource_type TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		metadata TEXT,
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS project_secrets (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		key_name TEXT NOT NULL,
		ciphertext TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE(project_id, key_name),
		FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_project_secrets_project ON project_secrets(project_id, key_name);
	CREATE INDEX IF NOT EXISTS idx_audit_events_created_at ON audit_events(created_at DESC);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	if err := ensureProjectColumns(db); err != nil {
		return err
	}

	return nil
}

func (s *Store) CreateProject(p types.Project) error {
	_, err := s.db.Exec(
		`INSERT INTO projects (id, name, repo_url, template, dockerfile_location, base_directory, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.ID,
		p.Name,
		p.RepoURL,
		p.Template,
		defaultString(p.DockerfileLocation, "Dockerfile"),
		defaultString(p.BaseDirectory, "."),
		p.CreatedAt,
	)
	return err
}

func (s *Store) ListProjects() ([]types.Project, error) {
	rows, err := s.db.Query(
		`SELECT id, name, repo_url, template,
		COALESCE(NULLIF(dockerfile_location, ''), 'Dockerfile'),
		COALESCE(NULLIF(base_directory, ''), '.'),
		created_at
		FROM projects
		ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []types.Project
	for rows.Next() {
		var p types.Project
		if err := rows.Scan(
			&p.ID,
			&p.Name,
			&p.RepoURL,
			&p.Template,
			&p.DockerfileLocation,
			&p.BaseDirectory,
			&p.CreatedAt,
		); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *Store) GetProject(id string) (types.Project, error) {
	var project types.Project
	err := s.db.QueryRow(
		`SELECT id, name, repo_url, template,
		COALESCE(NULLIF(dockerfile_location, ''), 'Dockerfile'),
		COALESCE(NULLIF(base_directory, ''), '.'),
		created_at
		FROM projects
		WHERE id = ?`,
		id,
	).Scan(
		&project.ID,
		&project.Name,
		&project.RepoURL,
		&project.Template,
		&project.DockerfileLocation,
		&project.BaseDirectory,
		&project.CreatedAt,
	)
	return project, err
}

func (s *Store) CreateDeployJob(job types.DeployJob) error {
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = job.CreatedAt
	}

	_, err := s.db.Exec(
		`INSERT INTO deploy_jobs
		(id, project_id, status, plan_id, error, created_at, updated_at, started_at, finished_at, attempt_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID,
		job.ProjectID,
		job.Status,
		nullIfEmpty(job.PlanID),
		nullIfEmpty(job.Error),
		job.CreatedAt,
		job.UpdatedAt,
		job.StartedAt,
		job.FinishedAt,
		job.AttemptCount,
	)
	return err
}

func (s *Store) ClaimNextDeployJob() (*types.DeployJob, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRow(`
		SELECT id, project_id, status, plan_id, error, created_at, updated_at, started_at, finished_at, attempt_count
		FROM deploy_jobs
		WHERE status = ?
		ORDER BY created_at ASC
		LIMIT 1
	`, types.DeployJobStatusQueued)

	job, err := scanDeployJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	_, err = tx.Exec(
		`UPDATE deploy_jobs
		SET status = ?, updated_at = ?, started_at = COALESCE(started_at, ?), attempt_count = attempt_count + 1
		WHERE id = ?`,
		types.DeployJobStatusDispatching,
		now,
		now,
		job.ID,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	job.Status = types.DeployJobStatusDispatching
	job.UpdatedAt = now
	if job.StartedAt == nil {
		job.StartedAt = &now
	}
	job.AttemptCount++

	return &job, nil
}

func (s *Store) MarkDeployJobDispatched(jobID, planID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE deploy_jobs
		SET status = ?, plan_id = ?, error = NULL, updated_at = ?
		WHERE id = ? AND status = ?`,
		types.DeployJobStatusDispatched,
		planID,
		now,
		jobID,
		types.DeployJobStatusDispatching,
	)
	return err
}

func (s *Store) MarkDeployJobRunning(jobID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE deploy_jobs
		SET status = ?, updated_at = ?, error = NULL
		WHERE id = ? AND status IN (?, ?)`,
		types.DeployJobStatusRunning,
		now,
		jobID,
		types.DeployJobStatusDispatching,
		types.DeployJobStatusDispatched,
	)
	return err
}

func (s *Store) MarkDeployJobSucceeded(jobID, planID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE deploy_jobs
		SET status = ?, plan_id = COALESCE(NULLIF(?, ''), plan_id), error = NULL, updated_at = ?, finished_at = ?
		WHERE id = ? AND status IN (?, ?, ?)`,
		types.DeployJobStatusSucceeded,
		planID,
		now,
		now,
		jobID,
		types.DeployJobStatusDispatching,
		types.DeployJobStatusDispatched,
		types.DeployJobStatusRunning,
	)
	return err
}

func (s *Store) MarkDeployJobFailed(jobID, errorMessage string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE deploy_jobs
		SET status = ?, error = ?, updated_at = ?, finished_at = ?
		WHERE id = ? AND status IN (?, ?, ?, ?)`,
		types.DeployJobStatusFailed,
		errorMessage,
		now,
		now,
		jobID,
		types.DeployJobStatusQueued,
		types.DeployJobStatusDispatching,
		types.DeployJobStatusDispatched,
		types.DeployJobStatusRunning,
	)
	return err
}

func (s *Store) ListDeployJobs(projectID string, limit int) ([]types.DeployJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := `
		SELECT id, project_id, status, plan_id, error, created_at, updated_at, started_at, finished_at, attempt_count
		FROM deploy_jobs
	`
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id = ?"
		args = append(args, projectID)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []types.DeployJob
	for rows.Next() {
		job, err := scanDeployJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) GetDeployJob(id string) (types.DeployJob, error) {
	row := s.db.QueryRow(`
		SELECT id, project_id, status, plan_id, error, created_at, updated_at, started_at, finished_at, attempt_count
		FROM deploy_jobs
		WHERE id = ?
	`, id)
	return scanDeployJob(row)
}

func (s *Store) CreateAuditEvent(event types.AuditEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_events (id, action, actor, resource_type, resource_id, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.ID,
		event.Action,
		event.Actor,
		event.ResourceType,
		event.ResourceID,
		nullIfEmpty(event.Metadata),
		event.CreatedAt,
	)
	return err
}

func (s *Store) UpsertProjectSecret(projectID, keyName, cipherText string) (types.ProjectSecret, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO project_secrets (id, project_id, key_name, ciphertext, created_at, updated_at)
		VALUES (lower(hex(randomblob(16))), ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, key_name)
		DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		projectID,
		keyName,
		cipherText,
		now,
		now,
	)
	if err != nil {
		return types.ProjectSecret{}, err
	}

	return s.GetProjectSecret(projectID, keyName)
}

func (s *Store) GetProjectSecret(projectID, keyName string) (types.ProjectSecret, error) {
	var secret types.ProjectSecret
	err := s.db.QueryRow(
		`SELECT id, project_id, key_name, created_at, updated_at
		FROM project_secrets
		WHERE project_id = ? AND key_name = ?`,
		projectID,
		keyName,
	).Scan(&secret.ID, &secret.ProjectID, &secret.Key, &secret.CreatedAt, &secret.UpdatedAt)
	return secret, err
}

func (s *Store) ListProjectSecrets(projectID string) ([]types.ProjectSecret, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, key_name, created_at, updated_at
		FROM project_secrets
		WHERE project_id = ?
		ORDER BY key_name ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []types.ProjectSecret
	for rows.Next() {
		var secret types.ProjectSecret
		if err := rows.Scan(&secret.ID, &secret.ProjectID, &secret.Key, &secret.CreatedAt, &secret.UpdatedAt); err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, rows.Err()
}

func (s *Store) ListProjectSecretCiphertexts(projectID string) ([]ProjectSecretCiphertext, error) {
	rows, err := s.db.Query(
		`SELECT key_name, ciphertext
		FROM project_secrets
		WHERE project_id = ?`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var secrets []ProjectSecretCiphertext
	for rows.Next() {
		var secret ProjectSecretCiphertext
		if err := rows.Scan(&secret.Key, &secret.Ciphertext); err != nil {
			return nil, err
		}
		secrets = append(secrets, secret)
	}
	return secrets, rows.Err()
}

func (s *Store) DeleteProjectSecret(projectID, keyName string) (bool, error) {
	res, err := s.db.Exec(
		`DELETE FROM project_secrets WHERE project_id = ? AND key_name = ?`,
		projectID,
		keyName,
	)
	if err != nil {
		return false, err
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *Store) ListSecretRecords(projectID string) ([]SecretRecord, error) {
	query := `
		SELECT project_id, key_name, ciphertext
		FROM project_secrets
	`
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id = ?"
		args = append(args, projectID)
	}
	query += " ORDER BY project_id ASC, key_name ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []SecretRecord
	for rows.Next() {
		var record SecretRecord
		if err := rows.Scan(&record.ProjectID, &record.Key, &record.Ciphertext); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) UpdateProjectSecretCiphertext(projectID, keyName, cipherText string) error {
	_, err := s.db.Exec(
		`UPDATE project_secrets
		SET ciphertext = ?, updated_at = ?
		WHERE project_id = ? AND key_name = ?`,
		cipherText,
		time.Now().UTC(),
		projectID,
		keyName,
	)
	return err
}

func scanDeployJob(scanner interface{ Scan(dest ...any) error }) (types.DeployJob, error) {
	var job types.DeployJob
	var planID sql.NullString
	var errorMessage sql.NullString
	var startedAt sql.NullTime
	var finishedAt sql.NullTime

	err := scanner.Scan(
		&job.ID,
		&job.ProjectID,
		&job.Status,
		&planID,
		&errorMessage,
		&job.CreatedAt,
		&job.UpdatedAt,
		&startedAt,
		&finishedAt,
		&job.AttemptCount,
	)
	if err != nil {
		return types.DeployJob{}, err
	}

	if planID.Valid {
		job.PlanID = planID.String
	}
	if errorMessage.Valid {
		job.Error = errorMessage.String
	}
	if startedAt.Valid {
		startedAtValue := startedAt.Time
		job.StartedAt = &startedAtValue
	}
	if finishedAt.Valid {
		finishedAtValue := finishedAt.Time
		job.FinishedAt = &finishedAtValue
	}

	return job, nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func ensureProjectColumns(db *sql.DB) error {
	columns, err := projectColumns(db)
	if err != nil {
		return err
	}

	if !columns["dockerfile_location"] {
		if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN dockerfile_location TEXT NOT NULL DEFAULT 'Dockerfile'`); err != nil {
			return fmt.Errorf("add dockerfile_location column: %w", err)
		}
	}

	if !columns["base_directory"] {
		if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN base_directory TEXT NOT NULL DEFAULT '.'`); err != nil {
			return fmt.Errorf("add base_directory column: %w", err)
		}
	}

	return nil
}

func projectColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(projects)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}
