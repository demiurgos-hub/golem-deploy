package servers

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// buildWith records a successful build whose committed code had schema fp.
func (bf *backupFixture) buildWith(t *testing.T, n int, commit, fp string) store.Build {
	t.Helper()
	b := bf.build(t, n, commit)
	if fp != "" {
		if err := bf.st.SetBuildFingerprint(context.Background(), b.ID, fp); err != nil {
			t.Fatal(err)
		}
		b.SchemaFingerprint = fp
	}
	return b
}

func (bf *backupFixture) assign(t *testing.T, b store.Build) {
	t.Helper()
	if _, err := bf.svc.Update(context.Background(), bf.id, apitypes.UpdateServerRequest{BuildID: &b.ID}); err != nil {
		t.Fatal(err)
	}
}

func (bf *backupFixture) running(t *testing.T) bool {
	t.Helper()
	ct, ok := bf.f.Container(bf.row(t).ContainerID)
	return ok && ct.State == "running"
}

func (bf *backupFixture) volumeKept(t *testing.T, vol string) {
	t.Helper()
	if bf.row(t).DataVolume != vol || !bf.f.HasVolume(vol) {
		t.Fatalf("data volume changed: %+v", bf.row(t))
	}
	for _, v := range bf.f.Volumes() {
		if v != vol {
			t.Fatalf("volume %s left behind", v)
		}
	}
}

// Point 1: crash recovery leaves a server the schema guard protects alone.
func TestRecoverLeavesGuardedContainerRunning(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	before := bf.row(t)
	bf.assign(t, bf.buildWith(t, 2, commitB, fpB))
	bf.f.ResetCalls()
	var log bytes.Buffer
	err := bf.svc.Recover(WithJobID(ctx, 9), bf.id, &log)
	if !strings.Contains(detail(err), "build_id") || !refused(err) {
		t.Fatalf("%v\n%s", err, log.String())
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
	// An interrupted create: the guard refuses before it is recovered.
	bf.f.RemoveContainer(before.ContainerID)
	if err := bf.st.ClearServerContainer(ctx, bf.id, before.ContainerID); err != nil {
		t.Fatal(err)
	}
	launch := strings.Repeat("5", 32)
	if err := bf.st.SetServerPendingLaunch(ctx, bf.id, launch); err != nil {
		t.Fatal(err)
	}
	pending := bf.f.AddContainer(dockertest.Container{Name: "golem-deploy-arena", Image: bf.b.ImageID,
		Labels: dockertest.OurLabels(docker.LabelServerID, "1", docker.LabelServer, "arena", docker.LabelLaunchID, launch)})
	if err := bf.svc.Recover(ctx, bf.id, io.Discard); !refused(err) {
		t.Fatalf("%v", err)
	}
	if _, ok := bf.f.Container(pending); !ok {
		t.Fatal("the pending create was removed before the schema check")
	}
}

// Point 2: a game's report fills in an empty build fingerprint but never
// replaces one; the server's data fingerprint follows its game.
func TestGameReportDoesNotReplaceBuildFingerprint(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	log := bf.start(t)
	if b, _ := bf.st.GetBuild(ctx, bf.b.ID); b.SchemaFingerprint != fpA || !strings.Contains(log, "recorded schema "+fpA) {
		t.Fatalf("empty fingerprint not filled in: %q\n%s", b.SchemaFingerprint, log)
	}
	// Another server of the same build whose game reports another schema.
	other, err := bf.svc.Create(ctx, apitypes.CreateServerRequest{Name: "other", BuildID: bf.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	bf.admin.Set(func(s *gameadmintest.Server) { s.Fingerprint = fpB })
	var buf bytes.Buffer
	if err := bf.svc.Start(ctx, other.ID, &buf); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if b, _ := bf.st.GetBuild(ctx, bf.b.ID); b.SchemaFingerprint != fpA {
		t.Fatalf("the report replaced the build's fingerprint: %q", b.SchemaFingerprint)
	}
	if !strings.Contains(buf.String(), "warning: the game reports schema "+fpB+" but build 1 has schema "+fpA) {
		t.Fatal(buf.String())
	}
	if sv, _ := bf.st.GetServer(ctx, other.ID); sv.DataFingerprint != fpB {
		t.Fatalf("%+v", sv)
	}
	// The first server is still judged by the build's own value.
	if v, _ := bf.svc.Get(ctx, bf.id); v.Data.SchemaMismatch {
		t.Fatalf("%+v", v.Data)
	}
}

// Point 3: every backup records what it left out; a safety backup with
// omitted entries is stored and then stops the restore or reset before
// anything is replaced, and zero kept files with omitted entries is not
// "nothing to back up".
func TestSafetyBackupMustBeComplete(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bk := bf.takeTestBackup(t, snapshotFile(fpA))
	if bk.OmittedEntries != 0 {
		t.Fatalf("%+v", bk)
	}
	vol := bf.row(t).DataVolume
	bf.f.SetVolumeFiles(vol, snapshotFile(fpA), dockertest.Special("link", tar.TypeSymlink, "/etc/passwd"))
	log, err := bf.restore(t, RestoreOptions{BackupID: bk.ID})
	msg := detail(err)
	pre, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupPreRestore})
	if len(pre) != 1 || pre[0].OmittedEntries != 1 || pre[0].FileCount != 1 {
		t.Fatalf("%+v", pre)
	}
	if !errors.Is(err, ErrSafetyBackup) || !strings.Contains(msg, "counted above: 1") || !strings.Contains(msg, "stored as backup "+strconv.FormatInt(pre[0].ID, 10)) ||
		!strings.Contains(msg, "skip_safety_backup") || !strings.Contains(msg, "nothing was replaced") {
		t.Fatalf("%v\n%s", err, log)
	}
	bf.volumeKept(t, vol)
	if !bf.running(t) {
		t.Fatal("not started again")
	}
	if v := BackupView(pre[0]); v.OmittedEntries != 1 {
		t.Fatalf("%+v", v)
	}
	// Only entries a backup cannot hold: failed, nothing stored.
	bf.f.SetVolumeFiles(vol, dockertest.Special("link", tar.TypeSymlink, "/etc/passwd"))
	var buf bytes.Buffer
	err = bf.svc.ResetData(ctx, bf.id, ResetOptions{}, &buf)
	if !errors.Is(err, ErrSafetyBackup) || !strings.Contains(detail(err), "no backup was stored") {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if list, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupPreReset}); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
	bf.volumeKept(t, vol)
	// An empty directory is nothing to back up; skip_safety_backup goes
	// ahead with omitted entries.
	if log, err := bf.restore(t, RestoreOptions{BackupID: bk.ID, SkipSafetyBackup: true}); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	bf.f.SetVolumeFiles(bf.row(t).DataVolume)
	buf.Reset()
	if err := bf.svc.ResetData(ctx, bf.id, ResetOptions{}, &buf); err != nil || !strings.Contains(buf.String(), "no current data") {
		t.Fatalf("%v\n%s", err, buf.String())
	}
}

// Point 4: deleting a server stores a last backup first; when it fails,
// nothing is removed and the server runs again.
func TestDeleteStoresLastBackup(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	sv := bf.row(t)
	bf.f.SetVolumeFiles(sv.DataVolume, snapshotFile(fpA), dockertest.Special("pipe", tar.TypeFifo, ""))
	var log bytes.Buffer
	acct := int64(1)
	err := bf.svc.Delete(WithJobID(ctx, 3), bf.id, DeleteOptions{CreatedBy: &acct}, &log)
	if !errors.Is(err, ErrSafetyBackup) || !strings.Contains(detail(err), "nothing was deleted") || !strings.Contains(detail(err), "skip_safety_backup=1") {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if _, err := bf.st.GetServer(ctx, bf.id); err != nil || !bf.f.HasVolume(sv.DataVolume) {
		t.Fatalf("removed something: %v", err)
	}
	if !bf.running(t) {
		t.Fatal("not started again")
	}
	pre, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupPreDelete})
	if len(pre) != 1 || pre[0].OmittedEntries != 1 || *pre[0].BuildID != bf.b.ID {
		t.Fatalf("%+v", pre)
	}
	// A complete last backup: stored, then everything goes.
	bf.f.SetVolumeFiles(sv.DataVolume, snapshotFile(fpA))
	log.Reset()
	if err := bf.svc.Delete(ctx, bf.id, DeleteOptions{}, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	pre, _ = bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupPreDelete})
	if len(pre) != 2 || pre[0].OmittedEntries != 0 || pre[0].ServerID != nil || pre[0].ServerName != "arena" || bf.f.HasVolume(sv.DataVolume) {
		t.Fatalf("%+v", pre)
	}
	if _, err := bf.st.GetServer(ctx, bf.id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	// Skipped on request, and nothing to do without a volume.
	two, _ := bf.svc.Create(ctx, apitypes.CreateServerRequest{Name: "two", BuildID: bf.b.ID})
	if err := bf.svc.Start(ctx, two.ID, io.Discard); err != nil {
		t.Fatal(err)
	}
	vol := func() string { r, _ := bf.st.GetServer(ctx, two.ID); return r.DataVolume }()
	bf.f.SetVolumeFiles(vol, snapshotFile(fpA))
	if err := bf.svc.Delete(ctx, two.ID, DeleteOptions{SkipSafetyBackup: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	three, _ := bf.svc.Create(ctx, apitypes.CreateServerRequest{Name: "three", BuildID: bf.b.ID})
	if err := bf.svc.Delete(ctx, three.ID, DeleteOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if list, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupPreDelete}); len(list) != 2 {
		t.Fatalf("%+v", list)
	}
	bf.noHelpers(t)
}

// Point 5: a restore stops before the switch when the image put files into
// the fresh volume.
func TestRestoreRefusesImageDataInFreshVolume(t *testing.T) {
	bf := newBackupFixture(t)
	bf.start(t)
	bk := bf.takeTestBackup(t, snapshotFile(fpA))
	vol := bf.row(t).DataVolume
	img := dockertest.Image{ID: bf.b.ImageID, Tags: []string{"golem-deploy/game:" + commitA}, AssignPort: bf.admin.Port,
		Labels: dockertest.BuildLabels(buildRef(bf.b)), Content: map[string][]dockertest.File{"/data": {dockertest.Reg("from-image", "x")}}}
	bf.f.AddImage(img)
	log, err := bf.restore(t, RestoreOptions{BackupID: bk.ID, SkipSafetyBackup: true})
	if !strings.Contains(detail(err), "already has files in /data") || !strings.Contains(detail(err), "before anything was replaced") {
		t.Fatalf("%v\n%s", err, log)
	}
	bf.volumeKept(t, vol)
	if !bf.running(t) {
		t.Fatal("not started again")
	}
	for _, in := range bf.f.CopiedIn() {
		if len(in) > 0 {
			t.Fatalf("copied into the volume: %+v", in)
		}
	}
	bf.noHelpers(t)
}

// The job log counts left-out entries per reason and never names them
// (a name can hold an environment value no redaction knows: a short one,
// or one from before an environment change); neither does the daemon log.
// File system errors stay in the daemon log.
func TestBackupJobLogCountsLeftOutEntries(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	if _, err := bf.svc.Update(ctx, bf.id, apitypes.UpdateServerRequest{EnvSet: map[string]string{"SECRET": marker, "SHORT": "q7z"}}); err != nil {
		t.Fatal(err)
	}
	bf.start(t)
	token, err := bf.svc.openAdminToken(bf.row(t))
	if err != nil {
		t.Fatal(err)
	}
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA), dockertest.Special("x-"+marker, tar.TypeSymlink, "/"),
		dockertest.Special("t-"+token, tar.TypeFifo, ""), dockertest.Special("q7z", tar.TypeSymlink, "/"),
		dockertest.Special("old-value-before-a-change", tar.TypeSymlink, "/"), dockertest.Reg("wq\x01xk-entry", "x"))
	log, err := bf.backup(t, BackupOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if !strings.Contains(log, "left out 5 entries: 3 symbolic links, 1 named pipe, 1 with a name that is not allowed\n") {
		t.Fatal(log)
	}
	for _, name := range []string{marker, token, "q7z", "old-value", "xk-entry"} {
		if strings.Contains(log, name) || strings.Contains(bf.logs.String(), name) {
			t.Fatalf("%q reached a log\n%s\n%s", name, log, bf.logs.String())
		}
	}
	// The backup directory disappears: the failure names no path.
	if err := os.RemoveAll(bf.dir); err != nil {
		t.Fatal(err)
	}
	_, err = bf.backup(t, BackupOptions{})
	if err == nil || strings.Contains(err.Error(), bf.dir) || strings.Contains(err.Error(), "backups") && !strings.Contains(detail(err), "backup file") {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(detail(err), "see the daemon log") {
		t.Fatalf("%v", err)
	}
}

// Point 9: one deadline for the whole snapshot request, the port taken
// from a fresh inspection before each retry, and 503 as "not ready".
func TestSnapshotRequestDeadlineAndPort(t *testing.T) {
	bf := newBackupFixture(t, func(c *Config) { c.SnapshotDeadline = 200 * time.Millisecond; c.SnapshotRetry = 5 * time.Millisecond })
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotBusyFor = 1 << 30 })
	start := time.Now()
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "within 200ms") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the deadline did not bound the request")
	}
	// The container's admin port moves while the game reports a snapshot
	// in progress: the next request goes to the new port only.
	second := gameadmintest.New(t)
	second.Set(func(s *gameadmintest.Server) { s.ExpectToken = func() string { return bf.f.LatestEnv(EnvAdminToken) } })
	cid := bf.row(t).ContainerID
	bf.admin.Set(func(s *gameadmintest.Server) {
		s.SnapshotBusyFor = 1
		s.Snapshots = 0
		s.OnBusy = func() { bf.f.SetPublishedPort(cid, "7778/tcp", second.Port) }
	})
	log, err := bf.backup(t, BackupOptions{})
	if err != nil || !strings.Contains(log, "fresh snapshot") {
		t.Fatalf("%v\n%s", err, log)
	}
	bf.admin.Set(func(s *gameadmintest.Server) {
		if s.Snapshots != 1 {
			t.Errorf("old port asked %d times", s.Snapshots)
		}
	})
	second.Set(func(s *gameadmintest.Server) {
		if s.Snapshots != 1 {
			t.Errorf("new port asked %d times", s.Snapshots)
		}
	})
	second.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 503 })
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "try again shortly") {
		t.Fatalf("%v", err)
	}
}

// cancelOn runs the fake docker CLI and cancels when match accepts a
// finished command.
func (bf *backupFixture) cancelOn(t *testing.T, cancel context.CancelFunc, match func(args []string) bool) {
	t.Helper()
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		err := bf.f.Run(ctx, path, cmd)
		if err == nil && match(cmd.Args) {
			cancel()
		}
		return err
	})
}

// Point 10: a cancellation right before the switch leaves the old data
// and starts the server again; one after the switch ends successfully
// without the automatic start.
func TestRestoreAndResetCancellation(t *testing.T) {
	bf := newBackupFixture(t)
	bf.start(t)
	bk := bf.takeTestBackup(t, snapshotFile(fpA))
	vol := bf.row(t).DataVolume

	ctx, cancel := context.WithCancel(context.Background())
	bf.cancelOn(t, cancel, func(a []string) bool { return a[0] == "cp" && slices.Contains(a, "-") && a[len(a)-1] != "-" })
	var log bytes.Buffer
	err := bf.svc.Restore(ctx, bf.id, RestoreOptions{BackupID: bk.ID, SkipSafetyBackup: true}, &log)
	if !errors.Is(err, context.Canceled) || !strings.Contains(log.String(), "cancelled before the server was switched") {
		t.Fatalf("%v\n%s", err, log.String())
	}
	bf.volumeKept(t, vol)
	if !bf.running(t) {
		t.Fatalf("not started again\n%s", log.String())
	}

	// Cancelled while the old volume is removed, after the switch.
	ctx, cancel = context.WithCancel(context.Background())
	bf.cancelOn(t, cancel, func(a []string) bool { return a[0] == "volume" && a[1] == "rm" })
	log.Reset()
	if err := bf.svc.Restore(ctx, bf.id, RestoreOptions{BackupID: bk.ID, SkipSafetyBackup: true}, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	sv := bf.row(t)
	if sv.DataVolume == vol || bf.f.HasVolume(vol) || sv.ContainerID != "" || !strings.Contains(log.String(), "skipping the automatic start") {
		t.Fatalf("%+v\n%s", sv, log.String())
	}

	// A reset cancelled once its safety backup is stored.
	bf.start(t)
	vol = bf.row(t).DataVolume
	ctx, cancel = context.WithCancel(context.Background())
	bf.cancelOn(t, cancel, func(a []string) bool { return a[0] == "cp" && a[len(a)-1] == "-" })
	log.Reset()
	err = bf.svc.ResetData(ctx, bf.id, ResetOptions{}, &log)
	if !errors.Is(err, context.Canceled) || !strings.Contains(log.String(), "cancelled before the data was detached") {
		t.Fatalf("%v\n%s", err, log.String())
	}
	bf.volumeKept(t, vol)
	if !bf.running(t) {
		t.Fatalf("not started again\n%s", log.String())
	}
}

// Point 11: the job checks again that the backup is due, and the delay
// after a failure survives a daemon restart.
func TestScheduledBackupChecksAgain(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	if _, err := bf.svc.Update(ctx, bf.id, apitypes.UpdateServerRequest{BackupSchedule: &apitypes.BackupSchedule{IntervalHours: 1, Keep: 3}}); err != nil {
		t.Fatal(err)
	}
	run := func(svc *Service, before func()) (string, []int64, error) {
		var log bytes.Buffer
		var bodyErr error
		started, err := svc.ScheduleBackups(ctx, func(id int64, body func(context.Context, io.Writer) error) error {
			if before != nil {
				before()
			}
			bodyErr = body(ctx, &log)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return log.String(), started, bodyErr
	}
	count := func() int { l, _ := bf.st.ListBackups(ctx, store.BackupFilter{}); return len(l) }
	off := apitypes.BackupSchedule{IntervalHours: 0, Keep: 3}
	log, started, err := run(bf.svc, func() { _, _ = bf.svc.Update(ctx, bf.id, apitypes.UpdateServerRequest{BackupSchedule: &off}) })
	if len(started) != 1 || err != nil || !strings.Contains(log, "turned off") || count() != 0 {
		t.Fatalf("%v %v %d\n%s", started, err, count(), log)
	}
	on := apitypes.BackupSchedule{IntervalHours: 1, Keep: 3}
	_, _ = bf.svc.Update(ctx, bf.id, apitypes.UpdateServerRequest{BackupSchedule: &on})
	log, _, err = run(bf.svc, func() { _ = bf.svc.Stop(ctx, bf.id, io.Discard) })
	if err != nil || !strings.Contains(log, "no longer running") || count() != 0 {
		t.Fatalf("%v %d\n%s", err, count(), log)
	}
	bf.start(t)
	log, _, err = run(bf.svc, func() { _, _ = bf.backup(t, BackupOptions{}) })
	if err != nil || !strings.Contains(log, "taken meanwhile") || count() != 1 {
		t.Fatalf("%v %d\n%s", err, count(), log)
	}
	// A failed scheduled backup; a restarted daemon still waits.
	bf.mu.Lock()
	bf.offset += 2 * time.Hour
	bf.mu.Unlock()
	bf.f.SetCopyHook(func(dockertest.CopyCall) error { return errors.New("broken") })
	if _, started, err := run(bf.svc, nil); len(started) != 1 || err == nil {
		t.Fatalf("%v %v", started, err)
	}
	bf.f.SetCopyHook(nil)
	if bf.row(t).BackupFailedAt == nil {
		t.Fatal("failure not recorded")
	}
	restarted := New(bf.st, bf.svc.box, bf.svc.docker, bf.svc.cfg, bf.svc.logger)
	if _, started, _ := run(restarted, nil); len(started) != 0 {
		t.Fatal("retried right after a restart")
	}
	bf.mu.Lock()
	bf.offset += ScheduleRetryDelay
	bf.mu.Unlock()
	if _, started, err := run(restarted, nil); len(started) != 1 || err != nil || bf.row(t).BackupFailedAt != nil {
		t.Fatalf("%v %v", started, err)
	}
}

// Point 12: a backup records the build that wrote the data (the recorded
// container's) and uses the first build whose image still exists.
func TestBackupProvenance(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	b2 := bf.build(t, 2, commitB)
	bf.assign(t, b2)
	bf.f.DeleteImage(b2.ImageID)
	if log, err := bf.backup(t, BackupOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	list, _ := bf.st.ListBackups(ctx, store.BackupFilter{})
	if b := list[0]; b.BuildID == nil || *b.BuildID != bf.b.ID || b.Commit != commitA || b.ProjectName != "game" {
		t.Fatalf("%+v", b)
	}
	for _, c := range bf.f.CallsTo("create") {
		if slices.Contains(c.Args, b2.ImageID) {
			t.Fatalf("helper from the missing image: %q", c.Args)
		}
	}
	// Stopped: still the container's build.
	if err := bf.svc.Stop(ctx, bf.id, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := bf.backup(t, BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	if list, _ := bf.st.ListBackups(ctx, store.BackupFilter{}); *list[0].BuildID != bf.b.ID {
		t.Fatalf("%+v", list[0])
	}
	// Without a recorded container: the assigned build.
	bf.f.RemoveContainer(bf.row(t).ContainerID)
	if err := bf.st.ClearServerContainer(ctx, bf.id, bf.row(t).ContainerID); err != nil {
		t.Fatal(err)
	}
	if _, err := bf.backup(t, BackupOptions{}); !errors.Is(err, ErrNoBuild) {
		t.Fatalf("%v", err)
	}
	bf.f.AddImage(dockertest.Image{ID: b2.ImageID, Labels: dockertest.BuildLabels(buildRef(b2)), AssignPort: bf.admin.Port})
	if _, err := bf.backup(t, BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	if list, _ := bf.st.ListBackups(ctx, store.BackupFilter{}); *list[0].BuildID != b2.ID || list[0].Commit != commitB {
		t.Fatalf("%+v", list[0])
	}
}

// Point 13: a helper create whose outcome is unknown keeps its record
// until the cleanup has looked for it for helperForgetAfter.
func TestLostHelperCreateKeepsRecord(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	bf.interceptDocker(t, func(ctx context.Context, path string, cmd docker.Cmd) error {
		if cmd.Args[0] == "create" && slices.ContainsFunc(cmd.Args, func(a string) bool {
			return strings.HasPrefix(a, docker.LabelHelper+"=") && a != docker.LabelHelper+"="
		}) {
			return context.DeadlineExceeded // no reply, and (for now) no container
		}
		return bf.f.Run(ctx, path, cmd)
	})
	if _, err := bf.backup(t, BackupOptions{}); err == nil {
		t.Fatal("backup succeeded")
	}
	rows, _ := bf.st.ListVolumeHelpers(ctx)
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	if err := bf.svc.CleanupHelpers(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := bf.st.ListVolumeHelpers(ctx); len(rows) != 1 {
		t.Fatal("record forgotten on the first look")
	}
	bf.mu.Lock()
	bf.offset += helperForgetAfter + time.Minute
	bf.mu.Unlock()
	if err := bf.svc.CleanupHelpers(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := bf.st.ListVolumeHelpers(ctx); len(rows) != 0 {
		t.Fatalf("%+v", rows)
	}
}

// Point 14: field errors are worded for people.
func TestDataSettingsWording(t *testing.T) {
	bad, zero := 5, 0
	for want, err := range map[string]error{
		"the autosave period must be 0 (off) or 10 to 86400 seconds": func() error { _, _, _, e := dataSettings("", &bad, nil, 0, 0, 1); return e }(),
		"the backup interval must be 0 (off) or 1 to 720 hours": func() error {
			_, _, _, e := dataSettings("", nil, &apitypes.BackupSchedule{IntervalHours: 721, Keep: 1}, 0, 0, 1)
			return e
		}(),
		"the number of scheduled backups to keep must be 1 to 100": func() error {
			_, _, _, e := dataSettings("", nil, &apitypes.BackupSchedule{IntervalHours: zero, Keep: 0}, 0, 0, 1)
			return e
		}(),
	} {
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Msg != want {
			t.Errorf("%v, want %q", err, want)
		}
	}
}
