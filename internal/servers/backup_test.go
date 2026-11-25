package servers

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

const (
	fpA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fpB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// snapshotFile is a world.snapshot with fingerprint fp.
func snapshotFile(fp string) dockertest.File {
	b := []byte{'G', 'L', 'M', 1}
	b = binary.AppendUvarint(b, 2)
	b = binary.AppendUvarint(b, 1732400000)
	b = binary.AppendUvarint(b, uint64(len(fp)))
	return dockertest.Reg("world.snapshot", string(append(b, fp...))+"entities")
}

type backupFixture struct {
	*fixture
	dir string
	b   store.Build
	id  int64
	// offset shifts the schedule's clock.
	mu      sync.Mutex
	offset  time.Duration
	lastJob int64
}

// newBackupFixture returns a fixture with a backup store and a created,
// started server whose game reports fpA.
func newBackupFixture(t *testing.T, mutate ...func(*Config)) *backupFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backups")
	bs, err := backups.Open(dir, backups.Options{CheckFree: func(string, int64) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	bf := &backupFixture{dir: dir}
	cfg := Config{Backups: bs, SnapshotRetry: time.Millisecond, Now: func() time.Time {
		bf.mu.Lock()
		defer bf.mu.Unlock()
		return time.Now().Add(bf.offset)
	}}
	for _, m := range mutate {
		m(&cfg)
	}
	bf.fixture = newFixture(t, cfg)
	bf.admin.Set(func(s *gameadmintest.Server) { s.Fingerprint = fpA })
	bf.b = bf.build(t, 1, commitA)
	sv, err := bf.svc.Create(context.Background(), apitypes.CreateServerRequest{Name: "arena", BuildID: bf.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	bf.id = sv.ID
	return bf
}

func (bf *backupFixture) row(t *testing.T) store.Server {
	t.Helper()
	sv, err := bf.st.GetServer(context.Background(), bf.id)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

func (bf *backupFixture) start(t *testing.T) string {
	t.Helper()
	var log bytes.Buffer
	if err := bf.svc.Start(WithJobID(context.Background(), 1), bf.id, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	return log.String()
}

// backup runs a backup job body and returns its log.
func (bf *backupFixture) backup(t *testing.T, o BackupOptions) (string, error) {
	t.Helper()
	var log bytes.Buffer
	job, err := bf.st.CreateJob(context.Background(), store.NewJob{Kind: "server.backup", SubjectType: "server", SubjectID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	bf.lastJob = job.ID
	err = bf.svc.Backup(WithJobID(context.Background(), job.ID), bf.id, o, &log)
	return log.String(), err
}

// archives lists the files in the backup directory.
func (bf *backupFixture) archives(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(bf.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (bf *backupFixture) noHelpers(t *testing.T) {
	t.Helper()
	for _, ct := range bf.f.Containers() {
		if ct.Labels[docker.LabelHelper] != "" {
			t.Fatalf("helper container left: %+v", ct)
		}
	}
	if rows, _ := bf.st.ListVolumeHelpers(context.Background()); len(rows) != 0 {
		t.Fatalf("helper records left: %+v", rows)
	}
}

func detail(err error) string {
	var safe *jobs.SafeError
	if errors.As(err, &safe) {
		return safe.Msg
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// A start creates the data volume, mounts it with the snapshot settings and
// records the fingerprint the game reports; a recorded volume that is gone
// is replaced and its fingerprint forgotten.
func TestStartGivesDataVolume(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	log := bf.start(t)
	sv := bf.row(t)
	if !strings.HasPrefix(sv.DataVolume, "golem-deploy-data-1-") || sv.DataFingerprint != fpA || !strings.Contains(log, "created the data volume") {
		t.Fatalf("%+v\n%s", sv, log)
	}
	labels := bf.f.VolumeLabels(sv.DataVolume)
	if labels[docker.LabelServerID] != "1" || labels[docker.LabelInstance] != dockertest.InstanceID {
		t.Fatalf("%v", labels)
	}
	ct, _ := bf.f.Container(sv.ContainerID)
	if ct.MountTargets["/data"] != sv.DataVolume || ct.Env[EnvSnapshotDir] != "/data" || ct.Env[EnvSnapshotInterval] != "300s" {
		t.Fatalf("%+v", ct)
	}
	// A restart keeps the volume; a changed autosave period needs one.
	if v, _ := bf.svc.Get(ctx, bf.id); v.RestartRequired || v.Data.Volume != sv.DataVolume || v.Data.SchemaFingerprint != fpA || v.SnapshotIntervalSeconds != 300 {
		t.Fatalf("%+v", v)
	}
	zero := 0
	if v, err := bf.svc.Update(ctx, bf.id, apitypes.UpdateServerRequest{SnapshotIntervalSeconds: &zero}); err != nil || !v.RestartRequired {
		t.Fatalf("%+v %v", v, err)
	}
	bf.start(t)
	if again := bf.row(t); again.DataVolume != sv.DataVolume {
		t.Fatalf("volume changed: %s", again.DataVolume)
	}
	ct, _ = bf.f.Container(bf.row(t).ContainerID)
	if ct.Env[EnvSnapshotInterval] != "0" {
		t.Fatalf("%v", ct.Env)
	}
	// The recorded volume disappears behind the daemon's back.
	if err := bf.st.SetServerDataFingerprint(ctx, bf.id, sv.DataVolume, fpB); err != nil {
		t.Fatal(err)
	}
	bf.f.RemoveContainer(bf.row(t).ContainerID)
	bf.f.SetVolumeFiles(sv.DataVolume)
	bf.svc.docker.RemoveVolume(ctx, sv.DataVolume) //nolint:errcheck
	log = bf.start(t)
	next := bf.row(t)
	if next.DataVolume == sv.DataVolume || next.DataVolume == "" || next.DataFingerprint != fpA || !strings.Contains(log, "no longer exists") {
		t.Fatalf("%+v\n%s", next, log)
	}
}

// A schema mismatch fails a start or restart before the running container
// is touched, and the view reports it.
func TestSchemaGuardLeavesRunningContainer(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	before := bf.row(t)
	if err := bf.st.SetBuildFingerprint(ctx, bf.b.ID, fpB); err != nil {
		t.Fatal(err)
	}
	bf.f.ResetCalls()
	var log bytes.Buffer
	err := bf.svc.Restart(ctx, bf.id, &log)
	msg := detail(err)
	if err == nil || !strings.Contains(msg, fpA) || !strings.Contains(msg, fpB) || !strings.Contains(msg, "restore") || !strings.Contains(msg, "reset") ||
		!strings.Contains(msg, "build_id") {
		t.Fatalf("%v", err)
	}
	for _, c := range bf.f.Calls() {
		if c.Args[0] == "stop" || c.Args[0] == "rm" || c.Args[0] == "create" {
			t.Fatalf("touched the container: %q", c.Args)
		}
	}
	ct, _ := bf.f.Container(before.ContainerID)
	if ct.State != "running" || bf.row(t).ContainerID != before.ContainerID {
		t.Fatalf("%+v", ct)
	}
	if v, _ := bf.svc.Get(ctx, bf.id); !v.Data.SchemaMismatch {
		t.Fatalf("%+v", v.Data)
	}
	// An unknown fingerprint on either side passes.
	if err := bf.st.SetServerDataFingerprint(ctx, bf.id, before.DataVolume, ""); err != nil {
		t.Fatal(err)
	}
	bf.start(t)
}

// A backup of a running game asks for a fresh snapshot first; a stopped
// server or a game without the snapshot hook is copied as it is.
func TestBackupLiveAndStopped(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	vol := bf.row(t).DataVolume
	bf.admin.Set(func(s *gameadmintest.Server) {
		s.SnapshotBusyFor = 2
		s.OnSnapshot = func() {
			bf.f.SetVolumeFiles(vol, snapshotFile(fpA), dockertest.Dir("content"), dockertest.Reg("content/terrain.bin", "terrain"),
				dockertest.Special("escape", tar.TypeSymlink, "/etc/shadow"), dockertest.Special("pipe", tar.TypeFifo, ""))
		}
	})
	acct := int64(5)
	log, err := bf.backup(t, BackupOptions{Kind: store.BackupManual, Note: "before the event", CreatedBy: &acct})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	list, _ := bf.st.ListBackups(ctx, store.BackupFilter{})
	if len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	b := list[0]
	if !b.Live || b.SchemaFingerprint != fpA || b.FileCount != 2 || b.Kind != "manual" || b.Note != "before the event" || b.ServerName != "arena" ||
		*b.BuildID != bf.b.ID || *b.ProjectID != bf.b.ProjectID || b.ProjectName != "game" || b.Commit != commitA || b.JobID == nil || *b.JobID != bf.lastJob || b.CreatedBy != nil {
		t.Fatalf("%+v", b) // account 5 does not exist, so created_by is null
	}
	if !strings.Contains(log, "left out 2 entries: 1 symbolic link, 1 named pipe\n") || strings.Contains(log, "escape") || !strings.Contains(log, "already in progress") {
		t.Fatal(log)
	}
	if err := bf.svc.cfg.Backups.Verify(b.File, b.SizeBytes, b.SHA256); err != nil {
		t.Fatal(err)
	}
	bf.noHelpers(t)
	if strings.Contains(bf.logs.String(), "ignoring a container") {
		t.Fatal(bf.logs.String())
	}
	if v, _ := bf.svc.Get(ctx, bf.id); v.Data.LastBackupAt == nil {
		t.Fatalf("%+v", v.Data)
	}
	// No snapshot hook: copied as it is.
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	if log, err := bf.backup(t, BackupOptions{}); err != nil || !strings.Contains(log, "no snapshot hook") {
		t.Fatalf("%v\n%s", err, log)
	}
	// The game fails its snapshot: the backup fails with a clear detail.
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 500 })
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "could not write its snapshot") {
		t.Fatalf("%v", err)
	}
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 504 })
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "in time") {
		t.Fatalf("%v", err)
	}
	// Stopped: no snapshot request.
	if err := bf.svc.Stop(ctx, bf.id, io.Discard); err != nil {
		t.Fatal(err)
	}
	before := 0
	bf.admin.Set(func(s *gameadmintest.Server) { before = s.Snapshots })
	if log, err := bf.backup(t, BackupOptions{}); err != nil || !strings.Contains(log, "not running") {
		t.Fatalf("%v\n%s", err, log)
	}
	bf.admin.Set(func(s *gameadmintest.Server) {
		if s.Snapshots != before {
			t.Error("asked a stopped server for a snapshot")
		}
	})
	list, _ = bf.st.ListBackups(ctx, store.BackupFilter{})
	if len(list) != 3 || list[0].Live || list[1].Live || !list[2].Live {
		t.Fatalf("%+v", list)
	}
	if len(bf.archives(t)) != 3 {
		t.Fatal(bf.archives(t))
	}
	bf.noHelpers(t)
}

// Nothing to back up, a data directory over the limit and a copy failure
// fail the job without leaving files or helpers behind.
func TestBackupFailures(t *testing.T) {
	bf := newBackupFixture(t, func(c *Config) { c.BackupMaxBytes = 100 })
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "no saved data yet") {
		t.Fatalf("%v", err)
	}
	bf.start(t)
	vol := bf.row(t).DataVolume
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	bf.f.SetVolumeFiles(vol, dockertest.Special("only-a-link", tar.TypeSymlink, "/"))
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "holds no regular files (entries a backup cannot hold: 1)") {
		t.Fatalf("%v", err)
	}
	bf.f.SetVolumeFiles(vol, dockertest.Reg("big", strings.Repeat("x", 101)))
	if _, err := bf.backup(t, BackupOptions{}); !strings.Contains(detail(err), "backup size limit of 0 MiB") {
		t.Fatalf("%v", err)
	}
	bf.f.SetVolumeFiles(vol, dockertest.Reg("small", "x"))
	bf.f.SetCopyHook(func(dockertest.CopyCall) error { return errors.New("disk on fire " + marker) })
	_, err := bf.backup(t, BackupOptions{})
	if !strings.Contains(detail(err), "Docker could not copy the data directory") || strings.Contains(detail(err), marker) {
		t.Fatalf("%v", err)
	}
	if got := bf.archives(t); len(got) != 0 {
		t.Fatalf("files left: %v", got)
	}
	if list, _ := bf.st.ListBackups(context.Background(), store.BackupFilter{}); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
	bf.noHelpers(t)
}

// takeTestBackup stores a backup of the given files.
func (bf *backupFixture) takeTestBackup(t *testing.T, files ...dockertest.File) store.Backup {
	t.Helper()
	vol := bf.row(t).DataVolume
	bf.f.SetVolumeFiles(vol, files...)
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	if log, err := bf.backup(t, BackupOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	list, _ := bf.st.ListBackups(context.Background(), store.BackupFilter{})
	return list[0]
}

func (bf *backupFixture) restore(t *testing.T, o RestoreOptions) (string, error) {
	t.Helper()
	var log bytes.Buffer
	err := bf.svc.Restore(WithJobID(context.Background(), 3), bf.id, o, &log)
	return log.String(), err
}

func names(files []dockertest.File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Name)
	}
	slices.Sort(out)
	return out
}

// A restore copies into a fresh volume and switches only once the copy
// succeeded; a failed copy leaves the server on its old data.
func TestRestoreSwitchesAfterCopy(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	b1 := bf.takeTestBackup(t, snapshotFile(fpA), dockertest.Dir("c"), dockertest.Reg("c/x", "one"))
	old := bf.row(t).DataVolume
	bf.f.SetVolumeFiles(old, snapshotFile(fpA), dockertest.Reg("later", "two"))

	// The copy into the new volume fails: the row keeps the old volume, the
	// new one is removed and the server runs again.
	bf.f.SetCopyHook(func(c dockertest.CopyCall) error {
		if !c.Out {
			return errors.New("copy failed")
		}
		return nil
	})
	log, err := bf.restore(t, RestoreOptions{BackupID: b1.ID, SkipSafetyBackup: true})
	if err == nil || bf.row(t).DataVolume != old {
		t.Fatalf("%v %+v\n%s", err, bf.row(t), log)
	}
	if vols := bf.f.Volumes(); len(vols) != 1 || vols[0] != old {
		t.Fatalf("volumes %v", vols)
	}
	if ct, _ := bf.f.Container(bf.row(t).ContainerID); ct.State != "running" || !strings.Contains(log, "starting the server again") {
		t.Fatalf("%+v\n%s", ct, log)
	}
	bf.noHelpers(t)

	// Success: safety backup, new volume with the backup's files owned by
	// the container user, old volume gone, server running.
	bf.f.SetCopyHook(nil)
	log, err = bf.restore(t, RestoreOptions{BackupID: b1.ID})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	sv := bf.row(t)
	if sv.DataVolume == old || sv.DataFingerprint != fpA || bf.f.HasVolume(old) {
		t.Fatalf("%+v", sv)
	}
	files := bf.f.VolumeFiles(sv.DataVolume)
	if got := names(files); !slices.Equal(got, []string{"c", "c/x", "world.snapshot"}) {
		t.Fatalf("%v", got)
	}
	for _, f := range files {
		if f.UID != docker.DefaultUID || f.GID != docker.DefaultGID {
			t.Fatalf("%+v", f)
		}
	}
	if ct, _ := bf.f.Container(sv.ContainerID); ct.State != "running" || ct.MountTargets["/data"] != sv.DataVolume {
		t.Fatalf("%+v", ct)
	}
	list, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupPreRestore})
	if len(list) != 1 || list[0].FileCount != 2 {
		t.Fatalf("%+v", list)
	}
	bf.noHelpers(t)
}

// The schema rule: a known mismatch needs force, and a forced restore of
// data the build cannot load does not start the server.
func TestRestoreSchemaRule(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bk := bf.takeTestBackup(t, snapshotFile(fpB))
	if bk.SchemaFingerprint != fpB {
		t.Fatalf("%+v", bk)
	}
	var mm *SchemaMismatchError
	if err := bf.svc.CheckRestore(ctx, bf.id, RestoreOptions{BackupID: bk.ID}); !errors.As(err, &mm) || mm.BackupFingerprint != fpB || mm.BuildFingerprint != fpA {
		t.Fatalf("%v", err)
	}
	if _, err := bf.restore(t, RestoreOptions{BackupID: bk.ID}); !strings.Contains(detail(err), "force") {
		t.Fatalf("%v", err)
	}
	log, err := bf.restore(t, RestoreOptions{BackupID: bk.ID, Force: true, SkipSafetyBackup: true})
	if err != nil || !strings.Contains(log, "not starting the server") {
		t.Fatalf("%v\n%s", err, log)
	}
	sv := bf.row(t)
	if sv.DataFingerprint != fpB || sv.ContainerID != "" || sv.DesiredState != "running" {
		t.Fatalf("%+v", sv)
	}
	if v, _ := bf.svc.Get(ctx, bf.id); !v.Data.SchemaMismatch {
		t.Fatalf("%+v", v.Data)
	}
	// Other check failures.
	var fe *FieldError
	if err := bf.svc.CheckRestore(ctx, bf.id, RestoreOptions{BackupID: 999}); !errors.As(err, &fe) || fe.Field != "backup_id" {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(filepath.Join(bf.dir, bk.File), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bf.svc.CheckRestore(ctx, bf.id, RestoreOptions{BackupID: bk.ID, Force: true}); !errors.Is(err, ErrBackupDamaged) {
		t.Fatalf("%v", err)
	}
	bf.f.DeleteImage(bf.b.ImageID)
	other := bf.takeTestBackupRow(t)
	if err := bf.svc.CheckRestore(ctx, bf.id, RestoreOptions{BackupID: other.ID}); !errors.Is(err, ErrNoBuild) {
		t.Fatalf("%v", err)
	}
}

// takeTestBackupRow records a valid backup without Docker.
func (bf *backupFixture) takeTestBackupRow(t *testing.T) store.Backup {
	t.Helper()
	p, err := bf.svc.cfg.Backups.Create()
	if err != nil {
		t.Fatal(err)
	}
	res, err := backups.Normalize(bytes.NewReader(tarOf(t, dockertest.Reg("data/x", "1"))), "data", 1<<20, p)
	if err != nil {
		t.Fatal(err)
	}
	file, size, sum, err := p.Commit()
	if err != nil {
		t.Fatal(err)
	}
	b, err := bf.st.CreateBackup(context.Background(), store.Backup{Kind: "manual", File: file, ServerName: "x", SizeBytes: size, ContentBytes: res.ContentBytes,
		FileCount: res.Files, SHA256: sum})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tarOf(t *testing.T, files ...dockertest.File) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o700})
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f.Name, Typeflag: f.Type, Mode: f.Mode, Size: int64(len(f.Data))})
		_, _ = tw.Write(f.Data)
	}
	_ = tw.Close()
	return buf.Bytes()
}

// A failed safety backup starts the server again and keeps its data.
func TestRestoreSafetyBackupFailure(t *testing.T) {
	bf := newBackupFixture(t)
	bf.start(t)
	bk := bf.takeTestBackup(t, snapshotFile(fpA))
	old := bf.row(t).DataVolume
	bf.f.SetCopyHook(func(c dockertest.CopyCall) error {
		if c.Out {
			return errors.New("read failed")
		}
		return nil
	})
	log, err := bf.restore(t, RestoreOptions{BackupID: bk.ID})
	if !strings.Contains(detail(err), "safety backup") || bf.row(t).DataVolume != old {
		t.Fatalf("%v\n%s", err, log)
	}
	if ct, _ := bf.f.Container(bf.row(t).ContainerID); ct.State != "running" {
		t.Fatalf("%+v", ct)
	}
}

// A reset takes a safety backup, removes the volume and starts the server
// with an empty world.
func TestResetData(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	old := bf.row(t).DataVolume
	bf.f.SetVolumeFiles(old, snapshotFile(fpA))
	var log bytes.Buffer
	if err := bf.svc.ResetData(WithJobID(ctx, 4), bf.id, ResetOptions{}, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	sv := bf.row(t)
	if sv.DataVolume == old || sv.DataVolume == "" || bf.f.HasVolume(old) || len(bf.f.VolumeFiles(sv.DataVolume)) != 0 {
		t.Fatalf("%+v %v", sv, bf.f.Volumes())
	}
	if list, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupPreReset}); len(list) != 1 || list[0].SchemaFingerprint != fpA {
		t.Fatalf("%+v", list)
	}
	// Stopped and without data: no safety backup, no volume afterwards.
	if err := bf.svc.Stop(ctx, bf.id, io.Discard); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := bf.svc.ResetData(ctx, bf.id, ResetOptions{}, &log); err != nil || !strings.Contains(log.String(), "no current data") {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if sv := bf.row(t); sv.DataVolume != "" || sv.DataFingerprint != "" {
		t.Fatalf("%+v", sv)
	}
	bf.noHelpers(t)
}

// Deleting a server removes its container and volume and keeps its
// backups; DELETE /volumes refuses a current volume even when unmounted.
func TestDeleteAndVolumeRules(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bk := bf.takeTestBackup(t, snapshotFile(fpA))
	if err := bf.svc.Stop(ctx, bf.id, io.Discard); err != nil {
		t.Fatal(err)
	}
	sv := bf.row(t)
	bf.f.RemoveContainer(sv.ContainerID)
	if err := bf.svc.RemoveVolume(ctx, sv.DataVolume); !errors.Is(err, ErrVolumeCurrent) {
		t.Fatalf("%v", err)
	}
	stray := "golem-deploy-data-1-ffffffffffffffff"
	bf.f.AddVolume(dockertest.Volume{Name: stray, Labels: dockertest.OurLabels(docker.LabelServerID, "1", docker.LabelServer, "arena")})
	vols, _ := bf.svc.Volumes(ctx)
	for _, v := range vols {
		if (v.Name == sv.DataVolume) != v.Current {
			t.Fatalf("%+v", v)
		}
	}
	if err := bf.svc.RemoveVolume(ctx, stray); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := bf.svc.Delete(ctx, bf.id, DeleteOptions{}, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if bf.f.HasVolume(sv.DataVolume) {
		t.Fatal("volume kept")
	}
	got, err := bf.st.GetBackup(ctx, bk.ID)
	if err != nil || got.ServerID != nil || got.ServerName != "arena" {
		t.Fatalf("%+v %v", got, err)
	}
}

// Scheduled backups run when due, respect the retention, and wait after a
// failure.
func TestScheduleAndRetention(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	bf.f.SetVolumeFiles(bf.row(t).DataVolume, snapshotFile(fpA))
	bf.admin.Set(func(s *gameadmintest.Server) { s.SnapshotStatus = 501 })
	var errs []error
	run := func() []int64 {
		started, err := bf.svc.ScheduleBackups(ctx, func(id int64, body func(context.Context, io.Writer) error) error {
			errs = append(errs, body(WithJobID(ctx, 0), io.Discard))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return started
	}
	advance := func(d time.Duration) { bf.mu.Lock(); bf.offset += d; bf.mu.Unlock() }
	if got := run(); len(got) != 0 {
		t.Fatalf("no schedule yet: %v", got)
	}
	one, keep := 1, 2
	if _, err := bf.svc.Update(ctx, bf.id, apitypes.UpdateServerRequest{BackupSchedule: &apitypes.BackupSchedule{IntervalHours: one, Keep: keep}}); err != nil {
		t.Fatal(err)
	}
	if got := run(); len(got) != 1 {
		t.Fatalf("%v %v", got, errs)
	}
	if got := run(); len(got) != 0 {
		t.Fatal("not due yet")
	}
	// A manual backup counts as the newest backup and is never pruned.
	if _, err := bf.backup(t, BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		advance(61 * time.Minute)
		if got := run(); len(got) != 1 {
			t.Fatalf("round %d: %v %v", i, got, errs)
		}
	}
	sched, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupScheduled})
	manual, _ := bf.st.ListBackups(ctx, store.BackupFilter{Kind: store.BackupManual})
	if len(sched) != 2 || len(manual) != 1 || len(bf.archives(t)) != 3 {
		t.Fatalf("%d scheduled %d manual %v", len(sched), len(manual), bf.archives(t))
	}
	// A failure waits ScheduleRetryDelay before the next attempt.
	bf.f.SetCopyHook(func(dockertest.CopyCall) error { return errors.New("broken") })
	advance(61 * time.Minute)
	if got := run(); len(got) != 1 || errs[len(errs)-1] == nil {
		t.Fatalf("%v %v", got, errs)
	}
	advance(10 * time.Minute)
	if got := run(); len(got) != 0 {
		t.Fatal("retried too early")
	}
	advance(6 * time.Minute)
	bf.f.SetCopyHook(nil)
	if got := run(); len(got) != 1 || errs[len(errs)-1] != nil {
		t.Fatalf("%v %v", got, errs)
	}
	// A stopped server is not backed up.
	if err := bf.svc.Stop(ctx, bf.id, io.Discard); err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Hour)
	if got := run(); len(got) != 0 {
		t.Fatal("backed up a stopped server")
	}
}

// Helpers left by a crash are removed by their recorded launch id only;
// they are never reported as foreign containers.
func TestCleanupHelpers(t *testing.T) {
	bf := newBackupFixture(t)
	ctx := context.Background()
	bf.start(t)
	sv := bf.row(t)
	img := bf.b.ImageID
	pid := bf.b.ProjectID
	mk := func(launch string) string {
		id, err := bf.svc.docker.CreateHelper(ctx, docker.HelperSpec{LaunchID: launch, ServerID: sv.ID, ServerName: sv.Name, ProjectID: &pid,
			BuildID: bf.b.ID, Image: img, Volume: sv.DataVolume, Target: "/data", ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	recorded := strings.Repeat("1", 32)
	lost := strings.Repeat("2", 32)
	never := strings.Repeat("3", 32)
	unrecorded := strings.Repeat("4", 32)
	for _, l := range []string{recorded, lost, never} {
		if err := bf.st.RecordVolumeHelper(ctx, l, sv.ID); err != nil {
			t.Fatal(err)
		}
	}
	recordedID := mk(recorded)
	_ = bf.st.SetVolumeHelperContainer(ctx, recorded, recordedID)
	lostID := mk(lost)
	unrecordedID := mk(unrecorded) // carries the labels but no record: foreign
	if err := bf.svc.CleanupHelpers(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{recordedID, lostID} {
		if _, ok := bf.f.Container(id); ok {
			t.Fatalf("helper %s kept", id)
		}
	}
	if _, ok := bf.f.Container(unrecordedID); !ok {
		t.Fatal("an unrecorded container was removed")
	}
	// A record without a container id whose helper is not found may be a
	// create whose reply was lost: it stays until helperForgetAfter passed.
	if rows, _ := bf.st.ListVolumeHelpers(ctx); len(rows) != 1 || rows[0].LaunchID != never {
		t.Fatalf("%+v", rows)
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
	if _, err := bf.svc.List(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bf.logs.String(), "ignoring a container") {
		t.Fatal("the unrecorded container was not reported")
	}
}
