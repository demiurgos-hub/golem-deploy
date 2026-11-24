package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

type fakeACME struct {
	t *testing.T
	*Manager
	pairs     map[string]*certificatePair
	err       error
	calls     int
	renewAt   time.Time
	refreshAt time.Time
}

func (f *fakeACME) Load(_ context.Context, _ ACMEAccount, host string) ([]byte, []byte, error) {
	p := f.pairs[host]
	if p == nil {
		return nil, nil, fs.ErrNotExist
	}
	return p.chain, p.key, nil
}
func (f *fakeACME) Issue(_ context.Context, _ ACMEAccount, host string, _ bool, _ io.Writer) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.pairs[host] = testACMEPair(f.t, host, f.now())
	return nil
}
func (f *fakeACME) RenewalInfo(context.Context, ACMEAccount, string, *x509.Certificate) (time.Time, time.Time, error) {
	return f.renewAt, f.refreshAt, nil
}
func (*fakeACME) HTTPChallenge(http.ResponseWriter, *http.Request) bool       { return false }
func (*fakeACME) TLSChallenge(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }

func TestACMEPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name, host, directory, want string
		terms, challenge            bool
	}{
		{"terms", "example.com", "", "terms_agreed", false, true},
		{"host", "", "", "game_host", true, true},
		{"challenge", "example.com", "", "mode", true, false},
		{"loopback", "127.0.0.1", "", "mode", true, true},
		{"private", "10.4.3.2", "", "mode", true, true},
		{"link-local", "169.254.5.2", "", "mode", true, true},
		{"cgnat", "100.64.0.2", "", "mode", true, true},
		{"reserved", "192.0.2.2", "", "mode", true, true},
		{"unique-local", "fd00::1", "", "mode", true, true},
		{"localhost", "localhost", "", "mode", true, true},
		{"single-label", "workstation", "", "mode", true, true},
		{"public-ip", "8.8.8.8", "", "", true, true},
		{"public-dns", "example.com", "", "", true, true},
		{"private-ca", "127.0.0.1", "https://localhost/dir", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Automation{AutomationOptions: AutomationOptions{Directory: tc.directory, HTTP01: tc.challenge}}
			var names []string
			if tc.host != "" {
				names = []string{tc.host}
			}
			err := a.precheck(accountSettings{TermsAgreed: tc.terms}, names)
			var ve *ValidationError
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &ve) || ve.Field != tc.want {
				t.Fatalf("wanted %s, got %v", tc.want, err)
			}
		})
	}
}

func TestACMEEmail(t *testing.T) {
	for _, email := range []string{"", "admin@example.com", "admin+alerts@example.com"} {
		if !validEmail(email) {
			t.Errorf("rejected %q", email)
		}
	}
	for _, email := range []string{" admin@example.com", "name <admin@example.com>", "<admin@example.com>", "admin@example.com (name)", "a@b,c@d", "a\n@b", "a\x00@b", strings.Repeat("a", 251) + "@abc"} {
		if validEmail(email) {
			t.Errorf("accepted %q", email)
		}
	}
}

func TestACMERetryAfter(t *testing.T) {
	ca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"urn:ietf:params:acme:error:rateLimited","detail":"Order quota exhausted. Retry at 2099-01-01T00:00:00Z"}`)
	}))
	defer ca.Close()
	client := NewACMEClient(t.TempDir(), nil, slog.New(slog.DiscardHandler), ACMEListeners{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := time.Now()
	err := client.Issue(ctx, ACMEAccount{Directory: ca.URL}, "example.com", false, io.Discard)
	var failure *acmeFailure
	if !errors.As(err, &failure) || failure.retry.Before(started.Add(10*time.Minute)) || failure.retry.After(time.Now().Add(10*time.Minute)) ||
		!strings.Contains(failure.message, "Retry after") {
		t.Fatalf("expected Retry-After from the CA's response, got %v", err)
	}
}

type localTLSConn struct {
	net.Conn
	addr net.Addr
}

func (c localTLSConn) LocalAddr() net.Addr { return c.addr }

func TestPanelCertificateSelection(t *testing.T) {
	game, _, _ := newManager(t)
	fake := &fakeACME{t: t, Manager: game, pairs: make(map[string]*certificatePair)}
	a := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost("play.example.com"), PublicHosts: []string{"panel.example.com", "8.8.8.8"},
		PanelMode: apitypes.PanelCertificateModeSelfSigned, PanelDir: filepath.Join(filepath.Dir(game.dir), "panel"), Client: fake})
	ctx := t.Context()
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	fallback := a.panel.Load().fallback
	if !fallback.info.Covers("play.example.com") || !fallback.info.Covers("panel.example.com") || !fallback.info.Covers("::1") {
		t.Fatal("missing fallback SANs")
	}
	state := *a.panel.Load()
	state.mode = apitypes.PanelCertificateModeACME
	for _, name := range state.hosts {
		if err := fake.Issue(ctx, ACMEAccount{}, name, false, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	state.issued = fake.pairs
	a.panel.Store(&state)
	for _, tc := range []struct{ sni, addr, want string }{
		{"PANEL.EXAMPLE.COM.", "8.8.8.8:443", "panel.example.com"},
		{"", "8.8.8.8:443", "8.8.8.8"},
		{"", "127.0.0.1:443", "8.8.8.8"},
		{"unknown.example.com", "8.8.8.8:443", ""},
	} {
		conn, err := net.ResolveTCPAddr("tcp", tc.addr)
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.GetCertificate(&tls.ClientHelloInfo{ServerName: tc.sni, Conn: localTLSConn{addr: conn}})
		want := fallback
		if tc.want != "" {
			want = state.issued[tc.want]
		}
		if err != nil || got != want.tls {
			t.Errorf("SNI %q at %s selected the wrong pair: %v", tc.sni, tc.addr, err)
		}
	}
	secondIP := "1.1.1.1"
	if err := fake.Issue(ctx, ACMEAccount{}, secondIP, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	state.hosts = append(state.hosts, secondIP)
	if got := state.selectPair(a.now(), "", "10.0.0.1"); got != state.issued["panel.example.com"] {
		t.Fatal("ambiguous IP fallback bypassed the first panel host")
	}
	if got := state.selectPair(a.now(), "", secondIP); got != state.issued[secondIP] {
		t.Fatal("ambiguous fallback bypassed the listener IP")
	}

	state.issued[secondIP].info.NotAfter = a.now()
	if got := state.selectPair(a.now(), "", "10.0.0.1"); got != state.issued["8.8.8.8"] {
		t.Fatal("an expired IP certificate defeated the sole usable IP fallback")
	}

	if _, err := a.GetCertificate(&tls.ClientHelloInfo{SupportedProtos: []string{"acme-tls/1"}}); err == nil {
		t.Fatal("accepted an inactive challenge")
	}
}

func TestACMEReconcileNewerStorage(t *testing.T) {
	game, c, st := newManager(t)
	ctx := t.Context()
	host := "game.example.com"
	fake := &fakeACME{t: t, Manager: game, pairs: make(map[string]*certificatePair)}
	if err := fake.Issue(ctx, ACMEAccount{}, host, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	old := fake.pairs[host]
	if _, err := game.useShared(ctx, ModeACME, old.chain, old.key); err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour)
	if err := fake.Issue(ctx, ACMEAccount{}, host, true, io.Discard); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Dir: game.dir, Settings: st, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	st.m[store.SettingACME] = `{"TermsAgreed":true}`
	a := NewAutomation(AutomationOptions{Game: reopened, GameHost: fixedHost(host), PanelMode: apitypes.PanelCertificateModeNone, Client: fake, HTTP01: true})
	lastAttempt := c.now()
	a.record(ctx, (accountSettings{}).account(""), host, attemptState{At: &lastAttempt, Detail: "Previous renewal failed", Failures: 1})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{game.dir, game.ServerDir(1), game.ServerDir(2)} {
		result, err := load(dir)
		if err != nil || result.Fingerprint != fake.pairs[host].info.Fingerprint || result.Fingerprint == old.info.Fingerprint {
			t.Fatalf("did not reconcile %s: %v", dir, err)
		}
	}
	if fake.calls != 2 {
		t.Fatal("local reconciliation contacted the CA")
	}
	status, err := a.ACME(ctx)
	if err != nil || len(status.Certificates) != 1 || status.Certificates[0].Status != "issued" || status.Certificates[0].LastError != "" {
		t.Fatalf("status did not reflect the recovered pair: %+v, %v", status, err)
	}
}

func TestACMECommittedStageRecovery(t *testing.T) {
	game, c, st := newManager(t)
	ctx := t.Context()
	host := "game.example.com"
	old, err := game.Ensure(ctx, 1, fixedHost(host))
	if err != nil {
		t.Fatal(err)
	}
	chain, key, issued, err := game.generate(host)
	if err != nil {
		t.Fatal(err)
	}
	stage := game.stagingDir()
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := game.install(filepath.Join(stage, "custom"), chain, key); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stage, "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	record := "acme:committed-generation"
	if err := game.writeFile(stage, generationFile, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, SettingMode, record); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(Options{Dir: game.dir, Settings: st, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := recovered.Ensure(ctx, 1, fixedHost(host))
	if err != nil || pair.Mode != ModeACME || pair.Fingerprint != issued.Fingerprint || pair.Fingerprint == old.Fingerprint {
		t.Fatalf("committed ACME stage was not recovered: %v", err)
	}
}

func TestACMEHostChangeKeepsPairAndRetries(t *testing.T) {
	game, clock, _ := newManager(t)
	ctx := t.Context()
	host := "before.example.com"
	fake := &fakeACME{t: t, Manager: game, pairs: make(map[string]*certificatePair)}
	a := NewAutomation(AutomationOptions{
		Game: game, GameHost: func(context.Context) (string, error) { return host, nil },
		PanelMode: apitypes.PanelCertificateModeNone, Client: fake, HTTP01: true,
	})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	agree := true
	if _, err := a.UpdateSettings(ctx, apitypes.UpdateACMERequest{TermsAgreed: &agree}); err != nil {
		t.Fatal(err)
	}
	if err := a.run(ctx, IssueGame, []string{host}, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := game.Current(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ChangeHost(func() error {
		host = "after.example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake.err = errors.New("CA temporarily unavailable")
	if err := a.check(ctx, true); err != nil {
		t.Fatal(err)
	}
	current, err := game.Current(ctx, 0)
	if err != nil || current.Fingerprint != before.Fingerprint || current.Covers(host) {
		t.Fatalf("previous certificate changed during failed issuance: %+v, %v", current, err)
	}
	status, err := a.ACME(ctx)
	if err != nil || len(status.Certificates) != 1 || status.Certificates[0].Identifier != host ||
		status.Certificates[0].Status != "error" || status.Certificates[0].Certificate != nil {
		t.Fatalf("unexpected certificate status after failure: %+v, %v", status, err)
	}
	fake.err = nil
	if err := a.check(ctx, true); err != nil || fake.calls != 2 {
		t.Fatalf("background check ignored retry backoff: %d, %v", fake.calls, err)
	}
	clock.add(time.Minute)
	if err := a.check(ctx, true); err != nil {
		t.Fatal(err)
	}
	current, err = game.Current(ctx, 0)
	if err != nil || current.Fingerprint == before.Fingerprint || !current.Covers(host) {
		t.Fatalf("retry did not install the new pair: %+v, %v", current, err)
	}
	if _, err := game.UseSelfSigned(ctx, fixedHost(host)); err != nil {
		t.Fatal(err)
	}
	if err := a.check(ctx, true); err != nil || fake.calls != 3 {
		t.Fatalf("background check obtained a certificate after leaving ACME: %d, %v", fake.calls, err)
	}
}

func TestBlockedRenewalRecordsReasonAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, host, want string
		terms, challenge bool
	}{
		{"terms", "game.example.com", "accept the ACME subscriber agreement", false, true},
		{"challenge", "game.example.com", "--http-listen", true, false},
		{"ineligible host", "game.test", "use a public DNS name or IP address", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			game, c, st := newManager(t)
			var logs bytes.Buffer
			game.logger = slog.New(slog.NewTextHandler(&logs, nil))
			fake := &fakeACME{t: t, Manager: game, pairs: make(map[string]*certificatePair)}
			host := "game.example.com"
			if err := fake.Issue(ctx, ACMEAccount{}, host, false, io.Discard); err != nil {
				t.Fatal(err)
			}
			old := fake.pairs[host]
			if _, err := game.useShared(ctx, ModeACME, old.chain, old.key); err != nil {
				t.Fatal(err)
			}
			st.m[store.SettingACME] = fmt.Sprintf(`{"TermsAgreed":%t}`, tc.terms)
			a := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost(tc.host),
				PanelMode: apitypes.PanelCertificateModeNone, Client: fake, HTTP01: tc.challenge})
			if err := a.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			first := c.now()
			c.add(time.Hour)
			a.maintainOnce(ctx)
			out, err := a.ACME(ctx)
			if err != nil || len(out.Certificates) != 1 {
				t.Fatalf("status: %+v, %v", out, err)
			}
			got := out.Certificates[0]
			if got.Status != "error" || !strings.Contains(got.LastError, tc.want) || got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(first) {
				t.Fatalf("blocked status: %+v", got)
			}
			if strings.Count(logs.String(), "automatic certificate renewal blocked") != 1 || fake.calls != 1 {
				t.Fatalf("repeated log or issuance while blocked: %s, %d requests", &logs, fake.calls)
			}
			current, err := game.Current(ctx, 0)
			if err != nil || current.Fingerprint != old.info.Fingerprint {
				t.Fatalf("blocking discarded the installed certificate: %v", err)
			}
			if tc.host == host {
				c.add(SelfSignedValidity)
				out, err := a.ACME(ctx)
				if err != nil || out.Certificates[0].Status != "error" ||
					!strings.Contains(out.Certificates[0].LastError, "has expired") ||
					!strings.Contains(out.Certificates[0].LastError, tc.want) {
					t.Fatalf("expired status: %+v, %v", out, err)
				}
			}
		})
	}
}

func TestExpiredManagedPairWithoutPreviousError(t *testing.T) {
	game, c, st := newManager(t)
	ctx := t.Context()
	host := "game.example.com"
	chain, key, _, err := game.generate(host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := game.useShared(ctx, ModeACME, chain, key); err != nil {
		t.Fatal(err)
	}
	st.m[store.SettingACME] = `{"TermsAgreed":true}`
	a := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost(host), HTTP01: true,
		PanelMode: apitypes.PanelCertificateModeNone, Client: &fakeACME{}})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	c.add(SelfSignedValidity)
	out, err := a.ACME(ctx)
	if err != nil || out.Certificates[0].Status != "error" || !strings.Contains(out.Certificates[0].LastError, "has expired") {
		t.Fatalf("expired status: %+v, %v", out, err)
	}
}

func TestSavedPairInstallationFailureBacksOff(t *testing.T) {
	game, c, st := newManager(t)
	ctx := t.Context()
	host := "game.example.com"
	fake := &fakeACME{t: t, Manager: game, pairs: make(map[string]*certificatePair)}
	for i := range 2 {
		if err := fake.Issue(ctx, ACMEAccount{}, host, i != 0, io.Discard); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			old := fake.pairs[host]
			if _, err := game.useShared(ctx, ModeACME, old.chain, old.key); err != nil {
				t.Fatal(err)
			}
			c.add(time.Hour)
		}
	}
	st.m[store.SettingACME] = `{"TermsAgreed":true}`
	var writes int
	st.settingFault = func(key, _ string) (bool, error) {
		if key == SettingMode {
			writes++
			return false, errors.New("injected install failure")
		}
		return true, nil
	}
	a := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost(host), HTTP01: true,
		PanelMode: apitypes.PanelCertificateModeNone, Client: fake})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := a.ACME(ctx)
	if err != nil || out.Certificates[0].Status != "error" || out.Certificates[0].LastAttemptAt == nil {
		t.Fatalf("failed install status: %+v, %v", out, err)
	}
	a.maintainOnce(ctx)
	if writes != 1 {
		t.Fatalf("retried install without backoff: %d writes", writes)
	}
	st.settingFault = nil
	c.add(time.Minute)
	a.maintainOnce(ctx)
	out, err = a.ACME(ctx)
	if err != nil || out.Certificates[0].Status != "issued" || out.Certificates[0].Certificate.FingerprintSHA256 != fake.pairs[host].info.Fingerprint || fake.calls != 2 {
		t.Fatalf("backoff did not recover locally: %+v, %v (CA calls=%d)", out, err, fake.calls)
	}
}

func TestReadBackRecognizesCommittedGameAndPanelModes(t *testing.T) {
	game, _, st := newManager(t)
	ctx := t.Context()
	host := "game.example.com"
	a := NewAutomation(AutomationOptions{Game: game, GameHost: fixedHost(host),
		PanelMode: apitypes.PanelCertificateModeSelfSigned, PanelDir: filepath.Join(filepath.Dir(game.dir), "panel"), Client: &fakeACME{}})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	st.settingFault = func(_, _ string) (bool, error) {
		return true, errors.New("lost acknowledgement after commit")
	}
	chain, key, info, err := game.generate(host)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := game.UseCustom(ctx, chain, key); err != nil || got.Fingerprint != info.Fingerprint || got.Mode != ModeCustom {
		t.Fatalf("committed game pair: %+v, %v", got, err)
	}
	panel := *a.panel.Load()
	panel.mode = apitypes.PanelCertificateModeACME
	if err := a.commitPanel(ctx, &panel); err != nil || a.panel.Load() != &panel {
		t.Fatalf("committed panel mode: %v", err)
	}
}

func TestRenewalLeavesRetryMargin(t *testing.T) {
	for _, tc := range []struct {
		lifetime, margin time.Duration
	}{
		{24 * time.Hour, 12 * time.Hour},
		{160 * time.Hour, 40 * time.Hour},
		{90 * 24 * time.Hour, 7 * 24 * time.Hour},
	} {
		game, c, _ := newManager(t)
		now := c.now()
		info := Info{NotAfter: now.Add(tc.margin + time.Minute), NotBefore: now.Add(tc.margin + time.Minute - tc.lifetime), Fingerprint: "leaf"}
		fake := &fakeACME{renewAt: info.NotAfter.Add(-2 * time.Hour)}
		a := NewAutomation(AutomationOptions{Game: game, Client: fake})
		host := "game.example.com"
		ctx := t.Context()
		if a.renewalDue(ctx, ACMEAccount{}, host, &certificatePair{info: info}) {
			t.Fatalf("renewed too early for lifetime %s", tc.lifetime)
		}
		c.add(time.Minute)
		if !a.renewalDue(ctx, ACMEAccount{}, host, &certificatePair{info: info}) {
			t.Fatalf("late ARI consumed the margin for lifetime %s", tc.lifetime)
		}
		for range 8 {
			a.recordFailure(ctx, ACMEAccount{}, host, c.now(), errors.New("failed"), false)
		}
		last, err := a.attempt(ctx, ACMEAccount{}, host)
		if err != nil || last.Retry.Sub(c.now()) > 30*time.Minute {
			t.Fatalf("local backoff consumed the retry margin: %+v, %v", last, err)
		}
	}
}

func TestACMEStoredDeferralsAreBounded(t *testing.T) {
	game, c, _ := newManager(t)
	now := c.now()
	for _, tc := range []struct {
		wait, failure, ari time.Duration
	}{
		{-time.Hour, time.Minute, time.Hour},
		{2 * time.Hour, 2 * time.Hour, 2 * time.Hour},
		{100 * time.Hour, 24 * time.Hour, 24 * time.Hour},
	} {
		t.Run(tc.wait.String(), func(t *testing.T) {
			host := "game.example.com"
			account := ACMEAccount{Directory: tc.wait.String()}
			client := &fakeACME{refreshAt: now.Add(tc.wait)}
			a := NewAutomation(AutomationOptions{Game: game, Client: client})
			a.recordFailure(t.Context(), account, host, now, &acmeFailure{message: "rate limited", retry: now.Add(tc.wait)}, false)
			last, err := a.attempt(t.Context(), account, host)
			if err != nil || !last.Retry.Equal(now.Add(tc.failure)) {
				t.Fatalf("stored failure retry: %v, %v", last.Retry, err)
			}
			a.renewalDue(t.Context(), account, host, testACMEPair(t, host, now))
			if refresh := a.schedules[host].refresh; !refresh.Equal(now.Add(tc.ari)) {
				t.Fatalf("stored ARI refresh: %v", refresh)
			}
		})
	}
}

func TestCancelledRenewAllKeepsCommitAndRecordsCancellation(t *testing.T) {
	game, clock, settings := newManager(t)
	ctx := t.Context()
	const gameHost, panelHost = "game.example.com", "panel.example.com"
	fake := &fakeACME{t: t, Manager: game, pairs: make(map[string]*certificatePair)}
	old := testACMEPair(t, gameHost, clock.now())
	fake.pairs[gameHost] = old
	if _, err := game.useShared(ctx, ModeACME, old.chain, old.key); err != nil {
		t.Fatal(err)
	}
	settings.m[store.SettingACME] = `{"TermsAgreed":true}`
	a := NewAutomation(AutomationOptions{
		Game: game, GameHost: fixedHost(gameHost), Client: fake, HTTP01: true, PublicHosts: []string{panelHost},
		PanelMode: apitypes.PanelCertificateModeSelfSigned, PanelDir: filepath.Join(filepath.Dir(game.dir), "panel"),
	})
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.run(ctx, IssuePanel, []string{panelHost}, io.Discard); err != nil {
		t.Fatal(err)
	}
	release, err := game.HoldForStart(ctx, 1, ModeACME)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runner := jobs.New(st, dir, jobs.Options{Workers: 1})
	defer func() { <-runner.Stop() }()
	armed := make(chan struct{})
	job, err := runner.Start(ctx, "certificate.renew", certificateSubject(), nil, func(ctx context.Context, log io.Writer) error {
		select {
		case <-armed:
			return a.run(ctx, RenewAll, nil, log)
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	settings.settingFault = func(key, _ string) (bool, error) {
		if key == SettingMode {
			if err := runner.Cancel(job.ID); err != nil {
				t.Error(err)
			}
		}
		return true, nil
	}
	close(armed)
	select {
	case <-runner.Done(job.ID):
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not leave the runner")
	}
	if result, err := st.GetJob(ctx, job.ID); err != nil || result.Status != store.JobCancelled {
		t.Fatalf("partially completed renewal should be cancelled: %+v, %v", result, err)
	}
	if fake.calls != 2 {
		t.Fatalf("renewal attempted the later panel host after cancellation: %d CA calls", fake.calls)
	}
	if current, err := game.Current(ctx, 0); err != nil || current.Fingerprint == old.info.Fingerprint {
		t.Fatalf("cancelled renewal lost the committed generation: %+v, %v", current, err)
	}
	if current, err := load(game.ServerDir(1)); err != nil || current.Fingerprint != old.info.Fingerprint {
		t.Fatalf("cancelled renewal rewrote a held mount: %+v, %v", current, err)
	}
}

func TestACMERejectsOversizedDirectory(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%t", chunked), func(t *testing.T) {
			body := `{"newNonce":"http://localhost/nonce","extra":"` + strings.Repeat("X", maxACMEResponseBytes) + `"}`
			ca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if chunked {
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				}
				_, _ = io.WriteString(w, body)
			}))
			defer ca.Close()
			client := NewACMEClient(t.TempDir(), nil, slog.New(slog.DiscardHandler), ACMEListeners{HTTP: "127.0.0.1:80"})
			err := client.Issue(t.Context(), ACMEAccount{Directory: ca.URL}, "game.example.com", false, io.Discard)
			var limit *http.MaxBytesError
			if !errors.As(err, &limit) || limit.Limit != maxACMEResponseBytes {
				t.Fatalf("oversized response error: %v", err)
			}
		})
	}
}
