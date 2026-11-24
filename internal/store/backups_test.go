package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

// Backups keep their row when what they reference is deleted; references
// that are already gone are stored as NULL.
func TestBackupsSurviveDeletions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, err := s.CreateProject(ctx, Project{Name: "game", RepoURL: "https://example.com/g.git"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.CreateBuild(ctx, NewBuild{ProjectID: p.ID})
	_ = s.MarkBuildRunning(ctx, b.ID, "main", "c")
	fp := "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	if err := s.FinishBuild(ctx, b.ID, BuildResult{Status: BuildStatusSuccess, ImageID: "sha256:" + fp, SchemaFingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBuild(ctx, b.ID); got.SchemaFingerprint != fp {
		t.Fatalf("build fingerprint %q", got.SchemaFingerprint)
	}
	port := 7001
	sv, err := s.CreateServer(ctx, Server{Name: "arena", BuildID: &b.ID, Port: &port, SnapshotIntervalSeconds: DefaultSnapshotIntervalSeconds}, nil)
	if err != nil || sv.BackupKeep != DefaultBackupKeep || sv.SnapshotIntervalSeconds != 300 || sv.DataVolume != "" {
		t.Fatalf("%+v %v", sv, err)
	}
	gone := int64(424242)
	job, _ := s.CreateJob(ctx, NewJob{Kind: "server.backup", SubjectType: "server", SubjectID: "1"})
	bk, err := s.CreateBackup(ctx, Backup{Kind: BackupManual, File: "f1.tar.gz", ServerID: &sv.ID, ServerName: sv.Name, ProjectID: &p.ID, BuildID: &b.ID,
		SizeBytes: 10, ContentBytes: 20, FileCount: 1, SHA256: "x", JobID: &job.ID, CreatedBy: &gone})
	if err != nil {
		t.Fatal(err)
	}
	if bk.CreatedBy != nil || bk.CreatedByUsername != nil || *bk.JobID != job.ID || *bk.ServerID != sv.ID {
		t.Fatalf("%+v", bk)
	}
	if _, err := s.CreateBackup(ctx, Backup{Kind: "weird", File: "f2.tar.gz", ServerName: "x", SHA256: "y"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := s.CreateBackup(ctx, Backup{Kind: BackupManual, File: "f1.tar.gz", ServerName: "x", SHA256: "y"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate file: %v", err)
	}
	last, _ := s.LastBackups(ctx)
	if !last[sv.ID].Equal(bk.CreatedAt) {
		t.Fatalf("%v", last)
	}
	// Data columns: volume, switch (compare and swap), fingerprint.
	if err := s.SetServerDataVolume(ctx, sv.ID, "v1", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetServerDataFingerprint(ctx, sv.ID, "other", fp); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(ctx, sv.ID); got.DataFingerprint != "" {
		t.Fatal("fingerprint written for another volume")
	}
	if err := s.SwitchServerData(ctx, sv.ID, "stale", "v2", fp); !errors.Is(err, ErrStale) {
		t.Fatalf("%v", err)
	}
	if err := s.SwitchServerData(ctx, sv.ID, "v1", "v2", fp); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(ctx, sv.ID); got.DataVolume != "v2" || got.DataFingerprint != fp {
		t.Fatalf("%+v", got)
	}
	// Deleting the server, the build, the project and the job keeps the row.
	if err := s.DeleteServer(ctx, sv.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBuild(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackup(ctx, bk.ID)
	if err != nil || got.ServerID != nil || got.BuildID != nil || got.ProjectID != nil || got.JobID != nil || got.ServerName != "arena" {
		t.Fatalf("%+v %v", got, err)
	}
	files, _ := s.BackupFiles(ctx)
	if !files["f1.tar.gz"] || len(files) != 1 {
		t.Fatalf("%v", files)
	}
	del, err := s.DeleteBackup(ctx, bk.ID)
	if err != nil || del.File != "f1.tar.gz" {
		t.Fatalf("%+v %v", del, err)
	}
	if _, err := s.DeleteBackup(ctx, bk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestVolumeHelperRecords(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.RecordVolumeHelper(ctx, "l1", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVolumeHelperContainer(ctx, "l1", "c1"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListVolumeHelpers(ctx)
	if len(list) != 1 || list[0].ContainerID != "c1" || list[0].ServerID != 3 {
		t.Fatalf("%+v", list)
	}
	if err := s.DeleteVolumeHelper(ctx, "l1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListVolumeHelpers(ctx); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

// Migration 0011 keeps the stored backups and their id sequence, adds
// omitted_entries and accepts the pre_delete kind.
func TestBackupCompletenessMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.db")+"?_foreign_keys=1&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, now: time.Now}
	t.Cleanup(func() { _ = db.Close() })
	old := fstest.MapFS{}
	all, _ := fs.Glob(migrationFS, "migrations/*.sql")
	for _, n := range all {
		if path.Base(n) < "0011" {
			body, _ := fs.ReadFile(migrationFS, n)
			old[n] = &fstest.MapFile{Data: body}
		}
	}
	if err := s.migrateFS(ctx, old); err != nil {
		t.Fatal(err)
	}
	for i, f := range []string{"a.tar.gz", "b.tar.gz", "c.tar.gz"} {
		if _, err := db.Exec(`INSERT INTO backups (kind, file, server_name, size_bytes, content_bytes, file_count, sha256, created_at)
			VALUES ('manual', ?, 'x', 1, 2, ?, 's', '2026-01-01T00:00:00.000Z')`, f, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`DELETE FROM backups WHERE file = 'c.tar.gz'`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListBackups(ctx, BackupFilter{})
	if err != nil || len(list) != 2 || list[0].FileCount != 2 || list[0].OmittedEntries != 0 {
		t.Fatalf("%+v %v", list, err)
	}
	b, err := s.CreateBackup(ctx, Backup{Kind: BackupPreDelete, File: "d.tar.gz", ServerName: "x", SHA256: "y", FileCount: 1, OmittedEntries: 4})
	if err != nil || b.ID != 4 || b.OmittedEntries != 4 || b.Kind != BackupPreDelete {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := db.Exec(`UPDATE backups SET omitted_entries = -1`); err == nil {
		t.Fatal("negative omitted_entries accepted")
	}
}

// A build's fingerprint is only filled in while empty; the scheduled
// backup failure time round-trips.
func TestFillBuildFingerprintAndBackupFailure(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, _ := s.CreateProject(ctx, Project{Name: "game", RepoURL: "https://example.com/g.git"}, nil)
	b, _ := s.CreateBuild(ctx, NewBuild{ProjectID: p.ID})
	if ok, err := s.FillBuildFingerprint(ctx, b.ID, "first"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, err := s.FillBuildFingerprint(ctx, b.ID, "second"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if got, _ := s.GetBuild(ctx, b.ID); got.SchemaFingerprint != "first" {
		t.Fatalf("%q", got.SchemaFingerprint)
	}
	port := 7001
	sv, _ := s.CreateServer(ctx, Server{Name: "arena", Port: &port}, nil)
	if sv.BackupFailedAt != nil {
		t.Fatalf("%+v", sv)
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := s.SetServerBackupFailed(ctx, sv.ID, &at); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(ctx, sv.ID); got.BackupFailedAt == nil || !got.BackupFailedAt.Equal(at) {
		t.Fatalf("%+v", got.BackupFailedAt)
	}
	if err := s.SetServerBackupFailed(ctx, sv.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(ctx, sv.ID); got.BackupFailedAt != nil {
		t.Fatalf("%+v", got.BackupFailedAt)
	}
	if _, ok, err := s.LastServerBackup(ctx, sv.ID); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
}
