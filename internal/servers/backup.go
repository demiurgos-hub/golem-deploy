package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Errors of the backup operations besides ErrNotFound.
var (
	// ErrBackupNotFound: no backup with that id.
	ErrBackupNotFound = errors.New("servers: backup not found")
	// ErrBackupInUse: a running job reads the backup.
	ErrBackupInUse = errors.New("servers: the backup is in use")
	// ErrBackupDamaged: the backup's archive is missing or does not match
	// its recorded size and checksum.
	ErrBackupDamaged = errors.New("servers: the backup's archive is missing or damaged")
	// ErrNoBuild: the server has no successful build with an existing
	// image.
	ErrNoBuild = errors.New("servers: the server has no successful build with an image")
	// ErrBackupsUnavailable: the daemon has no backup store.
	ErrBackupsUnavailable = errors.New("servers: backups are not configured")
	// ErrVolumeCurrent: the volume is a server's current data volume (or
	// one a running job is about to use).
	ErrVolumeCurrent = errors.New("servers: the volume is a server's current data volume")
)

// SchemaMismatchError: the backup's fingerprint and the build's are both
// known and differ. It matches ErrConflict for callers that do not care.
type SchemaMismatchError struct {
	BackupFingerprint string
	BuildFingerprint  string
	BuildID           int64
}

func (e *SchemaMismatchError) Error() string {
	return fmt.Sprintf("the backup has schema %s but build %d has schema %s", e.BackupFingerprint, e.BuildID, e.BuildFingerprint)
}

// MaxBackupNote bounds a backup's note in characters.
const MaxBackupNote = 200

// errNothingToBackUp: the server has no data volume or no files in it.
type nothingError struct{ msg string }

func (e *nothingError) Error() string { return e.msg }

// BackupOptions are the inputs of a backup job.
type BackupOptions struct {
	Kind      string
	Note      string
	CreatedBy *int64
}

// RestoreOptions are the inputs of a restore job.
type RestoreOptions struct {
	BackupID         int64
	Force            bool
	SkipSafetyBackup bool
	CreatedBy        *int64
}

// ResetOptions are the inputs of a data reset job.
type ResetOptions struct {
	SkipSafetyBackup bool
	CreatedBy        *int64
}

// ValidateNote checks a backup note: a single line of at most
// MaxBackupNote characters without control characters.
func ValidateNote(note string) error {
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > MaxBackupNote {
		return fieldErr("note", "note must be at most %d characters", MaxBackupNote)
	}
	for _, r := range note {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ' || (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩') {
			return fieldErr("note", "note must be a single line without control characters")
		}
	}
	return nil
}

// BackupView renders a stored backup.
func BackupView(b store.Backup) apitypes.Backup {
	return apitypes.Backup{
		ID: b.ID, Kind: apitypes.BackupKind(b.Kind), ServerID: b.ServerID, ServerName: b.ServerName,
		ProjectID: b.ProjectID, ProjectName: b.ProjectName, BuildID: b.BuildID, Commit: b.Commit, EngineVersion: b.EngineVersion,
		SchemaFingerprint: b.SchemaFingerprint, Live: b.Live, Note: b.Note, SizeBytes: b.SizeBytes, ContentBytes: b.ContentBytes,
		FileCount: b.FileCount, SHA256: b.SHA256, JobID: b.JobID, CreatedBy: b.CreatedBy, CreatedByUsername: b.CreatedByUsername,
		CreatedAt: b.CreatedAt.UTC(),
	}
}

// ListBackups returns the stored backups, newest first, optionally only
// those taken from one server.
func (s *Service) ListBackups(ctx context.Context, serverID *int64) (apitypes.BackupList, error) {
	rows, err := s.store.ListBackups(ctx, store.BackupFilter{ServerID: serverID})
	if err != nil {
		return apitypes.BackupList{}, err
	}
	total, err := s.store.BackupTotalBytes(ctx)
	if err != nil {
		return apitypes.BackupList{}, err
	}
	out := apitypes.BackupList{Items: make([]apitypes.Backup, 0, len(rows)), TotalBytes: total, MaxContentBytes: s.cfg.BackupMaxBytes}
	for _, b := range rows {
		out.Items = append(out.Items, BackupView(b))
	}
	return out, nil
}

// GetBackup returns one backup (ErrBackupNotFound).
func (s *Service) GetBackup(ctx context.Context, id int64) (store.Backup, error) {
	b, err := s.store.GetBackup(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Backup{}, ErrBackupNotFound
	}
	return b, err
}

// OpenBackup opens a backup's archive for download (ErrBackupNotFound,
// ErrBackupDamaged when the file is missing).
func (s *Service) OpenBackup(ctx context.Context, id int64) (*os.File, fs.FileInfo, store.Backup, error) {
	if s.cfg.Backups == nil {
		return nil, nil, store.Backup{}, ErrBackupsUnavailable
	}
	b, err := s.GetBackup(ctx, id)
	if err != nil {
		return nil, nil, store.Backup{}, err
	}
	f, info, err := s.cfg.Backups.OpenFile(b.File)
	if errors.Is(err, backups.ErrMissing) {
		return nil, nil, b, ErrBackupDamaged
	}
	return f, info, b, err
}

// DeleteBackup deletes a backup and its archive. A backup a running job
// reads is ErrBackupInUse.
func (s *Service) DeleteBackup(ctx context.Context, id int64) error {
	if s.cfg.Backups == nil {
		return ErrBackupsUnavailable
	}
	err := s.cfg.Backups.Delete(id, func() (string, error) {
		b, err := s.store.DeleteBackup(ctx, id)
		return b.File, err
	})
	switch {
	case errors.Is(err, backups.ErrInUse):
		return ErrBackupInUse
	case errors.Is(err, store.ErrNotFound):
		return ErrBackupNotFound
	case errors.Is(err, backups.ErrFileKept):
		s.logger.Warn("backup deleted; its file is removed at the next start", "backup_id", id, "err", err)
	case err != nil:
		return err
	}
	s.logger.Info("backup deleted", "backup_id", id)
	return nil
}

// Backup is the body of a server.backup job (see takeBackup).
func (s *Service) Backup(ctx context.Context, id int64, o BackupOptions, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	if o.Kind == "" {
		o.Kind = store.BackupManual
	}
	_, err = s.takeBackup(ctx, sv, o, true, log)
	var nothing *nothingError
	if errors.As(err, &nothing) {
		return jobs.Fail(nothing.msg, nil)
	}
	return err
}

// takeBackup copies the server's data directory into a new backup. With
// live set and the server's game running and answering, it first asks the
// game for a fresh snapshot. A server without a data volume, or whose data
// directory holds no files, is a *nothingError.
func (s *Service) takeBackup(ctx context.Context, sv store.Server, o BackupOptions, live bool, log io.Writer) (*store.Backup, error) {
	if s.cfg.Backups == nil {
		return nil, jobs.Fail("backups are not configured on this daemon", ErrBackupsUnavailable)
	}
	if sv.DataVolume == "" {
		return nil, &nothingError{"the server has no saved data yet (it gets a data volume when it starts); there is nothing to back up"}
	}
	gone, err := s.volumeState(ctx, sv)
	if err != nil {
		return nil, err
	}
	if gone {
		return nil, &nothingError{"the server's data volume no longer exists; there is nothing to back up"}
	}
	b, img, err := s.verifiedImage(ctx, sv)
	if err != nil {
		return nil, err
	}
	fresh := false
	if live {
		if fresh, err = s.snapshot(ctx, sv, log); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(log, "copying the data directory of server %q (volume %s)\n", sv.Name, sv.DataVolume)
	h, err := s.createHelper(ctx, sv, b, img, sv.DataVolume, true)
	if err != nil {
		return nil, err
	}
	defer s.removeHelper(ctx, h)
	pending, err := s.cfg.Backups.Create()
	if errors.Is(err, backups.ErrNoSpace) {
		return nil, jobs.Fail("not enough free disk space for a backup", err)
	}
	if err != nil {
		return nil, jobs.Fail("could not create the backup file", err)
	}
	defer pending.Discard()
	var res backups.Result
	err = s.docker.CopyFromHelper(ctx, h.launch, h.id, build.DataDir, func(r io.Reader) error {
		var nerr error
		res, nerr = backups.Normalize(r, "data", s.cfg.BackupMaxBytes, pending)
		return nerr
	})
	if err != nil {
		return nil, s.copyFailure(ctx, sv, err)
	}
	s.removeHelper(ctx, h)
	if res.SkippedCount > 0 {
		fmt.Fprintf(log, "left out %d entries that are not regular files or directories, or whose names are not allowed:\n", res.SkippedCount)
		for _, sk := range res.Skipped {
			fmt.Fprintf(log, "  %s (%s)\n", sk.Name, sk.Reason)
		}
		if res.SkippedCount > len(res.Skipped) {
			fmt.Fprintf(log, "  and %d more\n", res.SkippedCount-len(res.Skipped))
		}
	}
	if res.Files == 0 {
		return nil, &nothingError{"the server's data directory holds no files; there is nothing to back up"}
	}
	file, size, sum, err := pending.Commit()
	if err != nil {
		return nil, jobs.Fail("could not store the backup file", err)
	}
	row := store.Backup{
		Kind: o.Kind, File: file, ServerID: &sv.ID, ServerName: sv.Name, BuildID: &b.ID, Commit: b.Commit, EngineVersion: b.EngineVersion,
		SchemaFingerprint: res.Fingerprint, Live: fresh, Note: o.Note, SizeBytes: size, ContentBytes: res.ContentBytes, FileCount: res.Files,
		SHA256: sum, CreatedBy: o.CreatedBy,
	}
	pid := b.ProjectID
	row.ProjectID = &pid
	if p, err := s.store.GetProject(ctx, b.ProjectID); err == nil {
		row.ProjectName = p.Name
	}
	if jid := jobIDFrom(ctx); jid != 0 {
		row.JobID = &jid
	}
	created, err := s.store.CreateBackup(context.WithoutCancel(ctx), row)
	if err != nil {
		if rerr := s.cfg.Backups.Remove(file); rerr != nil {
			s.logger.Warn("could not remove an unrecorded backup file; it is removed at the next start", "err", rerr)
		}
		return nil, jobs.Fail("could not record the backup", err)
	}
	fmt.Fprintf(log, "backup %d (%s) stored: %d files, %d bytes of content, %d bytes compressed, schema %s\n",
		created.ID, created.Kind, created.FileCount, created.ContentBytes, created.SizeBytes, orDash(created.SchemaFingerprint))
	s.logger.Info("backup stored", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "backup_id", created.ID, "kind", created.Kind, "bytes", created.SizeBytes)
	return &created, nil
}

// copyFailure maps a failed copy of a data directory to a job failure.
func (s *Service) copyFailure(ctx context.Context, sv store.Server, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, backups.ErrNoSpace):
		return jobs.Fail("not enough free disk space for the backup", err)
	case errors.Is(err, backups.ErrTooLarge):
		return jobs.Fail(fmt.Sprintf("the data directory is larger than the backup size limit of %d MiB (--backup-max-mb)", s.cfg.BackupMaxBytes>>20), err)
	case errors.Is(err, backups.ErrTooManyEntries):
		return jobs.Fail(fmt.Sprintf("the data directory has more than %d entries", backups.MaxEntries), err)
	case errors.Is(err, backups.ErrDamaged):
		return jobs.Fail("the backup's archive is damaged", err)
	}
	var cmdErr *docker.CmdError
	if errors.As(err, &cmdErr) {
		return s.failDocker(ctx, sv, "copy the data directory", err)
	}
	s.logger.Warn("copying a data directory failed", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
	return jobs.Fail("copying the data directory failed; see the daemon log", nil)
}

// snapshot asks the server's running game for a fresh snapshot. It
// reports whether one was written; a stopped server, a game without the
// snapshot hook or an admin endpoint that does not answer give false. Any
// other answer fails the backup.
func (s *Service) snapshot(ctx context.Context, sv store.Server, log io.Writer) (bool, error) {
	ct, err := s.container(ctx, sv)
	if err != nil {
		return false, s.failDocker(ctx, sv, "inspect the container", err)
	}
	if ct == nil || !adminRunning(*ct) {
		fmt.Fprintln(log, "the server is not running; copying the saved files as they are")
		return false, nil
	}
	port := ct.LoopbackPort(AdminContainerPort)
	token, terr := s.openAdminToken(sv)
	if port == 0 || terr != nil {
		fmt.Fprintln(log, "the game's admin endpoint is not reachable; copying the saved files as they are")
		return false, nil
	}
	client, err := gameadmin.New(port, token)
	if err != nil {
		fmt.Fprintln(log, "the game's admin endpoint is not reachable; copying the saved files as they are")
		return false, nil
	}
	fmt.Fprintln(log, "asking the game for a fresh snapshot")
	const maxBusy = 30
	for attempt := 0; ; attempt++ {
		took, err := client.Snapshot(ctx)
		switch {
		case err == nil:
			fmt.Fprintf(log, "the game wrote a fresh snapshot in %s\n", took)
			return true, nil
		case ctx.Err() != nil:
			return false, ctx.Err()
		case errors.Is(err, gameadmin.ErrSnapshotInProgress):
			if attempt >= maxBusy {
				return false, jobs.Fail("the game kept reporting a snapshot in progress; try again later", err)
			}
			if attempt == 0 {
				fmt.Fprintln(log, "a snapshot is already in progress; waiting for it")
			}
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(s.cfg.SnapshotRetry):
			}
		case errors.Is(err, gameadmin.ErrSnapshotUnsupported):
			fmt.Fprintln(log, "the game has no snapshot hook; copying the saved files as they are")
			return false, nil
		case errors.Is(err, gameadmin.ErrUnreachable):
			fmt.Fprintln(log, "the game's admin endpoint did not answer; copying the saved files as they are")
			return false, nil
		case errors.Is(err, gameadmin.ErrSnapshotTimeout):
			return false, jobs.Fail("the game did not finish its snapshot in time; see the server log", err)
		case errors.Is(err, gameadmin.ErrSnapshotFailed):
			return false, jobs.Fail("the game could not write its snapshot; see the server log", err)
		default:
			s.logger.Warn("unexpected admin answer to a snapshot request", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
			return false, jobs.Fail("the game's admin endpoint answered the snapshot request outside the Golem Engine admin contract", nil)
		}
	}
}

// CheckRestore verifies a restore request for the HTTP handler: the backup
// exists (a FieldError on backup_id otherwise) and its archive matches its
// record (ErrBackupDamaged), the server exists (ErrNotFound) and has a
// successful build whose image exists (ErrNoBuild), and the schema rule
// holds (*SchemaMismatchError unless force). Docker being unreachable is
// docker.ErrUnavailable.
func (s *Service) CheckRestore(ctx context.Context, id int64, o RestoreOptions) error {
	_, _, _, _, err := s.checkRestore(ctx, id, o)
	var safe *jobs.SafeError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoBuild):
		return ErrNoBuild
	case errors.Is(err, docker.ErrUnavailable):
		return docker.ErrUnavailable
	case errors.As(err, &safe):
		return fmt.Errorf("check restore: %s", safe.Msg)
	}
	return err
}

// checkRestore is CheckRestore returning what the restore uses; build and
// image failures are job failures.
func (s *Service) checkRestore(ctx context.Context, id int64, o RestoreOptions) (store.Server, store.Build, docker.Image, store.Backup, error) {
	var none store.Backup
	if s.cfg.Backups == nil {
		return store.Server{}, store.Build{}, docker.Image{}, none, ErrBackupsUnavailable
	}
	sv, err := s.load(ctx, id)
	if err != nil {
		return sv, store.Build{}, docker.Image{}, none, err
	}
	bk, err := s.store.GetBackup(ctx, o.BackupID)
	if errors.Is(err, store.ErrNotFound) {
		return sv, store.Build{}, docker.Image{}, none, fieldErr("backup_id", "backup %d does not exist", o.BackupID)
	}
	if err != nil {
		return sv, store.Build{}, docker.Image{}, none, err
	}
	if err := s.cfg.Backups.Verify(bk.File, bk.SizeBytes, bk.SHA256); err != nil {
		if errors.Is(err, backups.ErrMissing) {
			return sv, store.Build{}, docker.Image{}, bk, ErrBackupDamaged
		}
		return sv, store.Build{}, docker.Image{}, bk, err
	}
	b, img, err := s.verifiedImage(ctx, sv)
	if err != nil {
		return sv, b, img, bk, err
	}
	if schemaMismatch(bk.SchemaFingerprint, b.SchemaFingerprint) && !o.Force {
		return sv, b, img, bk, &SchemaMismatchError{BackupFingerprint: bk.SchemaFingerprint, BuildFingerprint: b.SchemaFingerprint, BuildID: b.ID}
	}
	return sv, b, img, bk, nil
}

// Restore is the body of a server.restore job: it checks the request again,
// stops the server without changing its desired state, takes a safety
// backup of the current data (kind pre_restore, unless skipped or there is
// none), copies the backup into a fresh volume and only then switches the
// server to that volume and to the backup's fingerprint in one write. The
// old volume is removed afterwards. The server starts again through the
// health-gated start when it is meant to run, except after a forced restore
// of a backup whose schema the build cannot load. A failure before the
// switch leaves the server on its old data (and starts it again when it is
// meant to run).
func (s *Service) Restore(ctx context.Context, id int64, o RestoreOptions, log io.Writer) error {
	if s.cfg.Backups == nil {
		return jobs.Fail("backups are not configured on this daemon", ErrBackupsUnavailable)
	}
	release := s.cfg.Backups.Acquire(o.BackupID)
	defer release()
	sv, b, img, bk, err := s.checkRestore(ctx, id, o)
	if err != nil {
		return restoreCheckFailure(err)
	}
	fmt.Fprintf(log, "restoring backup %d (%s, taken %s from server %q) into server %q\n", bk.ID, bk.Kind,
		bk.CreatedAt.UTC().Format(time.RFC3339), bk.ServerName, sv.Name)
	mismatch := schemaMismatch(bk.SchemaFingerprint, b.SchemaFingerprint)
	switch {
	case mismatch:
		fmt.Fprintf(log, "warning: the backup has schema %s but build %d has schema %s; restoring anyway (force)\n", bk.SchemaFingerprint, b.ID, b.SchemaFingerprint)
	case bk.SchemaFingerprint == "" || b.SchemaFingerprint == "":
		fmt.Fprintf(log, "note: the schema of the backup (%s) or of build %d (%s) is unknown, so it is not checked\n",
			orDash(bk.SchemaFingerprint), b.ID, orDash(b.SchemaFingerprint))
	default:
		fmt.Fprintf(log, "the backup and build %d have the same schema %s\n", b.ID, b.SchemaFingerprint)
	}
	if err := s.stopKeepingDesired(ctx, sv, log); err != nil {
		return err
	}
	if err := s.safetyBackup(ctx, sv, store.BackupPreRestore, o.SkipSafetyBackup, o.CreatedBy, log); err != nil {
		s.startAgain(ctx, id, log)
		return err
	}
	newVol, releaseVol, err := s.newDataVolume(ctx, sv)
	if err != nil {
		s.startAgain(ctx, id, log)
		return err
	}
	defer releaseVol()
	fmt.Fprintf(log, "copying the backup into the new data volume %s\n", newVol)
	if err := s.copyIn(ctx, sv, b, img, newVol, bk); err != nil {
		s.removeVolumeQuietly(ctx, sv, newVol)
		s.startAgain(ctx, id, log)
		return err
	}
	if err := s.store.SwitchServerData(context.WithoutCancel(ctx), sv.ID, sv.DataVolume, newVol, bk.SchemaFingerprint); err != nil {
		s.removeVolumeQuietly(ctx, sv, newVol)
		s.startAgain(ctx, id, log)
		return jobs.Fail("could not switch the server to the restored data", err)
	}
	fmt.Fprintf(log, "the server now uses the data volume %s with the backup's data (schema %s)\n", newVol, orDash(bk.SchemaFingerprint))
	s.logger.Info("backup restored", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "backup_id", bk.ID, "volume", newVol)
	s.dropOldData(ctx, sv, log)
	cur, err := s.load(context.WithoutCancel(ctx), id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	if cur.DesiredState != "running" {
		fmt.Fprintln(log, "the server is stopped; start it to play on the restored data")
		return nil
	}
	if mismatch {
		fmt.Fprintf(log, "not starting the server: build %d cannot load data with schema %s; assign a build with that schema, then start the server\n",
			b.ID, bk.SchemaFingerprint)
		return nil
	}
	if err := s.start(ctx, cur, log); err != nil {
		return restartFailure("the backup was restored but the server did not start", err)
	}
	return nil
}

// restoreCheckFailure maps a failed restore check inside the job.
func restoreCheckFailure(err error) error {
	var fe *FieldError
	var mm *SchemaMismatchError
	var safe *jobs.SafeError
	switch {
	case errors.As(err, &safe):
		return err
	case errors.Is(err, ErrNotFound):
		return jobs.Fail("server not found", err)
	case errors.As(err, &fe):
		return jobs.Fail(fe.Msg, err)
	case errors.Is(err, ErrBackupDamaged):
		return jobs.Fail("the backup's archive is missing or does not match its checksum", err)
	case errors.Is(err, ErrNoBuild):
		return jobs.Fail("the server has no successful build whose image exists", err)
	case errors.As(err, &mm):
		return jobs.Fail(mm.Error()+"; restore with force to do it anyway", err)
	case errors.Is(err, docker.ErrUnavailable):
		return jobs.Fail("Docker is unreachable", err)
	}
	return jobs.Fail("could not check the restore", err)
}

// restartFailure prefixes the detail of a failed start.
func restartFailure(prefix string, err error) error {
	var safe *jobs.SafeError
	if errors.As(err, &safe) {
		return jobs.Fail(prefix+": "+safe.Msg, safe.Err)
	}
	return err
}

// copyIn copies backup bk into the data volume vol through a read-write
// helper, with every entry owned by the container user.
func (s *Service) copyIn(ctx context.Context, sv store.Server, b store.Build, img docker.Image, vol string, bk store.Backup) error {
	f, _, err := s.cfg.Backups.OpenFile(bk.File)
	if err != nil {
		return jobs.Fail("the backup's archive is missing", err)
	}
	defer f.Close()
	h, err := s.createHelper(ctx, sv, b, img, vol, false)
	if err != nil {
		return err
	}
	defer s.removeHelper(ctx, h)
	err = s.docker.CopyToHelper(ctx, h.launch, h.id, build.DataDir, func(w io.Writer) error {
		return backups.Rewrite(f, docker.DefaultUID, docker.DefaultGID, bk.FileCount, bk.ContentBytes, w)
	})
	if err != nil {
		return s.copyFailure(ctx, sv, err)
	}
	return nil
}

// stopKeepingDesired gracefully stops the server's container without
// changing its desired state.
func (s *Service) stopKeepingDesired(ctx context.Context, sv store.Server, log io.Writer) error {
	if err := s.removePending(ctx, sv, log); err != nil {
		return s.failDocker(ctx, sv, "recover the previous create", err)
	}
	stopped, err := s.docker.StopServer(ctx, ref(sv), s.cfg.StopTimeout)
	if errors.Is(err, docker.ErrNotOurs) {
		s.logForeignRef(ref(sv))
		stopped, err = false, nil
	}
	if err != nil {
		return s.failDocker(ctx, sv, "stop the container", err)
	}
	if stopped {
		fmt.Fprintf(log, "stopped the server (graceful, timeout %s); the game saved its snapshot\n", s.cfg.StopTimeout)
	}
	return nil
}

// safetyBackup backs up the server's current data before a restore or
// reset replaces it. It is skipped when asked to and silently when there
// is no data.
func (s *Service) safetyBackup(ctx context.Context, sv store.Server, kind string, skip bool, createdBy *int64, log io.Writer) error {
	if skip {
		fmt.Fprintln(log, "skipping the safety backup of the current data (skip_safety_backup)")
		return nil
	}
	fmt.Fprintln(log, "taking a safety backup of the current data")
	_, err := s.takeBackup(ctx, sv, BackupOptions{Kind: kind, CreatedBy: createdBy}, false, log)
	var nothing *nothingError
	if errors.As(err, &nothing) {
		fmt.Fprintln(log, "no current data to back up")
		return nil
	}
	if err != nil {
		return restartFailure("the safety backup of the current data failed", err)
	}
	return nil
}

// startAgain starts the server again after a failed restore or reset when
// it is meant to run, through the normal start path. Its outcome only
// reaches the job log.
func (s *Service) startAgain(ctx context.Context, id int64, log io.Writer) {
	if ctx.Err() != nil {
		return
	}
	sv, err := s.load(ctx, id)
	if err != nil || sv.DesiredState != "running" {
		return
	}
	fmt.Fprintln(log, "starting the server again with its previous data")
	if err := s.start(ctx, sv, log); err != nil {
		msg := "see the daemon log"
		var safe *jobs.SafeError
		if errors.As(err, &safe) {
			msg = safe.Msg
		}
		fmt.Fprintf(log, "the server did not start again: %s\n", msg)
	}
}

// dropOldData removes the container that mounts the server's previous
// volume and then that volume. Failures are logged and leave a volume that
// is no longer current, which an admin can delete.
func (s *Service) dropOldData(ctx context.Context, old store.Server, log io.Writer) {
	if err := s.removeRecorded(ctx, old, log); err != nil {
		s.logger.Warn("could not remove the container of the previous data", "job_id", jobIDFrom(ctx), "server_id", old.ID, "err", err)
	}
	if old.DataVolume == "" {
		return
	}
	if err := s.docker.RemoveDataVolume(context.WithoutCancel(ctx), old.ID, old.DataVolume); err != nil {
		s.logger.Warn("could not remove the previous data volume", "job_id", jobIDFrom(ctx), "server_id", old.ID, "volume", old.DataVolume, "err", err)
		fmt.Fprintf(log, "warning: could not remove the previous data volume %s; delete it under volumes once nothing uses it\n", old.DataVolume)
		return
	}
	fmt.Fprintf(log, "removed the previous data volume %s\n", old.DataVolume)
}

// ResetData is the body of a server.data_reset job: it stops the server
// without changing its desired state, takes a safety backup (kind
// pre_reset, unless skipped or there is no data), detaches and deletes the
// data volume, forgets the recorded fingerprint and starts the server again
// with an empty world when it is meant to run.
func (s *Service) ResetData(ctx context.Context, id int64, o ResetOptions, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	fmt.Fprintf(log, "resetting the saved data of server %q\n", sv.Name)
	if err := s.stopKeepingDesired(ctx, sv, log); err != nil {
		return err
	}
	if err := s.safetyBackup(ctx, sv, store.BackupPreReset, o.SkipSafetyBackup, o.CreatedBy, log); err != nil {
		s.startAgain(ctx, id, log)
		return err
	}
	if err := s.store.SwitchServerData(context.WithoutCancel(ctx), sv.ID, sv.DataVolume, "", ""); err != nil {
		s.startAgain(ctx, id, log)
		return jobs.Fail("could not detach the data volume", err)
	}
	if sv.DataVolume == "" {
		fmt.Fprintln(log, "the server had no data volume")
	} else {
		fmt.Fprintf(log, "detached the data volume %s\n", sv.DataVolume)
	}
	s.logger.Info("server data reset", "job_id", jobIDFrom(ctx), "server_id", sv.ID)
	s.dropOldData(ctx, sv, log)
	cur, err := s.load(context.WithoutCancel(ctx), id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	if cur.DesiredState != "running" {
		fmt.Fprintln(log, "the server is stopped; it starts with an empty world next time")
		return nil
	}
	if err := s.start(ctx, cur, log); err != nil {
		return restartFailure("the data was reset but the server did not start", err)
	}
	return nil
}
