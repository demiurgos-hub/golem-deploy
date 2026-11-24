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
	// Workers bounds how many jobs of the default pool run at once
	// (default 4). Further jobs stay queued.
	Workers int
	// Pools are further named worker pools with their own bound (at least
	// 1 each). A job started with InPool(name) waits only for a worker of
	// that pool, so jobs of one pool never delay jobs of another.
	Pools  map[string]int
	Logger *slog.Logger
	// MaxLogBytes caps each job's log file (default DefaultMaxLogBytes).
	// Output beyond it is replaced by one truncation marker line and
	// discarded; the job keeps running.
	MaxLogBytes int64
}

// DefaultMaxLogBytes is the default cap of a job log file.
const DefaultMaxLogBytes = 16 << 20

// Runner executes jobs. Create it with New after store.Reconcile and stop it
// with Shutdown.
type Runner struct {
	store  *store.Store
	logDir string
	logger *slog.Logger
	maxLog int64

	baseCtx    context.Context
	cancelBase context.CancelFunc
	wg         sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	subjects map[Subject]int64
	active   map[int64]*activeJob
	// pools by name ("" is the default pool).
	pools map[string]*pool
}

// pool is a set of workers. queue holds its jobs waiting for a worker, in
// the order they were started; running counts the jobs holding one.
// Admission happens under Runner.mu (admitLocked), so a pool's jobs run in
// start order whatever order their goroutines are scheduled in.
type pool struct {
	workers int
	queue   []*activeJob
	running int
}

type activeJob struct {
	pool    *pool
	subject Subject
	cancel  context.CancelCauseFunc
	done    chan struct{}
	// admit is closed when the job is given a worker; admitted records it
	// (under Runner.mu) so a job that is cancelled at that same moment can
	// tell which happened.
	admit    chan struct{}
	admitted bool
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
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = DefaultMaxLogBytes
	}
	pools := map[string]*pool{"": {workers: opts.Workers}}
	for name, n := range opts.Pools {
		if name != "" {
			pools[name] = &pool{workers: max(n, 1)}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		store:      st,
		logDir:     logDir,
		logger:     opts.Logger,
		maxLog:     opts.MaxLogBytes,
		baseCtx:    ctx,
		cancelBase: cancel,
		subjects:   map[Subject]int64{},
		active:     map[int64]*activeJob{},
		pools:      pools,
	}
}

// LogPath returns the log file of job id.
func (r *Runner) LogPath(id int64) string {
	return filepath.Join(r.logDir, strconv.FormatInt(id, 10)+".log")
}

// StartOption adjusts the job started by Start.
type StartOption func(*startConfig)

type startConfig struct {
	job  store.NewJob
	pool string
}

// Restricted marks the job as restricted: only admins may read its log or
// cancel it (enforced by the HTTP layer).
func Restricted(restricted bool) StartOption {
	return func(c *startConfig) { c.job.Restricted = restricted }
}

// InPool runs the job on the named pool of Options.Pools instead of the
// default pool (an unknown name is the default pool).
func InPool(name string) StartOption {
	return func(c *startConfig) { c.pool = name }
}

// Busy reports whether a subject is reserved by a queued or running job.
func (r *Runner) Busy(subject Subject) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, busy := r.subjects[subject]
	return busy
}

// Start persists a queued job and runs fn in the background. It fails with
// ErrSubjectBusy when the subject already has an active job.
func (r *Runner) Start(ctx context.Context, kind string, subject Subject, createdBy *int64, fn Func, opts ...StartOption) (store.Job, error) {
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

	cfg := startConfig{job: store.NewJob{Kind: kind, SubjectType: subject.Type, SubjectID: subject.ID, CreatedBy: createdBy}}
	for _, o := range opts {
		o(&cfg)
	}
	job, err := r.store.CreateJob(ctx, cfg.job)
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
	p := r.pools[cfg.pool]
	if p == nil {
		p = r.pools[""]
	}
	aj := &activeJob{pool: p, subject: subject, cancel: cancel, done: make(chan struct{}), admit: make(chan struct{})}

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
	p.queue = append(p.queue, aj)
	r.admitLocked(p)
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
	case <-aj.admit:
	case <-ctx.Done():
	}
	r.mu.Lock()
	if !aj.admitted {
		// Cancelled while waiting for a worker.
		r.dequeueLocked(aj)
		r.mu.Unlock()
		status, detail = store.JobCancelled, cancelDetail(ctx)
		return
	}
	r.mu.Unlock()
	defer r.releaseWorker(aj.pool)
	if ctx.Err() != nil {
		// Admitted at the moment it was cancelled (select picks either
		// ready case): a cancelled job never starts.
		status, detail = store.JobCancelled, cancelDetail(ctx)
		return
	}

	if err := r.store.MarkJobRunning(context.Background(), job.ID); err != nil {
		r.logger.Error("job: mark running failed", "job_id", job.ID, "err", err)
		return
	}
	r.logger.Info("job started", "job_id", job.ID, "kind", job.Kind, "subject_type", job.SubjectType, "subject_id", job.SubjectID)

	// The job writes through the cap; the runner's own final lines below
	// go to the file directly so they survive a truncated log.
	err := r.call(ctx, job, newCappedWriter(logFile, r.maxLog), fn)
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

// admitLocked gives free workers of p to its oldest waiting jobs. r.mu
// must be held.
func (r *Runner) admitLocked(p *pool) {
	for p.running < p.workers && len(p.queue) > 0 {
		aj := p.queue[0]
		p.queue[0] = nil
		p.queue = p.queue[1:]
		aj.admitted = true
		p.running++
		close(aj.admit)
	}
}

// dequeueLocked removes a job that gave up waiting. r.mu must be held.
func (r *Runner) dequeueLocked(aj *activeJob) {
	p := aj.pool
	for i, q := range p.queue {
		if q == aj {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			return
		}
	}
}

// releaseWorker returns a worker of p and admits its next waiting job.
func (r *Runner) releaseWorker(p *pool) {
	r.mu.Lock()
	p.running--
	r.admitLocked(p)
	r.mu.Unlock()
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

// cappedWriter forwards up to limit bytes to w, then writes one truncation
// marker line and discards the rest. It never fails or blocks the writer
// (write errors of w are dropped too), so a job is not affected by the cap.
type cappedWriter struct {
	mu        sync.Mutex
	w         io.Writer
	limit     int64
	left      int64
	truncated bool
}

func newCappedWriter(w io.Writer, limit int64) *cappedWriter {
	return &cappedWriter{w: w, limit: limit, left: limit}
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return len(p), nil
	}
	if int64(len(p)) <= c.left {
		c.left -= int64(len(p))
		_, _ = c.w.Write(p)
		return len(p), nil
	}
	if c.left > 0 {
		_, _ = c.w.Write(p[:c.left])
	}
	c.left = 0
	c.truncated = true
	_, _ = fmt.Fprintf(c.w, "\n[log truncated: the job log reached its limit of %d MiB; further output is discarded]\n", c.limit>>20)
	return len(p), nil
}
