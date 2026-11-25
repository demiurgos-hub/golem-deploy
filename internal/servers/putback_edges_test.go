package servers

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
)

// incompleteData makes the server's next safety backup incomplete, so a
// restore, reset or deletion fails after its stop.
func (bf *backupFixture) incompleteData(t *testing.T) {
	t.Helper()
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA), dockertest.Special("link", tar.TypeSymlink, "/"))
}

// A server the operator asked to be stopped is not started again, even when
// its container was still running when the data operation stopped it.
func TestPutBackRespectsDesiredState(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	if err := bf.st.SetServerDesiredState(ctx, bf.id, "stopped"); err != nil {
		t.Fatal(err)
	}
	bf.incompleteData(t)
	bf.f.ResetCalls()
	var log bytes.Buffer
	if err := bf.svc.ResetData(WithJobID(ctx, 3), bf.id, ResetOptions{}, &log); !errors.Is(err, ErrSafetyBackup) {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if starts := bf.f.CallsTo("start"); len(starts) != 0 || bf.running(t) {
		t.Fatalf("started a server that is meant to be stopped: %+v\n%s", starts, log.String())
	}
}

// A container Docker was restarting is stopped by the data operation too,
// so it is started again like a running one.
func TestPutBackRestartingContainer(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	cid := bf.row(t).ContainerID
	bf.incompleteData(t)
	bf.f.SetContainerState(cid, "restarting", 0)
	bf.f.ResetCalls()
	var log bytes.Buffer
	if err := bf.svc.ResetData(WithJobID(ctx, 3), bf.id, ResetOptions{}, &log); !errors.Is(err, ErrSafetyBackup) {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if starts := bf.f.CallsTo("start"); len(starts) != 1 || !slices.Contains(starts[0].Args, cid) || !bf.running(t) {
		t.Fatalf("%+v\n%s", starts, log.String())
	}
}

// Docker finishes a stop it accepted even when the command that asked for
// it was cancelled. The stop is allowed to finish before the container is
// started again, so the server is not reported back and then stops.
func TestInterruptedStopFinishesBeforePutBack(t *testing.T) {
	bf := newBackupFixture(t)
	bf.start(t)
	cid := bf.row(t).ContainerID
	ctx, cancel := context.WithCancel(WithJobID(context.Background(), 3))
	defer cancel()
	var interrupted atomic.Bool
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		if cmd.Args[0] == "stop" && interrupted.CompareAndSwap(false, true) {
			// Accepted by Docker, but the command is cancelled before the
			// container has stopped: it is still running at this moment.
			cancel()
			return context.Canceled
		}
		return bf.f.Run(ctx, path, cmd)
	})
	bf.f.ResetCalls()
	var log bytes.Buffer
	if err := bf.svc.ResetData(ctx, bf.id, ResetOptions{}, &log); err == nil {
		t.Fatalf("succeeded\n%s", log.String())
	}
	var order []string
	for _, c := range bf.f.Calls() {
		if c.Args[0] == "stop" || c.Args[0] == "start" {
			order = append(order, c.Args[0])
		}
	}
	// The fake records only the commands it ran: the stop that finishes the
	// interrupted one, then the start.
	if !slices.Equal(order, []string{"stop", "start"}) || !bf.running(t) || bf.row(t).ContainerID != cid {
		t.Fatalf("%v\n%s", order, log.String())
	}
	if !strings.Contains(log.String(), "the server is running again with its previous data") {
		t.Fatal(log.String())
	}
}

// Crash recovery leaves the recorded container alone when its removal did
// not happen: only a removal that succeeded counts as having replaced it.
// Removing a container stops it first; here that stop fails, so nothing was
// changed, and recovery must not ask for another stop.
func TestRecoverLeavesContainerWhenRemovalFails(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	before := bf.row(t)
	// Another image, same schema: recovery does not adopt the container and
	// the start gets as far as removing it.
	bf.assign(t, bf.buildWith(t, 2, commitB, fpA))
	var stops atomic.Int32
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		if cmd.Args[0] == "stop" {
			stops.Add(1)
			return errors.New("stop failed")
		}
		return bf.f.Run(ctx, path, cmd)
	})
	var log bytes.Buffer
	if err := bf.svc.Recover(WithJobID(ctx, 9), bf.id, &log); err == nil {
		t.Fatalf("recovered\n%s", log.String())
	}
	if n := stops.Load(); n != 1 {
		t.Fatalf("%d stop requests, want only the removal's own\n%s", n, log.String())
	}
	if ct, _ := bf.f.Container(before.ContainerID); ct.State != "running" || bf.row(t).ContainerID != before.ContainerID {
		t.Fatalf("%+v\n%s", ct, log.String())
	}
}

// The first inspection of the snapshot step runs under the step's deadline
// as well.
func TestSnapshotDeadlineBoundsFirstInspection(t *testing.T) {
	bf := newBackupFixture(t, func(c *Config) { c.SnapshotDeadline = 200 * time.Millisecond })
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	var arm, hung atomic.Bool
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		if arm.Load() && len(cmd.Args) > 1 && cmd.Args[0] == "container" && cmd.Args[1] == "inspect" && hung.CompareAndSwap(false, true) {
			<-ctx.Done()
			return ctx.Err()
		}
		return bf.f.Run(ctx, path, cmd)
	})
	sv := bf.row(t)
	arm.Store(true)
	start := time.Now()
	var log bytes.Buffer
	_, err := bf.svc.snapshot(context.Background(), sv, &log)
	if !strings.Contains(detail(err), "within 200ms") {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the step took %s", took)
	}
}

// What docker cp prints about a failed copy can name files of the game; it
// reaches neither the job log nor the daemon log.
func TestCopyFailureKeepsDockerOutputOutOfLogs(t *testing.T) {
	bf := newBackupFixture(t)
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	const name = "secret-file-name-7c1f"
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		if cmd.Args[0] == "cp" {
			if cmd.Stderr != nil {
				_, _ = cmd.Stderr.Write([]byte("Error response from daemon: lstat /data/" + name + ": permission denied"))
			}
			return errors.New("exit status 1")
		}
		return bf.f.Run(ctx, path, cmd)
	})
	log, err := bf.backup(t, BackupOptions{})
	if err == nil {
		t.Fatalf("succeeded\n%s", log)
	}
	if strings.Contains(log, name) || strings.Contains(bf.logs.String(), name) || strings.Contains(err.Error(), name) {
		t.Fatalf("the name reached a log or the error\n%s\n%s\n%v", log, bf.logs.String(), err)
	}
}

// A deletion that fails after it removed the container leaves a server that
// is recorded as stopped, so the next daemon start does not start what is
// left of it.
func TestFailedDeletionLeavesServerStopped(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	bf.failOn(t, false, errors.New("volume rm failed"), func(a []string) bool { return len(a) > 1 && a[0] == "volume" && a[1] == "rm" })
	var log bytes.Buffer
	if err := bf.svc.Delete(WithJobID(ctx, 5), bf.id, DeleteOptions{}, &log); err == nil {
		t.Fatalf("deleted\n%s", log.String())
	}
	row := bf.row(t)
	if row.DesiredState != "stopped" || row.ContainerID != "" {
		t.Fatalf("%+v\n%s", row, log.String())
	}
	if starts := bf.f.CallsTo("start"); len(starts) > 1 {
		t.Fatalf("started again: %+v", starts)
	}
}
