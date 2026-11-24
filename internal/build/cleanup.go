package build

import (
	"context"
	"errors"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// removeImage retries a durable deletion. A remaining successful reference
// discharges this attempt. Deleting that build will enqueue it again.
func (s *Service) removeImage(ctx context.Context, imageID string) {
	n, err := s.o.Store.CountBuildsWithImage(ctx, imageID, 0)
	if err == nil && n == 0 {
		err = s.o.Docker.RemoveImage(ctx, imageID)
	}
	if err == nil || errors.Is(err, docker.ErrNotFound) || errors.Is(err, docker.ErrNotManaged) {
		err = s.o.Store.ClearBuildImageCleanup(ctx, imageID)
	}
	if err != nil {
		detail := safeBuildError(err)
		if errors.Is(err, docker.ErrImageMultipleTags) {
			detail = docker.ErrImageMultipleTags.Error()
		}
		s.o.Logger.Warn("build image cleanup will be retried", "image", imageID, "error", detail)
	}
}

// removeExport removes only a tag owned by this build. Allow a grace period
// for a missing tag because Docker may have started importing the image
// just before its client disconnected.
func (s *Service) removeExport(ctx context.Context, e store.BuildExport) {
	err := s.o.Docker.RemoveBuildOutput(ctx, docker.BuildSpec{
		BuildID: e.BuildID, ProjectID: e.ProjectID, ProjectName: e.ProjectName,
		Commit: e.Commit, EngineVersion: e.EngineVersion,
	})
	switch {
	case err == nil, errors.Is(err, docker.ErrNotManaged), errors.Is(err, docker.ErrNotOurs):
		err = s.o.Store.ClearBuildExport(ctx, e.BuildID)
	case errors.Is(err, docker.ErrNotFound):
		if e.EligibleSince.IsZero() || time.Since(e.EligibleSince) < time.Minute {
			return
		}
		err = s.o.Store.ClearBuildExport(ctx, e.BuildID)
	}
	if err != nil {
		s.o.Logger.Warn("build export cleanup will be retried", "build_id", e.BuildID, "error", safeBuildError(err))
	}
}

// CleanupImages retries removals left by failed exports and deleted builds.
// Run it after startup reconciliation and periodically until shutdown.
func (s *Service) CleanupImages(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, err := s.o.Store.PendingBuildImages(ctx)
	if err != nil {
		s.o.Logger.Warn("could not list deleted build images", "error", safeBuildError(err))
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.removeImage(ctx, id)
	}
	exports, err := s.o.Store.PendingBuildExports(ctx)
	if err != nil {
		s.o.Logger.Warn("could not list unpublished build exports", "error", safeBuildError(err))
	}
	for _, e := range exports {
		if ctx.Err() != nil {
			return
		}
		s.removeExport(ctx, e)
	}
}
