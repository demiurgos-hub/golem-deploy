package servers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// The inspections between snapshot retries run under the request's
// deadline, so a Docker that hangs cannot stretch the step.
func TestSnapshotDeadlineBoundsInspections(t *testing.T) {
	bf := newBackupFixture(t, func(c *Config) { c.SnapshotDeadline = 200 * time.Millisecond; c.SnapshotRetry = 5 * time.Millisecond })
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	var busy atomic.Bool
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotBusyFor = 1 << 30; s.OnBusy = func() { busy.Store(true) } })
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		if busy.Load() && len(cmd.Args) > 1 && cmd.Args[0] == "container" && cmd.Args[1] == "inspect" {
			<-ctx.Done()
			return ctx.Err()
		}
		return bf.f.Run(ctx, path, cmd)
	})
	start := time.Now()
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "within 200ms") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the step took %s", time.Since(start))
	}
}

// The re-check once the job slot is held honours a failure recorded after
// the tick read the server, and its own errors record a failure.
func TestScheduledRecheckHonoursFailures(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	if _, err := bf.svc.Update(ctx, bf.id, apitypes.UpdateServerRequest{BackupSchedule: &apitypes.BackupSchedule{IntervalHours: 1, Keep: 3}}); err != nil {
		t.Fatal(err)
	}
	run := func(before func()) (string, error) {
		var log bytes.Buffer
		var bodyErr error
		started, err := bf.svc.ScheduleBackups(ctx, func(id int64, body func(context.Context, io.Writer) error) error {
			before()
			bodyErr = body(ctx, &log)
			return nil
		})
		if err != nil || len(started) != 1 {
			t.Fatalf("%v %v", started, err)
		}
		return log.String(), bodyErr
	}
	// A failing job records its failure after the tick read the row.
	log, err := run(func() {
		now := bf.svc.cfg.Now()
		if err := bf.st.SetServerBackupFailed(ctx, bf.id, &now); err != nil {
			t.Fatal(err)
		}
	})
	if list, _ := bf.st.ListBackups(ctx, store.BackupFilter{}); err != nil || len(list) != 0 || !strings.Contains(log, "a scheduled backup failed at") {
		t.Fatalf("%v %d\n%s", err, len(list), log)
	}
	// The re-check cannot inspect the container: a failure is recorded.
	if err := bf.st.SetServerBackupFailed(ctx, bf.id, nil); err != nil {
		t.Fatal(err)
	}
	var broken atomic.Bool
	bf.failOn(t, false, errors.New("Cannot connect to the Docker daemon"), func(a []string) bool {
		return broken.Load() && len(a) > 1 && a[0] == "container" && a[1] == "inspect"
	})
	if _, err := run(func() { broken.Store(true) }); err == nil {
		t.Fatal("the re-check did not fail")
	}
	if bf.row(t).BackupFailedAt == nil {
		t.Fatal("the failure of the re-check was not recorded")
	}
}
