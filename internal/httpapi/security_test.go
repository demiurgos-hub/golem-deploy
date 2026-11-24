package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// reqOpts describes a request for send.
type reqOpts struct {
	method, path, token string
	body                any // nil, raw JSON string, or a value to marshal
	remote              string
	host                string
	header              http.Header
}

// send is like harness.do but safe to call from any goroutine and with
// control over the peer address, Host and headers.
func send(h *harness, o reqOpts) *httptest.ResponseRecorder {
	var rd io.Reader
	switch b := o.body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(o.method, o.path, rd)
	req.RemoteAddr = "192.0.2.10:5555"
	if o.remote != "" {
		req.RemoteAddr = o.remote
	}
	if o.host != "" {
		req.Host = o.host
	}
	for k, vs := range o.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if o.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	rr := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rr, req)
	return rr
}

func loginFrom(h *harness, remote, user, pw string) *httptest.ResponseRecorder {
	return send(h, reqOpts{method: "POST", path: "/api/v1/auth/login", body: apitypes.LoginRequest{Username: user, Password: pw}, remote: remote})
}

// remoteN returns a distinct documentation-range peer address.
func remoteN(i int) string {
	return "198.18." + strconv.Itoa(i/250) + "." + strconv.Itoa(i%250+1) + ":4000"
}

// withoutRequestID strips the request id from an error body.
func withoutRequestID(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `"request_id"`)
	if i < 0 {
		t.Fatalf("no request id in %s", body)
	}
	return body[:i]
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLogs(h *harness) *syncBuffer {
	buf := &syncBuffer{}
	h.srv.logger = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

// ---------------------------------------------------------------- F1

// TestF1_DeletedAccountSessionCannotAuthenticateAsNextAccount reproduces the
// reviewer's scenario: bob has a session, bob is deleted, carol is created as
// an admin; bob's token must not work, in particular not as carol.
func TestF1_DeletedAccountSessionCannotAuthenticateAsNextAccount(t *testing.T) {
	h := newHarness(t)
	root := h.account("root", store.RoleAdmin, true)
	rootTok := h.session(root)

	rr := h.do("POST", "/api/v1/accounts", rootTok, apitypes.CreateAccountRequest{Username: "bob", Password: testPassword, Role: apitypes.RoleDeveloper, Approved: true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create bob: %d %s", rr.Code, rr.Body.String())
	}
	bob := decode[apitypes.Account](t, rr)
	bobTok := mustLogin(t, h, "bob").Token
	if rr := h.do("GET", "/api/v1/account", bobTok, nil); rr.Code != http.StatusOK {
		t.Fatalf("bob before delete: %d", rr.Code)
	}
	if rr := h.do("DELETE", "/api/v1/accounts/"+strconv.FormatInt(bob.ID, 10), rootTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete bob: %d", rr.Code)
	}
	rr = h.do("POST", "/api/v1/accounts", rootTok, apitypes.CreateAccountRequest{Username: "carol", Password: testPassword, Role: apitypes.RoleAdmin, Approved: true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create carol: %d %s", rr.Code, rr.Body.String())
	}
	carol := decode[apitypes.Account](t, rr)
	if carol.ID == bob.ID {
		t.Fatalf("carol reused bob's id %d", bob.ID)
	}
	for _, p := range []string{"/api/v1/account", "/api/v1/accounts", "/api/v1/status"} {
		errBody(t, h.do("GET", p, bobTok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	}
	errBody(t, h.do("POST", "/api/v1/auth/refresh", bobTok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

// TestF1_SessionBoundToAccountRow: even if a row with the same id came back
// (simulated by issuing a token for carol's id with the version bob's row
// would have had at creation), the random per-row token_version makes it
// fail.
func TestF1_SessionBoundToAccountRow(t *testing.T) {
	h := newHarness(t)
	a := h.account("alice", store.RoleAdmin, true)
	b := h.account("bob", store.RoleDeveloper, true)
	if a.TokenVersion == b.TokenVersion {
		t.Fatal("two accounts share a token_version")
	}
	// A token carrying alice's id but bob's version is rejected.
	forged, _, err := h.sessions.Issue(a.ID, b.TokenVersion, h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	errBody(t, h.do("GET", "/api/v1/account", forged, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

// ---------------------------------------------------------------- F2

// TestF2_ConcurrentLoginsCannotExceedLimit sends 200 concurrent logins with a
// wrong password from one address: at most the per-IP limit reach bcrypt,
// and the correct password afterwards is refused.
func TestF2_ConcurrentLoginsCannotExceedLimit(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	before := h.srv.hashSlots.Acquired()

	const n = 200
	codes := make(chan int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes <- loginFrom(h, "192.0.2.10:1", "alice", "wrong-password-123").Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if bcrypts := h.srv.hashSlots.Acquired() - before; bcrypts > loginFailuresPerWindow {
		t.Fatalf("%d password checks ran, limit %d (codes %v)", bcrypts, loginFailuresPerWindow, count)
	}
	if count[http.StatusUnauthorized] > loginFailuresPerWindow || count[http.StatusUnauthorized]+count[http.StatusTooManyRequests] != n {
		t.Fatalf("codes %v", count)
	}
	// Requests refused for concurrency were not charged; use up whatever is
	// left of the allowance sequentially. The total never exceeds the limit.
	for i := 0; i < loginFailuresPerWindow && loginFrom(h, "192.0.2.10:1", "alice", "wrong-password-123").Code == http.StatusUnauthorized; i++ {
	}
	if bcrypts := h.srv.hashSlots.Acquired() - before; bcrypts != loginFailuresPerWindow {
		t.Fatalf("%d password checks ran in total, want exactly %d", bcrypts, loginFailuresPerWindow)
	}
	errBody(t, loginFrom(h, "192.0.2.10:1", "alice", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
}

// TestF2_ConcurrentLoginsFromManyAddresses: 200 concurrent logins for one
// username from 200 addresses: at most the per-username limit reach bcrypt.
func TestF2_ConcurrentLoginsFromManyAddresses(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	before := h.srv.hashSlots.Acquired()
	const n = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			loginFrom(h, remoteN(i), "alice", "wrong-password-123")
		}(i)
	}
	close(start)
	wg.Wait()
	if bcrypts := h.srv.hashSlots.Acquired() - before; bcrypts > usernameFailuresPerWindow {
		t.Fatalf("%d password checks ran, username limit %d", bcrypts, usernameFailuresPerWindow)
	}
	for i := 0; i < usernameFailuresPerWindow && loginFrom(h, remoteN(n+1+i), "alice", "wrong-password-123").Code == http.StatusUnauthorized; i++ {
	}
	if bcrypts := h.srv.hashSlots.Acquired() - before; bcrypts != usernameFailuresPerWindow {
		t.Fatalf("%d password checks ran in total, want exactly %d", bcrypts, usernameFailuresPerWindow)
	}
	errBody(t, loginFrom(h, remoteN(2*n), "alice", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
}

// TestF2_OneLoginInFlightPerUsername: a second concurrent login for the same
// username, or too many from one address, is refused before bcrypt.
func TestF2_OneLoginInFlightPerUsername(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	release, ok := h.srv.loginFlightU.Acquire("alice")
	if !ok {
		t.Fatal("acquire")
	}
	rr := loginFrom(h, remoteN(1), "alice", testPassword)
	errBody(t, rr, http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	// The refused attempt was not counted.
	if h.srv.loginIP.Count("198.18.0.2") != 0 || h.srv.loginUser.Count("alice") != 0 {
		t.Fatal("in-flight refusal was counted as an attempt")
	}
	release()
	if rr := loginFrom(h, remoteN(1), "alice", testPassword); rr.Code != http.StatusOK {
		t.Fatalf("after release: %d", rr.Code)
	}

	ip := "198.18.0.9"
	var rels []func()
	for i := 0; i < loginsInFlightPerIP; i++ {
		r, ok := h.srv.loginFlightIP.Acquire(ip)
		if !ok {
			t.Fatal("acquire ip")
		}
		rels = append(rels, r)
	}
	errBody(t, loginFrom(h, ip+":1", "bob", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	for _, r := range rels {
		r()
	}
}

// TestF2_PasswordHashingIsBounded: when no bcrypt slot frees up in time,
// every password route answers 503 with Retry-After and does not charge the
// client an attempt.
func TestF2_PasswordHashingIsBounded(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	h.account("alice", store.RoleDeveloper, true)
	adminTok := h.session(admin)
	if rr := h.do("PATCH", "/api/v1/settings", adminTok, apitypes.UpdateSettingsRequest{RegistrationEnabled: ptr(true)}); rr.Code != http.StatusOK {
		t.Fatal("enable registration")
	}
	h.srv.hashSlots = auth.NewSlots(1, 20*time.Millisecond)
	hold, err := h.srv.hashSlots.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	cases := []struct {
		name string
		rr   *httptest.ResponseRecorder
	}{
		{"login existing", h.do("POST", "/api/v1/auth/login", "", apitypes.LoginRequest{Username: "alice", Password: testPassword})},
		{"login unknown (dummy compare)", h.do("POST", "/api/v1/auth/login", "", apitypes.LoginRequest{Username: "ghost", Password: testPassword})},
		{"register", h.do("POST", "/api/v1/auth/register", "", apitypes.RegisterRequest{Username: "newbie", Password: testPassword})},
		{"password change", h.do("POST", "/api/v1/account/password", adminTok, apitypes.ChangePasswordRequest{CurrentPassword: testPassword, NewPassword: "another-good-password"})},
		{"account create", h.do("POST", "/api/v1/accounts", adminTok, apitypes.CreateAccountRequest{Username: "carol", Password: testPassword, Role: apitypes.RoleDeveloper})},
		{"token create", h.do("POST", "/api/v1/account/tokens", adminTok, apitypes.CreateAPITokenRequest{Name: "ci", CurrentPassword: testPassword})},
	}
	for _, c := range cases {
		errBody(t, c.rr, http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable)
		if c.rr.Header().Get("Retry-After") == "" {
			t.Errorf("%s: missing Retry-After", c.name)
		}
	}
	if n := h.srv.loginIP.Count(clientIPForTests); n != 0 {
		t.Fatalf("busy responses charged %d attempts", n)
	}
	if n := h.srv.registerIP.Count(clientIPForTests); n != 0 {
		t.Fatalf("busy registration charged %d attempts", n)
	}
}

// TestF2_BcryptOnlyThroughPolicy keeps every bcrypt call in the HTTP layer
// behind the slot semaphore and the attempt limits: only passwords.go may
// touch the hasher.
func TestF2_BcryptOnlyThroughPolicy(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "passwords.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "s.hasher.") || strings.Contains(string(b), "bcrypt.") {
			t.Errorf("%s calls the password hasher directly; use hashPassword/comparePassword/login in passwords.go", f)
		}
	}
}

// ---------------------------------------------------------------- F3 / F4

// TestF3_UsernameLimitIsUniform: existing and unknown usernames are counted
// and answered identically, before and after the per-username limit.
func TestF3_UsernameLimitIsUniform(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	var last [2]*httptest.ResponseRecorder
	for i := 0; i < usernameFailuresPerWindow; i++ {
		a := loginFrom(h, remoteN(i), "alice", "wrong-password-123")
		g := loginFrom(h, remoteN(1000+i), "ghost", "wrong-password-123")
		if a.Code != http.StatusUnauthorized || g.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d / %d", i, a.Code, g.Code)
		}
		if withoutRequestID(t, a.Body.String()) != withoutRequestID(t, g.Body.String()) {
			t.Fatalf("bodies differ:\n%s\n%s", a.Body.String(), g.Body.String())
		}
	}
	last[0] = loginFrom(h, remoteN(500), "alice", testPassword)
	last[1] = loginFrom(h, remoteN(1500), "ghost", testPassword)
	bodies := []string{last[0].Body.String(), last[1].Body.String()}
	for _, rr := range last {
		errBody(t, rr, http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	}
	if withoutRequestID(t, bodies[0]) != withoutRequestID(t, bodies[1]) ||
		last[0].Header().Get("Retry-After") != last[1].Header().Get("Retry-After") {
		t.Fatal("limited responses differ between existing and unknown usernames")
	}
}

// TestF3_KnownGoodIPBypassesUsernameLimit: someone hammering alice's
// username from many addresses cannot lock alice out of the address she
// has logged in from before; the per-IP limit still applies to it.
func TestF3_KnownGoodIPBypassesUsernameLimit(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleAdmin, true)
	home := "203.0.113.5:1000"
	if rr := loginFrom(h, home, "alice", testPassword); rr.Code != http.StatusOK {
		t.Fatalf("first login: %d", rr.Code)
	}
	for i := 0; i < usernameFailuresPerWindow; i++ {
		loginFrom(h, remoteN(i), "alice", "wrong-password-123")
	}
	errBody(t, loginFrom(h, "203.0.113.99:1", "alice", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	if rr := loginFrom(h, home, "alice", testPassword); rr.Code != http.StatusOK {
		t.Fatalf("known-good address locked out: %d %s", rr.Code, rr.Body.String())
	}
	// The same /64 counts as the same address for IPv6.
	if rr := loginFrom(h, "[2001:db8:1:2::10]:1", "alice", testPassword); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("unknown v6 address: %d", rr.Code)
	}
	// Known-good addresses still have the per-IP limit.
	for i := 0; i < loginFailuresPerWindow; i++ {
		loginFrom(h, home, "alice", "wrong-password-123")
	}
	errBody(t, loginFrom(h, home, "alice", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
}

// TestF3_LoginFloodDoesNotClearBlock: with a nearly full username limiter,
// flooding it with junk usernames does not lift a username block.
func TestF3_LoginFloodDoesNotClearBlock(t *testing.T) {
	h := newHarness(t)
	h.account("alice", store.RoleDeveloper, true)
	h.srv.loginUser = auth.NewLimiter(usernameFailuresPerWindow, loginWindow, 30, h.clock.Now)
	for i := 0; i < usernameFailuresPerWindow; i++ {
		loginFrom(h, remoteN(i), "alice", "wrong-password-123")
	}
	for i := 0; i < 300; i++ {
		loginFrom(h, remoteN(100+i), "junk"+strconv.Itoa(i), "wrong-password-123")
	}
	errBody(t, loginFrom(h, remoteN(999), "alice", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
}

func TestF4_ResolveClientIP(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8", "2001:db8:ffff::/48", "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trustedProxies: trusted}
	none := &Server{}
	tests := []struct {
		name   string
		srv    *Server
		remote string
		xff    []string
		want   string
	}{
		{"no proxy configured ignores header", none, "203.0.113.1:5", []string{"198.51.100.1"}, "203.0.113.1"},
		{"untrusted peer ignores header", s, "203.0.113.1:5", []string{"198.51.100.1"}, "203.0.113.1"},
		{"trusted peer uses right-most", s, "10.1.2.3:5", []string{"198.51.100.1"}, "198.51.100.1"},
		{"left-most spoofed entries ignored", s, "10.1.2.3:5", []string{"6.6.6.6, 198.51.100.1"}, "198.51.100.1"},
		{"multiple headers concatenate", s, "10.1.2.3:5", []string{"6.6.6.6", "198.51.100.1"}, "198.51.100.1"},
		{"trusted hops skipped", s, "10.1.2.3:5", []string{"6.6.6.6, 198.51.100.1, 10.9.9.9, 192.0.2.1"}, "198.51.100.1"},
		{"garbage in the chain falls back to the peer", s, "10.1.2.3:5", []string{"198.51.100.1, not-an-ip"}, "10.1.2.3"},
		{"empty header falls back to the peer", s, "10.1.2.3:5", nil, "10.1.2.3"},
		{"all trusted uses the left-most", s, "10.1.2.3:5", []string{"10.0.0.7, 10.0.0.8"}, "10.0.0.7"},
		{"entry with port", s, "10.1.2.3:5", []string{"198.51.100.1:443"}, "198.51.100.1"},
		{"ipv6 client grouped by /64", s, "[2001:db8:ffff::1]:5", []string{"[2001:db8:1:2:3::4]"}, "2001:db8:1:2::/64"},
		{"mapped ipv4 peer", s, "[::ffff:10.0.0.1]:5", []string{"198.51.100.1"}, "198.51.100.1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tt.remote
		for _, v := range tt.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := tt.srv.resolveClientIP(r); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
	for _, bad := range []string{"10.0.0.0/33", "example.com", "fe80::1%eth0"} {
		if _, err := ParseTrustedProxies([]string{bad}); err == nil {
			t.Errorf("ParseTrustedProxies(%q) accepted", bad)
		}
	}
}

// TestF4_TrustedProxyRateLimitsRealClient: behind a trusted proxy, limits
// apply per forwarded client; a header sent by an untrusted peer, and
// spoofed left-most entries, change nothing. The access log records the
// resolved client.
func TestF4_TrustedProxyRateLimitsRealClient(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} })
	logs := captureLogs(h)
	h.account("alice", store.RoleDeveloper, true)
	viaProxy := func(xff, user, pw string) *httptest.ResponseRecorder {
		return send(h, reqOpts{method: "POST", path: "/api/v1/auth/login", remote: "10.0.0.2:4000",
			header: http.Header{"X-Forwarded-For": {xff}}, body: apitypes.LoginRequest{Username: user, Password: pw}})
	}
	for i := 0; i < loginFailuresPerWindow; i++ {
		viaProxy("198.51.100.7", "user"+strconv.Itoa(i), "wrong-password-123")
	}
	errBody(t, viaProxy("198.51.100.7", "alice", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	// Spoofing a different left-most address does not escape the limit.
	errBody(t, viaProxy("1.2.3.4, 198.51.100.7", "alice", testPassword), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
	// Another client behind the same proxy is unaffected.
	if rr := viaProxy("198.51.100.8", "alice", testPassword); rr.Code != http.StatusOK {
		t.Fatalf("other client: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logs.String(), "remote=198.51.100.8") {
		t.Fatalf("access log does not show the forwarded client:\n%s", logs.String())
	}

	// An untrusted peer cannot pick its address with the header.
	direct := func(xff string) *httptest.ResponseRecorder {
		return send(h, reqOpts{method: "POST", path: "/api/v1/auth/login", remote: "203.0.113.50:1",
			header: http.Header{"X-Forwarded-For": {xff}}, body: apitypes.LoginRequest{Username: "bob", Password: "wrong-password-123"}})
	}
	for i := 0; i < loginFailuresPerWindow; i++ {
		direct("198.51.100." + strconv.Itoa(100+i))
	}
	errBody(t, direct("198.51.100.200"), http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited)
}

// ---------------------------------------------------------------- F5

func createToken(t *testing.T, h *harness, sess, name string) apitypes.CreateAPITokenResponse {
	t.Helper()
	rr := h.do("POST", "/api/v1/account/tokens", sess, apitypes.CreateAPITokenRequest{Name: name, CurrentPassword: testPassword})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create token: %d %s", rr.Code, rr.Body.String())
	}
	return decode[apitypes.CreateAPITokenResponse](t, rr)
}

// TestF5_TokenCreationRequiresCurrentPassword: a session alone cannot mint a
// token; wrong passwords count against the login limits.
func TestF5_TokenCreationRequiresCurrentPassword(t *testing.T) {
	h := newHarness(t)
	h.account("dev1", store.RoleDeveloper, true)
	sess := h.session(mustAccountByName(t, h, "dev1"))
	if e := errBody(t, h.do("POST", "/api/v1/account/tokens", sess, apitypes.CreateAPITokenRequest{Name: "ci"}), http.StatusBadRequest, apitypes.ErrorCodeValidationFailed); e.Field != "current_password" {
		t.Fatalf("field %q", e.Field)
	}
	rr := h.do("POST", "/api/v1/account/tokens", sess, apitypes.CreateAPITokenRequest{Name: "ci", CurrentPassword: "wrong-password-123"})
	if e := errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeInvalidCurrentPassword); e.Field != "current_password" {
		t.Fatalf("field %q", e.Field)
	}
	if h.srv.loginIP.Count(clientIPForTests) != 1 || h.srv.loginUser.Count("dev1") != 1 {
		t.Fatal("wrong current password not counted per IP and per username")
	}
	createToken(t, h, sess, "ci")
	if list := decode[apitypes.APITokenList](t, h.do("GET", "/api/v1/account/tokens", sess, nil)); len(list.Items) != 1 {
		t.Fatalf("tokens %+v", list)
	}
}

func mustAccountByName(t *testing.T, h *harness, name string) store.Account {
	t.Helper()
	a, err := h.st.GetAccountByUsername(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestF5_PasswordChangeRevokesTokens: by default a password change deletes
// the account's tokens and says how many; revoke_tokens=false keeps them.
// Password-change failures are limited per IP and per username.
func TestF5_PasswordChangeRevokesTokens(t *testing.T) {
	h := newHarness(t)
	h.account("dev1", store.RoleDeveloper, true)
	sess := mustLogin(t, h, "dev1").Token
	t1 := createToken(t, h, sess, "a")
	t2 := createToken(t, h, sess, "b")

	// From an address dev1 has not logged in from (the harness address is
	// known-good for dev1 after mustLogin, so it skips the username limit).
	rr := send(h, reqOpts{method: "POST", path: "/api/v1/account/password", token: sess, remote: "203.0.113.9:1",
		body: apitypes.ChangePasswordRequest{CurrentPassword: "wrong-password-123", NewPassword: "another-good-password"}})
	errBody(t, rr, http.StatusBadRequest, apitypes.ErrorCodeInvalidCurrentPassword)
	if h.srv.loginIP.Count("203.0.113.9") != 1 || h.srv.loginUser.Count("dev1") != 1 {
		t.Fatal("password change failure not counted per IP and per username")
	}

	rr = h.do("POST", "/api/v1/account/password", sess, apitypes.ChangePasswordRequest{CurrentPassword: testPassword, NewPassword: "another-good-password", RevokeTokens: ptr(false)})
	resp := decode[apitypes.ChangePasswordResponse](t, rr)
	if rr.Code != http.StatusOK || resp.TokensRevoked != 0 {
		t.Fatalf("keep tokens: %d %+v", rr.Code, resp)
	}
	if rr := h.do("GET", "/api/v1/status", t1.Secret, nil); rr.Code != http.StatusOK {
		t.Fatalf("kept token rejected: %d", rr.Code)
	}

	rr = h.do("POST", "/api/v1/account/password", resp.Token, apitypes.ChangePasswordRequest{CurrentPassword: "another-good-password", NewPassword: "a-third-good-password"})
	resp = decode[apitypes.ChangePasswordResponse](t, rr)
	if rr.Code != http.StatusOK || resp.TokensRevoked != 2 || resp.Token == "" || resp.Account.Username != "dev1" {
		t.Fatalf("revoke tokens: %d %+v", rr.Code, resp)
	}
	for _, tok := range []string{t1.Secret, t2.Secret} {
		errBody(t, h.do("GET", "/api/v1/status", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	}
}

// TestF5_UnapproveThenReapproveDoesNotReviveTokens.
func TestF5_UnapproveThenReapproveDoesNotReviveTokens(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	dev := h.account("dev1", store.RoleDeveloper, true)
	adminTok := h.session(admin)
	tok := createToken(t, h, h.session(dev), "ci").Secret
	path := "/api/v1/accounts/" + strconv.FormatInt(dev.ID, 10)
	for _, approved := range []bool{false, true} {
		if rr := h.do("PATCH", path, adminTok, apitypes.UpdateAccountRequest{Approved: ptr(approved)}); rr.Code != http.StatusOK {
			t.Fatalf("approved=%v: %d", approved, rr.Code)
		}
	}
	errBody(t, h.do("GET", "/api/v1/status", tok, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
	if toks, _ := h.st.ListAPITokens(context.Background(), dev.ID); len(toks) != 0 {
		t.Fatalf("tokens kept: %d", len(toks))
	}
}

// TestF5_AdminCanListAndRevokeOthersTokens.
func TestF5_AdminCanListAndRevokeOthersTokens(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	dev := h.account("dev1", store.RoleDeveloper, true)
	other := h.account("dev2", store.RoleDeveloper, true)
	adminTok := h.session(admin)
	created := createToken(t, h, h.session(dev), "laptop")
	base := "/api/v1/accounts/" + strconv.FormatInt(dev.ID, 10) + "/tokens"

	rr := h.do("GET", base, adminTok, nil)
	if strings.Contains(rr.Body.String(), auth.HashAPIToken(created.Secret)) || strings.Contains(rr.Body.String(), created.Secret) {
		t.Fatal("admin listing leaks the secret or its hash")
	}
	list := decode[apitypes.APITokenList](t, rr)
	if len(list.Items) != 1 || list.Items[0].ID != created.Token.ID || list.Items[0].Prefix != created.Token.Prefix {
		t.Fatalf("list %+v", list)
	}
	errBody(t, h.do("GET", "/api/v1/accounts/999999/tokens", adminTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	// The token id must belong to the account in the path.
	wrong := "/api/v1/accounts/" + strconv.FormatInt(other.ID, 10) + "/tokens/" + strconv.FormatInt(created.Token.ID, 10)
	errBody(t, h.do("DELETE", wrong, adminTok, nil), http.StatusNotFound, apitypes.ErrorCodeNotFound)
	// Developers cannot use the admin routes.
	errBody(t, h.do("GET", base, h.session(other), nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)

	if rr := h.do("DELETE", base+"/"+strconv.FormatInt(created.Token.ID, 10), adminTok, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
	}
	errBody(t, h.do("GET", "/api/v1/status", created.Secret, nil), http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated)
}

// ---------------------------------------------------------------- F6

func startTestJob(t *testing.T, h *harness, creator *store.Account, access Access, subjectID int64) apitypes.Job {
	t.Helper()
	ctx := context.WithValue(context.Background(), routeAccessKey, access)
	if creator != nil {
		ctx = context.WithValue(ctx, identityKey, &Identity{Account: *creator, Kind: KindSession})
	}
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	job, err := h.srv.startJob(req, apitypes.JobKindServerStart, apitypes.SubjectTypeServer, subjectID, func(ctx context.Context, log io.Writer) error {
		_, _ = io.WriteString(log, "sensitive output\n")
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the job is running and has written its output.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, _ := os.ReadFile(h.runner.LogPath(job.ID)); strings.Contains(string(b), "sensitive output") {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestF6_JobAccessMatrix: restricted jobs (started from admin routes) are
// visible to developers but only admins can read their log or cancel them;
// other jobs' logs are readable by developers and cancellable by the creator
// or an admin.
func TestF6_JobAccessMatrix(t *testing.T) {
	h := newHarness(t)
	admin := h.account("admin1", store.RoleAdmin, true)
	dev1 := h.account("dev1", store.RoleDeveloper, true)
	dev2 := h.account("dev2", store.RoleDeveloper, true)
	idents := map[string]string{
		"admin session":           h.session(admin),
		"developer creator":       h.session(dev1),
		"developer non-creator":   h.session(dev2),
		"creator's api token":     h.apiToken(dev1, nil),
		"admin's api token":       h.apiToken(admin, nil),
		"non-creator's api token": h.apiToken(dev2, nil),
	}
	restricted := startTestJob(t, h, &admin, AccessAdmin, 1)
	open := startTestJob(t, h, &dev1, AccessDeveloper, 2)
	if !restricted.Restricted || open.Restricted {
		t.Fatalf("restricted flags: %v %v", restricted.Restricted, open.Restricted)
	}
	jp := func(j apitypes.Job) string { return "/api/v1/jobs/" + strconv.FormatInt(j.ID, 10) }

	certificate, err := h.runner.Start(t.Context(), "certificate.renew", jobs.Subject{Type: "certificate", ID: "acme"},
		&admin.ID, func(ctx context.Context, log io.Writer) error { <-ctx.Done(); return ctx.Err() }, jobs.Restricted(true))
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := jp(toAPIJob(certificate))

	for name, tok := range idents {
		// Everyone sees both jobs and their flag.
		if got := decode[apitypes.Job](t, h.do("GET", jp(restricted), tok, nil)); !got.Restricted {
			t.Errorf("%s: restricted flag missing", name)
		}
		adminSession := name == "admin session"
		wantAll, wantCertificates := 2, 0
		if adminSession {
			wantAll, wantCertificates = 3, 1
		}
		for query, n := range map[string]int{"": wantAll, "?subject_type=certificate&subject_id=acme": wantCertificates} {
			if list := decode[apitypes.JobList](t, h.do("GET", "/api/v1/jobs"+query, tok, nil)); len(list.Items) != n {
				t.Errorf("%s: %s has %d jobs, want %d", name, query, len(list.Items), n)
			}
		}
		latest := decode[apitypes.JobList](t, h.do("GET", "/api/v1/jobs?limit=1", tok, nil))
		wantLatest := open.ID
		if adminSession {
			wantLatest = certificate.ID
		}
		if len(latest.Items) != 1 || latest.Items[0].ID != wantLatest {
			t.Errorf("%s: incorrect limit after visibility filter: %+v", name, latest)
		}
		for _, tc := range []struct{ method, suffix string }{{"GET", ""}, {"GET", "/log"}, {"POST", "/cancel"}} {
			rec := h.do(tc.method, certificatePath+tc.suffix, tok, nil)
			if adminSession {
				if rec.Code != http.StatusOK && !(tc.suffix == "/cancel" && rec.Code == http.StatusAccepted) {
					t.Errorf("admin certificate job %s: %d", tc.suffix, rec.Code)
				}
			} else {
				errBody(t, rec, http.StatusForbidden, apitypes.ErrorCodeForbidden)
			}
		}
		// Logs.
		rr := h.do("GET", jp(restricted)+"/log", tok, nil)
		if name == "admin session" {
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "sensitive output") {
				t.Errorf("%s: restricted log %d", name, rr.Code)
			}
		} else {
			errBody(t, rr, http.StatusForbidden, apitypes.ErrorCodeForbidden)
		}
		if rr := h.do("GET", jp(open)+"/log", tok, nil); rr.Code != http.StatusOK {
			t.Errorf("%s: open log %d", name, rr.Code)
		}
	}

	// Cancel: denials first, then the permitted identities.
	for _, name := range []string{"developer creator", "developer non-creator", "creator's api token", "admin's api token", "non-creator's api token"} {
		errBody(t, h.do("POST", jp(restricted)+"/cancel", idents[name], nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	}
	for _, name := range []string{"developer non-creator", "admin's api token", "non-creator's api token"} {
		errBody(t, h.do("POST", jp(open)+"/cancel", idents[name], nil), http.StatusForbidden, apitypes.ErrorCodeForbidden)
	}
	for _, name := range []string{"creator's api token", "developer creator", "admin session"} {
		rr := h.do("POST", jp(open)+"/cancel", idents[name], nil)
		if rr.Code != http.StatusAccepted && rr.Code != http.StatusConflict {
			t.Errorf("%s: cancel open job %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if rr := h.do("POST", jp(restricted)+"/cancel", idents["admin session"], nil); rr.Code != http.StatusAccepted {
		t.Fatalf("admin cancel restricted: %d", rr.Code)
	}
	<-h.runner.Done(restricted.ID)
	<-h.runner.Done(open.ID)
}

// TestF6_FollowStreamsCappedPerAccount: a fifth concurrent ?follow=1 stream
// for one account is refused; closing one frees its slot.
func TestF6_FollowStreamsCappedPerAccount(t *testing.T) {
	h := newHarness(t)
	dev1 := h.account("dev1", store.RoleDeveloper, true)
	dev2 := h.account("dev2", store.RoleDeveloper, true)
	job := startTestJob(t, h, &dev1, AccessDeveloper, 3)
	ts := httptest.NewServer(h.srv.Handler())
	defer ts.Close()
	url := ts.URL + "/api/v1/jobs/" + strconv.FormatInt(job.ID, 10) + "/log?follow=1"
	open := func(tok string) (*http.Response, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		return resp, cancel
	}
	tok1 := h.session(dev1)
	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for i := 0; i < logStreamsPerAccount; i++ {
		resp, cancel := open(tok1)
		cancels = append(cancels, cancel)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %d", i, resp.StatusCode)
		}
	}
	resp, cancel := open(tok1)
	cancel()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("fifth stream: %d", resp.StatusCode)
	}
	// Another account has its own allowance.
	resp, cancel = open(h.session(dev2))
	cancels = append(cancels, cancel)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other account: %d", resp.StatusCode)
	}
	// Disconnecting releases the slot.
	cancels[0]()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, cancel := open(tok1)
		if resp.StatusCode == http.StatusOK {
			cancels = append(cancels, cancel)
			break
		}
		cancel()
		if time.Now().After(deadline) {
			t.Fatal("slot not released after disconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rr := h.do("POST", "/api/v1/jobs/"+strconv.FormatInt(job.ID, 10)+"/cancel", tok1, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d", rr.Code)
	}
	<-h.runner.Done(job.ID)
}

// ---------------------------------------------------------------- F7

// TestF7_HostAllowlist: a DNS-rebinding host name is refused for API and SPA
// paths on a loopback bind; IP literals, localhost and configured names are
// served; a missing Host is refused.
func TestF7_HostAllowlist(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.PublicHosts = []string{"Deploy.Example.com."} })
	logs := captureLogs(h)
	for _, host := range []string{"evil.example", "evil.example:8000", "localhost.evil.example", "127.0.0.1.nip.io", "example.com", ""} {
		for _, path := range []string{"/api/v1/status", "/", "/servers"} {
			rr := send(h, reqOpts{method: "GET", path: path, host: host})
			if host == "" {
				// httptest.NewRequest fills an empty Host; clear it explicitly.
				req := httptest.NewRequest("GET", path, nil)
				req.Host = ""
				rr = httptest.NewRecorder()
				h.srv.Handler().ServeHTTP(rr, req)
			}
			errBody(t, rr, http.StatusMisdirectedRequest, apitypes.ErrorCodeBadHost)
			if strings.Contains(rr.Body.String(), "<html") {
				t.Fatalf("%q %s served HTML", host, path)
			}
		}
	}
	for _, host := range []string{"127.0.0.1:8000", "127.0.0.1", "[::1]:8000", "[::1]", "localhost:8000", "LOCALHOST", "localhost.", "192.0.2.4:443", "deploy.example.com", "DEPLOY.example.com:8443"} {
		if rr := send(h, reqOpts{method: "GET", path: "/", host: host}); rr.Code != http.StatusOK {
			t.Errorf("host %q: %d %s", host, rr.Code, rr.Body.String())
		}
	}
	if n := strings.Count(logs.String(), "rejected request for unknown host"); n != 1 {
		t.Fatalf("bad-host log lines: %d, want 1 (sampled)", n)
	}
	if _, err := NormalizePublicHosts([]string{"https://x.example"}); err == nil {
		t.Fatal("scheme accepted in public host")
	}
	if _, err := NormalizePublicHosts([]string{"x.example:443"}); err == nil {
		t.Fatal("port accepted in public host")
	}
	if got, err := NormalizePublicHosts([]string{" A.Example. ", ""}); err != nil || len(got) != 1 || got[0] != "a.example" {
		t.Fatalf("normalise: %v %v", got, err)
	}
}

// ---------------------------------------------------------------- F8

// TestF8_RegistrationEnumerationIsDocumented: registration necessarily
// reveals whether a username exists; the API doc says so next to the
// setting.
func TestF8_RegistrationEnumerationIsDocumented(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "API.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "reveals whether a username exists") {
		t.Fatal("docs/API.md does not document username enumeration through registration")
	}
}

// ---------------------------------------------------------------- other items

// TestCurrentAccountReachableByAPIToken: GET /account tells sessions and API
// tokens apart and lets a token identify itself.
func TestCurrentAccountReachableByAPIToken(t *testing.T) {
	h := newHarness(t)
	dev := h.account("dev1", store.RoleDeveloper, true)
	sess := h.session(dev)
	me := decode[apitypes.CurrentAccount](t, h.do("GET", "/api/v1/account", sess, nil))
	if me.Auth != apitypes.AuthKindSession || me.APIToken != nil || me.Username != "dev1" {
		t.Fatalf("session me %+v", me)
	}
	tok := createToken(t, h, sess, "ci-bot")
	rr := h.do("GET", "/api/v1/account", tok.Secret, nil)
	if !strings.Contains(rr.Body.String(), `"api_token":{`) {
		t.Fatalf("body %s", rr.Body.String())
	}
	me = decode[apitypes.CurrentAccount](t, rr)
	if me.Auth != apitypes.AuthKindAPIToken || me.APIToken == nil || me.APIToken.Name != "ci-bot" || me.APIToken.Prefix != tok.Token.Prefix || me.ID != dev.ID {
		t.Fatalf("token me %+v", me)
	}
	if strings.Contains(rr.Body.String(), tok.Secret) {
		t.Fatal("secret echoed")
	}
	// Everything else under /account stays session-only.
	errBody(t, h.do("GET", "/api/v1/account/tokens", tok.Secret, nil), http.StatusForbidden, apitypes.ErrorCodeSessionRequired)
}

func TestCheckListenResolvesLocalhost(t *testing.T) {
	loop := func(string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}, nil
	}
	if err := checkListen("localhost:8000", false, false, loop); err != nil {
		t.Fatalf("loopback localhost refused: %v", err)
	}
	hijacked := func(string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("192.0.2.7")}, nil
	}
	if err := checkListen("localhost:8000", false, false, hijacked); !errors.Is(err, ErrPlaintextNonLoopback) {
		t.Fatalf("non-loopback localhost accepted: %v", err)
	}
	failing := func(string) ([]netip.Addr, error) { return nil, errors.New("no resolver") }
	if err := checkListen("localhost:8000", false, false, failing); !errors.Is(err, ErrPlaintextNonLoopback) {
		t.Fatalf("unresolvable localhost accepted: %v", err)
	}
	if err := checkListen("localhost:8000", true, false, failing); err != nil {
		t.Fatalf("TLS bind needs no resolution: %v", err)
	}
}
