package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func newRunner(t *testing.T, workers int) (*Runner, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	r := New(st, logs, Options{Workers: workers})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Shutdown(ctx)
		_ = st.Close()
	})
	return r, st
}

func waitDone(t *testing.T, r *Runner, id int64) {
	t.Helper()
	select {
	case <-r.Done(id):
	case <-time.After(10 * time.Second):
		t.Fatalf("job %d did not finish", id)
	}
}

func getJob(t *testing.T, st *store.Store, id int64) store.Job {
	t.Helper()
	j, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

var subj = Subject{Type: "project", ID: "1"}

func TestJobSuccessWritesLog(t *testing.T) {
	r, st := newRunner(t, 2)
	j, err := r.Start(context.Background(), "project.pull", subj, nil, func(ctx context.Context, log io.Writer) error {
		fmt.Fprintln(log, "hello from job")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != store.JobQueued {
		t.Fatalf("initial status %s", j.Status)
	}
	waitDone(t, r, j.ID)
	got := getJob(t, st, j.ID)
	if got.Status != store.JobSuccess || got.Detail != "" || got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("final job %+v", got)
	}
	b, _ := os.ReadFile(r.LogPath(j.ID))
	if !strings.Contains(string(b), "hello from job") {
		t.Fatalf("log %q", b)
	}
}

// TestF6_RestrictedOptionPersisted: the restricted flag is stored with the
// job; jobs are unrestricted by default.
func TestF6_RestrictedOptionPersisted(t *testing.T) {
	r, st := newRunner(t, 2)
	noop := func(context.Context, io.Writer) error { return nil }
	plain, err := r.Start(context.Background(), "project.pull", Subject{Type: "project", ID: "61"}, nil, noop)
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := r.Start(context.Background(), "project.pull", Subject{Type: "project", ID: "62"}, nil, noop, Restricted(true))
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, plain.ID)
	waitDone(t, r, restricted.ID)
	if getJob(t, st, plain.ID).Restricted || !getJob(t, st, restricted.ID).Restricted || !restricted.Restricted {
		t.Fatal("restricted flag not persisted")
	}
}

func TestPerSubjectExclusivity(t *testing.T) {
	r, st := newRunner(t, 4)
	release := make(chan struct{})
	j1, err := r.Start(context.Background(), "k", subj, nil, func(ctx context.Context, _ io.Writer) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error { return nil }); !errors.Is(err, ErrSubjectBusy) {
		t.Fatalf("second job for same subject: %v", err)
	}
	if id, ok := r.ActiveFor(subj); !ok || id != j1.ID {
		t.Fatalf("ActiveFor = %d %v", id, ok)
	}
	// A different subject runs concurrently.
	j2, err := r.Start(context.Background(), "k", Subject{Type: "project", ID: "2"}, nil, func(context.Context, io.Writer) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, j2.ID)
	close(release)
	waitDone(t, r, j1.ID)
	// Once finished, the subject is free again.
	j3, err := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error { return nil })
	if err != nil {
		t.Fatalf("subject not released: %v", err)
	}
	waitDone(t, r, j3.ID)
	if got := getJob(t, st, j3.ID); got.Status != store.JobSuccess {
		t.Fatalf("j3 %+v", got)
	}
}

func TestCancel(t *testing.T) {
	r, st := newRunner(t, 1)
	started := make(chan struct{})
	j, err := r.Start(context.Background(), "k", subj, nil, func(ctx context.Context, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := r.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, j.ID)
	if got := getJob(t, st, j.ID); got.Status != store.JobCancelled || got.Detail != DetailCancelled {
		t.Fatalf("cancelled job %+v", got)
	}
	if err := r.Cancel(j.ID); !errors.Is(err, ErrNotActive) {
		t.Fatalf("cancel finished job: %v", err)
	}
}

func TestQueuedJobCancelledBeforeRunning(t *testing.T) {
	r, st := newRunner(t, 1)
	block := make(chan struct{})
	j1, _ := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error { <-block; return nil })
	ran := false
	j2, err := r.Start(context.Background(), "k", Subject{Type: "project", ID: "2"}, nil, func(context.Context, io.Writer) error { ran = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Cancel(j2.ID); err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, j2.ID)
	close(block)
	waitDone(t, r, j1.ID)
	got := getJob(t, st, j2.ID)
	if ran || got.Status != store.JobCancelled || got.StartedAt != nil {
		t.Fatalf("queued job ran=%v %+v", ran, got)
	}
}

func TestPanicRecovery(t *testing.T) {
	r, st := newRunner(t, 1)
	j, err := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error {
		panic("kaboom /secret/path")
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, j.ID)
	got := getJob(t, st, j.ID)
	if got.Status != store.JobError || got.Detail != DetailPanic {
		t.Fatalf("panicked job %+v", got)
	}
	b, _ := os.ReadFile(r.LogPath(j.ID))
	if !strings.Contains(string(b), "kaboom") {
		t.Fatal("panic not in job log")
	}
	// The runner keeps working afterwards.
	j2, err := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, j2.ID)
}

func TestErrorDetailIsSafe(t *testing.T) {
	r, st := newRunner(t, 1)
	raw := errors.New("fatal: could not read from /home/user/.ssh/id_rsa: token=abc")
	j1, _ := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error { return raw })
	waitDone(t, r, j1.ID)
	if got := getJob(t, st, j1.ID); got.Status != store.JobError || got.Detail != DetailFailed {
		t.Fatalf("plain error detail %q", got.Detail)
	}
	b, _ := os.ReadFile(r.LogPath(j1.ID))
	if !strings.Contains(string(b), "id_rsa") {
		t.Fatal("full error missing from log")
	}
	j2, _ := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error {
		return Fail("authentication with the remote failed", raw)
	})
	waitDone(t, r, j2.ID)
	if got := getJob(t, st, j2.ID); got.Detail != "authentication with the remote failed" {
		t.Fatalf("safe detail %q", got.Detail)
	}
}

func TestBoundedWorkers(t *testing.T) {
	r, st := newRunner(t, 1)
	block, running := make(chan struct{}), make(chan struct{})
	j1, _ := r.Start(context.Background(), "k", Subject{"s", "1"}, nil, func(context.Context, io.Writer) error { close(running); <-block; return nil })
	<-running // j1 holds the only worker
	j2, _ := r.Start(context.Background(), "k", Subject{"s", "2"}, nil, func(context.Context, io.Writer) error { return nil })
	time.Sleep(100 * time.Millisecond)
	if got := getJob(t, st, j2.ID); got.Status != store.JobQueued {
		t.Fatalf("second job should wait for a worker, status %s", got.Status)
	}
	close(block)
	waitDone(t, r, j1.ID)
	waitDone(t, r, j2.ID)
	if got := getJob(t, st, j2.ID); got.Status != store.JobSuccess {
		t.Fatalf("j2 %+v", got)
	}
}

func TestShutdownCancelsJobs(t *testing.T) {
	r, st := newRunner(t, 1)
	started := make(chan struct{})
	j, _ := r.Start(context.Background(), "k", subj, nil, func(ctx context.Context, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := getJob(t, st, j.ID); got.Status != store.JobCancelled || got.Detail != DetailShutdown {
		t.Fatalf("job after shutdown %+v", got)
	}
	if _, err := r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error { return nil }); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("start after shutdown: %v", err)
	}
}

func TestShutdownGraceExpires(t *testing.T) {
	r, _ := newRunner(t, 1)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	_, _ = r.Start(context.Background(), "k", subj, nil, func(context.Context, io.Writer) error {
		close(started)
		<-release // ignores cancellation
		return nil
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Shutdown(ctx); err == nil {
		t.Fatal("expected grace period error")
	}
}

func TestRestartReconciliation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	r := New(st, dir, Options{})
	started := make(chan struct{})
	j, _ := r.Start(context.Background(), "k", subj, nil, func(ctx context.Context, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return nil
	})
	<-started
	// Simulate a crash: the process dies without the runner finishing the
	// job, so the row stays "running".
	if got := getJob(t, st, j.ID); got.Status != store.JobRunning {
		t.Fatalf("status %s", got.Status)
	}
	_ = r.Shutdown(context.Background())
	if _, err := st.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// A row left running by a crashed process.
	st2, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	crashed, _ := st2.CreateJob(context.Background(), store.NewJob{Kind: "k", SubjectType: "project", SubjectID: "9"})
	_ = st2.MarkJobRunning(context.Background(), crashed.ID)
	if _, err := st2.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := getJob(t, st2, crashed.ID); got.Status != store.JobError || got.Detail != store.InterruptedDetail {
		t.Fatalf("crashed job %+v", got)
	}
}

// TestG3_JobLogCap: a job that writes far more than the cap keeps running
// to completion; the log holds the first MaxLogBytes, one truncation
// marker and the runner's final error line.
func TestG3_JobLogCap(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const limit = 1 << 20
	r := New(st, dir, Options{MaxLogBytes: limit})
	defer func() { _ = r.Shutdown(context.Background()) }()
	chunk := []byte(strings.Repeat("x", 999) + "\n")
	written := 0
	j, err := r.Start(context.Background(), "project.pull", subj, nil, func(ctx context.Context, log io.Writer) error {
		for written < 5*limit {
			n, err := log.Write(chunk)
			if err != nil || n != len(chunk) {
				return fmt.Errorf("write: %d %v", n, err)
			}
			written += n
		}
		return Fail("boom", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, r, j.ID)
	if got := getJob(t, st, j.ID); got.Status != store.JobError || got.Detail != "boom" {
		t.Fatalf("job %+v", got)
	}
	data, err := os.ReadFile(r.LogPath(j.ID))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Count(log, "[log truncated") != 1 || !strings.Contains(log, "limit of 1 MiB") {
		t.Fatalf("marker missing or repeated")
	}
	if !strings.HasSuffix(log, "\nerror: boom\n") {
		t.Fatalf("final error line missing: ...%q", log[len(log)-120:])
	}
	if len(data) > limit+256 {
		t.Fatalf("log is %d bytes, cap %d", len(data), limit)
	}
	if !strings.HasPrefix(log, string(chunk)) {
		t.Fatal("head of the log not kept")
	}
}
