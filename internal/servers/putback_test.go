package servers

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// isHelperCreate reports whether a docker create made a helper container.
func isHelperCreate(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return strings.HasPrefix(a, docker.LabelHelper+"=") && a != docker.LabelHelper+"="
	})
}

// failOn runs the fake docker CLI but fails the commands match accepts,
// after running them when after is set.
func (bf *backupFixture) failOn(t *testing.T, after bool, fail error, match func(args []string) bool) {
	t.Helper()
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		if !match(cmd.Args) {
			return bf.f.Run(ctx, path, cmd)
		}
		if after {
			if err := bf.f.Run(ctx, path, cmd); err != nil {
				return err
			}
		}
		return fail
	})
}

// Every exit of a restore, reset or deletion after its stop and before the
// data is switched or removed starts the same container again: no new
// container, no removal, and the assigned build (here one the schema guard
// would refuse) is never consulted.
func TestFailedDataOperationPutsBackStoppedContainer(t *testing.T) {
	restore := func(ctx context.Context, bf *backupFixture, bk store.Backup, log io.Writer) error {
		return bf.svc.Restore(ctx, bf.id, RestoreOptions{BackupID: bk.ID, Force: true}, log)
	}
	reset := func(ctx context.Context, bf *backupFixture, _ store.Backup, log io.Writer) error {
		return bf.svc.ResetData(ctx, bf.id, ResetOptions{}, log)
	}
	del := func(ctx context.Context, bf *backupFixture, _ store.Backup, log io.Writer) error {
		return bf.svc.Delete(ctx, bf.id, DeleteOptions{}, log)
	}
	symlink := func(t *testing.T, bf *backupFixture, _ context.CancelFunc, _ string) {
		bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA), dockertest.Special("link", tar.TypeSymlink, "/"))
	}
	copyOutFails := func(t *testing.T, bf *backupFixture, _ context.CancelFunc, _ string) {
		bf.f.SetCopyHook(func(c dockertest.CopyCall) error {
			if c.Out {
				return errors.New("copy failed")
			}
			return nil
		})
	}
	stopFails := func(t *testing.T, bf *backupFixture, _ context.CancelFunc, _ string) {
		bf.failOn(t, true, errors.New("stop failed"), func(a []string) bool { return a[0] == "stop" })
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, bf *backupFixture, cancel context.CancelFunc, cid string)
		run     func(ctx context.Context, bf *backupFixture, bk store.Backup, log io.Writer) error
	}{
		{"restore: the stop fails after stopping", stopFails, restore},
		{"restore: the stop is cancelled", func(t *testing.T, bf *backupFixture, cancel context.CancelFunc, _ string) {
			bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
				err := bf.f.Run(ctx, path, cmd)
				if cmd.Args[0] == "stop" {
					cancel()
					return context.Canceled
				}
				return err
			})
		}, restore},
		{"restore: incomplete safety backup", symlink, restore},
		{"restore: safety backup fails", copyOutFails, restore},
		{"restore: new volume", func(t *testing.T, bf *backupFixture, _ context.CancelFunc, _ string) {
			bf.failOn(t, false, errors.New("volume create failed"), func(a []string) bool { return len(a) > 1 && a[0] == "volume" && a[1] == "create" })
		}, restore},
		{"restore: copy into the new volume", func(t *testing.T, bf *backupFixture, _ context.CancelFunc, _ string) {
			bf.f.SetCopyHook(func(c dockertest.CopyCall) error {
				if !c.Out {
					return errors.New("copy failed")
				}
				return nil
			})
		}, restore},
		{"restore: image data in the new volume", func(t *testing.T, bf *backupFixture, _ context.CancelFunc, _ string) {
			b2, _ := bf.st.GetBuild(context.Background(), *bf.row(t).BuildID)
			bf.f.AddImage(dockertest.Image{ID: b2.ImageID, AssignPort: bf.admin.Port, Labels: dockertest.BuildLabels(buildRef(b2)),
				Content: map[string][]dockertest.File{"/data": {dockertest.Reg("from-image", "x")}}})
		}, restore},
		{"restore: cancelled before the switch", func(t *testing.T, bf *backupFixture, cancel context.CancelFunc, _ string) {
			bf.cancelOn(t, cancel, func(a []string) bool { return a[0] == "cp" && slices.Contains(a, "-") && a[len(a)-1] != "-" })
		}, restore},
		{"reset: the stop fails after stopping", stopFails, reset},
		{"reset: safety backup fails", copyOutFails, reset},
		{"reset: incomplete safety backup", symlink, reset},
		{"delete: the stop fails after stopping", stopFails, del},
		{"delete: incomplete last backup", symlink, del},
		{"delete: last backup fails", copyOutFails, del},
		{"delete: removing the container fails", func(t *testing.T, bf *backupFixture, _ context.CancelFunc, cid string) {
			bf.failOn(t, false, errors.New("rm failed"), func(a []string) bool { return a[0] == "rm" && slices.Contains(a, cid) })
		}, del},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bf := newBackupFixture(t)
			bf.start(t)
			// The server runs build 1; build 2, assigned since, has another
			// schema, so a normal start would be refused.
			bf.assign(t, bf.buildWith(t, 2, commitB, fpB))
			bk := bf.takeTestBackup(t, snapshotFile(fpA))
			before := bf.row(t)
			ctx, cancel := context.WithCancel(WithJobID(context.Background(), 7))
			defer cancel()
			c.prepare(t, bf, cancel, before.ContainerID)
			bf.f.ResetCalls()
			var log bytes.Buffer
			if err := c.run(ctx, bf, bk, &log); err == nil {
				t.Fatalf("succeeded\n%s", log.String())
			}
			after := bf.row(t)
			ct, ok := bf.f.Container(before.ContainerID)
			if !ok || ct.State != "running" || after.ContainerID != before.ContainerID || !after.StartComplete {
				t.Fatalf("%+v %+v\n%s", ct, after, log.String())
			}
			if !strings.Contains(log.String(), "the server is running again with its previous data") {
				t.Fatal(log.String())
			}
			for _, call := range bf.f.Calls() {
				switch {
				case call.Args[0] == "create" && !isHelperCreate(call.Args):
					t.Fatalf("created a server container: %q", call.Args)
				case call.Args[0] == "rm" && slices.Contains(call.Args, before.ContainerID) && !strings.Contains(c.name, "removing"):
					t.Fatalf("removed the server container: %q", call.Args)
				}
			}
			if starts := bf.f.CallsTo("start"); len(starts) != 1 || !slices.Contains(starts[0].Args, before.ContainerID) {
				t.Fatalf("%+v", starts)
			}
			bf.volumeKept(t, before.DataVolume)
			bf.noHelpers(t)
		})
	}
}

// A server whose container was not running before is left stopped; a
// container that stops again before its game is ready is reported.
func TestPutBackOnlyWhatRan(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	if err := bf.svc.Stop(ctx, bf.id, io.Discard); err != nil {
		t.Fatal(err)
	}
	bk := bf.takeTestBackup(t, snapshotFile(fpA))
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA), dockertest.Special("link", tar.TypeSymlink, "/"))
	bf.f.ResetCalls()
	if log, err := bf.restore(t, RestoreOptions{BackupID: bk.ID}); !errors.Is(err, ErrSafetyBackup) || strings.Contains(log, "again") {
		t.Fatalf("%v\n%s", err, log)
	}
	if starts := bf.f.CallsTo("start"); len(starts) != 0 || bf.running(t) {
		t.Fatalf("started a server that was not running: %+v", starts)
	}

	// Running before, but the game does not come back.
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA), dockertest.Special("link", tar.TypeSymlink, "/"))
	bf.svc.cfg.StartTimeout = 100 * time.Millisecond
	bf.admin.Set(func(s *gameadmintest.Server) { s.NotReadyFor = 1 << 30 })
	log, err := bf.restore(t, RestoreOptions{BackupID: bk.ID})
	if !errors.Is(err, ErrSafetyBackup) || !strings.Contains(log, "runs again, but its game did not report ready within 100ms") {
		t.Fatalf("%v\n%s", err, log)
	}
}

// Crash recovery stops the recorded container only when its own start got
// as far as replacing it: any earlier failure leaves it running.
func TestRecoverStopsOnlyWhatItReplaced(t *testing.T) {
	for _, fault := range []string{"missing image", "foreign volume"} {
		t.Run(fault, func(t *testing.T) {
			bf := newBackupFixture(t)
			ctx := context.Background()
			bf.start(t)
			before := bf.row(t)
			b2 := bf.build(t, 2, commitB)
			bf.assign(t, b2)
			switch fault {
			case "missing image":
				bf.f.DeleteImage(b2.ImageID)
			case "foreign volume":
				bf.f.AddVolume(dockertest.Volume{Name: "someone-elses", Labels: map[string]string{}})
				if err := bf.st.SetServerDataVolume(ctx, bf.id, "someone-elses", false); err != nil {
					t.Fatal(err)
				}
			}
			bf.f.ResetCalls()
			var log bytes.Buffer
			if err := bf.svc.Recover(WithJobID(ctx, 9), bf.id, &log); err == nil {
				t.Fatalf("recovered\n%s", log.String())
			}
			for _, c := range bf.f.Calls() {
				if c.Args[0] == "stop" || c.Args[0] == "rm" || c.Args[0] == "create" || c.Args[0] == "kill" {
					t.Fatalf("touched a container: %q", c.Args)
				}
			}
			if ct, _ := bf.f.Container(before.ContainerID); ct.State != "running" || bf.row(t).ContainerID != before.ContainerID {
				t.Fatalf("%+v", ct)
			}
			if !strings.Contains(log.String(), "left as it is") {
				t.Fatal(log.String())
			}
		})
	}
}
