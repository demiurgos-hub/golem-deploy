package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Project is a stored project. AuthSecret is opaque ciphertext produced by
// internal/secrets with purpose secrets.ProjectAuthPurpose(ID); the store
// never interprets it.
type Project struct {
	ID           int64
	Name         string
	RepoURL      string
	Branch       string
	AuthType     string
	AuthUsername string
	AuthSecret   []byte `json:"-"`
	LastCommit   string
	LastPulledAt *time.Time
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SealFunc produces the ciphertext of an encrypted column for a row whose id
// is only known after insert. Returning nil stores NULL.
type SealFunc func(id int64) ([]byte, error)

const projectCols = `id, name, repo_url, branch, auth_type, auth_username, auth_secret, last_commit, last_pulled_at, status, created_at, updated_at`

func scanProject(r scanner) (Project, error) {
	var p Project
	var lastPulled sql.NullString
	var created, updated string
	if err := r.Scan(&p.ID, &p.Name, &p.RepoURL, &p.Branch, &p.AuthType, &p.AuthUsername, &p.AuthSecret,
		&p.LastCommit, &lastPulled, &p.Status, &created, &updated); err != nil {
		return Project{}, err
	}
	var err error
	if p.LastPulledAt, err = parseNullTime(lastPulled); err != nil {
		return Project{}, err
	}
	if p.CreatedAt, err = parseTime(created); err != nil {
		return Project{}, err
	}
	if p.UpdatedAt, err = parseTime(updated); err != nil {
		return Project{}, err
	}
	return p, nil
}

func blobArg(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

// CreateProject inserts p (ID, CreatedAt and UpdatedAt are ignored; an empty
// Status becomes "idle"). When seal is non-nil it is called with the new id
// inside the same transaction and its result stored as auth_secret, so the
// ciphertext can be bound to the row id. ErrConflict means the name exists.
func (s *Store) CreateProject(ctx context.Context, p Project, seal SealFunc) (Project, error) {
	if p.Status == "" {
		p.Status = "idle"
	}
	if p.AuthType == "" {
		p.AuthType = "none"
	}
	var out Project
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := formatTime(s.nowUTC())
		var err error
		out, err = scanProject(tx.QueryRowContext(ctx,
			`INSERT INTO projects (name, repo_url, branch, auth_type, auth_username, auth_secret, last_commit, last_pulled_at, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?) RETURNING `+projectCols,
			p.Name, p.RepoURL, p.Branch, p.AuthType, p.AuthUsername, p.LastCommit, formatTimePtr(p.LastPulledAt), p.Status, now, now))
		if err != nil {
			return mapErr("create project", err)
		}
		if seal == nil {
			return nil
		}
		ct, err := seal(out.ID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET auth_secret = ? WHERE id = ?`, blobArg(ct), out.ID); err != nil {
			return mapErr("create project", err)
		}
		out.AuthSecret = ct
		return nil
	})
	return out, err
}

// GetProject returns project id, or ErrNotFound.
func (s *Store) GetProject(ctx context.Context, id int64) (Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
	if err != nil {
		return Project{}, mapErr("get project", err)
	}
	return p, nil
}

// GetProjectByName returns the project called name, or ErrNotFound.
func (s *Store) GetProjectByName(ctx context.Context, name string) (Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE name = ?`, name))
	if err != nil {
		return Project{}, mapErr("get project", err)
	}
	return p, nil
}

// ListProjects returns all projects ordered by name.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY name`)
	if err != nil {
		return nil, mapErr("list projects", err)
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, mapErr("list projects", err)
		}
		out = append(out, p)
	}
	return out, mapErr("list projects", rows.Err())
}

// CountProjects returns the number of projects.
func (s *Store) CountProjects(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&n)
	return n, mapErr("count projects", err)
}

// UpdateProject overwrites every mutable column of project p.ID with p's
// values (Name, CreatedAt are not changed; UpdatedAt is set to now) and
// returns the stored row. Callers read-modify-write; AuthSecret must already
// be sealed for p.ID.
func (s *Store) UpdateProject(ctx context.Context, p Project) (Project, error) {
	out, err := scanProject(s.db.QueryRowContext(ctx,
		`UPDATE projects SET repo_url = ?, branch = ?, auth_type = ?, auth_username = ?, auth_secret = ?,
		 last_commit = ?, last_pulled_at = ?, status = ?, updated_at = ?
		 WHERE id = ? RETURNING `+projectCols,
		p.RepoURL, p.Branch, p.AuthType, p.AuthUsername, blobArg(p.AuthSecret),
		p.LastCommit, formatTimePtr(p.LastPulledAt), p.Status, formatTime(s.nowUTC()), p.ID))
	if err != nil {
		return Project{}, mapErr("update project", err)
	}
	return out, nil
}

// ProjectCheckout is the checkout state a project job records.
type ProjectCheckout struct {
	Branch       string
	LastCommit   string
	LastPulledAt *time.Time
	Status       string
}

// SetProjectCheckout updates only the checkout columns (branch,
// last_commit, last_pulled_at, status), so a job finishing concurrently
// with an UpdateProjectRemote never overwrites the other's columns.
func (s *Store) SetProjectCheckout(ctx context.Context, id int64, c ProjectCheckout) (Project, error) {
	out, err := scanProject(s.db.QueryRowContext(ctx,
		`UPDATE projects SET branch = ?, last_commit = ?, last_pulled_at = ?, status = ?, updated_at = ?
		 WHERE id = ? RETURNING `+projectCols,
		c.Branch, c.LastCommit, formatTimePtr(c.LastPulledAt), c.Status, formatTime(s.nowUTC()), id))
	if err != nil {
		return Project{}, mapErr("set project checkout", err)
	}
	return out, nil
}

// ProjectRemoteUpdate changes how a project's remote is reached. Nil
// fields are unchanged.
type ProjectRemoteUpdate struct {
	RepoURL *string
	Auth    *ProjectAuthUpdate
}

// ProjectAuthUpdate replaces a project's credentials. Secret must already
// be sealed for the project's id (nil stores NULL).
type ProjectAuthUpdate struct {
	Type     string
	Username string
	Secret   []byte
}

// UpdateProjectRemote updates only the remote columns (repo_url and the
// auth columns) of project id and returns the stored row.
func (s *Store) UpdateProjectRemote(ctx context.Context, id int64, u ProjectRemoteUpdate) (Project, error) {
	sets := []string{"updated_at = ?"}
	args := []any{formatTime(s.nowUTC())}
	if u.RepoURL != nil {
		sets = append(sets, "repo_url = ?")
		args = append(args, *u.RepoURL)
	}
	if u.Auth != nil {
		sets = append(sets, "auth_type = ?", "auth_username = ?", "auth_secret = ?")
		args = append(args, u.Auth.Type, u.Auth.Username, blobArg(u.Auth.Secret))
	}
	args = append(args, id)
	out, err := scanProject(s.db.QueryRowContext(ctx,
		`UPDATE projects SET `+strings.Join(sets, ", ")+` WHERE id = ? RETURNING `+projectCols, args...))
	if err != nil {
		return Project{}, mapErr("update project remote", err)
	}
	return out, nil
}

// SetProjectStatus updates only the status column.
func (s *Store) SetProjectStatus(ctx context.Context, id int64, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET status = ?, updated_at = ? WHERE id = ?`,
		status, formatTime(s.nowUTC()), id)
	if err != nil {
		return mapErr("set project status", err)
	}
	return checkAffected("set project status", res)
}

// DeleteProject deletes project id. Servers referencing it keep a NULL
// project_id.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return mapErr("delete project", err)
	}
	return checkAffected("delete project", res)
}
