package build

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

type buildDockerOverride struct {
	Docker
	verify  func(context.Context, docker.BuildSpec) (docker.Image, error)
	remove  func(context.Context, string) error
	cleanup func(context.Context, docker.BuildSpec) error
}

func (d buildDockerOverride) BuiltImage(ctx context.Context, spec docker.BuildSpec) (docker.Image, error) {
	if d.verify != nil {
		return d.verify(ctx, spec)
	}
	return d.Docker.BuiltImage(ctx, spec)
}

func (d buildDockerOverride) RemoveImage(ctx context.Context, id string) error {
	if d.remove != nil {
		return d.remove(ctx, id)
	}
	return d.Docker.RemoveImage(ctx, id)
}

func (d buildDockerOverride) RemoveBuildOutput(ctx context.Context, spec docker.BuildSpec) error {
	if d.cleanup != nil {
		return d.cleanup(ctx, spec)
	}
	return d.Docker.RemoveBuildOutput(ctx, spec)
}

func TestCancelledVerificationNeverPublishesAndRetriesLateExport(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var image docker.Image
	cleanupAttempts := 0
	e.svc.o.Docker = buildDockerOverride{
		Docker: e.dc,
		verify: func(vctx context.Context, spec docker.BuildSpec) (docker.Image, error) {
			var err error
			image, err = e.dc.BuiltImage(vctx, spec)
			cancel()
			if !errors.Is(vctx.Err(), context.Canceled) {
				t.Error("verification context was detached from the job")
			}
			// Even a verifier that returns a successful result as it is
			// cancelled must not publish it.
			return image, err
		},
		cleanup: func(ctx context.Context, spec docker.BuildSpec) error {
			cleanupAttempts++
			if cleanupAttempts == 1 {
				return docker.ErrNotFound
			}
			return e.dc.RemoveBuildOutput(ctx, spec)
		},
	}
	b, _, err := e.run(ctx, p, nil)
	if !errors.Is(err, context.Canceled) || b.Status != store.BuildStatusCancelled || b.ImageID != "" {
		t.Fatalf("cancelled build published: %+v, %v", b, err)
	}
	e.svc.CleanupImages(t.Context())
	if cleanupAttempts != 2 || e.fake.HasImage(image.ID) {
		t.Fatalf("late export remained after %d attempts", cleanupAttempts)
	}
}

func TestBuildDeletionRetriesImageCleanup(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	b, log, err := e.run(t.Context(), p, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	e.svc.o.Docker = buildDockerOverride{Docker: e.dc, remove: func(context.Context, string) error { return docker.ErrUnavailable }}
	if err := e.svc.Delete(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
	if !e.fake.HasImage(b.ImageID) {
		t.Fatal("test did not leave the image behind")
	}
	// A new service (as at restart) drains the same durable queue.
	opts := e.svc.o
	opts.Docker = e.dc
	New(opts).CleanupImages(t.Context())
	if e.fake.HasImage(b.ImageID) {
		t.Fatal("orphaned image was not retried")
	}
	if pending, err := e.st.PendingBuildImages(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("cleanup queue %v: %v", pending, err)
	}
}

func TestFailedBuildResultWriteFailsJobAndReconciles(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(e.repoDir), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER break_finish BEFORE UPDATE ON builds WHEN NEW.finished_at IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'cannot finish build'); END`); err != nil {
		t.Fatal(err)
	}
	b, _, err := e.run(t.Context(), p, nil)
	if err == nil || !strings.Contains(err.Error(), "result could not be recorded") || b.Status != store.BuildStatusRunning {
		t.Fatalf("finish failure did not propagate: %+v, %v", b, err)
	}
	if imgs, err := e.dc.ListImages(t.Context()); err != nil || len(imgs) != 0 {
		t.Fatalf("unpublished export left behind: %v, %v", imgs, err)
	}
	if _, err := db.Exec("DROP TRIGGER break_finish"); err != nil {
		t.Fatal(err)
	}
	if n, err := e.st.ReconcileBuilds(t.Context()); err != nil || n != 1 {
		t.Fatalf("reconcile updated %d builds: %v", n, err)
	}
	b, err = e.st.GetBuild(t.Context(), b.ID)
	if err != nil || b.Status != store.BuildStatusError || b.Detail != store.InterruptedDetail {
		t.Fatalf("reconciled build %+v: %v", b, err)
	}
}

func TestRetentionPagesPastFiveHundredBuilds(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Retention = 1 })
	p := e.project("arena", nil)
	old, log, err := e.run(t.Context(), p, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	for range 501 {
		b, err := e.st.CreateBuild(t.Context(), store.NewBuild{ProjectID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.st.FinishBuild(t.Context(), b.ID, store.BuildResult{Status: store.BuildStatusError}); err != nil {
			t.Fatal(err)
		}
	}
	if _, log, err := e.run(t.Context(), p, nil); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if _, err := e.st.GetBuild(t.Context(), old.ID); !errors.Is(err, store.ErrNotFound) || e.fake.HasImage(old.ImageID) {
		t.Fatalf("retention missed the old build: %v", err)
	}
}

func TestBuildRejectsCheckoutWithDifferentHEAD(t *testing.T) {
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required")
	}
	e := newEnv(t)
	p := e.project("arena", nil)
	checkout := git.CheckoutDir(e.repoDir, p.ID)
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture"}} {
		cmd := exec.CommandContext(t.Context(), binary, args...)
		cmd.Dir = checkout
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runner, err := git.New(git.Options{TempDir: e.tmpDir, KnownHostsFile: filepath.Join(filepath.Dir(e.tmpDir), "known_hosts")})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.o.HeadCommit = runner.HeadCommit
	if head, err := runner.HeadCommit(t.Context(), checkout); err != nil || head == p.LastCommit {
		t.Fatalf("test needs a valid different HEAD: %q, %v", head, err)
	}
	b, _, err := e.run(t.Context(), p, nil)
	if err == nil || b.Detail != DetailCheckoutMismatch || len(e.fake.CallsTo("build")) != 0 {
		t.Fatalf("checkout mismatch allowed: %+v, %v", b, err)
	}
}

func TestBuildErrorSinksHideDockerStderrAndPaths(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	const marker = "HOST-PATH-MARKER"
	e.svc.o.Docker = buildDockerOverride{Docker: e.dc, verify: func(context.Context, docker.BuildSpec) (docker.Image, error) {
		return docker.Image{}, &docker.CmdError{Op: "image inspect", Detail: "C:/" + marker + "\x1b[31m"}
	}}
	_, log, err := e.run(t.Context(), p, nil)
	if err == nil || strings.Contains(err.Error()+log+e.daemon.String(), marker) {
		t.Fatalf("verification error leaked: %v\n%s\n%s", err, log, e.daemon.String())
	}
	// Simulate an internal filesystem error in the build staging path.
	e.svc.o.TempDir = filepath.Join(filepath.Dir(e.tmpDir), marker)
	if err := os.WriteFile(e.svc.o.TempDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, log, err = e.run(t.Context(), p, nil)
	if err == nil || strings.Contains(err.Error()+log+e.daemon.String(), marker) {
		t.Fatalf("build staging path leaked: %v\n%s\n%s", err, log, e.daemon.String())
	}
}

func TestLabelMismatchDoesNotRemoveImageFromMetadata(t *testing.T) {
	e := newEnv(t)
	p := e.project("arena", nil)
	foreignID := dockertest.ImageID(421)
	e.fake.AddImage(dockertest.Image{ID: foreignID, Labels: dockertest.ManagedLabels()})
	e.svc.o.Docker = buildDockerOverride{Docker: e.dc, verify: func(context.Context, docker.BuildSpec) (docker.Image, error) {
		return docker.Image{ID: foreignID}, docker.ErrLabelMismatch
	}}
	b, _, err := e.run(t.Context(), p, nil)
	if err == nil || !strings.Contains(b.Detail, "reserved") || !e.fake.HasImage(foreignID) {
		t.Fatalf("mismatched metadata led to foreign image removal: %+v, %v", b, err)
	}
	if calls := e.fake.CallsTo("image", "rm"); len(calls) != 1 || calls[0].Args[3] != fmt.Sprintf("golem-deploy/arena:%s-b%d", p.LastCommit[:12], b.ID) {
		t.Fatalf("cleanup did not use the unique tag: %+v", calls)
	}
}
