package servers

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
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
	// ErrSafetyBackup: the backup a restore, reset or deletion takes of the
	// current data first failed or would be incomplete, so nothing was
	// replaced or removed. The job detail says why.
	ErrSafetyBackup = errors.New("servers: the safety backup failed")
)

// safetyError is the cause of a failed safety backup: it matches
// ErrSafetyBackup and wraps the kind of the failure (docker.ErrUnavailable)
// when there is one.
type safetyError struct{ kind error }

func (e safetyError) Error() string        { return ErrSafetyBackup.Error() }
func (e safetyError) Is(target error) bool { return target == ErrSafetyBackup }
func (e safetyError) Unwrap() error        { return e.kind }

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

// nothingError: the server has no data volume, or its data directory holds
// no regular files. omitted counts the entries a backup cannot hold that
// the directory held all the same.
type nothingError struct {
	msg     string
	omitted int
}

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

// restartAllowance is what putting a stopped server back may take besides
// StartTimeout (the Docker calls before the readiness wait).
const restartAllowance = time.Minute

// ValidateNote checks a backup note: a single line of at most
// MaxBackupNote characters without control characters.
func ValidateNote(note string) error {
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > MaxBackupNote {
		return fieldErr("note", "note must be at most %d characters", MaxBackupNote)
	}
	for _, r := range note {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\u2028' || r == '\u2029' || (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') {
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
		FileCount: b.FileCount, OmittedEntries: b.OmittedEntries, SHA256: b.SHA256, JobID: b.JobID, CreatedBy: b.CreatedBy,
		CreatedByUsername: b.CreatedByUsername, CreatedAt: b.CreatedAt.UTC(),
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

// OpenBackup opens a backup's archive for download, once and without
// following links (ErrBackupNotFound, ErrBackupDamaged when the file is
// missing or not a regular file). Serve the returned file.
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

// failFS logs a file system error with the job id and returns a job
// failure with a fixed message: such errors name paths, and the job log
// shows a failure's cause.
func (s *Service) failFS(ctx context.Context, msg string, err error) error {
	s.logger.Warn(msg, "job_id", jobIDFrom(ctx), "err", err)
	return jobs.Fail(msg+"; see the daemon log", nil)
}

// source is the build that wrote a server's saved data, as a backup
// records it. Build is nil when that build was deleted.
type source struct {
	Build         *store.Build
	ProjectID     *int64
	Commit        string
	EngineVersion string
}

func sourceOf(b store.Build) source {
	pid := b.ProjectID
	return source{Build: &b, ProjectID: &pid, Commit: b.Commit, EngineVersion: b.EngineVersion}
}

// dataSource returns the build that wrote the server's saved data and the
// builds whose image a helper may use, in order. With a recorded container
// (running or stopped) the writer is the build its labels name (set by
// the daemon at create, verified against that build's row and image),
// otherwise the assigned build; the assigned build is the second choice
// for the helper image.
func (s *Service) dataSource(ctx context.Context, sv store.Server) (source, []store.Build, error) {
	var src source
	var images []store.Build
	if sv.ContainerID != "" {
		ct, err := s.container(ctx, sv)
		if err != nil {
			return src, nil, s.failDocker(ctx, sv, "inspect the container", err)
		}
		if ct != nil {
			src, images = s.containerSource(ctx, sv, *ct)
		}
	}
	assigned, aerr := s.assignedBuild(ctx, sv)
	if aerr == nil && (len(images) == 0 || images[0].ID != assigned.ID) {
		images = append(images, assigned)
	}
	if src.Commit == "" && src.Build == nil {
		if aerr != nil {
			return src, nil, aerr
		}
		src = sourceOf(assigned)
	}
	return src, images, nil
}

// containerSource reads the build of a server container from its labels.
// The build's row must agree with them (project, commit, engine version,
// image); a deleted build leaves the labels' values without a build id.
// Labels that do not hold up give an empty source.
func (s *Service) containerSource(ctx context.Context, sv store.Server, ct docker.Container) (source, []store.Build) {
	id, err := strconv.ParseInt(ct.Labels[docker.LabelBuildID], 10, 64)
	if err != nil || id <= 0 {
		return source{}, nil
	}
	commit, engine := ct.Labels[docker.LabelCommit], ct.Labels[docker.LabelEngineVersion]
	var pid *int64
	if p, err := strconv.ParseInt(ct.Labels[docker.LabelProjectID], 10, 64); err == nil && p > 0 {
		pid = &p
	}
	b, err := s.store.GetBuild(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return source{ProjectID: pid, Commit: commit, EngineVersion: engine}, nil
	case err != nil:
		s.logger.Warn("could not read the build of a server's container", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "build_id", id, "err", err)
		return source{}, nil
	case b.Commit != commit || b.EngineVersion != engine || pid == nil || *pid != b.ProjectID || b.ImageID == "" || ct.ImageID != b.ImageID:
		s.logger.Warn("the labels of a server's container do not match its build; recording the assigned build", "job_id", jobIDFrom(ctx),
			"server_id", sv.ID, "build_id", id)
		return source{}, nil
	}
	return sourceOf(b), []store.Build{b}
}

// helperImage returns the first of builds whose image exists and verifies.
// A Docker failure stops at once; when no image holds up, the failure of
// the last build is returned.
func (s *Service) helperImage(ctx context.Context, sv store.Server, builds []store.Build) (store.Build, docker.Image, error) {
	err := jobs.Fail(fmt.Sprintf("the server has no build; assign a successful build with PATCH /api/v1/servers/%d (build_id)", sv.ID), ErrNoBuild)
	for _, b := range builds {
		var img docker.Image
		img, err = s.verifyBuildImage(ctx, sv, b)
		if err == nil {
			return b, img, nil
		}
		if !errors.Is(err, ErrNoBuild) {
			return b, img, err
		}
	}
	return store.Build{}, docker.Image{}, err
}

// takeBackup copies the server's data directory into a new backup. With
// live set and the server's game running and answering, it first asks the
// game for a fresh snapshot. A server without a data volume, or whose data
// directory holds no regular files, is a *nothingError (with the number of
// entries left out). Every stored backup records how many entries it left
// out (OmittedEntries).
func (s *Service) takeBackup(ctx context.Context, sv store.Server, o BackupOptions, live bool, log io.Writer) (*store.Backup, error) {
	if sv.DataVolume == "" {
		return nil, &nothingError{msg: "the server has no saved data yet (it gets a data volume when it starts); there is nothing to back up"}
	}
	if s.cfg.Backups == nil {
		return nil, jobs.Fail("backups are not configured on this daemon", ErrBackupsUnavailable)
	}
	gone, err := s.volumeState(ctx, sv)
	if err != nil {
		return nil, err
	}
	if gone {
		return nil, &nothingError{msg: "the server's data volume no longer exists; there is nothing to back up"}
	}
	src, candidates, err := s.dataSource(ctx, sv)
	if err != nil {
		return nil, err
	}
	hb, img, err := s.helperImage(ctx, sv, candidates)
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
	h, err := s.createHelper(ctx, sv, hb, img, sv.DataVolume, true)
	if err != nil {
		return nil, err
	}
	defer s.removeHelper(ctx, h)
	pending, err := s.cfg.Backups.Create()
	if errors.Is(err, backups.ErrNoSpace) {
		return nil, jobs.Fail("not enough free disk space for a backup", backups.ErrNoSpace)
	}
	if err != nil {
		return nil, s.failFS(ctx, "could not create the backup file", err)
	}
	defer pending.Discard()
	var res backups.Result
	err = s.docker.CopyFromHelper(ctx, h.launch, h.id, build.DataDir, func(r io.Reader) error {
		var nerr error
		res, nerr = backups.Normalize(r, path.Base(build.DataDir), s.cfg.BackupMaxBytes, pending)
		return nerr
	})
	if err != nil {
		return nil, s.copyFailure(ctx, sv, err)
	}
	s.removeHelper(ctx, h)
	if res.SkippedCount > 0 {
		// Only counts: the names were chosen by the game and can hold
		// anything, also values of the server's environment.
		fmt.Fprintf(log, "left out %d %s: %s\n", res.SkippedCount, plural(res.SkippedCount, "entry", "entries"), backups.DescribeSkipped(res.Skipped))
	}
	if res.Files == 0 {
		msg := "the server's data directory holds no files; there is nothing to back up"
		if res.SkippedCount > 0 {
			msg = fmt.Sprintf("the server's data directory holds no regular files (entries a backup cannot hold: %d); there is nothing to back up", res.SkippedCount)
		}
		return nil, &nothingError{msg: msg, omitted: res.SkippedCount}
	}
	file, size, sum, err := pending.Commit()
	if err != nil {
		return nil, s.failFS(ctx, "could not store the backup file", err)
	}
	row := store.Backup{
		Kind: o.Kind, File: file, ServerID: &sv.ID, ServerName: sv.Name, ProjectID: src.ProjectID, Commit: src.Commit, EngineVersion: src.EngineVersion,
		SchemaFingerprint: res.Fingerprint, Live: fresh, Note: o.Note, SizeBytes: size, ContentBytes: res.ContentBytes, FileCount: res.Files,
		OmittedEntries: res.SkippedCount, SHA256: sum, CreatedBy: o.CreatedBy,
	}
	if src.Build != nil {
		bid := src.Build.ID
		row.BuildID = &bid
	}
	if src.ProjectID != nil {
		if p, err := s.store.GetProject(ctx, *src.ProjectID); err == nil {
			row.ProjectName = p.Name
		}
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
	built := "a deleted build"
	if created.BuildID != nil {
		built = fmt.Sprintf("build %d", *created.BuildID)
	}
	fmt.Fprintf(log, "backup %d (%s) stored: %d files, %d bytes of content, %d bytes compressed, schema %s, data written by %s (commit %s)\n",
		created.ID, created.Kind, created.FileCount, created.ContentBytes, created.SizeBytes, orDash(created.SchemaFingerprint), built, orDash(shortID(created.Commit)))
	s.logger.Info("backup stored", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "backup_id", created.ID, "kind", created.Kind, "bytes", created.SizeBytes,
		"omitted_entries", created.OmittedEntries)
	return &created, nil
}

// copyFailure maps a failed copy of a data directory to a job failure. The
// failure's cause is only the kind of error; the error itself goes to the
// daemon log.
func (s *Service) copyFailure(ctx context.Context, sv store.Server, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var cmdErr *docker.CmdError
	if errors.As(err, &cmdErr) {
		return s.failDocker(ctx, sv, "copy the data directory", err)
	}
	s.logger.Warn("copying a data directory failed", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
	switch {
	case errors.Is(err, backups.ErrNoSpace):
		return jobs.Fail("not enough free disk space for the backup", backups.ErrNoSpace)
	case errors.Is(err, backups.ErrTooLarge):
		return jobs.Fail(fmt.Sprintf("the data directory is larger than the backup size limit of %d MiB (--backup-max-mb)", s.cfg.BackupMaxBytes>>20), backups.ErrTooLarge)
	case errors.Is(err, backups.ErrTooManyEntries):
		return jobs.Fail(fmt.Sprintf("the data directory has more than %d entries", backups.MaxEntries), backups.ErrTooManyEntries)
	case errors.Is(err, backups.ErrDamaged):
		return jobs.Fail("the backup's archive is damaged", backups.ErrDamaged)
	case errors.Is(err, errImageData):
		return jobs.Fail(fmt.Sprintf("the server's image already has files in %s, which Docker copies into every new data volume, so the restored "+
			"data would not be exactly the backup's; the restore stopped before anything was replaced. Build an image whose %s is empty", build.DataDir, build.DataDir), nil)
	}
	return jobs.Fail("copying the data directory failed; see the daemon log", nil)
}

// snapshot asks the server's running game for a fresh snapshot. It
// reports whether one was written; a stopped server, a game without the
// snapshot hook or an admin endpoint that does not answer give false. Any
// other answer fails the backup. The whole request, retries included, is
// bounded by SnapshotDeadline, and so are the inspections between retries:
// before every retry the recorded container is inspected again and the
// next request goes to the port that inspection reports.
func (s *Service) snapshot(ctx context.Context, sv store.Server, log io.Writer) (bool, error) {
	ct, err := s.container(ctx, sv)
	if err != nil {
		return false, s.failDocker(ctx, sv, "inspect the container", err)
	}
	if ct == nil || !adminRunning(*ct) {
		fmt.Fprintln(log, "the server is not running; copying the saved files as they are")
		return false, nil
	}
	token, terr := s.openAdminToken(sv)
	if terr != nil {
		fmt.Fprintln(log, "the game's admin endpoint is not reachable; copying the saved files as they are")
		return false, nil
	}
	sctx, cancel := context.WithTimeout(ctx, s.cfg.SnapshotDeadline)
	defer cancel()
	tooLong := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return jobs.Fail(fmt.Sprintf("the game did not finish a snapshot within %s; try again later", s.cfg.SnapshotDeadline), nil)
	}
	fmt.Fprintln(log, "asking the game for a fresh snapshot")
	waiting := false
	for {
		client, err := gameadmin.New(ct.LoopbackPort(AdminContainerPort), token)
		if err != nil {
			fmt.Fprintln(log, "the game's admin endpoint is not reachable; copying the saved files as they are")
			return false, nil
		}
		took, err := client.Snapshot(sctx)
		switch {
		case err == nil:
			fmt.Fprintf(log, "the game wrote a fresh snapshot in %s\n", took)
			return true, nil
		case sctx.Err() != nil:
			return false, tooLong()
		case errors.Is(err, gameadmin.ErrSnapshotInProgress):
			if !waiting {
				waiting = true
				fmt.Fprintln(log, "a snapshot is already in progress; waiting for it")
			}
			select {
			case <-sctx.Done():
				return false, tooLong()
			case <-time.After(s.cfg.SnapshotRetry):
			}
			if ct, err = s.container(sctx, sv); err != nil {
				if sctx.Err() != nil {
					return false, tooLong()
				}
				return false, s.failDocker(ctx, sv, "inspect the container", err)
			}
			if ct == nil || !adminRunning(*ct) {
				fmt.Fprintln(log, "the server stopped while the daemon waited for its snapshot; copying the saved files as they are")
				return false, nil
			}
		case errors.Is(err, gameadmin.ErrNotReady):
			return false, jobs.Fail("the game is not ready (it is starting or stopping); try again shortly", nil)
		case errors.Is(err, gameadmin.ErrSnapshotUnsupported):
			fmt.Fprintln(log, "the game has no snapshot hook; copying the saved files as they are")
			return false, nil
		case errors.Is(err, gameadmin.ErrUnreachable):
			fmt.Fprintln(log, "the game's admin endpoint did not answer; copying the saved files as they are")
			return false, nil
		case errors.Is(err, gameadmin.ErrSnapshotTimeout):
			return false, jobs.Fail("the game did not finish its snapshot in time; see the server log", nil)
		case errors.Is(err, gameadmin.ErrSnapshotFailed):
			return false, jobs.Fail("the game could not write its snapshot; see the server log", nil)
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
	_, _, _, _, f, err := s.checkRestore(ctx, id, o)
	if f != nil {
		f.Close()
	}
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

// checkRestore is CheckRestore returning what the restore uses, including
// the backup's archive opened once and verified (the caller closes it and
// reads it from its start). Build and image failures are job failures.
func (s *Service) checkRestore(ctx context.Context, id int64, o RestoreOptions) (store.Server, store.Build, docker.Image, store.Backup, *os.File, error) {
	var none store.Backup
	if s.cfg.Backups == nil {
		return store.Server{}, store.Build{}, docker.Image{}, none, nil, ErrBackupsUnavailable
	}
	sv, err := s.load(ctx, id)
	if err != nil {
		return sv, store.Build{}, docker.Image{}, none, nil, err
	}
	bk, err := s.store.GetBackup(ctx, o.BackupID)
	if errors.Is(err, store.ErrNotFound) {
		return sv, store.Build{}, docker.Image{}, none, nil, fieldErr("backup_id", "backup %d does not exist", o.BackupID)
	}
	if err != nil {
		return sv, store.Build{}, docker.Image{}, none, nil, err
	}
	f, err := s.cfg.Backups.OpenVerified(bk.File, bk.SizeBytes, bk.SHA256)
	if errors.Is(err, backups.ErrMissing) {
		return sv, store.Build{}, docker.Image{}, bk, nil, ErrBackupDamaged
	}
	if err != nil {
		return sv, store.Build{}, docker.Image{}, bk, nil, err
	}
	b, img, err := s.verifiedImage(ctx, sv)
	if err == nil && schemaMismatch(bk.SchemaFingerprint, b.SchemaFingerprint) && !o.Force {
		err = &SchemaMismatchError{BackupFingerprint: bk.SchemaFingerprint, BuildFingerprint: b.SchemaFingerprint, BuildID: b.ID}
	}
	if err != nil {
		f.Close()
		return sv, b, img, bk, nil, err
	}
	return sv, b, img, bk, f, nil
}

// Restore is the body of a server.restore job: it checks the request again,
// stops the server without changing its desired state, takes a safety
// backup of the current data (kind pre_restore, unless skipped or there is
// none; one that would leave entries out fails the job), copies the backup
// into a fresh volume and only then switches the server to that volume and
// to the backup's fingerprint in one write. The old volume is removed
// afterwards. The server starts again through the health-gated start when
// it is meant to run, except after a forced restore of a backup whose
// schema the build cannot load. A failure or cancellation before the
// switch leaves the server on its old data and puts back the container the
// job stopped (see putBack); after the switch the job finishes its
// bookkeeping and succeeds.
func (s *Service) Restore(ctx context.Context, id int64, o RestoreOptions, log io.Writer) error {
	if s.cfg.Backups == nil {
		return jobs.Fail("backups are not configured on this daemon", ErrBackupsUnavailable)
	}
	release := s.cfg.Backups.Acquire(o.BackupID)
	defer release()
	sv, b, img, bk, archive, err := s.checkRestore(ctx, id, o)
	if err != nil {
		return s.restoreCheckFailure(ctx, err)
	}
	defer archive.Close()
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
	if bk.OmittedEntries > 0 {
		fmt.Fprintf(log, "note: the backup left out %d entries of the data directory it was taken from\n", bk.OmittedEntries)
	}
	paused, err := s.stopForData(ctx, sv, log)
	if err != nil {
		return err
	}
	if err := s.safetyBackup(ctx, sv, store.BackupPreRestore, o.SkipSafetyBackup, o.CreatedBy, log); err != nil {
		s.putBack(ctx, paused, log)
		return err
	}
	newVol, releaseVol, err := s.newDataVolume(ctx, sv)
	if err != nil {
		s.putBack(ctx, paused, log)
		return err
	}
	defer releaseVol()
	fmt.Fprintf(log, "copying the backup into the new data volume %s\n", newVol)
	if err := s.copyIn(ctx, sv, b, img, newVol, bk, archive); err != nil {
		s.removeVolumeQuietly(ctx, sv, newVol)
		s.putBack(ctx, paused, log)
		return err
	}
	if ctx.Err() != nil {
		fmt.Fprintln(log, "the job was cancelled before the server was switched to the restored data")
		s.removeVolumeQuietly(ctx, sv, newVol)
		s.putBack(ctx, paused, log)
		return ctx.Err()
	}
	if err := s.store.SwitchServerData(context.WithoutCancel(ctx), sv.ID, sv.DataVolume, newVol, bk.SchemaFingerprint); err != nil {
		s.removeVolumeQuietly(ctx, sv, newVol)
		s.putBack(ctx, paused, log)
		return jobs.Fail("could not switch the server to the restored data", err)
	}
	fmt.Fprintf(log, "the server now uses the data volume %s with the backup's data (schema %s)\n", newVol, orDash(bk.SchemaFingerprint))
	s.logger.Info("backup restored", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "backup_id", bk.ID, "volume", newVol)
	if mismatch {
		return s.afterSwitch(ctx, id, log, sv, "the backup was restored", "the server is stopped; start it to play on the restored data",
			fmt.Sprintf("not starting the server: build %d cannot load data with schema %s; assign a build with that schema, then start the server",
				b.ID, bk.SchemaFingerprint))
	}
	return s.afterSwitch(ctx, id, log, sv, "the backup was restored", "the server is stopped; start it to play on the restored data", "")
}

// afterSwitch finishes a restore or reset once the server row names the
// new data: it removes the previous container and volume and starts the
// server when it is meant to run (unless noStart gives a reason not to;
// stopped is the job log line for a server that is not meant to run).
// From here on the job never ends as cancelled: a cancellation skips the
// automatic start with a line in the job log, and the job succeeds.
func (s *Service) afterSwitch(ctx context.Context, id int64, log io.Writer, old store.Server, done, stopped, noStart string) error {
	bctx := context.WithoutCancel(ctx)
	s.dropOldData(bctx, old, log)
	cur, err := s.load(bctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	switch {
	case cur.DesiredState != "running":
		fmt.Fprintln(log, stopped)
		return nil
	case noStart != "":
		fmt.Fprintln(log, noStart)
		return nil
	case ctx.Err() != nil:
		fmt.Fprintln(log, "the job was cancelled after the server was switched to the new data; skipping the automatic start (start the server yourself)")
		return nil
	}
	if err := s.start(ctx, cur, log); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(log, "the job was cancelled while the server started on the new data; the data is in place, start the server yourself")
			return nil
		}
		return restartFailure(done+" but the server did not start", err)
	}
	return nil
}

// restoreCheckFailure maps a failed restore check inside the job.
func (s *Service) restoreCheckFailure(ctx context.Context, err error) error {
	var fe *FieldError
	var mm *SchemaMismatchError
	var safe *jobs.SafeError
	switch {
	case errors.As(err, &safe):
		return err
	case errors.Is(err, ErrNotFound):
		return jobs.Fail("server not found", ErrNotFound)
	case errors.As(err, &fe):
		return jobs.Fail(fe.Msg, nil)
	case errors.Is(err, ErrBackupDamaged):
		return jobs.Fail("the backup's archive is missing or does not match its checksum", ErrBackupDamaged)
	case errors.Is(err, ErrNoBuild):
		return jobs.Fail("the server has no successful build whose image exists", ErrNoBuild)
	case errors.As(err, &mm):
		return jobs.Fail(mm.Error()+"; restore with force to do it anyway", nil)
	case errors.Is(err, docker.ErrUnavailable):
		return jobs.Fail("Docker is unreachable", docker.ErrUnavailable)
	}
	return s.failFS(ctx, "could not check the restore", err)
}

// restartFailure prefixes the detail of a failed start.
func restartFailure(prefix string, err error) error {
	var safe *jobs.SafeError
	if errors.As(err, &safe) {
		return jobs.Fail(prefix+": "+safe.Msg, safe.Err)
	}
	return err
}

// errImageData: the helper's fresh volume already holds what the image has
// in the data directory (Docker copies it into an empty volume).
var errImageData = errors.New("servers: the image has files in the data directory")

// copyIn copies the backup bk, read from its verified open archive, into
// the fresh data volume vol through a read-write helper, with every entry
// owned by the container user. Docker fills an empty volume with what the
// image holds at the mount point when the helper is created (it is what
// gives the volume root its owner); the copy only proceeds when that is
// nothing but the directory itself, so the volume ends up holding exactly
// the backup's files.
func (s *Service) copyIn(ctx context.Context, sv store.Server, b store.Build, img docker.Image, vol string, bk store.Backup, archive io.Reader) error {
	h, err := s.createHelper(ctx, sv, b, img, vol, false)
	if err != nil {
		return err
	}
	defer s.removeHelper(ctx, h)
	base := path.Base(build.DataDir)
	err = s.docker.CopyFromHelper(ctx, h.launch, h.id, build.DataDir, func(r io.Reader) error {
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if strings.TrimSuffix(hdr.Name, "/") != base {
				return errImageData
			}
		}
	})
	if err != nil {
		return s.copyFailure(ctx, sv, err)
	}
	err = s.docker.CopyToHelper(ctx, h.launch, h.id, build.DataDir, func(w io.Writer) error {
		return backups.Rewrite(archive, docker.DefaultUID, docker.DefaultGID, bk.FileCount, bk.ContentBytes, w)
	})
	if err != nil {
		return s.copyFailure(ctx, sv, err)
	}
	return nil
}

// stopped is what stopForData found before it stopped a server: the row
// as it was (the recorded container's id and sealed admin token) and
// whether that container was running, so putBack can start that same
// container again.
type stopped struct {
	sv         store.Server
	wasRunning bool
}

// stopForData gracefully stops the server's recorded container without
// changing its desired state, after noting whether it was running. When
// the stop itself fails or is cancelled, the container is put back (see
// putBack) before the failure is returned.
func (s *Service) stopForData(ctx context.Context, sv store.Server, log io.Writer) (*stopped, error) {
	p := &stopped{sv: sv}
	if err := s.removePending(ctx, sv, log); err != nil {
		return p, s.failDocker(ctx, sv, "recover the previous create", err)
	}
	ct, err := s.container(ctx, sv)
	if err != nil {
		return p, s.failDocker(ctx, sv, "inspect the container", err)
	}
	p.wasRunning = ct != nil && adminRunning(*ct)
	done, err := s.docker.StopServer(ctx, ref(sv), s.cfg.StopTimeout)
	if errors.Is(err, docker.ErrNotOurs) {
		s.logForeignRef(ref(sv))
		done, err = false, nil
	}
	if err != nil {
		ferr := s.failDocker(ctx, sv, "stop the container", err)
		s.putBack(ctx, p, log)
		return p, ferr
	}
	if done {
		fmt.Fprintf(log, "stopped the server (graceful, timeout %s); the game saved its snapshot\n", s.cfg.StopTimeout)
	}
	return p, nil
}

// safetyBackup backs up the server's current data before a restore or
// reset replaces it or a deletion removes it. It is skipped when asked to,
// and silently when the volume is missing or the data directory holds no
// entries at all. A backup that leaves entries out is stored (when it has
// a file) and then fails, like a backup that cannot be taken: the failure
// matches ErrSafetyBackup and says how to go ahead anyway.
func (s *Service) safetyBackup(ctx context.Context, sv store.Server, kind string, skip bool, createdBy *int64, log io.Writer) error {
	if skip {
		fmt.Fprintln(log, "skipping the backup of the current data (skip_safety_backup)")
		return nil
	}
	fmt.Fprintln(log, "taking a safety backup of the current data")
	b, err := s.takeBackup(ctx, sv, BackupOptions{Kind: kind, CreatedBy: createdBy}, false, log)
	var nothing *nothingError
	switch {
	case errors.As(err, &nothing) && nothing.omitted == 0:
		fmt.Fprintln(log, "no current data to back up")
		return nil
	case errors.As(err, &nothing):
		return safetyFailure(kind, true, fmt.Sprintf("the data directory holds no regular files, only %d entries a backup cannot hold (links, pipes, devices or names "+
			"that are not allowed), so no backup was stored", nothing.omitted), nil)
	case err != nil && ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		var safe *jobs.SafeError
		detail := "see the daemon log"
		if errors.As(err, &safe) {
			detail = safe.Msg
		}
		var kindErr error
		if errors.Is(err, docker.ErrUnavailable) {
			kindErr = docker.ErrUnavailable
		}
		return safetyFailure(kind, false, detail, kindErr)
	case b.OmittedEntries > 0:
		return safetyFailure(kind, true, fmt.Sprintf("%d of the entries in the data directory cannot be held by a backup (links, pipes, devices or names "+
			"that are not allowed); the incomplete backup was stored as backup %d", b.OmittedEntries, b.ID), nil)
	}
	return nil
}

// safetyFailure words a failed safety backup for its operation. An
// incomplete backup says how to go ahead without a complete one.
func safetyFailure(kind string, incomplete bool, detail string, kindErr error) error {
	what, nothing, override := "the safety backup of the current data", "nothing was replaced", "repeat the request with skip_safety_backup to go ahead"
	if kind == store.BackupPreDelete {
		what, nothing, override = "the last backup of the server's data", "nothing was deleted", "an admin can delete the server anyway with skip_safety_backup=1"
	}
	msg := what + " failed: " + detail + "; " + nothing
	if incomplete {
		msg = what + " is incomplete: " + detail + "; " + nothing + "; " + override
	}
	return jobs.Fail(msg, safetyError{kind: kindErr})
}

// putBack undoes stopForData after a restore, reset or deletion ended
// before it replaced or removed the data: when the recorded container was
// running before the stop and is not running now, it starts that same
// container again (docker start of the recorded id) and waits until its
// game reports ready with the recorded admin token. It never creates or
// removes a container and does not look at the assigned build, so a build
// assigned since (which the schema guard may refuse) does not keep the
// server down. It runs under a context that is not cancelled but bounded
// by StartTimeout plus restartAllowance, and its outcome only reaches the
// job log (Docker's own errors go to the daemon log).
func (s *Service) putBack(ctx context.Context, p *stopped, log io.Writer) {
	if p == nil || !p.wasRunning {
		return
	}
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.StartTimeout+restartAllowance)
	defer cancel()
	sv := p.sv
	ct, err := s.container(bctx, sv)
	switch {
	case err != nil:
		s.logger.Warn("could not inspect a stopped server container to start it again", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
		fmt.Fprintln(log, "the server was not started again: Docker could not inspect its container (details in the daemon log); start the server yourself")
		return
	case ct == nil:
		fmt.Fprintln(log, "the server was not started again: its container no longer exists; start the server yourself")
		return
	case adminRunning(*ct):
		fmt.Fprintf(log, "the server's container %s is still running\n", shortID(sv.ContainerID))
	default:
		fmt.Fprintf(log, "starting the server's container %s again with its previous data\n", shortID(sv.ContainerID))
		if err := s.docker.StartServer(bctx, ref(sv)); err != nil {
			s.logger.Warn("could not start a stopped server container again", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", s.redactStartError(sv, err))
			fmt.Fprintln(log, "the server was not started again: Docker could not start its container (details in the daemon log); start the server yourself")
			return
		}
	}
	if why := s.waitRecordedReady(bctx, sv); why != "" {
		fmt.Fprintf(log, "the server's container runs again, but %s; see the server log (GET /api/v1/servers/%d/logs)\n", why, sv.ID)
		return
	}
	fmt.Fprintln(log, "the server is running again with its previous data")
}

// redactStartError removes the server's environment values and admin
// token from the Docker output a failed docker start carries (best effort,
// like a start does) before it goes to the daemon log.
func (s *Service) redactStartError(sv store.Server, err error) error {
	var cmdErr *docker.CmdError
	if !errors.As(err, &cmdErr) {
		return err
	}
	values := map[string]string{}
	if env, eerr := s.openEnv(sv); eerr == nil {
		for k, v := range env {
			values[k] = v
		}
	}
	if token, terr := s.openAdminToken(sv); terr == nil {
		values[EnvAdminToken] = token
	}
	cmdErr.Detail = docker.RedactValues(cmdErr.Detail, values)
	return err
}

// waitRecordedReady polls the recorded container and its game's admin
// endpoint, with the recorded admin token and the port of a fresh
// inspection each time (the way a verified running container is checked),
// until the game reports ready, the container stops or StartTimeout
// passes. It returns "" when the game is ready, otherwise why not, for the
// job log.
func (s *Service) waitRecordedReady(ctx context.Context, sv store.Server) string {
	token, err := s.openAdminToken(sv)
	if err != nil {
		return "its admin token could not be read, so whether its game is ready is unknown"
	}
	probeCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()
	t := time.NewTicker(s.cfg.ReadyPoll)
	defer t.Stop()
	for {
		ct, err := s.container(probeCtx, sv)
		switch {
		case err != nil:
			if probeCtx.Err() == nil {
				s.logger.Warn("could not inspect a server container while waiting for its game", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
			}
		case ct == nil:
			return "its container disappeared"
		case !adminRunning(*ct):
			return fmt.Sprintf("its container is %s (exit code %d)", ct.State, ct.ExitCode)
		default:
			if port := ct.LoopbackPort(AdminContainerPort); port != 0 {
				if client, cerr := gameadmin.New(port, token); cerr == nil {
					r, rerr := client.Ready(probeCtx)
					client.Close()
					if rerr == nil && r.Ready {
						return ""
					}
				}
			}
		}
		select {
		case <-probeCtx.Done():
			return fmt.Sprintf("its game did not report ready within %s", s.cfg.StartTimeout)
		case <-t.C:
		}
	}
}

// plural returns one for n == 1 and many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
	if err := s.docker.RemoveDataVolume(ctx, old.ID, old.DataVolume); err != nil {
		s.logger.Warn("could not remove the previous data volume", "job_id", jobIDFrom(ctx), "server_id", old.ID, "volume", old.DataVolume, "err", err)
		fmt.Fprintf(log, "warning: could not remove the previous data volume %s; delete it under volumes once nothing uses it\n", old.DataVolume)
		return
	}
	fmt.Fprintf(log, "removed the previous data volume %s\n", old.DataVolume)
}

// ResetData is the body of a server.data_reset job: it stops the server
// without changing its desired state, takes a safety backup (kind
// pre_reset, with the rules of safetyBackup), detaches and deletes the
// data volume, forgets the recorded fingerprint and starts the server
// again with an empty world when it is meant to run. Failures,
// cancellation and putting the server back follow the rules of Restore.
func (s *Service) ResetData(ctx context.Context, id int64, o ResetOptions, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	fmt.Fprintf(log, "resetting the saved data of server %q\n", sv.Name)
	paused, err := s.stopForData(ctx, sv, log)
	if err != nil {
		return err
	}
	if err := s.safetyBackup(ctx, sv, store.BackupPreReset, o.SkipSafetyBackup, o.CreatedBy, log); err != nil {
		s.putBack(ctx, paused, log)
		return err
	}
	if ctx.Err() != nil {
		fmt.Fprintln(log, "the job was cancelled before the data was detached")
		s.putBack(ctx, paused, log)
		return ctx.Err()
	}
	if err := s.store.SwitchServerData(context.WithoutCancel(ctx), sv.ID, sv.DataVolume, "", ""); err != nil {
		s.putBack(ctx, paused, log)
		return jobs.Fail("could not detach the data volume", err)
	}
	if sv.DataVolume == "" {
		fmt.Fprintln(log, "the server had no data volume")
	} else {
		fmt.Fprintf(log, "detached the data volume %s\n", sv.DataVolume)
	}
	s.logger.Info("server data reset", "job_id", jobIDFrom(ctx), "server_id", sv.ID)
	return s.afterSwitch(ctx, id, log, sv, "the data was reset", "the server is stopped; it starts with an empty world next time", "")
}
