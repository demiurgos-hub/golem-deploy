package store

import (
	"context"
	"database/sql"
	"time"
)

// JobStatus is a stored job status.
type JobStatus string

// Job statuses.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSuccess   JobStatus = "success"
	JobError     JobStatus = "error"
	JobCancelled JobStatus = "cancelled"
)

// Terminal reports whether s is a final status.
func (s JobStatus) Terminal() bool {
	return s == JobSuccess || s == JobError || s == JobCancelled
}

// InterruptedDetail is the detail of jobs found active at startup.
const InterruptedDetail = "interrupted by daemon restart"

// Job is a stored job.
type Job struct {
	ID          int64
	Kind        string
	SubjectType string
	SubjectID   string
	Status      JobStatus
	Detail      string
	// Restricted jobs were started from an admin route: only admins may
	// read their log or cancel them.
	Restricted bool
	CreatedBy  *int64
	// CreatedByUsername is the creator's current username, read with the
	// job; nil when CreatedBy is nil.
	CreatedByUsername *string
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
}

// NewJob is the input of CreateJob.
type NewJob struct {
	Kind        string
	SubjectType string
	SubjectID   string
	CreatedBy   *int64
	Restricted  bool
}

// JobFilter selects jobs in ListJobs. Empty fields match everything.
type JobFilter struct {
	SubjectType string
	SubjectID   string
	// Limit caps the result (default 50, maximum 500).
	Limit int
}

// jobCols and jobFrom read jobs together with their creator's username in
// one query.
const (
	jobCols = `jobs.id, jobs.kind, jobs.subject_type, jobs.subject_id, jobs.status, jobs.detail, jobs.restricted, jobs.created_by, jobs.created_at, jobs.started_at, jobs.finished_at, accounts.username`
	jobFrom = ` FROM jobs LEFT JOIN accounts ON accounts.id = jobs.created_by`
	// jobReturning is jobCols for INSERT ... RETURNING, which cannot join.
	jobReturning = `id, kind, subject_type, subject_id, status, detail, restricted, created_by, created_at, started_at, finished_at, (SELECT username FROM accounts WHERE accounts.id = jobs.created_by)`
)

func scanJob(r scanner) (Job, error) {
	var j Job
	var createdBy sql.NullInt64
	var created string
	var started, finished, creator sql.NullString
	if err := r.Scan(&j.ID, &j.Kind, &j.SubjectType, &j.SubjectID, &j.Status, &j.Detail, &j.Restricted, &createdBy, &created, &started, &finished, &creator); err != nil {
		return Job{}, err
	}
	j.CreatedBy = int64Ptr(createdBy)
	if creator.Valid && j.CreatedBy != nil {
		j.CreatedByUsername = &creator.String
	}
	var err error
	if j.CreatedAt, err = parseTime(created); err != nil {
		return Job{}, err
	}
	if j.StartedAt, err = parseNullTime(started); err != nil {
		return Job{}, err
	}
	if j.FinishedAt, err = parseNullTime(finished); err != nil {
		return Job{}, err
	}
	return j, nil
}

// CreateJob inserts a queued job.
func (s *Store) CreateJob(ctx context.Context, in NewJob) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`INSERT INTO jobs (kind, subject_type, subject_id, status, restricted, created_by, created_at)
		 VALUES (?, ?, ?, 'queued', ?, ?, ?) RETURNING `+jobReturning,
		in.Kind, in.SubjectType, in.SubjectID, in.Restricted, nullInt64(in.CreatedBy), formatTime(s.nowUTC())))
	if err != nil {
		return Job{}, mapErr("create job", err)
	}
	return j, nil
}

// GetJob returns job id, or ErrNotFound.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+jobFrom+` WHERE jobs.id = ?`, id))
	if err != nil {
		return Job{}, mapErr("get job", err)
	}
	return j, nil
}

// ListJobs returns jobs matching f, newest first.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT ` + jobCols + jobFrom + ` WHERE 1=1`
	var args []any
	if f.SubjectType != "" {
		q += ` AND jobs.subject_type = ?`
		args = append(args, f.SubjectType)
	}
	if f.SubjectID != "" {
		q += ` AND jobs.subject_id = ?`
		args = append(args, f.SubjectID)
	}
	q += ` ORDER BY jobs.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapErr("list jobs", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, mapErr("list jobs", err)
		}
		out = append(out, j)
	}
	return out, mapErr("list jobs", rows.Err())
}

// CountActiveJobs counts queued and running jobs.
func (s *Store) CountActiveJobs(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN ('queued', 'running')`).Scan(&n)
	return n, mapErr("count jobs", err)
}

// MarkJobRunning moves a queued job to running.
func (s *Store) MarkJobRunning(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued'`,
		formatTime(s.nowUTC()), id)
	if err != nil {
		return mapErr("mark job running", err)
	}
	return checkAffected("mark job running", res)
}

// FinishJob moves an active job to a terminal status with a user-safe
// detail.
func (s *Store) FinishJob(ctx context.Context, id int64, status JobStatus, detail string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, detail = ?, finished_at = ? WHERE id = ? AND status IN ('queued', 'running')`,
		status, detail, formatTime(s.nowUTC()), id)
	if err != nil {
		return mapErr("finish job", err)
	}
	return checkAffected("finish job", res)
}

// ReconcileResult reports what Reconcile changed.
type ReconcileResult struct {
	Jobs     int64
	Projects int64
}

// Reconcile repairs state left behind by a daemon that stopped abruptly:
// active jobs become error/"interrupted by daemon restart" and projects in a
// transient status (cloning, pulling, switching_branch) become error,
// because the interrupted git operation may have left no checkout or a
// partially updated one; pulling the project repairs it. Call it once at
// startup, before the job runner starts.
func (s *Store) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var out ReconcileResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		now := formatTime(s.nowUTC())
		res, err := tx.ExecContext(ctx,
			`UPDATE jobs SET status = 'error', detail = ?, finished_at = ? WHERE status IN ('queued', 'running')`,
			InterruptedDetail, now)
		if err != nil {
			return mapErr("reconcile jobs", err)
		}
		if out.Jobs, err = res.RowsAffected(); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx,
			`UPDATE projects SET status = 'error', updated_at = ? WHERE status IN ('cloning', 'pulling', 'switching_branch')`,
			now)
		if err != nil {
			return mapErr("reconcile projects", err)
		}
		out.Projects, err = res.RowsAffected()
		return err
	})
	return out, err
}
