package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrRepoJobNotFound is returned when a repo job id does not exist.
var ErrRepoJobNotFound = errors.New("repo job not found")

const (
	RepoJobKindPull   = "pull"
	RepoJobKindBranch = "branch"

	RepoJobStatusRunning = "running"
	RepoJobStatusSuccess = "success"
	RepoJobStatusError   = "error"
)

// Repository represents a Git repository record
type Repository struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	URL        string     `json:"url"`
	Branch     string     `json:"branch"`
	AuthType   string     `json:"auth_type"` // "none", "https", "ssh"
	Username   string     `json:"username,omitempty"`
	Password   string     `json:"password,omitempty"`
	SSHKey     string     `json:"ssh_key,omitempty"`
	LastCommit string     `json:"last_commit,omitempty"`
	LastPulled *time.Time `json:"last_pulled,omitempty"`
	Status     string     `json:"status"` // "idle", "cloning", "pulling", "error"
	CreatedAt  time.Time  `json:"created_at"`
}

// AddRepository adds a new repository to the database
func AddRepository(repo *Repository) (int64, error) {
	if repo.Status == "" {
		repo.Status = "idle"
	}

	result, err := DB.Exec(`
		INSERT INTO repositories (name, url, branch, auth_type, username, password, ssh_key, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		repo.Name, repo.URL, repo.Branch, repo.AuthType, repo.Username, repo.Password, repo.SSHKey, repo.Status,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to insert repository: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

// GetRepository retrieves a repository by ID
func GetRepository(id int) (*Repository, error) {
	repo := &Repository{}
	var lastPulled sql.NullTime
	var lastCommit sql.NullString

	err := DB.QueryRow(`
		SELECT id, name, url, branch, auth_type, username, password, ssh_key,
		       last_commit, last_pulled, status, created_at
		FROM repositories WHERE id = ?`, id).Scan(
		&repo.ID, &repo.Name, &repo.URL, &repo.Branch, &repo.AuthType, &repo.Username,
		&repo.Password, &repo.SSHKey, &lastCommit, &lastPulled, &repo.Status, &repo.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("repository not found")
		}
		return nil, fmt.Errorf("failed to query repository: %w", err)
	}

	if lastPulled.Valid {
		repo.LastPulled = &lastPulled.Time
	}

	if lastCommit.Valid {
		repo.LastCommit = lastCommit.String
	}

	return repo, nil
}

// GetRepositoryByName retrieves a repository by name
func GetRepositoryByName(name string) (*Repository, error) {
	repo := &Repository{}
	var lastPulled sql.NullTime
	var lastCommit sql.NullString

	err := DB.QueryRow(`
		SELECT id, name, url, branch, auth_type, username, password, ssh_key,
		       last_commit, last_pulled, status, created_at
		FROM repositories WHERE name = ?`, name).Scan(
		&repo.ID, &repo.Name, &repo.URL, &repo.Branch, &repo.AuthType, &repo.Username,
		&repo.Password, &repo.SSHKey, &lastCommit, &lastPulled, &repo.Status, &repo.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("repository not found")
		}
		return nil, fmt.Errorf("failed to query repository: %w", err)
	}

	if lastPulled.Valid {
		repo.LastPulled = &lastPulled.Time
	}

	if lastCommit.Valid {
		repo.LastCommit = lastCommit.String
	}

	return repo, nil
}

// ListRepositories returns all repositories
func ListRepositories() ([]*Repository, error) {
	rows, err := DB.Query(`
		SELECT id, name, url, branch, auth_type, username, password, ssh_key,
		       last_commit, last_pulled, status, created_at
		FROM repositories ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("failed to query repositories: %w", err)
	}
	defer rows.Close()

	var repos []*Repository
	for rows.Next() {
		repo := &Repository{}
		var lastPulled sql.NullTime
		var lastCommit sql.NullString

		err := rows.Scan(
			&repo.ID, &repo.Name, &repo.URL, &repo.Branch, &repo.AuthType, &repo.Username,
			&repo.Password, &repo.SSHKey, &lastCommit, &lastPulled, &repo.Status, &repo.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan repository: %w", err)
		}

		if lastPulled.Valid {
			repo.LastPulled = &lastPulled.Time
		}

		if lastCommit.Valid {
			repo.LastCommit = lastCommit.String
		}

		repos = append(repos, repo)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating repositories: %w", err)
	}

	return repos, nil
}

// UpdateRepositoryPullInfo updates the last commit and last pulled timestamp
func UpdateRepositoryPullInfo(id int, commitHash string) error {
	result, err := DB.Exec(
		"UPDATE repositories SET last_commit = ?, last_pulled = ? WHERE id = ?",
		commitHash, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("failed to update pull info: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("repository not found")
	}

	return nil
}

// DeleteRepository deletes a repository by ID
func DeleteRepository(id int) error {
	result, err := DB.Exec("DELETE FROM repositories WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to delete repository: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("repository not found")
	}

	return nil
}

// UpdateRepository updates an existing repository
func UpdateRepository(repo *Repository) error {
	result, err := DB.Exec(`
		UPDATE repositories 
		SET name = ?, url = ?, branch = ?, auth_type = ?, username = ?, password = ?, ssh_key = ?, status = ?
		WHERE id = ?`,
		repo.Name, repo.URL, repo.Branch, repo.AuthType, repo.Username, repo.Password, repo.SSHKey, repo.Status, repo.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update repository: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("repository not found")
	}

	return nil
}

// UpdateRepositoryStatus updates only the status of a repository
func UpdateRepositoryStatus(id int, status string) error {
	result, err := DB.Exec("UPDATE repositories SET status = ? WHERE id = ?", status, id)
	if err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("repository not found")
	}

	return nil
}

// UpdateRepositoryBranch updates only the branch of a repository
func UpdateRepositoryBranch(id int, branch string) error {
	result, err := DB.Exec("UPDATE repositories SET branch = ? WHERE id = ?", branch, id)
	if err != nil {
		return fmt.Errorf("failed to update branch: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("repository not found")
	}

	return nil
}

// RepoJob is a persisted pull or branch job.
type RepoJob struct {
	ID         int
	RepoID     int
	Kind       string
	Status     string
	Detail     string
	CreatedAt  string
	FinishedAt string
}

func validRepoJobKind(kind string) bool {
	return kind == RepoJobKindPull || kind == RepoJobKindBranch
}

func validRepoJobTerminal(status string) bool {
	return status == RepoJobStatusSuccess || status == RepoJobStatusError
}

func scanRepoJob(scanner interface {
	Scan(dest ...any) error
}) (*RepoJob, error) {
	job := &RepoJob{}
	var detail, finishedAt sql.NullString
	if err := scanner.Scan(&job.ID, &job.RepoID, &job.Kind, &job.Status, &detail, &job.CreatedAt, &finishedAt); err != nil {
		return nil, err
	}
	if detail.Valid {
		job.Detail = detail.String
	}
	if finishedAt.Valid {
		job.FinishedAt = finishedAt.String
	}
	return job, nil
}

// CreateRepoJob inserts a running job and returns its id.
func CreateRepoJob(repoID int, kind string) (int, error) {
	if repoID <= 0 {
		return 0, fmt.Errorf("repository id is required")
	}
	if !validRepoJobKind(kind) {
		return 0, fmt.Errorf("invalid repo job kind %q", kind)
	}

	result, err := DB.Exec(
		`INSERT INTO repo_jobs (repo_id, kind, status) VALUES (?, ?, ?)`,
		repoID, kind, RepoJobStatusRunning,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to insert repo job: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get repo job id: %w", err)
	}
	return int(id), nil
}

// CompleteRepoJob marks a job success or error and records finished_at.
func CompleteRepoJob(id int, status, detail string) error {
	if id <= 0 {
		return fmt.Errorf("job id is required")
	}
	if !validRepoJobTerminal(status) {
		return fmt.Errorf("invalid repo job status %q", status)
	}

	result, err := DB.Exec(
		`UPDATE repo_jobs SET status = ?, detail = ?, finished_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, detail, id,
	)
	if err != nil {
		return fmt.Errorf("failed to complete repo job: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrRepoJobNotFound
	}
	return nil
}

// GetRepoJob retrieves a job by id.
func GetRepoJob(id int) (*RepoJob, error) {
	row := DB.QueryRow(
		`SELECT id, repo_id, kind, status, detail, created_at, finished_at FROM repo_jobs WHERE id = ?`,
		id,
	)
	job, err := scanRepoJob(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrRepoJobNotFound
		}
		return nil, fmt.Errorf("failed to query repo job: %w", err)
	}
	return job, nil
}
