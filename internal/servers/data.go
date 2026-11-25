package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Bounds of the saved data settings (see apitypes.BackupSchedule).
const (
	MinSnapshotIntervalSeconds = 10
	MaxSnapshotIntervalSeconds = 86400
	MaxBackupIntervalHours     = 720
	MaxBackupKeep              = 100
)

// dataSettings validates the saved data settings of a create, PATCH or
// import entry and returns the values to store. Absent settings keep cur*
// (the defaults on create); a schedule is always given whole. prefix is
// prepended to field names ("servers[2]." on import).
func dataSettings(prefix string, snapshot *int, schedule *apitypes.BackupSchedule, curSnapshot, curInterval, curKeep int) (int, int, int, error) {
	if snapshot != nil {
		v := *snapshot
		if v != 0 && (v < MinSnapshotIntervalSeconds || v > MaxSnapshotIntervalSeconds) {
			return 0, 0, 0, fieldErr(prefix+"snapshot_interval_seconds", "the autosave period must be 0 (off) or %d to %d seconds",
				MinSnapshotIntervalSeconds, MaxSnapshotIntervalSeconds)
		}
		curSnapshot = v
	}
	if schedule != nil {
		if schedule.IntervalHours < 0 || schedule.IntervalHours > MaxBackupIntervalHours {
			return 0, 0, 0, fieldErr(prefix+"backup_schedule.interval_hours", "the backup interval must be 0 (off) or 1 to %d hours", MaxBackupIntervalHours)
		}
		if schedule.Keep < 1 || schedule.Keep > MaxBackupKeep {
			return 0, 0, 0, fieldErr(prefix+"backup_schedule.keep", "the number of scheduled backups to keep must be 1 to %d", MaxBackupKeep)
		}
		curInterval, curKeep = schedule.IntervalHours, schedule.Keep
	}
	return curSnapshot, curInterval, curKeep, nil
}

// schemaMismatch reports whether saved data with fingerprint data cannot
// be loaded by a build with fingerprint build: both known and different.
func schemaMismatch(data, build string) bool {
	return data != "" && build != "" && data != build
}

// schemaGuard is the failure of a start whose saved data has another
// schema than the build: the game would refuse to load it.
func schemaGuard(sv store.Server, b store.Build, dataFingerprint string) error {
	return jobs.Fail(fmt.Sprintf("the server's saved data has schema %s but build %d has schema %s, so the game cannot load it; "+
		"assign a build with the saved schema (PATCH /api/v1/servers/%d with build_id), restore a backup with schema %s "+
		"(POST /api/v1/servers/%d/restore) or reset the data (POST /api/v1/servers/%d/data/reset)",
		dataFingerprint, b.ID, b.SchemaFingerprint, sv.ID, b.SchemaFingerprint, sv.ID, sv.ID), nil)
}

// volumeState inspects the server's recorded data volume: gone is true when
// it is recorded but no longer exists. A volume that is not this
// installation's volume of this server fails the job.
func (s *Service) volumeState(ctx context.Context, sv store.Server) (gone bool, err error) {
	if sv.DataVolume == "" {
		return false, nil
	}
	_, err = s.docker.DataVolume(ctx, sv.ID, sv.DataVolume)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, docker.ErrNotFound):
		return true, nil
	case errors.Is(err, docker.ErrNotManaged), errors.Is(err, docker.ErrNotOurs):
		s.logger.Warn("the data volume recorded for a server does not carry this installation's labels for it", "job_id", jobIDFrom(ctx),
			"server_id", sv.ID, "volume", sv.DataVolume)
		return false, jobs.Fail(fmt.Sprintf("the data volume %s recorded for the server does not carry this installation's labels for it; refusing to use it", sv.DataVolume), nil)
	}
	return false, s.failDocker(ctx, sv, "inspect the data volume", err)
}

// newDataVolume creates a fresh data volume for the server. It stays in
// pendingVolumes (so it cannot be deleted through the API) until the
// caller releases it after recording it or giving up.
func (s *Service) newDataVolume(ctx context.Context, sv store.Server) (name string, release func(), err error) {
	name, err = docker.NewDataVolumeName(sv.ID)
	if err != nil {
		return "", nil, jobs.Fail("could not name a data volume", err)
	}
	s.pendingVolumes.Store(name, true)
	release = func() { s.pendingVolumes.Delete(name) }
	err = s.docker.CreateDataVolume(ctx, sv.ID, sv.Name, name)
	switch {
	case err == nil:
		return name, release, nil
	case errors.Is(err, docker.ErrNotManaged), errors.Is(err, docker.ErrNotOurs):
		release()
		return "", nil, jobs.Fail("a Docker volume named "+name+" already exists and does not belong to this server", nil)
	}
	release()
	return "", nil, s.failDocker(ctx, sv, "create the data volume", err)
}

// ensureDataVolume gives a server that is about to start a data volume:
// a new one when it has none or the recorded one is gone (then the
// recorded fingerprint is forgotten as well). sv is updated.
func (s *Service) ensureDataVolume(ctx context.Context, sv *store.Server, gone bool, log io.Writer) error {
	if sv.DataVolume != "" && !gone {
		return nil
	}
	if gone {
		fmt.Fprintf(log, "the data volume %s recorded for the server no longer exists; creating a new, empty one (restore a backup to get the saved world back)\n", sv.DataVolume)
	}
	name, release, err := s.newDataVolume(ctx, *sv)
	if err != nil {
		return err
	}
	defer release()
	if err := s.store.SetServerDataVolume(context.WithoutCancel(ctx), sv.ID, name, gone); err != nil {
		s.removeVolumeQuietly(ctx, *sv, name)
		return jobs.Fail("could not record the data volume", err)
	}
	sv.DataVolume = name
	if gone {
		sv.DataFingerprint = ""
	}
	fmt.Fprintf(log, "created the data volume %s\n", name)
	return nil
}

// removeVolumeQuietly removes a data volume a failed operation created;
// a failure leaves a volume that is not current, which an admin can delete.
func (s *Service) removeVolumeQuietly(ctx context.Context, sv store.Server, name string) {
	if err := s.docker.RemoveDataVolume(context.WithoutCancel(ctx), sv.ID, name); err != nil {
		s.logger.Warn("could not remove a data volume", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "volume", name, "err", err)
	}
}

// helper is a helper container of a running operation.
type helper struct {
	launch string
	id     string
	server int64
}

// assignedBuild returns the server's assigned build when it is a
// successful build with an image. The failures are job failures matching
// ErrNoBuild.
func (s *Service) assignedBuild(ctx context.Context, sv store.Server) (store.Build, error) {
	if sv.BuildID == nil {
		return store.Build{}, jobs.Fail(fmt.Sprintf("the server has no build; assign a successful build with PATCH /api/v1/servers/%d (build_id)", sv.ID), ErrNoBuild)
	}
	b, err := s.store.GetBuild(ctx, *sv.BuildID)
	if err != nil {
		s.logger.Warn("could not read the server's build", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
		return store.Build{}, jobs.Fail("could not read the server's build", ErrNoBuild)
	}
	if b.Status != store.BuildStatusSuccess || b.ImageID == "" {
		return b, jobs.Fail(fmt.Sprintf("build %d is not a successful build; assign another build", b.ID), ErrNoBuild)
	}
	return b, nil
}

// verifyBuildImage verifies build b's image the way a start does. The
// failures are job failures; a missing or unlabelled image matches
// ErrNoBuild.
func (s *Service) verifyBuildImage(ctx context.Context, sv store.Server, b store.Build) (docker.Image, error) {
	img, err := s.docker.BuildImage(ctx, buildRef(b))
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return docker.Image{}, jobs.Fail(fmt.Sprintf("the image of build %d no longer exists; build the project again", b.ID), ErrNoBuild)
	case errors.Is(err, docker.ErrNotManaged), errors.Is(err, docker.ErrNotOurs):
		s.logger.Warn("refusing to use an image without this installation's build labels", "job_id", jobIDFrom(ctx), "server_id", sv.ID,
			"build_id", b.ID, "image", b.ImageID, "err", err)
		return docker.Image{}, jobs.Fail(fmt.Sprintf("the image recorded for build %d does not carry this installation's labels for that build; refusing to use it", b.ID), ErrNoBuild)
	case err != nil:
		return docker.Image{}, s.failDocker(ctx, sv, "inspect the server's image", err)
	}
	if len(img.Volumes) > docker.MaxImageVolumes {
		return docker.Image{}, jobs.Fail(fmt.Sprintf("the server's image declares more than %d volumes", docker.MaxImageVolumes), nil)
	}
	return img, nil
}

// verifiedImage returns the server's assigned build and its verified
// image. The failures are job failures.
func (s *Service) verifiedImage(ctx context.Context, sv store.Server) (store.Build, docker.Image, error) {
	b, err := s.assignedBuild(ctx, sv)
	if err != nil {
		return b, docker.Image{}, err
	}
	img, err := s.verifyBuildImage(ctx, sv, b)
	return b, img, err
}

// helperForgetAfter is how long CleanupHelpers keeps looking for a helper
// whose create may have happened although Docker's reply was lost.
const helperForgetAfter = 10 * time.Minute

// createHelper creates a helper container from a verified build image that
// mounts volume at the data directory. Its launch id is recorded before
// the create and its container id right after, so a crash leaves a record
// that CleanupHelpers resolves. Remove it with removeHelper.
func (s *Service) createHelper(ctx context.Context, sv store.Server, b store.Build, img docker.Image, volume string, readOnly bool) (*helper, error) {
	launch, err := docker.NewLaunchID()
	if err != nil {
		return nil, jobs.Fail("could not prepare a helper container", err)
	}
	s.activeHelpers.Store(launch, true)
	s.knownHelpers.Store(launch, true)
	h := &helper{launch: launch, server: sv.ID}
	if err := s.store.RecordVolumeHelper(ctx, launch, sv.ID); err != nil {
		s.activeHelpers.Delete(launch)
		s.knownHelpers.Delete(launch)
		return nil, jobs.Fail("could not record the helper container", err)
	}
	pid := b.ProjectID
	id, err := s.docker.CreateHelper(ctx, docker.HelperSpec{
		LaunchID: launch, ServerID: sv.ID, ServerName: sv.Name, ProjectID: &pid, BuildID: b.ID, Commit: b.Commit, EngineVersion: b.EngineVersion,
		Image: img.ID, ImageVolumes: img.Volumes, Volume: volume, Target: build.DataDir, ReadOnly: readOnly,
	})
	if err != nil {
		var invalid *docker.ValidationError
		if errors.As(err, &invalid) {
			s.removeHelper(ctx, h) // docker never ran
		} else {
			s.abandonHelper(ctx, h)
		}
		return nil, s.failDocker(ctx, sv, "create a helper container", err)
	}
	h.id = id
	if err := s.store.SetVolumeHelperContainer(context.WithoutCancel(ctx), launch, id); err != nil {
		s.logger.Warn("could not record a helper container id; it is found by its launch id instead", "server_id", sv.ID, "err", err)
	}
	return h, nil
}

// abandonHelper handles a helper whose create failed without saying
// whether Docker created it: a helper found now is removed and forgotten;
// when none is found the record stays, so CleanupHelpers keeps looking for
// helperForgetAfter (a create can still complete after its reply was
// lost).
func (s *Service) abandonHelper(ctx context.Context, h *helper) {
	cctx := context.WithoutCancel(ctx)
	defer s.activeHelpers.Delete(h.launch)
	found, err := s.docker.RemoveHelper(cctx, h.launch, "")
	if err != nil {
		s.logger.Warn("could not remove a helper container after a failed create; it is retried later", "job_id", jobIDFrom(ctx), "server_id", h.server, "err", err)
		return
	}
	if !found {
		return
	}
	if err := s.store.DeleteVolumeHelper(cctx, h.launch); err != nil {
		s.logger.Warn("could not forget a removed helper container", "server_id", h.server, "err", err)
		return
	}
	s.knownHelpers.Delete(h.launch)
}

// removeHelper removes a helper container (also when ctx is cancelled) and
// forgets it. A failure keeps the record, so CleanupHelpers retries.
func (s *Service) removeHelper(ctx context.Context, h *helper) {
	cctx := context.WithoutCancel(ctx)
	defer s.activeHelpers.Delete(h.launch)
	if _, err := s.docker.RemoveHelper(cctx, h.launch, h.id); err != nil {
		s.logger.Warn("could not remove a helper container; it is retried later", "job_id", jobIDFrom(ctx), "server_id", h.server, "err", err)
		return
	}
	if err := s.store.DeleteVolumeHelper(cctx, h.launch); err != nil {
		s.logger.Warn("could not forget a removed helper container", "server_id", h.server, "err", err)
		return
	}
	s.knownHelpers.Delete(h.launch)
}

// CleanupHelpers removes helper containers a crash or a failed removal
// left behind: exactly the containers whose launch id was recorded (by the
// recorded container id, or by the launch label and name when the create's
// reply was lost). A record without a container id whose helper is not
// found stays for helperForgetAfter after it was written, since that
// create may still complete. Helpers of running operations are skipped.
// It is safe to call at any time.
func (s *Service) CleanupHelpers(ctx context.Context) error {
	rows, err := s.store.ListVolumeHelpers(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, h := range rows {
		s.knownHelpers.Store(h.LaunchID, true)
		if _, active := s.activeHelpers.Load(h.LaunchID); active {
			continue
		}
		found, err := s.docker.RemoveHelper(ctx, h.LaunchID, h.ContainerID)
		if errors.Is(err, docker.ErrNotOurs) {
			// The recorded id names something else now: never touch it.
			s.logger.Warn("the container recorded for a helper is not that helper; forgetting the record", "server_id", h.ServerID)
			err = nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !found && h.ContainerID == "" && s.cfg.Now().Sub(h.CreatedAt) < helperForgetAfter {
			continue
		}
		if err := s.store.DeleteVolumeHelper(ctx, h.LaunchID); err != nil {
			errs = append(errs, err)
			continue
		}
		s.knownHelpers.Delete(h.LaunchID)
		if found {
			s.logger.Info("removed a helper container left by an interrupted operation", "server_id", h.ServerID)
		}
	}
	return errors.Join(errs...)
}
