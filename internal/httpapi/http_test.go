package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/api", "/api/", "/api/v1", "/api/v1/nope", "/api/v2/status", "/api/v1/status/extra"} {
		rr := h.do("GET", p, "", nil)
		errBody(t, rr, http.StatusNotFound, apitypes.ErrorCodeNotFound)
		if strings.Contains(rr.Body.String(), "<html") {
			t.Fatalf("%s served HTML", p)
		}
	}
}

func TestWrongMethodIsJSON405(t *testing.T) {
	h := newHarness(t)
	rr := h.do("DELETE", "/api/v1/status", "", nil)
	errBody(t, rr, http.StatusMethodNotAllowed, apitypes.ErrorCodeMethodNotAllowed)
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow %q", allow)
	}
}

func TestNonAPIPathsServeSPA(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/", "/servers", "/apix", "/login"} {
		rr := h.do("GET", p, "", nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "spa") {
			t.Fatalf("%s: %d %q", p, rr.Code, rr.Body.String())
		}
	}
}

func TestRequestDecoding(t *testing.T) {
	h := newHarness(t)
	big := `{"username":"` + strings.Repeat("a", defaultBodyLimit) + `","password":"x"}`
	errBody(t, h.do("POST", "/api/v1/auth/login", "", big), http.StatusRequestEntityTooLarge, apitypes.ErrorCodeRequestTooLarge)

	e := errBody(t, h.do("POST", "/api/v1/auth/login", "", `{"username":"a","password":"b","admin":true}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	if e.Field != "admin" {
		t.Fatalf("unknown field name %q", e.Field)
	}
	errBody(t, h.do("POST", "/api/v1/auth/login", "", `{"username":"a","password":"b"} {}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	errBody(t, h.do("POST", "/api/v1/auth/login", "", `{"username":"a",`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	errBody(t, h.do("POST", "/api/v1/auth/login", "", `[]`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	if e := errBody(t, h.do("POST", "/api/v1/auth/login", "", `{"username":5}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest); e.Field != "username" {
		t.Fatalf("type error field %q", e.Field)
	}

	req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"a","password":"b"}`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rr, req)
	errBody(t, rr, http.StatusUnsupportedMediaType, apitypes.ErrorCodeUnsupportedMediaType)

	req = httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rr = httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rr, req)
	errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
}

// Finding 7: internal errors never reach the client.
func TestErrorEnvelopeHidesInternalErrors(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	tok := h.session(admin)
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rr := range []*httptest.ResponseRecorder{
		h.do("GET", "/api/v1/accounts", tok, nil),
		h.do("POST", "/api/v1/auth/login", "", apitypes.LoginRequest{Username: "admin1", Password: testPassword}),
	} {
		e := errBody(t, rr, http.StatusInternalServerError, apitypes.ErrorCodeInternal)
		body := strings.ToLower(rr.Body.String())
		for _, leak := range []string{"sql", "database", "closed", "store:", ".db"} {
			if strings.Contains(body, leak) {
				t.Fatalf("response leaks %q: %s", leak, body)
			}
		}
		if e.Message != "internal server error" {
			t.Fatalf("message %q", e.Message)
		}
	}
}

func TestPanicRecovery(t *testing.T) {
	h := newHarness(t)
	handler := h.srv.outer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret panic detail")
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/x", nil))
	errBody(t, rr, http.StatusInternalServerError, apitypes.ErrorCodeInternal)
	if strings.Contains(rr.Body.String(), "secret") {
		t.Fatal("panic value leaked")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	check := func(rr *httptest.ResponseRecorder, csp string) {
		t.Helper()
		want := map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"Content-Security-Policy": csp,
		}
		for k, v := range want {
			if got := rr.Header().Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		if rr.Header().Get("X-Request-Id") == "" {
			t.Error("missing X-Request-Id")
		}
		if rr.Header().Get("Strict-Transport-Security") != "" {
			t.Error("HSTS sent without TLS")
		}
	}
	api := h.do("GET", "/api/v1/nope", "", nil)
	check(api, cspAPI)
	if api.Header().Get("Cache-Control") != "no-store" {
		t.Error("API responses must not be cached")
	}
	check(h.do("GET", "/", "", nil), cspSPA)

	tlsH := newHarness(t, func(c *Config) { c.TLSCert, c.TLSKey = "cert.pem", "key.pem" })
	if v := tlsH.do("GET", "/", "", nil).Header().Get("Strict-Transport-Security"); !strings.HasPrefix(v, "max-age=") {
		t.Fatalf("HSTS with TLS = %q", v)
	}
}

func TestCheckListen(t *testing.T) {
	tests := []struct {
		addr          string
		tls, insecure bool
		ok            bool
	}{
		{"127.0.0.1:8000", false, false, true},
		{"127.10.0.1:8000", false, false, true},
		{"[::1]:8000", false, false, true},
		{"localhost:8000", false, false, true},
		{"0.0.0.0:8000", false, false, false},
		{":8000", false, false, false},
		{"192.168.1.5:8000", false, false, false},
		{"[::]:8000", false, false, false},
		{"example.com:8000", false, false, false},
		{"0.0.0.0:8000", true, false, true},
		{"0.0.0.0:8000", false, true, true},
		{"127.0.0.1", false, false, false},
		{"127.0.0.1:", false, false, false},
	}
	for _, tt := range tests {
		err := CheckListen(tt.addr, tt.tls, tt.insecure)
		if (err == nil) != tt.ok {
			t.Errorf("CheckListen(%q, tls=%v, insecure=%v) = %v", tt.addr, tt.tls, tt.insecure, err)
		}
	}
	if err := CheckListen("0.0.0.0:8000", false, false); !errors.Is(err, ErrPlaintextNonLoopback) {
		t.Fatalf("want ErrPlaintextNonLoopback, got %v", err)
	}
}

func TestListenRefusesPlaintextNonLoopback(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Listen = "0.0.0.0:0" })
	if _, err := h.srv.Listen(); !errors.Is(err, ErrPlaintextNonLoopback) {
		t.Fatalf("got %v", err)
	}
	h2 := newHarness(t, func(c *Config) { c.Listen = "127.0.0.1:0"; c.TLSCert = "only-cert.pem" })
	if _, err := h2.srv.Listen(); err == nil {
		t.Fatal("cert without key accepted")
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	h := newHarness(t)
	ln, err := h.srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.srv.Serve(ctx, ln) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/v1/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("still accepting connections")
	}
}

func TestAccountAdministration(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	tok := h.session(admin)

	rr := h.do("POST", "/api/v1/accounts", tok, apitypes.CreateAccountRequest{Username: "Bob", Password: testPassword, Role: apitypes.RoleDeveloper, Approved: true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	bob := decode[apitypes.Account](t, rr)
	if bob.Username != "bob" || !bob.Approved || bob.Role != apitypes.RoleDeveloper {
		t.Fatalf("bob %+v", bob)
	}
	errBody(t, h.do("POST", "/api/v1/accounts", tok, apitypes.CreateAccountRequest{Username: "bob", Password: testPassword, Role: apitypes.RoleDeveloper}), http.StatusConflict, apitypes.ErrorCodeUsernameTaken)
	if e := errBody(t, h.do("POST", "/api/v1/accounts", tok, apitypes.CreateAccountRequest{Username: "carol", Password: testPassword, Role: "root"}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "role" {
		t.Fatalf("field %q", e.Field)
	}

	list := decode[apitypes.AccountList](t, h.do("GET", "/api/v1/accounts", tok, nil))
	if len(list.Items) != 2 {
		t.Fatalf("list %+v", list)
	}
	if strings.Contains(h.do("GET", "/api/v1/accounts", tok, nil).Body.String(), "$2a$") {
		t.Fatal("password hash exposed")
	}

	self := "/api/v1/accounts/" + strconv.FormatInt(admin.ID, 10)
	bobPath := "/api/v1/accounts/" + strconv.FormatInt(bob.ID, 10)
	errBody(t, h.do("DELETE", self, tok, nil), http.StatusConflict, apitypes.ErrorCodeCannotModifySelf)
	errBody(t, h.do("PATCH", self, tok, apitypes.UpdateAccountRequest{Role: ptr(apitypes.RoleDeveloper)}), http.StatusConflict, apitypes.ErrorCodeCannotModifySelf)
	errBody(t, h.do("PATCH", self, tok, apitypes.UpdateAccountRequest{Approved: ptr(false)}), http.StatusConflict, apitypes.ErrorCodeCannotModifySelf)
	errBody(t, h.do("PATCH", "/api/v1/accounts/99999", tok, apitypes.UpdateAccountRequest{Approved: ptr(true)}), http.StatusNotFound, apitypes.ErrorCodeNotFound)

	// Promote bob; bob (fresh login) demotes admin1; admin1 cannot then demote
	// the last admin.
	if rr := h.do("PATCH", bobPath, tok, apitypes.UpdateAccountRequest{Role: ptr(apitypes.RoleAdmin)}); rr.Code != http.StatusOK {
		t.Fatalf("promote: %d %s", rr.Code, rr.Body.String())
	}
	bobAcct, _ := h.st.GetAccount(context.Background(), bob.ID)
	bobTok := h.session(bobAcct)
	if rr := h.do("PATCH", self, bobTok, apitypes.UpdateAccountRequest{Role: ptr(apitypes.RoleDeveloper)}); rr.Code != http.StatusOK {
		t.Fatalf("demote admin1: %d %s", rr.Code, rr.Body.String())
	}
	errBody(t, h.do("PATCH", bobPath, bobTok, apitypes.UpdateAccountRequest{Role: ptr(apitypes.RoleDeveloper)}), http.StatusConflict, apitypes.ErrorCodeCannotModifySelf)
	// Unapproving a developer and deleting work.
	if rr := h.do("PATCH", self, bobTok, apitypes.UpdateAccountRequest{Approved: ptr(false)}); rr.Code != http.StatusOK {
		t.Fatalf("unapprove: %d", rr.Code)
	}
	if rr := h.do("DELETE", self, bobTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	errBody(t, h.do("DELETE", self, bobTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
}

func TestLastAdminViaHTTP(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	// A second admin that is unapproved does not count.
	h.account("admin2", store.RoleAdmin, false)
	// Only the CLI (actor 0) could hit the last-admin rule for admin1 since
	// the self rule fires first over HTTP; exercise it through the store.
	if _, err := h.st.UpdateAccount(context.Background(), 0, admin.ID, store.AccountUpdate{Role: ptr(store.RoleDeveloper)}); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("got %v", err)
	}
	if err := accountChangeError(store.ErrLastAdmin); err.(*apiError).code != apitypes.ErrorCodeLastAdmin {
		t.Fatal("mapping")
	}
}

func TestStatusAndSettings(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	h.account("pending", store.RoleDeveloper, false)
	tok := h.session(admin)
	st := decode[apitypes.Status](t, h.do("GET", "/api/v1/status", tok, nil))
	if st.Version != "test" || !st.DockerReachable || st.RegistrationEnabled || st.Counts.PendingAccounts != 1 {
		t.Fatalf("status %+v", st)
	}
	if s := decode[apitypes.Settings](t, h.do("GET", "/api/v1/settings", tok, nil)); s.RegistrationEnabled {
		t.Fatal("registration enabled by default")
	}
	// The public auth options follow the setting and reveal nothing else.
	opts := h.do("GET", "/api/v1/auth/options", "", nil)
	if opts.Code != http.StatusOK || strings.TrimSpace(opts.Body.String()) != `{"registration_enabled":false}` {
		t.Fatalf("auth options %d %s", opts.Code, opts.Body.String())
	}
	if s := decode[apitypes.Settings](t, h.do("PATCH", "/api/v1/settings", tok, `{"registration_enabled":true}`)); !s.RegistrationEnabled {
		t.Fatal("enable registration")
	}
	if o := decode[apitypes.AuthOptions](t, h.do("GET", "/api/v1/auth/options", "", nil)); !o.RegistrationEnabled {
		t.Fatal("auth options did not follow the setting")
	}
	if s := decode[apitypes.Settings](t, h.do("PATCH", "/api/v1/settings", tok, `{"registration_enabled":false}`)); s.RegistrationEnabled {
		t.Fatal("disable registration")
	}
	errBody(t, h.do("PATCH", "/api/v1/settings", tok, `{"registration_enabled":"yes"}`), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	if s := decode[apitypes.Settings](t, h.do("PATCH", "/api/v1/settings", tok, `{}`)); s.RegistrationEnabled {
		t.Fatal("empty patch changed settings")
	}
}

func TestJobEndpoints(t *testing.T) {
	h := newHarness(t)
	dev := h.account("dev1", store.RoleDeveloper, true)
	tok := h.apiToken(dev, nil)

	release := make(chan struct{})
	ctx := context.WithValue(context.Background(), identityKey, &Identity{Account: dev, Kind: KindAPIToken})
	ctx = context.WithValue(ctx, routeAccessKey, AccessDeveloper)
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	job, err := h.srv.startJob(req, apitypes.JobKindProjectPull, apitypes.SubjectTypeProject, 7, func(ctx context.Context, log io.Writer) error {
		fmt.Fprintln(log, "step one")
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		fmt.Fprintln(log, "step two")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.CreatedBy == nil || *job.CreatedBy != dev.ID || job.CreatedByUsername == nil || *job.CreatedByUsername != "dev1" || job.SubjectID != "7" || job.Kind != apitypes.JobKindProjectPull || job.Restricted {
		t.Fatalf("job %+v", job)
	}
	_, err = h.srv.startJob(req, apitypes.JobKindProjectPull, apitypes.SubjectTypeProject, 7, func(context.Context, io.Writer) error { return nil })
	var ae *apiError
	if !errors.As(err, &ae) || ae.code != apitypes.ErrorCodeJobInProgress || ae.status != http.StatusConflict {
		t.Fatalf("second job: %v", err)
	}

	jobPath := "/api/v1/jobs/" + strconv.FormatInt(job.ID, 10)
	got := decode[apitypes.Job](t, h.do("GET", jobPath, tok, nil))
	if got.ID != job.ID {
		t.Fatalf("get %+v", got)
	}
	list := decode[apitypes.JobList](t, h.do("GET", "/api/v1/jobs?subject_type=project&subject_id=7&limit=10", tok, nil))
	if len(list.Items) != 1 || list.Items[0].CreatedByUsername == nil || *list.Items[0].CreatedByUsername != "dev1" {
		t.Fatalf("list %+v", list)
	}
	errBody(t, h.do("GET", "/api/v1/jobs?limit=0", tok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
	errBody(t, h.do("GET", "/api/v1/jobs/123456", tok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)

	// Follow the log over a real connection; the stream must end on its own
	// once the job finishes.
	ts := httptest.NewServer(h.srv.Handler())
	defer ts.Close()
	streamReq, _ := http.NewRequest("GET", ts.URL+jobPath+"/log?follow=1", nil)
	streamReq.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	firstLine := make([]byte, len("step one\n"))
	if _, err := io.ReadFull(resp.Body, firstLine); err != nil || string(firstLine) != "step one\n" {
		t.Fatalf("first chunk %q, %v", firstLine, err)
	}
	close(release)
	rest := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		rest <- string(b)
	}()
	select {
	case r := <-rest:
		if !strings.Contains(r, "step two") {
			t.Fatalf("rest of stream %q", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("log stream did not end after the job finished")
	}

	<-h.runner.Done(job.ID)
	done := decode[apitypes.Job](t, h.do("GET", jobPath, tok, nil))
	if done.Status != apitypes.JobStatusSuccess || done.FinishedAt == nil {
		t.Fatalf("finished job %+v", done)
	}
	full := h.do("GET", jobPath+"/log", tok, nil)
	if full.Code != http.StatusOK || full.Body.String() != "step one\nstep two\n" {
		t.Fatalf("full log %d %q", full.Code, full.Body.String())
	}
	errBody(t, h.do("POST", jobPath+"/cancel", tok, nil), http.StatusConflict, apitypes.ErrorCodeJobNotActive)
	errBody(t, h.do("GET", jobPath+"/log?follow=maybe", tok, nil), http.StatusBadRequest, apitypes.ErrorCodeBadRequest)
}

func TestCancelJobViaHTTP(t *testing.T) {
	h := newHarness(t)
	dev := h.account("dev1", store.RoleDeveloper, true)
	admin := h.account("admin1", store.RoleAdmin, true)
	tok := h.session(dev)
	req := httptest.NewRequest("POST", "/", nil)
	started := make(chan struct{})
	job, err := h.srv.startJob(req, apitypes.JobKindServerStart, apitypes.SubjectTypeServer, 1, func(ctx context.Context, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.CreatedBy != nil || job.CreatedByUsername != nil || !job.Restricted {
		t.Fatalf("system job %+v: want no creator and restricted", job)
	}
	<-started
	// A system job is restricted: developers cannot cancel it, admins can.
	errBody(t, h.do("POST", "/api/v1/jobs/"+strconv.FormatInt(job.ID, 10)+"/cancel", tok, nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	rr := h.do("POST", "/api/v1/jobs/"+strconv.FormatInt(job.ID, 10)+"/cancel", h.session(admin), nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	<-h.runner.Done(job.ID)
	got := decode[apitypes.Job](t, h.do("GET", "/api/v1/jobs/"+strconv.FormatInt(job.ID, 10), tok, nil))
	if got.Status != apitypes.JobStatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
}
