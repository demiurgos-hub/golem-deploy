// Package jobs runs long operations (clone, pull, container start, ...) in
// the background with persisted status, a per-job log file, bounded
// concurrency and at most one active job per subject.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Errors returned by Runner.
var (
	// ErrSubjectBusy means a job for the same subject is queued or running.
	ErrSubjectBusy = errors.New("jobs: another job is active for this subject")
	// ErrNotActive means the job is not queued or running in this process.
	ErrNotActive = errors.New("jobs: job is not active")
	// ErrShuttingDown means the runner no longer accepts jobs.
	ErrShuttingDown = errors.New("jobs: runner is shutting down")
)

// Details recorded for jobs that did not succeed. They are shown to users.
const (
	DetailCancelled = "cancelled"
	DetailShutdown  = "cancelled: daemon shutting down"
	DetailFailed    = "job failed; see the job log"
	DetailPanic     = "internal error; see the job log"
)

// Subject identifies what a job acts on.
type Subject struct {
	Type string
	ID   string
}

// SubjectID formats an integer id for Subject.ID.
func SubjectID(id int64) string { return strconv.FormatInt(id, 10) }

// Func is the body of a job. It must honour ctx and write human-readable
// progress to log (which is also where full error output belongs). Return a
// *SafeError (see Fail) to control the detail shown to users; any other
// error is shown as DetailFailed and written to the log in full.
type Func func(ctx context.Context, log io.Writer) error

// SafeError carries a user-safe message alongside the underlying error.
type SafeError struct {
	Msg string
	Err error
}

func (e *SafeError) Error() string {
	if e.Err == nil {
		return e.Msg
	}
	return e.Msg + ": " + e.Err.Error()
}

func (e *SafeError) Unwrap() error { return e.Err }

// Fail returns an error whose Msg becomes the job's detail. msg must be safe
// to show to users (no command output, paths or credentials); cause is only
// written to the job log.
func Fail(msg string, cause error) error { return &SafeError{Msg: msg, Err: cause} }

// Options configures a Runner.
type Options struct {
	// Workers bounds how many jobs run at once (default 4). Further jobs
	// stay queued.
	Workers int
	Logger  *slog.Logger
}

// Runner executes jobs. Create it with New after store.Reconcile and stop it
// with Shutdown.
type Runner struct {
	store  *store.Store
	logDir string
	logger *slog.Logger
	sem    chan struct{}

	baseCtx    context.Context
	cancelBase context.CancelFunc
	wg         sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	subjects map[Subject]int64
	active   map[int64]*activeJob
}

type activeJob struct {
	subject Subject
	cancel  context.CancelCauseFunc
	done    chan struct{}
}

var errCancelledByUser = errors.New("cancelled by user")
var errShutdown = errors.New("daemon shutting down")

// New returns a runner that persists jobs in st and writes logs to logDir.
func New(st *store.Store, logDir string, opts Options) *Runner {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		store:      st,
		logDir:     logDir,
		logger:     opts.Logger,
		sem:        make(chan struct{}, opts.Workers),
		baseCtx:    ctx,
		cancelBase: cancel,
		subjects:   map[Subject]int64{},
		active:     map[int64]*activeJob{},
	}
}

// LogPath returns the log file of job id.
func (r *Runner) LogPath(id int64) string {
	return filepath.Join(r.logDir, strconv.FormatInt(id, 10)+".log")
}

// Start persists a queued job and runs fn in the background. It fails with
// ErrSubjectBusy when the subject already has an active job.
func (r *Runner) Start(ctx context.Context, kind string, subject Subject, createdBy *int64, fn Func) (store.Job, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return store.Job{}, ErrShuttingDown
	}
	if _, busy := r.subjects[subject]; busy {
		r.mu.Unlock()
		return store.Job{}, ErrSubjectBusy
	}
	r.subjects[subject] = 0 // reserve while the row is created
	r.mu.Unlock()

	release := func() {
		r.mu.Lock()
		delete(r.subjects, subject)
		r.mu.Unlock()
	}

	job, err := r.store.CreateJob(ctx, store.NewJob{Kind: kind, SubjectType: subject.Type, SubjectID: subject.ID, CreatedBy: createdBy})
	if err != nil {
		release()
		return store.Job{}, err
	}
	logFile, err := os.OpenFile(r.LogPath(job.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		release()
		r.finish(job.ID, store.JobError, DetailFailed)
		return store.Job{}, fmt.Errorf("create job log: %w", err)
	}

	jctx, cancel := context.WithCancelCause(r.baseCtx)
	aj := &activeJob{subject: subject, cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel(errShutdown)
		logFile.Close()
		release()
		r.finish(job.ID, store.JobCancelled, DetailShutdown)
		return store.Job{}, ErrShuttingDown
	}
	r.subjects[subject] = job.ID
	r.active[job.ID] = aj
	r.wg.Add(1)
	r.mu.Unlock()

	go r.run(jctx, job, aj, logFile, fn)
	return job, nil
}

func (r *Runner) run(ctx context.Context, job store.Job, aj *activeJob, logFile *os.File, fn Func) {
	defer r.wg.Done()
	status, detail := store.JobError, DetailFailed
	defer func() {
		_ = logFile.Close()
		r.finish(job.ID, status, detail)
		r.mu.Lock()
		delete(r.active, job.ID)
		if r.subjects[aj.subject] == job.ID {
			delete(r.subjects, aj.subject)
		}
		r.mu.Unlock()
		close(aj.done)
	}()

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		status, detail = store.JobCancelled, cancelDetail(ctx)
		return
	}

	if err := r.store.MarkJobRunning(context.Background(), job.ID); err != nil {
		r.logger.Error("job: mark running failed", "job_id", job.ID, "err", err)
		return
	}
	r.logger.Info("job started", "job_id", job.ID, "kind", job.Kind, "subject_type", job.SubjectType, "subject_id", job.SubjectID)

	err := r.call(ctx, job, logFile, fn)
	switch {
	case err == nil:
		status, detail = store.JobSuccess, ""
	case ctx.Err() != nil:
		status, detail = store.JobCancelled, cancelDetail(ctx)
		fmt.Fprintf(logFile, "\n%s\n", detail)
	default:
		var pe *panicError
		var se *SafeError
		switch {
		case errors.As(err, &pe):
			detail = DetailPanic
		case errors.As(err, &se) && se.Msg != "":
			detail = se.Msg
		}
		fmt.Fprintf(logFile, "\nerror: %v\n", err)
	}
	r.logger.Info("job finished", "job_id", job.ID, "status", status)
}

type panicError struct {
	value any
	stack []byte
}

func (p *panicError) Error() string { return fmt.Sprintf("panic: %v", p.value) }

func (r *Runner) call(ctx context.Context, job store.Job, log io.Writer, fn Func) (err error) {
	defer func() {
		if v := recover(); v != nil {
			pe := &panicError{value: v, stack: debug.Stack()}
			r.logger.Error("job panicked", "job_id", job.ID, "panic", fmt.Sprint(v), "stack", string(pe.stack))
			fmt.Fprintf(log, "\n%s\n%s\n", pe.Error(), pe.stack)
			err = pe
		}
	}()
	return fn(ctx, log)
}

func cancelDetail(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errCancelledByUser) {
		return DetailCancelled
	}
	return DetailShutdown
}

func (r *Runner) finish(id int64, status store.JobStatus, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.store.FinishJob(ctx, id, status, detail); err != nil {
		r.logger.Error("job: persist final status failed", "job_id", id, "status", status, "err", err)
	}
}

// Cancel requests cancellation of an active job. The job ends with status
// cancelled once its function returns.
func (r *Runner) Cancel(id int64) error {
	r.mu.Lock()
	aj, ok := r.active[id]
	r.mu.Unlock()
	if !ok {
		return ErrNotActive
	}
	aj.cancel(errCancelledByUser)
	return nil
}

// Done returns a channel closed when job id is no longer active in this
// process (already closed if it is not active).
func (r *Runner) Done(id int64) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if aj, ok := r.active[id]; ok {
		return aj.done
	}
	c := make(chan struct{})
	close(c)
	return c
}

// ActiveFor returns the id of the active job for subject, if any.
func (r *Runner) ActiveFor(subject Subject) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.subjects[subject]
	return id, ok && id != 0
}

// Shutdown stops accepting jobs, cancels active ones and waits for them to
// return until ctx is done.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	for _, aj := range r.active {
		aj.cancel(errShutdown)
	}
	r.mu.Unlock()
	r.cancelBase()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("jobs did not stop in time: %w", ctx.Err())
	}
}
