package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/servers"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Server routes limits.
const (
	// deleteWait bounds how long DELETE /servers/{id} waits for its job
	// (graceful stop plus removal).
	deleteWait = 3 * time.Minute
)

func dockerUnavailable(cause error) *apiError {
	e := newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "Docker is unreachable")
	e.cause = cause
	return e
}

// serversError maps service errors to API errors. what names the resource
// for not_found.
func serversError(what string, err error) error {
	var fe *servers.FieldError
	var ve *docker.ValidationError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &fe):
		return invalidField(fe.Field, fe.Msg)
	case errors.As(err, &ve):
		return invalidField(ve.Field, ve.Msg)
	case errors.Is(err, servers.ErrNotFound):
		return notFound(what)
	case errors.Is(err, servers.ErrNameTaken):
		return conflict(apitypes.ErrorCodeNameTaken, "a server with that name already exists")
	case errors.Is(err, servers.ErrPortInUse):
		return conflict(apitypes.ErrorCodePortInUse, "that port is already assigned to another server (or the port range is full)")
	case errors.Is(err, servers.ErrNoContainer):
		return conflict(apitypes.ErrorCodeConflict, "the server has no container yet; start it first")
	case errors.Is(err, servers.ErrNotRunning):
		return conflict(apitypes.ErrorCodeConflict, "the server is not running")
	case errors.Is(err, servers.ErrRunning):
		return conflict(apitypes.ErrorCodeConflict, "stop the server before changing its port")
	case errors.Is(err, servers.ErrInUse):
		return conflict(apitypes.ErrorCodeConflict, "the "+what+" is in use")
	case errors.Is(err, servers.ErrConflict):
		return conflict(apitypes.ErrorCodeConflict, "the change conflicts with a concurrent change; retry")
	case errors.Is(err, servers.ErrAdminUnavailable):
		e := newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable,
			"the game's admin endpoint did not answer; see the server log")
		e.cause = err
		return e
	case errors.Is(err, docker.ErrUnavailable):
		return dockerUnavailable(err)
	}
	return internal(err)
}

func (s *Server) needServers() error {
	if s.servers == nil {
		return errUnavailable
	}
	return nil
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	list, err := s.servers.List(r.Context())
	if err != nil {
		return serversError("server", err)
	}
	writeJSON(w, http.StatusOK, apitypes.ServerList{Items: list})
	return nil
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	v, err := s.servers.Get(r.Context(), id)
	if err != nil {
		return serversError("server", err)
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	var req apitypes.CreateServerRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	v, err := s.servers.Create(r.Context(), req)
	if err != nil {
		return serversError("server", err)
	}
	s.logger.Info("server created", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "server_id", v.ID)
	if v.DesiredState == apitypes.DesiredStateRunning {
		if _, err := s.startServerJob(r, apitypes.JobKindServerStart, v.ID, s.servers.Start); err != nil {
			s.logger.Warn("could not start new server", "request_id", requestIDFrom(r.Context()), "server_id", v.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusCreated, v)
	return nil
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req apitypes.UpdateServerRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	v, err := s.servers.Update(r.Context(), id, req)
	if err != nil {
		return serversError("server", err)
	}
	s.logger.Info("server updated", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "server_id", id)
	writeJSON(w, http.StatusOK, v)
	return nil
}

type serverJobFunc func(ctx context.Context, id int64, log io.Writer) error

// startServerJob starts fn as a job for server id. The job body gets the
// job id in its context (servers.WithJobID) so Docker failures can be
// correlated with the daemon log.
func (s *Server) startServerJob(r *http.Request, kind apitypes.JobKind, id int64, fn serverJobFunc) (apitypes.Job, error) {
	jobID := make(chan int64, 1)
	job, err := s.startJob(r, kind, apitypes.SubjectTypeServer, id, func(ctx context.Context, log io.Writer) error {
		var jid int64
		select {
		case jid = <-jobID:
		case <-ctx.Done():
		}
		return fn(servers.WithJobID(ctx, jid), id, log)
	})
	if err == nil {
		jobID <- job.ID
	}
	return job, err
}

// serverAction returns a handler that starts a server job and responds 202.
func (s *Server) serverAction(kind apitypes.JobKind, pick func() serverJobFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := s.needServers(); err != nil {
			return err
		}
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		if _, err := s.store.GetServer(r.Context(), id); errors.Is(err, store.ErrNotFound) {
			return notFound("server")
		} else if err != nil {
			return internal(err)
		}
		job, err := s.startServerJob(r, kind, id, pick())
		if err != nil {
			return err
		}
		s.logger.Info("server job started", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "server_id", id, "kind", kind, "job_id", job.ID)
		writeJSON(w, http.StatusAccepted, job)
		return nil
	}
}

func (s *Server) handleStartServer() handlerFunc {
	return s.serverAction(apitypes.JobKindServerStart, func() serverJobFunc { return s.servers.Start })
}

func (s *Server) handleStopServer() handlerFunc {
	return s.serverAction(apitypes.JobKindServerStop, func() serverJobFunc { return s.servers.Stop })
}

func (s *Server) handleRestartServer() handlerFunc {
	return s.serverAction(apitypes.JobKindServerRestart, func() serverJobFunc { return s.servers.Restart })
}

// handleDeleteServer runs the deletion as a server.delete job (so it is
// serialised with start/stop jobs and logged) and waits for it, answering
// 204 once the container and the row are gone.
func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if _, err := s.store.GetServer(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		return notFound("server")
	} else if err != nil {
		return internal(err)
	}
	var delErr error
	job, err := s.startServerJob(r, apitypes.JobKindServerDelete, id, func(ctx context.Context, id int64, log io.Writer) error {
		delErr = s.servers.Delete(ctx, id, log)
		return delErr
	})
	if err != nil {
		return err
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(deleteWait + 10*time.Second))
	timer := time.NewTimer(deleteWait)
	defer timer.Stop()
	select {
	case <-s.jobs.Done(job.ID):
	case <-r.Context().Done():
		return nil // client gone; the job finishes on its own
	case <-s.shutdown:
		return errUnavailable
	case <-timer.C:
		e := newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "deleting the server is taking too long; check job "+strconv.FormatInt(job.ID, 10))
		return e
	}
	final, err := s.store.GetJob(context.WithoutCancel(r.Context()), job.ID)
	if err != nil {
		return internal(err)
	}
	if final.Status == store.JobSuccess {
		s.logger.Info("server deleted", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "server_id", id, "job_id", job.ID)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	switch {
	case errors.Is(delErr, docker.ErrUnavailable):
		return dockerUnavailable(delErr)
	case errors.Is(delErr, servers.ErrNotFound):
		return notFound("server")
	case delErr == nil:
		return conflict(apitypes.ErrorCodeConflict, "the deletion was cancelled")
	}
	e := newError(http.StatusInternalServerError, apitypes.ErrorCodeInternal, "deleting the server failed; see the log of job "+strconv.FormatInt(job.ID, 10))
	e.cause = delErr
	return e
}

// streamWriter writes a streaming text response, extending the write
// deadline per chunk and flushing when follow is set.
type streamWriter struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	follow bool
}

func (sw *streamWriter) Write(p []byte) (int, error) {
	_ = sw.rc.SetWriteDeadline(time.Now().Add(streamWriteWindow))
	n, err := sw.w.Write(p)
	if err == nil && sw.follow {
		_ = sw.rc.Flush()
	}
	return n, err
}

// handleServerLogs returns the container's output as text. tail selects
// the trailing lines (1-5000, default 200); follow=1 keeps streaming until
// the container stops or is removed, the client disconnects or the daemon
// shuts down.
func (s *Server) handleServerLogs(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	opts := docker.LogOptions{Tail: docker.DefaultLogTail}
	if v := q.Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > docker.MaxLogTail {
			return badRequest("tail must be an integer from 1 to " + strconv.Itoa(docker.MaxLogTail))
		}
		opts.Tail = n
	}
	switch q.Get("follow") {
	case "", "0", "false":
	case "1", "true":
		opts.Follow = true
	default:
		return badRequest("follow must be 1 or 0")
	}
	if err := s.servers.CheckLogs(r.Context(), id); err != nil {
		return serversError("server", err)
	}
	ctx := r.Context()
	if opts.Follow {
		release, err := s.acquireLogStream(r)
		if err != nil {
			return err
		}
		defer release()
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-s.shutdown:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if opts.Follow {
		_ = rc.Flush()
	}
	sw := &streamWriter{w: w, rc: rc, follow: opts.Follow}
	if err := s.servers.Logs(ctx, id, opts, sw); err != nil && ctx.Err() == nil {
		s.logger.Warn("server logs ended with an error", "request_id", requestIDFrom(r.Context()), "server_id", id, "err", err)
	}
	return nil
}

func (s *Server) handleServerStats(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	st, err := s.servers.Stats(r.Context(), id, s.now())
	if err != nil {
		return serversError("server", err)
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

func (s *Server) handleServerRuntime(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	rt, err := s.servers.Runtime(r.Context(), id, s.now())
	if err != nil {
		return serversError("server", err)
	}
	writeJSON(w, http.StatusOK, rt)
	return nil
}

func (s *Server) handleExportServers(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	doc, err := s.servers.Export(r.Context())
	if err != nil {
		return serversError("server", err)
	}
	s.logger.Info("servers exported", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "count", len(doc.Servers))
	writeJSON(w, http.StatusOK, doc)
	return nil
}

func (s *Server) handleImportServers(w http.ResponseWriter, r *http.Request) error {
	if err := s.needServers(); err != nil {
		return err
	}
	// The body may be large (servers.MaxImportBytes): bound it by
	// inactivity, not total time.
	r.Body = io.NopCloser(&deadlineReader{r: r.Body, rc: http.NewResponseController(w)})
	var doc apitypes.ServerExport
	if err := decodeJSON(r, &doc); err != nil {
		return err
	}
	created, err := s.servers.Import(r.Context(), doc)
	if err != nil {
		return serversError("server", err)
	}
	s.logger.Info("servers imported", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID, "count", len(created))
	writeJSON(w, http.StatusCreated, apitypes.ServerImportResult{Created: created})
	return nil
}
