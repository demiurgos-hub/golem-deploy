package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/inspector"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// stubInspector records calls and answers with set values.
type stubInspector struct {
	status    apitypes.Inspector
	updateErr error
	sessErr   error
	lastReq   apitypes.UpdateInspectorRequest
	lastBy    *int64
	lastAcct  int64
	lastSrv   *int64
	job       store.Job
}

func (s *stubInspector) Status(context.Context) (apitypes.Inspector, error) { return s.status, nil }

func (s *stubInspector) Update(_ context.Context, req apitypes.UpdateInspectorRequest, by *int64) (store.Job, error) {
	s.lastReq, s.lastBy = req, by
	return s.job, s.updateErr
}

func (s *stubInspector) CreateSession(_ context.Context, acct store.Account, serverID *int64) (apitypes.InspectorSession, error) {
	s.lastAcct, s.lastSrv = acct.ID, serverID
	if s.sessErr != nil {
		return apitypes.InspectorSession{}, s.sessErr
	}
	return apitypes.InspectorSession{Path: inspector.EnterPath + "?ticket=t", Port: 8444, ExpiresAt: time.Unix(0, 0).UTC()}, nil
}

func TestInspectorRoutes(t *testing.T) {
	h := newHarness(t)
	admin := h.session(h.account("root", store.RoleAdmin, true))
	devAcct := h.account("dev", store.RoleDeveloper, true)
	dev := h.session(devAcct)
	devToken := h.apiToken(devAcct, nil)
	// Without the service the routes answer 503.
	errBody(t, h.do("GET", "/api/v1/inspector", dev, nil), http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)

	stub := &stubInspector{status: apitypes.Inspector{Available: true, State: apitypes.InspectorStateOff, Image: "img"},
		job: store.Job{ID: 5, Kind: string(apitypes.JobKindInspectorApply), SubjectType: "inspector", SubjectID: "hub", Status: store.JobQueued, Restricted: true}}
	h.srv.inspector = stub

	// GET: any developer identity, API tokens included.
	for _, tok := range []string{dev, devToken, admin} {
		rr := h.do("GET", "/api/v1/inspector", tok, nil)
		if rr.Code != 200 {
			t.Fatalf("%d %s", rr.Code, rr.Body)
		}
		if v := decode[apitypes.Inspector](t, rr); !v.Available || v.Image != "img" || v.Targets != nil {
			t.Fatalf("%+v", v)
		}
	}
	errBody(t, h.do("GET", "/api/v1/inspector", "", nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)

	// PATCH: admin sessions only.
	errBody(t, h.do("PATCH", "/api/v1/inspector", dev, `{"enabled":true}`), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	errBody(t, h.do("PATCH", "/api/v1/inspector", devToken, `{"enabled":true}`), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	rr := h.do("PATCH", "/api/v1/inspector", admin, `{"enabled":true,"pull_image":true}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if j := decode[apitypes.Job](t, rr); j.ID != 5 || j.Kind != apitypes.JobKindInspectorApply || j.SubjectType != apitypes.SubjectTypeInspector {
		t.Fatalf("%+v", j)
	}
	if stub.lastReq.Enabled == nil || !*stub.lastReq.Enabled || !stub.lastReq.PullImage || stub.lastBy == nil {
		t.Fatalf("%+v", stub.lastReq)
	}
	errBody(t, h.do("PATCH", "/api/v1/inspector", admin, `{"enable":true}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	for err, want := range map[error]struct {
		status int
		code   apitypes.ErrorCode
	}{
		&inspector.FieldError{Field: "pull_image", Msg: "x"}: {http.StatusBadRequest, apitypes.ErrorCodeValidationFailed},
		jobs.ErrSubjectBusy:  {http.StatusConflict, apitypes.ErrorCodeJobInProgress},
		jobs.ErrShuttingDown: {http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable},
	} {
		stub.updateErr = err
		e := errBody(t, h.do("PATCH", "/api/v1/inspector", admin, `{}`), want.status, want.code)
		if want.code == apitypes.ErrorCodeValidationFailed && e.Field != "pull_image" {
			t.Fatalf("field %q", e.Field)
		}
	}

	// POST sessions: login sessions only; the body may be empty.
	errBody(t, h.do("POST", "/api/v1/inspector/sessions", devToken, nil), http.StatusForbidden, apitypes.ErrorCodeSessionRequired)
	rr = h.do("POST", "/api/v1/inspector/sessions", dev, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	if v := decode[apitypes.InspectorSession](t, rr); v.Port != 8444 || v.Path == "" || stub.lastAcct != devAcct.ID || stub.lastSrv != nil {
		t.Fatalf("%+v", v)
	}
	if rr := h.do("POST", "/api/v1/inspector/sessions", dev, `{"server_id":3}`); rr.Code != http.StatusCreated || stub.lastSrv == nil || *stub.lastSrv != 3 {
		t.Fatalf("%d %v", rr.Code, stub.lastSrv)
	}
	for err, want := range map[error]struct {
		status int
		code   apitypes.ErrorCode
	}{
		inspector.ErrServerNotFound:                         {http.StatusNotFound, apitypes.ErrorCodeNotFound},
		&inspector.FieldError{Field: "server_id", Msg: "x"}: {http.StatusBadRequest, apitypes.ErrorCodeValidationFailed},
		inspector.ErrNotRunning:                             {http.StatusConflict, apitypes.ErrorCodeConflict},
		inspector.ErrTooManyTickets:                         {http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited},
	} {
		stub.sessErr = err
		errBody(t, h.do("POST", "/api/v1/inspector/sessions", dev, `{"server_id":3}`), want.status, want.code)
	}
}
