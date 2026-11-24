package store

import (
	"context"
	"database/sql"
	"time"
)

// Build statuses. queued and running are active; the others are terminal.
const (
	BuildStatusQueued    = "queued"
	BuildStatusRunning   = "running"
	BuildStatusSuccess   = "success"
	BuildStatusError     = "error"
	BuildStatusCancelled = "cancelled"
)

// Build is one build of a project's checkout into a managed image. ImageID
// and ImageTag are set only when Status is success. The image a server runs
// is always the ImageID of a successful build.
type Build struct {
	ID                int64
	ProjectID         int64
	Branch            string
	Commit            string
	Status            string
	Detail            string
	JobID             *int64
	ImageID           string
	ImageTag          string
	EngineVersion     string
	SchemaFingerprint string
	// WebTransportPath and WebSocketPath are the endpoint paths the game
	// serves (set on success; empty on rows recorded before they existed).
	WebTransportPath string
	WebSocketPath    string
	CreatedBy        *int64
	CreatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

// NewBuild is the input of CreateBuild.
type NewBuild struct {
	ProjectID int64
	Branch    string
	CreatedBy *int64
}

// BuildResult is the terminal outcome recorded by FinishBuild. Status must
// be success, error or cancelled; the image fields are stored only on
// success.
type BuildResult struct {
	Status           string
	Detail           string
	ImageID          string
	ImageTag         string
	EngineVersion    string
	WebTransportPath string
	WebSocketPath    string
	// SchemaFingerprint is the fingerprint read from the repository's
	// committed generated code ("" when unknown); stored only on success.
	SchemaFingerprint string
}

// BuildFilter narrows ListBuilds.
type BuildFilter struct {
	// ProjectID, when non-nil, selects one project's builds.
	ProjectID *int64
	// Status, when non-empty, selects builds in that status.
	Status string
	// Limit caps the result (default 50, maximum 500).
	Limit int
	// BeforeID is the exclusive cursor for internal callers walking all builds.
	BeforeID int64
}

const buildCols = `id, project_id, branch, commit_hash, status, detail, job_id, image_id, image_tag, engine_version, schema_fingerprint, webtransport_path, websocket_path, created_by, created_at, started_at, finished_at`

func scanBuild(r scanner) (Build, error) {
	var b Build
	var jobID, createdBy sql.NullInt64
	var created string
	var started, finished sql.NullString
	if err := r.Scan(&b.ID, &b.ProjectID, &b.Branch, &b.Commit, &b.Status, &b.Detail, &jobID, &b.ImageID, &b.ImageTag,
		&b.EngineVersion, &b.SchemaFingerprint, &b.WebTransportPath, &b.WebSocketPath, &createdBy, &created, &started, &finished); err != nil {
		return Build{}, err
	}
	b.JobID = int64Ptr(jobID)
	b.CreatedBy = int64Ptr(createdBy)
	var err error
	if b.CreatedAt, err = parseTime(created); err != nil {
		return Build{}, err
	}
	if b.StartedAt, err = parseNullTime(started); err != nil {
		return Build{}, err
	}
	if b.FinishedAt, err = parseNullTime(finished); err != nil {
		return Build{}, err
	}
	return b, nil
}

// CreateBuild inserts a queued build.
func (s *Store) CreateBuild(ctx context.Context, in NewBuild) (Build, error) {
	b, err := scanBuild(s.db.QueryRowContext(ctx,
		`INSERT INTO builds (project_id, branch, created_by, created_at) VALUES (?, ?, ?, ?) RETURNING `+buildCols,
		in.ProjectID, in.Branch, nullInt64(in.CreatedBy), formatTime(s.nowUTC())))
	if err != nil {
		return Build{}, mapErr("create build", err)
	}
	return b, nil
}

// SetBuildJob records the job that runs build id.
func (s *Store) SetBuildJob(ctx context.Context, id, jobID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE builds SET job_id = ? WHERE id = ?`, jobID, id)
	if err != nil {
		return mapErr("set build job", err)
	}
	return checkAffected("set build job", res)
}

// MarkBuildRunning moves a queued build to running and records the branch
// and commit being built. ErrNotFound means the build is not queued.
func (s *Store) MarkBuildRunning(ctx context.Context, id int64, branch, commit string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE builds SET status = 'running', branch = ?, commit_hash = ?, started_at = ? WHERE id = ? AND status = 'queued'`,
		branch, commit, formatTime(s.nowUTC()), id)
	if err != nil {
		return mapErr("mark build running", err)
	}
	return checkAffected("mark build running", res)
}

// FinishBuild records the terminal outcome of an active build. ErrNotFound
// means the build is not queued or running.
func (s *Store) FinishBuild(ctx context.Context, id int64, r BuildResult) error {
	imageID, imageTag, wtPath, wsPath, fingerprint := "", "", "", "", ""
	if r.Status == BuildStatusSuccess {
		imageID, imageTag, wtPath, wsPath, fingerprint = r.ImageID, r.ImageTag, r.WebTransportPath, r.WebSocketPath, r.SchemaFingerprint
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE builds SET status = ?, detail = ?, image_id = ?, image_tag = ?, engine_version = ?, webtransport_path = ?, websocket_path = ?,
		 schema_fingerprint = ?, finished_at = ?
		 WHERE id = ? AND status IN ('queued', 'running')`,
		r.Status, r.Detail, imageID, imageTag, r.EngineVersion, wtPath, wsPath, fingerprint, formatTime(s.nowUTC()), id)
	if err != nil {
		return mapErr("finish build", err)
	}
	return checkAffected("finish build", res)
}

// SetBuildFingerprint replaces the schema fingerprint of build id. The
// daemon itself never replaces a recorded value (see FillBuildFingerprint);
// this is for tests and repairs.
func (s *Store) SetBuildFingerprint(ctx context.Context, id int64, fingerprint string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE builds SET schema_fingerprint = ? WHERE id = ?`, fingerprint, id)
	if err != nil {
		return mapErr("set build fingerprint", err)
	}
	return checkAffected("set build fingerprint", res)
}

// FillBuildFingerprint records the schema fingerprint a game built from
// build id reported, only while the build has none: a value read from the
// committed code or recorded earlier is never replaced. It reports whether
// it wrote the value.
func (s *Store) FillBuildFingerprint(ctx context.Context, id int64, fingerprint string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE builds SET schema_fingerprint = ? WHERE id = ? AND schema_fingerprint = ''`, fingerprint, id)
	if err != nil {
		return false, mapErr("fill build fingerprint", err)
	}
	n, err := res.RowsAffected()
	return n == 1, mapErr("fill build fingerprint", err)
}

// GetBuild returns build id, or ErrNotFound.
func (s *Store) GetBuild(ctx context.Context, id int64) (Build, error) {
	b, err := scanBuild(s.db.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE id = ?`, id))
	if err != nil {
		return Build{}, mapErr("get build", err)
	}
	return b, nil
}

// ListBuilds returns builds matching f, newest first.
func (s *Store) ListBuilds(ctx context.Context, f BuildFilter) ([]Build, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT ` + buildCols + ` FROM builds WHERE 1 = 1`
	var args []any
	if f.ProjectID != nil {
		q += ` AND project_id = ?`
		args = append(args, *f.ProjectID)
	}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.BeforeID > 0 {
		q += ` AND id < ?`
		args = append(args, f.BeforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr("list builds", err)
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, mapErr("list builds", err)
		}
		out = append(out, b)
	}
	return out, mapErr("list builds", rows.Err())
}

// CountBuildsWithImage returns how many successful builds other than
// exceptID have image id imageID (an image is shared when identical inputs
// were built twice).
func (s *Store) CountBuildsWithImage(ctx context.Context, imageID string, exceptID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM builds WHERE image_id = ? AND status = 'success' AND id <> ?`, imageID, exceptID).Scan(&n)
	return n, mapErr("count builds with image", err)
}

// DeleteBuildUnused removes build id in one transaction unless it is
// active (ErrActive) or a server is configured with it (ErrInUse); it
// returns the deleted row. ErrNotFound when it does not exist.
func (s *Store) DeleteBuildUnused(ctx context.Context, id int64) (Build, error) {
	var b Build
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		b, err = scanBuild(tx.QueryRowContext(ctx, `SELECT `+buildCols+` FROM builds WHERE id = ?`, id))
		if err != nil {
			return mapErr("delete build", err)
		}
		if b.Status == BuildStatusQueued || b.Status == BuildStatusRunning {
			return ErrActive
		}
		ids, err := queryIDs(ctx, tx, "delete build", `SELECT id FROM servers WHERE build_id = ? LIMIT 1`, id)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			return ErrInUse
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM builds WHERE id = ?`, id)
		if err != nil {
			return mapErr("delete build", err)
		}
		return checkAffected("delete build", res)
	})
	return b, err
}

// BuildServerIDs returns the ids of the servers configured with build id
// (sorted).
func (s *Store) BuildServerIDs(ctx context.Context, id int64) ([]int64, error) {
	return queryIDs(ctx, s.db, "build servers", `SELECT id FROM servers WHERE build_id = ? ORDER BY id`, id)
}

// queryer is *sql.DB or *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryIDs runs a query returning one integer column.
func queryIDs(ctx context.Context, q queryer, op, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(op, err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, mapErr(op, err)
		}
		ids = append(ids, id)
	}
	return ids, mapErr(op, rows.Err())
}

// DeleteBuild removes build id, or returns ErrNotFound.
func (s *Store) DeleteBuild(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM builds WHERE id = ?`, id)
	if err != nil {
		return mapErr("delete build", err)
	}
	return checkAffected("delete build", res)
}

// ReconcileBuilds marks builds left queued or running by a previous daemon
// process as error. Call it once at startup, with Reconcile.
func (s *Store) ReconcileBuilds(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE builds SET status = 'error', detail = ?, finished_at = ? WHERE status IN ('queued', 'running')`,
		InterruptedDetail, formatTime(s.nowUTC()))
	if err != nil {
		return 0, mapErr("reconcile builds", err)
	}
	return res.RowsAffected()
}

// ServerIDsByBuild returns, for every build referenced by a server, the ids
// of the servers configured with it (sorted).
func (s *Store) ServerIDsByBuild(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT build_id, id FROM servers WHERE build_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, mapErr("list build usage", err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var buildID, serverID int64
		if err := rows.Scan(&buildID, &serverID); err != nil {
			return nil, mapErr("list build usage", err)
		}
		out[buildID] = append(out[buildID], serverID)
	}
	return out, mapErr("list build usage", rows.Err())
}
