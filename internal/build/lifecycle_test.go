package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func TestRunCancelAndTimeout(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	started := make(chan struct{}, 1)
	e.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
		started <- struct{}{}
		return dockertest.BuildOutcome{Block: true}
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	b, _, err := e.run(ctx, p, nil)
	if !errors.Is(err, context.Canceled) || b.Status != store.BuildStatusCancelled || b.Detail != DetailCancelled {
		t.Fatalf("%v %+v", err, b)
	}

	e2 := newEnv(t, func(o *Options) { o.Timeout = 100 * time.Millisecond })
	p2 := e2.project("arena", nil)
	e2.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome { return dockertest.BuildOutcome{Block: true} })
	b, _, err = e2.run(context.Background(), p2, nil)
	if err == nil || b.Status != store.BuildStatusError || !strings.Contains(b.Detail, "exceeded the time limit of 100ms") {
		t.Fatalf("%v %+v", err, b)
	}
	if entries, _ := os.ReadDir(e2.tmpDir); len(entries) != 0 {
		t.Fatalf("tmp not cleaned: %v", entries)
	}
}

// TestOneBuildAtATime pins the daemon-wide build slot: a second build
// waits (queued) until the first finishes.
func TestOneBuildAtATime(t *testing.T) {
	e := newEnv(t)
	p1 := e.project("one", nil)
	p2 := e.project("two", nil)
	release := make(chan struct{})
	first := make(chan struct{})
	var once sync.Once
	e.fake.SetBuildHook(func(c dockertest.BuildCall) dockertest.BuildOutcome {
		if strings.HasPrefix(c.Tag, "golem-deploy/one:") {
			once.Do(func() { close(first) })
			return dockertest.BuildOutcome{Block: true, Release: release}
		}
		return dockertest.BuildOutcome{}
	})
	done1 := make(chan error, 1)
	go func() { _, _, err := e.run(context.Background(), p1, nil); done1 <- err }()
	<-first
	b2, err := e.st.CreateBuild(context.Background(), store.NewBuild{ProjectID: p2.ID})
	if err != nil {
		t.Fatal(err)
	}
	log2 := &syncBuf{}
	done2 := make(chan error, 1)
	go func() { done2 <- e.svc.Run(context.Background(), b2.ID, log2, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log2.String(), "Waiting for another build") {
		if time.Now().After(deadline) {
			t.Fatalf("second build did not wait:\n%s", log2.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, _ := e.st.GetBuild(context.Background(), b2.ID); got.Status != store.BuildStatusQueued {
		t.Fatalf("second build is %s while waiting", got.Status)
	}
	if n := len(e.fake.CallsTo("build")); n != 1 {
		t.Fatalf("%d docker builds ran at once", n)
	}
	close(release)
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
}

func TestReserveCap(t *testing.T) {
	e := newEnv(t)
	var releases []func()
	for range MaxActiveBuilds {
		r, err := e.svc.Reserve()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	if _, err := e.svc.Reserve(); !errors.Is(err, ErrTooManyBuilds) {
		t.Fatalf("%v", err)
	}
	releases[0]()
	releases[0]() // idempotent
	if _, err := e.svc.Reserve(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Reserve(); !errors.Is(err, ErrTooManyBuilds) {
		t.Fatalf("double release freed two places: %v", err)
	}
}

func TestRetention(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Retention = 2 })
	p := e.project("arena", nil)
	ctx := context.Background()
	var builds []store.Build
	for range 3 {
		b, log, err := e.run(ctx, p, nil)
		if err != nil {
			t.Fatalf("%v\n%s", err, log)
		}
		builds = append(builds, b)
	}
	// The oldest of three is beyond retention 2: removed with its image.
	if _, err := e.st.GetBuild(ctx, builds[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("oldest build kept: %v", err)
	}
	if e.fake.HasImage(builds[0].ImageID) {
		t.Fatal("oldest build's image kept")
	}
	// A build a server is configured with is kept beyond retention.
	sv, err := e.st.CreateServer(ctx, store.Server{Name: "s1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.AssignServerBuild(ctx, sv.ID, &builds[1].ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := e.run(ctx, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.GetBuild(ctx, builds[1].ID); err != nil || !e.fake.HasImage(builds[1].ImageID) {
		t.Fatalf("referenced build removed: %v", err)
	}
	if _, err := e.st.GetBuild(ctx, builds[2].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unreferenced old build kept: %v", err)
	}
	list, _ := e.st.ListBuilds(ctx, store.BuildFilter{ProjectID: &p.ID})
	if len(list) != 3 {
		t.Fatalf("%d builds kept, want 3 (2 newest + 1 referenced)", len(list))
	}
	// Failed builds beyond KeepFailed are deleted.
	e.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome { return dockertest.BuildOutcome{Fail: true} })
	for range KeepFailed + 3 {
		_, _, _ = e.run(ctx, p, nil)
	}
	e.fake.SetBuildHook(nil)
	if _, _, err := e.run(ctx, p, nil); err != nil {
		t.Fatal(err)
	}
	failed, _ := e.st.ListBuilds(ctx, store.BuildFilter{ProjectID: &p.ID, Status: store.BuildStatusError, Limit: 500})
	if len(failed) != KeepFailed {
		t.Fatalf("%d failed builds kept, want %d", len(failed), KeepFailed)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.project("arena", nil)
	b1, _, err := e.run(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	queued, _ := e.st.CreateBuild(ctx, store.NewBuild{ProjectID: p.ID})
	if err := e.svc.Delete(ctx, queued.ID); !errors.Is(err, store.ErrActive) {
		t.Fatalf("active: %v", err)
	}
	if err := e.svc.Delete(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	sv, _ := e.st.CreateServer(ctx, store.Server{Name: "s1"}, nil)
	_ = e.st.AssignServerBuild(ctx, sv.ID, &b1.ID)
	if err := e.svc.Delete(ctx, b1.ID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("in use: %v", err)
	}
	_ = e.st.AssignServerBuild(ctx, sv.ID, nil)
	// A container (of any origin) created from the image blocks removal.
	cid := e.fake.AddContainer(dockertest.Container{Image: b1.ImageID, State: "exited"})
	if err := e.svc.Delete(ctx, b1.ID); !errors.Is(err, ErrImageInUse) {
		t.Fatalf("image in use: %v", err)
	}
	e.fake.RemoveContainer(cid)
	e.fake.SetDown(true)
	if err := e.svc.Delete(ctx, b1.ID); !errors.Is(err, docker.ErrUnavailable) {
		t.Fatalf("docker down: %v", err)
	}
	e.fake.SetDown(false)
	if err := e.svc.Delete(ctx, b1.ID); err != nil {
		t.Fatal(err)
	}
	if e.fake.HasImage(b1.ImageID) {
		t.Fatal("image kept")
	}
	if _, err := e.st.GetBuild(ctx, b1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("row kept")
	}
	// Failed builds have no image; deleting them touches no image.
	e.fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome { return dockertest.BuildOutcome{Fail: true} })
	bf, _, _ := e.run(ctx, p, nil)
	e.fake.ResetCalls()
	if err := e.svc.Delete(ctx, bf.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.CallsTo("image", "rm")) != 0 {
		t.Fatal("image rm for a failed build")
	}
}

func TestWatchRecordsUnfinishedBuilds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.project("arena", nil)
	b, _ := e.st.CreateBuild(ctx, store.NewBuild{ProjectID: p.ID})
	j, _ := e.st.CreateJob(ctx, store.NewJob{Kind: "project.build", SubjectType: "project", SubjectID: "1"})
	_ = e.st.SetBuildJob(ctx, b.ID, j.ID)
	_ = e.st.FinishJob(ctx, j.ID, store.JobCancelled, jobs.DetailCancelled)
	release, _ := e.svc.Reserve()
	done := make(chan struct{})
	close(done)
	e.svc.Watch(b.ID, done, release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := e.st.GetBuild(ctx, b.ID)
		if got.Status == store.BuildStatusCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("build still %s", got.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The reservation was released.
	for range MaxActiveBuilds {
		if _, err := e.svc.Reserve(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSweepTemp(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "build-1-abc", "static"), 0o700)
	_ = os.MkdirAll(filepath.Join(tmp, "git-clone-x"), 0o700)
	if err := SweepTemp(tmp); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 1 || entries[0].Name() != "git-clone-x" {
		t.Fatalf("%v", entries)
	}
	if err := SweepTemp(filepath.Join(tmp, "missing")); err != nil {
		t.Fatal(err)
	}
}
