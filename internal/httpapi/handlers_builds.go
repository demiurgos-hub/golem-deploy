package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func toAPIBuild(b store.Build, used map[int64][]int64) apitypes.Build {
	ids := used[b.ID]
	if ids == nil {
		ids = []int64{}
	}
	return apitypes.Build{
		ID:                b.ID,
		ProjectID:         b.ProjectID,
		Branch:            b.Branch,
		Commit:            b.Commit,
		Status:            apitypes.BuildStatus(b.Status),
		Detail:            b.Detail,
		JobID:             b.JobID,
		ImageID:           b.ImageID,
		ImageTag:          b.ImageTag,
		EngineVersion:     b.EngineVersion,
		SchemaFingerprint: b.SchemaFingerprint,
		WebTransportPath:  b.WebTransportPath,
		WebSocketPath:     b.WebSocketPath,
		ServerIDs:         ids,
		CreatedBy:         b.CreatedBy,
		CreatedAt:         b.CreatedAt.UTC(),
		StartedAt:         utcPtr(b.StartedAt),
		FinishedAt:        utcPtr(b.FinishedAt),
	}
}

func (s *Server) needBuilds() error {
	if s.builds == nil {
		return newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "builds are not available on this host")
	}
	return nil
}

// handleCreateBuild queues a build of the project's current checkout (or,
// with pull, of the checkout after a pull in the same job). The job's
// subject is the project, so a build never overlaps a clone, pull or
// branch switch of the same project.
func (s *Server) handleCreateBuild(w http.ResponseWriter, r *http.Request) error {
	if err := s.needBuilds(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req apitypes.CreateBuildRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			return err
		}
	}
	if req.Pull && s.proj.git == nil {
		return newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "git is not available on the host")
	}
	s.proj.mu.Lock()
	defer s.proj.mu.Unlock()
	p, err := s.store.GetProject(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("project")
	}
	if err != nil {
		return internal(err)
	}
	clone := s.needsClone(p)
	if clone && !req.Pull {
		return conflict(apitypes.ErrorCodeConflict, build.DetailNoCheckout)
	}
	if _, busy := s.jobs.ActiveFor(jobs.Subject{Type: string(apitypes.SubjectTypeProject), ID: jobs.SubjectID(p.ID)}); busy {
		return conflict(apitypes.ErrorCodeJobInProgress, "another job is already running for this project")
	}
	release, err := s.builds.Reserve()
	if errors.Is(err, build.ErrTooManyBuilds) {
		return conflict(apitypes.ErrorCodeConflict, fmt.Sprintf("too many builds are queued (at most %d); wait for one to finish", build.MaxActiveBuilds))
	}
	if err != nil {
		return internal(err)
	}
	createdBy := identityFrom(r).Account.ID
	b, err := s.store.CreateBuild(r.Context(), store.NewBuild{ProjectID: p.ID, Branch: p.Branch, CreatedBy: &createdBy})
	if err != nil {
		release()
		return internal(err)
	}
	var prepare jobs.Func
	if req.Pull {
		op := opPull
		if clone {
			op = opClone
		}
		prepare = s.projectJob(p.ID, op, p.Branch)
	}
	buildID := b.ID
	job, err := s.startJob(r, apitypes.JobKindProjectBuild, apitypes.SubjectTypeProject, p.ID, func(ctx context.Context, log io.Writer) error {
		return s.builds.Run(ctx, buildID, log, prepare)
	}, jobs.InPool(build.JobPool))
	if err != nil {
		release()
		if derr := s.store.DeleteBuild(context.WithoutCancel(r.Context()), b.ID); derr != nil {
			s.logger.Error("remove unstarted build failed", "build_id", b.ID, "err", derr)
		}
		return err
	}
	if err := s.store.SetBuildJob(context.WithoutCancel(r.Context()), b.ID, job.ID); err != nil {
		s.logger.Error("record build job failed", "build_id", b.ID, "job_id", job.ID, "err", err)
	}
	s.builds.Watch(b.ID, s.jobs.Done(job.ID), release)
	jobID := job.ID
	b.JobID = &jobID
	s.logger.Info("build queued", "request_id", requestIDFrom(r.Context()), "by", createdBy, "project_id", p.ID, "build_id", b.ID, "job_id", job.ID, "pull", req.Pull)
	writeJSON(w, http.StatusAccepted, apitypes.CreateBuildResponse{Build: toAPIBuild(b, nil), Job: job})
	return nil
}

// buildFilter parses the status and limit query parameters (and
// project_id when allowProject).
func buildFilter(r *http.Request, allowProject bool) (store.BuildFilter, error) {
	q := r.URL.Query()
	f := store.BuildFilter{Limit: 50}
	if v := q.Get("status"); v != "" {
		switch apitypes.BuildStatus(v) {
		case apitypes.BuildStatusQueued, apitypes.BuildStatusRunning, apitypes.BuildStatusSuccess,
			apitypes.BuildStatusError, apitypes.BuildStatusCancelled:
			f.Status = v
		default:
			return f, badRequest("status must be queued, running, success, error or cancelled")
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return f, badRequest("limit must be an integer from 1 to 500")
		}
		f.Limit = n
	}
	if v := q.Get("project_id"); v != "" && allowProject {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return f, badRequest("project_id must be a positive integer")
		}
		f.ProjectID = &id
	}
	return f, nil
}

func (s *Server) writeBuildList(w http.ResponseWriter, r *http.Request, f store.BuildFilter) error {
	list, err := s.store.ListBuilds(r.Context(), f)
	if err != nil {
		return internal(err)
	}
	used, err := s.store.ServerIDsByBuild(r.Context())
	if err != nil {
		return internal(err)
	}
	out := apitypes.BuildList{Items: make([]apitypes.Build, 0, len(list))}
	for _, b := range list {
		out.Items = append(out.Items, toAPIBuild(b, used))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleListProjectBuilds(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r)
	if err != nil {
		return err
	}
	f, err := buildFilter(r, false)
	if err != nil {
		return err
	}
	f.ProjectID = &p.ID
	return s.writeBuildList(w, r, f)
}

func (s *Server) handleListBuilds(w http.ResponseWriter, r *http.Request) error {
	f, err := buildFilter(r, true)
	if err != nil {
		return err
	}
	return s.writeBuildList(w, r, f)
}

func (s *Server) handleGetBuild(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	b, err := s.store.GetBuild(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("build")
	}
	if err != nil {
		return internal(err)
	}
	ids, err := s.store.BuildServerIDs(r.Context(), id)
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, toAPIBuild(b, map[int64][]int64{b.ID: ids}))
	return nil
}

func (s *Server) handleDeleteBuild(w http.ResponseWriter, r *http.Request) error {
	if err := s.needBuilds(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	err = s.builds.Delete(r.Context(), id)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return notFound("build")
	case errors.Is(err, store.ErrActive):
		return conflict(apitypes.ErrorCodeConflict, "the build is still queued or running; cancel its job first")
	case errors.Is(err, store.ErrInUse):
		return conflict(apitypes.ErrorCodeConflict, "servers are configured with this build; assign them another build or delete them first")
	case errors.Is(err, build.ErrImageInUse):
		return imageInUseConflict(err, "this build's image")
	case errors.Is(err, docker.ErrUnavailable):
		return dockerUnavailable(err)
	default:
		return internal(err)
	}
	s.logger.Info("build deleted", "request_id", requestIDFrom(r.Context()), "by", identityFrom(r).Account.ID, "build_id", id)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// imageInUseConflict is the 409 for a build image that containers still
// use (*build.ImageInUseError), naming the servers whose container it is.
func imageInUseConflict(err error, what string) error {
	var e *build.ImageInUseError
	if !errors.As(err, &e) || len(e.ServerIDs) == 0 {
		return conflict(apitypes.ErrorCodeConflict, "a container that is not a server's uses "+what+"; remove that container first")
	}
	return conflict(apitypes.ErrorCodeConflict, "the container of server "+joinIDs(e.ServerIDs)+" still uses "+what+
		" (a server keeps its container, also when stopped, until it is started again or deleted); restart or delete those servers first")
}

func joinIDs(ids []int64) string {
	strs := make([]string, 0, len(ids))
	for _, id := range ids {
		strs = append(strs, strconv.FormatInt(id, 10))
	}
	return strings.Join(strs, ", ")
}

// projectBuildsInUse returns a 409 when servers are configured with builds
// of project id.
func (s *Server) projectBuildsInUse(ctx context.Context, id int64) error {
	ids, err := s.store.ProjectBuildServerIDs(ctx, id)
	if err != nil {
		return internal(err)
	}
	if len(ids) == 0 {
		return nil
	}
	return conflict(apitypes.ErrorCodeConflict, "servers are configured with builds of this project (server ids "+joinIDs(ids)+"); assign them another build or delete them first")
}

// removeProjectImages deletes the images of a deleted project's builds in
// the background of the request (bounded); failures are logged.
func (s *Server) removeProjectImages(builds []store.Build) {
	if s.builds == nil || len(builds) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.builds.RemoveImages(ctx, builds)
}
