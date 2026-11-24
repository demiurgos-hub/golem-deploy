package httpapi

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
)

// gameFingerprint is what the fake game reports in /info.
const gameFingerprint = "sf-0123456789abcdef"

func snapshotOf(fp string) dockertest.File {
	b := []byte{'G', 'L', 'M', 1}
	b = binary.AppendUvarint(b, 1)
	b = binary.AppendUvarint(b, 1)
	b = binary.AppendUvarint(b, uint64(len(fp)))
	return dockertest.Reg("world.snapshot", string(append(b, fp...)))
}

func newBackupHarness(t *testing.T) (*dockerHarness, *backups.Store) {
	t.Helper()
	bs, err := backups.Open(filepath.Join(t.TempDir(), "backups"), backups.Options{CheckFree: func(string, int64) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	d := newDockerHarness(t, func(c *servers.Config) { c.Backups = bs; c.BackupMaxBytes = 1 << 20 })
	return d, bs
}

func (d *dockerHarness) accepted(rr *httptest.ResponseRecorder) apitypes.Job {
	d.t.Helper()
	if rr.Code != http.StatusAccepted {
		d.t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.Job](d.t, rr)
}

func TestBackupRoutes(t *testing.T) {
	d, bs := newBackupHarness(t)
	img := d.managedImage(1)

	// Settings on create, validated.
	for field, req := range map[string]apitypes.CreateServerRequest{
		"snapshot_interval_seconds":      {Name: "bad", BuildID: img.ID, SnapshotIntervalSeconds: ptr(5)},
		"backup_schedule.interval_hours": {Name: "bad", BuildID: img.ID, BackupSchedule: &apitypes.BackupSchedule{IntervalHours: 721, Keep: 1}},
		"backup_schedule.keep":           {Name: "bad", BuildID: img.ID, BackupSchedule: &apitypes.BackupSchedule{IntervalHours: 1, Keep: 101}},
	} {
		if e := errBody(t, d.call("POST", "/api/v1/servers", d.devTok, req), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != field {
			t.Errorf("%s: field %q", field, e.Field)
		}
	}
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID, SnapshotIntervalSeconds: ptr(60),
		BackupSchedule: &apitypes.BackupSchedule{IntervalHours: 6, Keep: 7}})
	if s.SnapshotIntervalSeconds != 60 || s.BackupSchedule != (apitypes.BackupSchedule{IntervalHours: 6, Keep: 7}) || s.Data.Volume != "" || s.Data.LastBackupAt != nil {
		t.Fatalf("%+v", s)
	}
	base := fmt.Sprintf("/api/v1/servers/%d", s.ID)

	// Nothing to back up before the first start.
	j := d.waitJob(d.accepted(d.call("POST", base+"/backups", d.devTok, nil)))
	if j.Status != "error" || !strings.Contains(j.Detail, "nothing to back up") {
		t.Fatalf("%+v", j)
	}
	d.start(s.ID)
	s = d.getServer(d.devTok, s.ID)
	if s.Data.Volume == "" || s.Data.SchemaFingerprint != gameFingerprint || s.Data.SchemaMismatch {
		t.Fatalf("%+v", s.Data)
	}
	d.fake.SetVolumeFiles(s.Data.Volume, snapshotOf(gameFingerprint), dockertest.Reg("extra.bin", "abc"))

	// A manual backup with a note (validated).
	e := errBody(t, d.call("POST", base+"/backups", d.devTok, apitypes.CreateBackupRequest{Note: "two\nlines"}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e.Field != "note" {
		t.Fatalf("%+v", e)
	}
	job := d.accepted(d.call("POST", base+"/backups", d.devTok, apitypes.CreateBackupRequest{Note: " before the update "}))
	if job.Kind != apitypes.JobKindServerBackup || job.Restricted {
		t.Fatalf("%+v", job)
	}
	if j := d.waitJob(job); j.Status != "success" {
		t.Fatalf("%+v\n%s", j, d.jobLog(j.ID))
	}
	rr := d.call("GET", "/api/v1/backups", d.devTok, nil)
	list := decode[apitypes.BackupList](t, rr)
	if len(list.Items) != 1 || list.MaxContentBytes != 1<<20 || list.TotalBytes != list.Items[0].SizeBytes {
		t.Fatalf("%+v", list)
	}
	b := list.Items[0]
	if b.Kind != apitypes.BackupKindManual || b.Note != "before the update" || *b.ServerID != s.ID || b.ServerName != "arena" || b.SchemaFingerprint != gameFingerprint ||
		!b.Live || b.FileCount != 2 || *b.JobID != job.ID || *b.CreatedBy != d.dev.ID || *b.CreatedByUsername != "dev1" || b.ProjectName != "game" {
		t.Fatalf("%+v", b)
	}
	if got := decode[apitypes.BackupList](t, d.call("GET", "/api/v1/backups?server_id=999", d.devTok, nil)); len(got.Items) != 0 {
		t.Fatalf("%+v", got)
	}
	errBody(t, d.call("GET", "/api/v1/backups?server_id=x", d.devTok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	if got := decode[apitypes.Backup](t, d.call("GET", fmt.Sprintf("/api/v1/backups/%d", b.ID), d.devTok, nil)); got.SHA256 != b.SHA256 {
		t.Fatalf("%+v", got)
	}
	errBody(t, d.call("GET", "/api/v1/backups/999", d.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	if v := d.getServer(d.devTok, s.ID); v.Data.LastBackupAt == nil {
		t.Fatalf("%+v", v.Data)
	}

	// Download: exact bytes, a safe attachment name, ranges.
	rr = d.do("GET", fmt.Sprintf("/api/v1/backups/%d/download", b.ID), d.devTok, nil)
	sum := sha256.Sum256(rr.Body.Bytes())
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/gzip" || hex.EncodeToString(sum[:]) != b.SHA256 ||
		!strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment; filename=arena-backup-") {
		t.Fatalf("%d %v", rr.Code, rr.Header())
	}
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/backups/%d/download", b.ID), nil)
	req.Header.Set("Authorization", "Bearer "+d.devTok)
	req.Header.Set("Range", "bytes=0-1")
	rec := httptest.NewRecorder()
	d.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatalf("%d %x", rec.Code, rec.Body.Bytes())
	}

	// Restore checks: request, missing backup, schema mismatch, force.
	errBody(t, d.call("POST", base+"/restore", d.adminSs, apitypes.RestoreServerRequest{}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed)
	if e := errBody(t, d.call("POST", base+"/restore", d.adminSs, apitypes.RestoreServerRequest{BackupID: 999}), http.StatusBadRequest,
		apitypes.ErrorCodeValidationFailed); e.Field != "backup_id" {
		t.Fatalf("%+v", e)
	}
	errBody(t, d.call("POST", "/api/v1/servers/999/restore", d.adminSs, apitypes.RestoreServerRequest{BackupID: b.ID}), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	if err := d.st.SetBuildFingerprint(context.Background(), img.ID, "other-fingerprint"); err != nil {
		t.Fatal(err)
	}
	errBody(t, d.call("POST", base+"/restore", d.adminSs, apitypes.RestoreServerRequest{BackupID: b.ID}), http.StatusConflict, apitypes.ErrorCodeSchemaMismatch)
	if err := d.st.SetBuildFingerprint(context.Background(), img.ID, gameFingerprint); err != nil {
		t.Fatal(err)
	}
	job = d.accepted(d.call("POST", base+"/restore", d.adminSs, apitypes.RestoreServerRequest{BackupID: b.ID}))
	if job.Kind != apitypes.JobKindServerRestore || !job.Restricted {
		t.Fatalf("%+v", job)
	}
	if j := d.waitJob(job); j.Status != "success" {
		t.Fatalf("%+v\n%s", j, d.jobLog(j.ID))
	}
	after := d.getServer(d.devTok, s.ID)
	if after.Data.Volume == s.Data.Volume || after.State != apitypes.ServerStateRunning {
		t.Fatalf("%+v", after)
	}
	list = decode[apitypes.BackupList](t, d.call("GET", fmt.Sprintf("/api/v1/backups?server_id=%d", s.ID), d.devTok, nil))
	if len(list.Items) != 2 || list.Items[0].Kind != apitypes.BackupKindPreRestore {
		t.Fatalf("%+v", list.Items)
	}

	// Volumes: the current one cannot be deleted even when unmounted.
	vols := decode[apitypes.VolumeList](t, d.call("GET", "/api/v1/volumes", d.devTok, nil))
	if len(vols.Items) != 1 || !vols.Items[0].Current || vols.Items[0].Name != after.Data.Volume {
		t.Fatalf("%+v", vols)
	}
	errBody(t, d.call("DELETE", "/api/v1/volumes/"+after.Data.Volume, d.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)

	// A backup a running job reads cannot be deleted.
	release := bs.Acquire(b.ID)
	errBody(t, d.call("DELETE", fmt.Sprintf("/api/v1/backups/%d", b.ID), d.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	release()
	if rr := d.call("DELETE", fmt.Sprintf("/api/v1/backups/%d", b.ID), d.adminSs, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	errBody(t, d.call("DELETE", fmt.Sprintf("/api/v1/backups/%d", b.ID), d.adminSs, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)

	// Reset: restricted, body optional.
	job = d.accepted(d.call("POST", base+"/data/reset", d.adminSs, nil))
	if job.Kind != apitypes.JobKindServerDataReset || !job.Restricted {
		t.Fatalf("%+v", job)
	}
	if j := d.waitJob(job); j.Status != "success" {
		t.Fatalf("%+v\n%s", j, d.jobLog(j.ID))
	}
	reset := d.getServer(d.devTok, s.ID)
	if reset.Data.Volume == after.Data.Volume || reset.Data.Volume == "" {
		t.Fatalf("%+v", reset.Data)
	}

	// PATCH, export and import carry the settings.
	if e := errBody(t, d.call("PATCH", base, d.devTok, apitypes.UpdateServerRequest{BackupSchedule: &apitypes.BackupSchedule{IntervalHours: 0}}),
		http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "backup_schedule.keep" {
		t.Fatalf("%+v", e)
	}
	rr = d.call("PATCH", base, d.devTok, apitypes.UpdateServerRequest{SnapshotIntervalSeconds: ptr(0), BackupSchedule: &apitypes.BackupSchedule{IntervalHours: 0, Keep: 7}})
	if v := decode[apitypes.Server](t, rr); v.SnapshotIntervalSeconds != 0 || v.BackupSchedule != (apitypes.BackupSchedule{Keep: 7}) || !v.RestartRequired {
		t.Fatalf("%+v", v)
	}
	doc := decode[apitypes.ServerExport](t, d.call("GET", "/api/v1/servers/export", d.adminSs, nil))
	if sp := doc.Servers[0]; *sp.SnapshotIntervalSeconds != 0 || sp.BackupSchedule.Keep != 7 {
		t.Fatalf("%+v", sp)
	}
	doc.Servers[0].Name = "copy"
	doc.Servers[0].Port = nil
	doc.Servers[0].BackupSchedule = &apitypes.BackupSchedule{IntervalHours: 0, Keep: 1000}
	if e := errBody(t, d.call("POST", "/api/v1/servers/import", d.adminSs, doc), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "servers[0].backup_schedule.keep" {
		t.Fatalf("%+v", e)
	}
	doc.Servers[0].BackupSchedule = &apitypes.BackupSchedule{IntervalHours: 24, Keep: 3}
	rr = d.call("POST", "/api/v1/servers/import", d.adminSs, doc)
	if res := decode[apitypes.ServerImportResult](t, rr); res.Created[0].BackupSchedule.IntervalHours != 24 || res.Created[0].SnapshotIntervalSeconds != 0 {
		t.Fatalf("%+v", res)
	}
	d.assertNoAdminToken()
}

// Backup, restore and reset are server jobs: one at a time per server.
func TestBackupJobsAreSerialised(t *testing.T) {
	d, _ := newBackupHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	release := make(chan struct{})
	_, err := d.runner.Start(context.Background(), string(apitypes.JobKindServerStart), jobs.Subject{Type: "server", ID: jobs.SubjectID(s.ID)}, nil,
		func(ctx context.Context, _ io.Writer) error { <-release; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer close(release)
	base := fmt.Sprintf("/api/v1/servers/%d", s.ID)
	errBody(t, d.call("POST", base+"/backups", d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	errBody(t, d.call("POST", base+"/restore", d.adminSs, apitypes.RestoreServerRequest{BackupID: 1}), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
	errBody(t, d.call("POST", base+"/data/reset", d.adminSs, nil), http.StatusConflict, apitypes.ErrorCodeJobInProgress)
}

// DELETE /servers/{id} stores a last backup first; a failed one is 409
// with the reason, and skipping it is for admin sessions only.
func TestDeleteServerLastBackup(t *testing.T) {
	d, _ := newBackupHarness(t)
	img := d.managedImage(1)
	s := d.createServer(d.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	base := fmt.Sprintf("/api/v1/servers/%d", s.ID)
	d.start(s.ID)
	s = d.getServer(d.devTok, s.ID)
	d.fake.SetVolumeFiles(s.Data.Volume, snapshotOf(gameFingerprint), dockertest.Special("link", tar.TypeSymlink, "/"))

	errBody(t, d.call("DELETE", base+"?skip_safety_backup=1", d.devTok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	adminTok := d.apiToken(d.admin, nil) // API tokens are capped at developer
	errBody(t, d.call("DELETE", base+"?skip_safety_backup=true", adminTok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	errBody(t, d.call("DELETE", base+"?skip_safety_backup=yes", d.adminSs, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	e := errBody(t, d.call("DELETE", base, d.devTok, nil), http.StatusConflict, apitypes.ErrorCodeConflict)
	if !strings.Contains(e.Message, "is incomplete") || !strings.Contains(e.Message, "nothing was deleted") || !strings.Contains(e.Message, "skip_safety_backup=1") {
		t.Fatalf("%+v", e)
	}
	list := decode[apitypes.BackupList](t, d.call("GET", "/api/v1/backups", d.devTok, nil))
	if len(list.Items) != 1 || list.Items[0].Kind != apitypes.BackupKindPreDelete || list.Items[0].OmittedEntries != 1 {
		t.Fatalf("%+v", list)
	}
	if got := d.getServer(d.devTok, s.ID); got.State != apitypes.ServerStateRunning {
		t.Fatalf("%+v", got)
	}
	if rr := d.call("DELETE", base+"?skip_safety_backup=1", d.adminSs, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if list := decode[apitypes.BackupList](t, d.call("GET", "/api/v1/backups", d.devTok, nil)); len(list.Items) != 1 {
		t.Fatalf("%+v", list)
	}
	errBody(t, d.call("GET", base, d.devTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}
