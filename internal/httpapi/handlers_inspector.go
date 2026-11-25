package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/inspector"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// InspectorService backs the Inspector routes (internal/inspector).
type InspectorService interface {
	Status(ctx context.Context) (apitypes.Inspector, error)
	Update(ctx context.Context, req apitypes.UpdateInspectorRequest, by *int64) (store.Job, error)
	CreateSession(ctx context.Context, acct store.Account, serverID *int64) (apitypes.InspectorSession, error)
}

func (s *Server) needInspector() error {
	if s.inspector == nil {
		return errUnavailable
	}
	return nil
}

func inspectorError(err error) error {
	var fe *inspector.FieldError
	switch {
	case errors.As(err, &fe):
		return invalidField(fe.Field, fe.Msg)
	case errors.Is(err, inspector.ErrServerNotFound):
		return notFound("server")
	case errors.Is(err, inspector.ErrNotRunning):
		return conflict(apitypes.ErrorCodeConflict, "the Inspector is not running")
	case errors.Is(err, inspector.ErrUnavailable):
		return conflict(apitypes.ErrorCodeConflict, "the Inspector is unavailable: the daemon runs with --inspector-listen off")
	case errors.Is(err, inspector.ErrTooManyTickets):
		e := newError(http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited, "too many open Inspector links; use one or wait a minute")
		e.header = http.Header{"Retry-After": {"60"}}
		return e
	case errors.Is(err, jobs.ErrSubjectBusy):
		return conflict(apitypes.ErrorCodeJobInProgress, "another job is already running for the Inspector")
	case errors.Is(err, jobs.ErrShuttingDown):
		return errUnavailable
	}
	return internal(err)
}

func (s *Server) handleGetInspector(w http.ResponseWriter, r *http.Request) error {
	if err := s.needInspector(); err != nil {
		return err
	}
	v, err := s.inspector.Status(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

func (s *Server) handleUpdateInspector(w http.ResponseWriter, r *http.Request) error {
	if err := s.needInspector(); err != nil {
		return err
	}
	var req apitypes.UpdateInspectorRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	job, err := s.inspector.Update(r.Context(), req, creatorOf(r))
	if err != nil {
		return inspectorError(err)
	}
	// Enabled is optional: log what was asked, not the pointer.
	enabled := "unchanged"
	if req.Enabled != nil {
		enabled = strconv.FormatBool(*req.Enabled)
	}
	s.logger.Info("inspector apply requested", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID,
		"enabled", enabled, "pull_image", req.PullImage, "rotate_secrets", req.RotateSecrets, "job_id", job.ID)
	writeJSON(w, http.StatusAccepted, toAPIJob(job))
	return nil
}

func (s *Server) handleCreateInspectorSession(w http.ResponseWriter, r *http.Request) error {
	if err := s.needInspector(); err != nil {
		return err
	}
	var req apitypes.CreateInspectorSessionRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			return err
		}
	}
	v, err := s.inspector.CreateSession(r.Context(), identityFrom(r).Account, req.ServerID)
	if err != nil {
		return inspectorError(err)
	}
	writeJSON(w, http.StatusCreated, v)
	return nil
}
