package httpapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
	"github.com/mholt/acmez/v3/acme"
)

type fakeACMEClient struct {
	chain, key string
	issue      func(context.Context, string, bool) error
	http       func(http.ResponseWriter, *http.Request) bool
}

func (f *fakeACMEClient) Load(_ context.Context, _ certs.ACMEAccount, _ string) ([]byte, []byte, error) {
	if f.chain == "" {
		return nil, nil, fs.ErrNotExist
	}
	return []byte(f.chain), []byte(f.key), nil
}
func (f *fakeACMEClient) Issue(ctx context.Context, _ certs.ACMEAccount, host string, force bool, _ io.Writer) error {
	return f.issue(ctx, host, force)
}
func (*fakeACMEClient) RenewalInfo(context.Context, certs.ACMEAccount, string, *x509.Certificate) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, nil
}
func (f *fakeACMEClient) HTTPChallenge(w http.ResponseWriter, r *http.Request) bool {
	return f.http != nil && f.http(w, r)
}
func (*fakeACMEClient) TLSChallenge(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }

func setupFakeACME(t *testing.T, h *dockerHarness, client certs.ACMEClient, override string, http01 bool) {
	t.Helper()
	h.srv.automation = certs.NewAutomation(certs.AutomationOptions{
		Game: h.certs, GameHost: h.svc.GameHost, PanelMode: apitypes.PanelCertificateModeSelfSigned,
		PanelDir: filepath.Join(t.TempDir(), "panel"), Client: client, Directory: override, HTTP01: http01, Jobs: h.runner,
	})
	if err := h.srv.automation.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCertificateJobsCommitAfterIssuance(t *testing.T) {
	h := newDockerHarness(t)
	ctx := t.Context()
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeACMEClient{}
	fake.issue = func(ctx context.Context, host string, force bool) error {
		if force {
			return acme.Problem{Type: acme.ProblemTypeConnection, Detail: "CA could not connect\n\x1b" + strings.Repeat("a", 400)}
		}
		close(started)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	setupFakeACME(t, h, fake, "", true)
	const game = "/api/v1/certificates/game"
	const panel = "/api/v1/certificates/panel"
	const settings = "/api/v1/certificates/acme"
	req := apitypes.UpdateGameCertificateRequest{Mode: "acme"}
	errBody(t, h.call("PUT", game, h.adminSs, req), 400, apitypes.ErrorCodeValidationFailed)
	errBody(t, h.call("POST", settings+"/renew", h.adminSs, nil), 409, apitypes.ErrorCodeConflict)
	errBody(t, h.call("PUT", settings, h.adminSs, apitypes.UpdateACMERequest{Email: ptr("Display <test@example.com>"), TermsAgreed: ptr(true)}), 400, apitypes.ErrorCodeValidationFailed)
	before := decode[apitypes.ACME](t, h.call("GET", settings, h.adminSs, nil))
	if before.TermsAgreed {
		t.Fatal("invalid update saved terms")
	}
	account := decode[apitypes.ACME](t, h.call("PUT", settings, h.adminSs, apitypes.UpdateACMERequest{Email: ptr("admin@example.com"), TermsAgreed: ptr(true)}))
	if !account.TermsAgreed || account.Email != "admin@example.com" {
		t.Fatalf("%+v", account)
	}

	r := h.call("PUT", game, h.adminSs, req)
	resp := decode[apitypes.UpdateGameCertificateResponse](t, r)
	if r.Code != 202 || resp.Job == nil || resp.Job.Kind != "certificate.issue" || resp.Job.SubjectID != "acme" ||
		resp.Job.SubjectType != "certificate" || resp.Certificate.Mode != "self_signed" {
		t.Fatalf("%d %+v", r.Code, resp)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("certificate job did not start")
	}
	if pending := decode[apitypes.GameCertificate](t, h.call("GET", game, h.adminSs, nil)); pending.Mode != "self_signed" {
		t.Fatal("mode changed while CA was still issuing")
	}
	errBody(t, h.call("PUT", panel, h.adminSs, apitypes.UpdatePanelCertificateRequest{Mode: "acme"}), 409, apitypes.ErrorCodeJobInProgress)
	errBody(t, h.call("GET", "/api/v1/jobs/"+strconv.FormatInt(resp.Job.ID, 10)+"/log", h.devTok, nil), 403, apitypes.ErrorCodeForbidden)
	fake.chain, fake.key = testCertPEM(t, "play.example.com", time.Now().Add(time.Hour))
	close(release)
	wait := func(job *apitypes.Job) apitypes.Job {
		t.Helper()
		select {
		case <-h.runner.Done(job.ID):
		case <-time.After(5 * time.Second):
			t.Fatal("certificate job did not finish")
		}
		return decode[apitypes.Job](t, h.call("GET", "/api/v1/jobs/"+strconv.FormatInt(job.ID, 10), h.adminSs, nil))
	}
	if done := wait(resp.Job); done.Status != "success" {
		t.Fatalf("%+v", done)
	}
	active := decode[apitypes.GameCertificate](t, h.call("GET", game, h.adminSs, nil))
	if active.Mode != "acme" || active.Certificate == nil || !active.Certificate.CoversGameHost {
		t.Fatalf("%+v", active)
	}
	errBody(t, h.call("PUT", settings, h.adminSs, apitypes.UpdateACMERequest{Staging: ptr(true)}), 409, apitypes.ErrorCodeConflict)
	terms := errBody(t, h.call("PUT", settings, h.adminSs, apitypes.UpdateACMERequest{TermsAgreed: ptr(false)}), 409, apitypes.ErrorCodeConflict)
	if !strings.Contains(terms.Message, "switch both game and panel away from ACME") {
		t.Fatal(terms.Message)
	}

	job := decode[apitypes.Job](t, h.call("POST", settings+"/renew", h.adminSs, nil))
	done := wait(&job)
	if done.Status != "error" || !strings.Contains(done.Detail, "port 80") || len(done.Detail) > 400 || strings.ContainsAny(done.Detail, "\n\x1b") {
		t.Fatalf("unexpected failed job: %+v", done)
	}
	status := decode[apitypes.ACME](t, h.call("GET", settings, h.adminSs, nil))
	if len(status.Certificates) != 1 {
		t.Fatalf("%+v", status)
	}
	c := status.Certificates[0]
	if c.Status != "error" || c.LastError != done.Detail || c.LastAttemptAt == nil || c.Certificate.FingerprintSHA256 != active.Certificate.FingerprintSHA256 {
		t.Fatalf("%+v", c)
	}
	if _, err := h.st.GetJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPChallengeOrHTTPSRedirect(t *testing.T) {
	h := newDockerHarness(t)
	fake := &fakeACMEClient{http: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Host == "challenge-only.example" && r.URL.Path == "/.well-known/acme-challenge/active" {
			_, _ = io.WriteString(w, "authorization")
			return true
		}
		return false
	}}
	setupFakeACME(t, h, fake, "", true)
	for _, tc := range []struct {
		host, path, port, location string
		status                     int
	}{
		{"challenge-only.example", "/.well-known/acme-challenge/active", "443", "", 200},
		{"challenge-only.example", "/.well-known/acme-challenge/inactive", "443", "", 421},
		{"unknown.example", "/api/v1/status", "443", "", 421},
		{"example.com:80", "/api/v1/status?q=x", "443", "https://example.com/api/v1/status?q=x", 308},
		{"[2001:db8::1]:80", "/dir/a%2Fb?", "8443", "https://[2001:db8::1]:8443/dir/a%2Fb?", 308},
		{"localhost", "//evil.example/path", "443", "https://localhost//evil.example/path", 308},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://other.example"+tc.path, nil)
		r.Host = tc.host
		w := httptest.NewRecorder()
		h.srv.redirectHandler(tc.port).ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Location") != tc.location {
			t.Errorf("%s %s: %d %v", tc.host, tc.path, w.Code, w.Header())
		}
		if tc.status == 200 && w.Body.String() != "authorization" {
			t.Fatal(w.Body.String())
		}
	}
}

func TestCommittedGameChangeReportsPartialPublicationAndConverges(t *testing.T) {
	h := newDockerHarness(t)
	img := h.managedImage(1)
	one := h.createServer(h.devTok, apitypes.CreateServerRequest{Name: "one", BuildID: img.ID})
	two := h.createServer(h.devTok, apitypes.CreateServerRequest{Name: "two", BuildID: img.ID})
	chain, key := testCertPEM(t, "play.example.com", time.Now().Add(time.Hour))
	mount := h.certs.ServerDir(two.ID)
	if err := os.WriteFile(mount, []byte("block second mount"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.automation.Maintain(ctx)
	}()
	defer func() { cancel(); <-done }()

	resp := h.call("PUT", "/api/v1/certificates/game", h.adminSs,
		apitypes.UpdateGameCertificateRequest{Mode: "custom", CertificatePEM: chain, PrivateKeyPEM: key})
	report := errBody(t, resp, http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	if !strings.Contains(report.Message, "new game certificate mode is active") || !strings.Contains(report.Message, "daemon will retry automatically") {
		t.Fatal(report.Message)
	}
	first, err := tls.LoadX509KeyPair(filepath.Join(h.certs.ServerDir(one.ID), certs.CertFile), filepath.Join(h.certs.ServerDir(one.ID), certs.KeyFile))
	if err != nil || first.Leaf == nil || first.Leaf.Subject.CommonName != "play.example.com" {
		t.Fatalf("first mount was not published: %v", err)
	}
	if err := os.Remove(mount); err != nil {
		t.Fatal(err)
	}
	// Observe the filesystem directly so the test does not trigger recovery.
	deadline := time.Now().Add(5 * time.Second)
	for {
		second, err := tls.LoadX509KeyPair(filepath.Join(mount, certs.CertFile), filepath.Join(mount, certs.KeyFile))
		if err == nil && second.Leaf.Equal(first.Leaf) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance did not finish copying: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type blockingCertificateSettings struct {
	*store.Store
	committed, release chan struct{}
}

func (s blockingCertificateSettings) SetSetting(ctx context.Context, key, value string) error {
	err := s.Store.SetSetting(ctx, key, value)
	if key == certs.SettingMode && err == nil {
		close(s.committed)
		<-s.release
	}
	return err
}

func TestShutdownJoinsSynchronousCertificatePublication(t *testing.T) {
	h := newDockerHarness(t)
	st := blockingCertificateSettings{Store: h.st, committed: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(st.release) })
	defer release()
	mgr, err := certs.Open(certs.Options{Dir: filepath.Join(t.TempDir(), "certs", "game"), Settings: st})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.certs = mgr
	h.srv.automation = nil
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- h.do("PUT", "/api/v1/certificates/game", h.adminSs, apitypes.UpdateGameCertificateRequest{Mode: "self_signed"})
	}()
	select {
	case <-st.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("certificate request did not reach the commit")
	}
	h.srv.beginShutdown()
	select {
	case <-h.srv.Drained():
		t.Fatal("HTTP layer drained before synchronous publication")
	default:
	}
	rejected := h.call("PUT", "/api/v1/certificates/game", h.adminSs, apitypes.UpdateGameCertificateRequest{Mode: "self_signed"})
	errBody(t, rejected, http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
	release()
	select {
	case <-h.srv.Drained():
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP layer did not drain after publication")
	}
	if got := <-response; got.Code != http.StatusOK {
		t.Fatalf("committed change: %d %s", got.Code, got.Body.String())
	}
}

func TestStartRepairsInterruptedMountAfterGameHostChange(t *testing.T) {
	h := newDockerHarness(t)
	img := h.managedImage(1)
	sv := h.createServer(h.devTok, apitypes.CreateServerRequest{Name: "arena", BuildID: img.ID})
	chain, key := testCertPEM(t, "play.example.com", time.Now().Add(time.Hour))
	if _, err := h.certs.UseCustom(t.Context(), []byte(chain), []byte(key)); err != nil {
		t.Fatal(err)
	}
	// Use the installed pair as the previous ACME certificate without a CA request.
	if err := h.st.SetSetting(t.Context(), certs.SettingMode, string(certs.ModeACME)); err != nil {
		t.Fatal(err)
	}
	host := "next.example.com"
	h.call("PATCH", "/api/v1/settings", h.adminSs, apitypes.UpdateSettingsRequest{GameHost: &host})
	dir := h.certs.ServerDir(sv.ID)
	certPath, keyPath := filepath.Join(dir, certs.CertFile), filepath.Join(dir, certs.KeyFile)
	previous, _ := testCertPEM(t, "play.example.com", time.Now().Add(time.Hour))
	if err := os.WriteFile(certPath, []byte(previous), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath+".new", []byte(chain), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		t.Fatal("test did not reproduce the window after the key rename")
	}
	j := h.action(h.devTok, sv.ID, "start")
	if j.Status != store.JobSuccess || !strings.Contains(h.jobLog(j.ID), "game certificate does not cover the game host "+host) {
		t.Fatalf("start with previous certificate: %+v\n%s", j, h.jobLog(j.ID))
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("server started without a repaired pair: %v", err)
	}
	if got := h.getServer(h.devTok, sv.ID); got.Certificate == nil || got.Certificate.CoversGameHost {
		t.Fatalf("previous pair's coverage: %+v", got.Certificate)
	}
}

func TestCancelledRenewalInstallsSavedPairOnNextCheck(t *testing.T) {
	h := newDockerHarness(t)
	ctx := t.Context()
	const host = "play.example.com"
	chain, key := testCertPEM(t, host, time.Now().Add(time.Hour))
	old, err := h.certs.UseCustom(ctx, []byte(chain), []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	nextChain, nextKey := testCertPEM(t, host, time.Now().Add(2*time.Hour))
	started := make(chan struct{})
	fake := &fakeACMEClient{chain: chain, key: key}
	fake.issue = func(ctx context.Context, host string, force bool) error {
		close(started)
		<-ctx.Done()
		fake.chain, fake.key = nextChain, nextKey
		return nil
	}
	setupFakeACME(t, h, fake, "http://127.0.0.1/unused", true)
	h.call("PUT", "/api/v1/certificates/acme", h.adminSs, apitypes.UpdateACMERequest{TermsAgreed: ptr(true)})
	if err := h.st.SetSetting(ctx, certs.SettingMode, string(certs.ModeACME)); err != nil {
		t.Fatal(err)
	}
	job := decode[apitypes.Job](t, h.call("POST", "/api/v1/certificates/acme/renew", h.adminSs, nil))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not reach the issuer")
	}
	if err := h.runner.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	if done := h.waitJob(job); done.Status != store.JobCancelled {
		t.Fatalf("renewal job: %+v", done)
	}
	if got, err := h.certs.Current(ctx, 0); err != nil || got.Fingerprint != old.Fingerprint {
		t.Fatalf("job unexpectedly published: %+v, %v", got, err)
	}
	// Initialize runs the same storage reconciliation as the periodic check.
	if err := h.srv.automation.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := h.certs.Current(ctx, 0); err != nil || got.NotAfter.Equal(old.NotAfter) || !got.Covers(host) {
		t.Fatalf("did not install the issued renewal: %+v, %v", got, err)
	}
}
