package inspector

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// A ticket enters once: the session cookie is set with its attributes, the
// hub's cookies never reach the browser, and the redirect carries the
// ticket's target (never the query's).
func TestEnter(t *testing.T) {
	e := newTestEnv(t, nil)
	ctx := context.Background()
	p := 7001
	arena, err := e.st.CreateServer(ctx, store.Server{Name: "arena", Port: &p, Inspector: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2 := 7002
	plain, _ := e.st.CreateServer(ctx, store.Server{Name: "plain", Port: &p2}, nil)
	ses, err := e.s.CreateSession(ctx, e.acct, &arena.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ses.Path, EnterPath+"?ticket=") || !strings.HasSuffix(ses.Path, "&targetId=arena") ||
		ses.Port != e.s.port() || !ses.ExpiresAt.Equal(e.clock.Now().Add(TicketTTL)) {
		t.Fatalf("%+v", ses)
	}
	resp := e.do("GET", ses.Path+"&targetId=evil", "", nil, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?targetId=arena" {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name != SessionCookie {
			t.Fatalf("cookie %q reached the browser", c.Name)
		}
		cookie = c
	}
	if cookie == nil || !cookie.HttpOnly || cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" ||
		cookie.MaxAge != int(SessionMaxAge/time.Second) {
		t.Fatalf("%+v", cookie)
	}
	if again := e.do("GET", ses.Path, "", nil, nil); again.StatusCode != http.StatusForbidden || !strings.Contains(readBody(t, again), "already used") {
		t.Fatalf("second use: %d", again.StatusCode)
	}
	if head := e.do("HEAD", ses.Path, "", nil, nil); head.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD: %d", head.StatusCode)
	}
	// The session request's own errors.
	var fe *FieldError
	if _, err := e.s.CreateSession(ctx, e.acct, &plain.ID); !errors.As(err, &fe) || fe.Field != "server_id" {
		t.Fatalf("switch off: %v", err)
	}
	missing := int64(999)
	if _, err := e.s.CreateSession(ctx, e.acct, &missing); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("unknown server: %v", err)
	}
	if ses, err := e.s.CreateSession(ctx, e.acct, nil); err != nil || strings.Contains(ses.Path, "targetId") {
		t.Fatalf("%+v %v", ses, err)
	}
	// Expired tickets do not enter.
	ses, _ = e.s.CreateSession(ctx, e.acct, nil)
	e.clock.Advance(TicketTTL)
	if r := e.do("GET", ses.Path, "", nil, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("expired ticket: %d", r.StatusCode)
	}
	// A ticket of an account whose password changed meanwhile does not.
	ses, _ = e.s.CreateSession(ctx, e.acct, nil)
	if err := e.st.BumpTokenVersion(ctx, e.acct.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", ses.Path, "", nil, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("stale ticket: %d", r.StatusCode)
	}
	// Not running: no ticket.
	e.s.closeListener()
	if _, err := e.s.CreateSession(ctx, e.acct, nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("not running: %v", err)
	}
}

// Failed entries are limited per client address.
func TestEnterRateLimit(t *testing.T) {
	e := newTestEnv(t, nil)
	for i := 0; i < entryFailuresPerWindow; i++ {
		if r := e.do("GET", EnterPath+"?ticket=x", "", nil, nil); r.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	ses, _ := e.s.CreateSession(context.Background(), e.acct, nil)
	if r := e.do("GET", ses.Path, "", nil, nil); r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("limited: %d", r.StatusCode)
	}
}

// Proxied requests carry only the session's hub cookie: the browser's
// cookies, credentials and forwarding headers are dropped, the Host is the
// hub's, and the hub's Set-Cookie is removed.
func TestProxyHeaders(t *testing.T) {
	e := newTestEnv(t, nil)
	e.hub.set(func(h *fakeHub) { h.targets = []map[string]any{{"targetId": "arena", "connected": true}} })
	cookie := e.enter(nil)
	req, _ := http.NewRequest("GET", e.base()+"/api/targets", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	req.AddCookie(&http.Cookie{Name: hubCookie, Value: "forged"})
	req.AddCookie(&http.Cookie{Name: "panel", Value: "other"})
	req.Header.Set("Authorization", "Bearer gd_token")
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("Forwarded", "for=6.6.6.6")
	req.Header.Set("X-Real-Ip", "6.6.6.6")
	resp, err := plainClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(body, "arena") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("Set-Cookie reached the browser: %v", resp.Header.Values("Set-Cookie"))
	}
	got, _ := e.hub.last("/api/targets")
	cookies := got.Header.Values("Cookie")
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0], hubCookie+"=") || strings.Contains(cookies[0], "forged") ||
		strings.Contains(cookies[0], "panel") || strings.Contains(cookies[0], cookie) {
		t.Fatalf("cookies at the hub: %q", cookies)
	}
	for _, h := range []string{"Authorization", "X-Forwarded-For", "Forwarded", "X-Real-Ip", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("%s reached the hub: %q", h, v)
		}
	}
	if got.Host != hubHost {
		t.Fatalf("host %q", got.Host)
	}
	if strings.Contains(e.logs.String(), cookie) {
		t.Fatal("session cookie in the daemon log")
	}
}

// WebSocket upgrades and requests other than GET and HEAD need the Origin
// the browser used; it is rewritten to the hub's origin.
func TestOriginRules(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.enter(nil)
	own := "http://" + e.addr()
	for _, o := range []string{"", "http://evil.example", "https://" + e.addr(), "null"} {
		hdr := map[string]string{"Content-Type": "application/json"}
		if o != "" {
			hdr["Origin"] = o
		}
		r := e.do("POST", "/api/anything", cookie, hdr, strings.NewReader(`{}`))
		if r.StatusCode != http.StatusForbidden || !strings.Contains(readBody(t, r), "origin_not_allowed") {
			t.Fatalf("origin %q: %d", o, r.StatusCode)
		}
	}
	r := e.do("POST", "/api/anything", cookie, map[string]string{"Origin": own}, strings.NewReader(`{"a":1}`))
	if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, `{\"a\":1}`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if got, _ := e.hub.last("/api/anything"); got.Header.Get("Origin") != HubOrigin {
		t.Fatalf("origin at the hub %q", got.Header.Get("Origin"))
	}
	// GET needs no Origin.
	if r := e.do("GET", "/auth/session", cookie, nil, nil); r.StatusCode != 200 {
		t.Fatalf("GET: %d", r.StatusCode)
	}
	hdr := http.Header{"Cookie": {SessionCookie + "=" + cookie}}
	if _, _, code := dialWS(t, e.addr(), "/ws/client", hdr); code != http.StatusForbidden {
		t.Fatalf("WebSocket without Origin: %d", code)
	}
	hdr.Set("Origin", own)
	conn, br, code := dialWS(t, e.addr(), "/ws/client", hdr)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket: %d", code)
	}
	defer conn.Close()
	echoOnce(t, conn, br, "hello")
	// Large bodies are refused.
	big := strings.Repeat("x", maxProxyBody+1)
	if r := e.do("POST", "/api/anything", cookie, map[string]string{"Origin": own}, strings.NewReader(big)); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body: %d", r.StatusCode)
	}
}

// With --inspector-public-url the expected Origin is that URL, and its
// host passes the Host policy.
func TestPublicURL(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.PublicURL = "https://inspector.example.com" })
	cookie := e.enter(nil)
	host := map[string]string{"Host": "inspector.example.com"}
	if r := e.do("GET", "/auth/session", cookie, host, nil); r.StatusCode != 200 {
		t.Fatalf("public host: %d", r.StatusCode)
	}
	if r := e.do("POST", "/api/x", cookie, map[string]string{"Origin": "http://" + e.addr()}, strings.NewReader("{}")); r.StatusCode != http.StatusForbidden {
		t.Fatalf("own origin with a public URL: %d", r.StatusCode)
	}
	if r := e.do("POST", "/api/x", cookie, map[string]string{"Origin": "https://inspector.example.com"}, strings.NewReader("{}")); r.StatusCode != 200 {
		t.Fatalf("public origin: %d", r.StatusCode)
	}
	if r := e.do("GET", "/auth/session", cookie, map[string]string{"Host": "evil.example"}, nil); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("bad host: %d", r.StatusCode)
	}
	ses, _ := e.s.CreateSession(context.Background(), e.acct, nil)
	if ses.PublicURL != "https://inspector.example.com" {
		t.Fatalf("%+v", ses)
	}
	// The cookie is Secure behind an https proxy.
	resp := e.do("GET", ses.Path, "", nil, nil)
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie && !c.Secure {
			t.Fatal("cookie not Secure behind https")
		}
	}
}

// Login, agent and metrics paths never reach the hub from browsers.
func TestBlockedPaths(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.enter(nil)
	own := map[string]string{"Origin": "http://" + e.addr()}
	for _, p := range []string{"/auth/login", "/metrics", "/auth/../auth/login", "//metrics"} {
		for _, m := range []string{"GET", "POST"} {
			if r := e.do(m, p, cookie, own, nil); r.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: %d", m, p, r.StatusCode)
			}
		}
	}
	if _, ok := e.hub.last("/metrics"); ok {
		t.Fatal("metrics reached the hub")
	}
	if e.hub.loginCount() != 1 {
		t.Fatalf("logins %d", e.hub.loginCount())
	}
}

// Without a valid session a page navigation gets the static page and
// anything else the hub's JSON error.
func TestUnauthenticated(t *testing.T) {
	e := newTestEnv(t, nil)
	r := e.do("GET", "/", "", map[string]string{"Accept": "text/html,application/xhtml+xml"}, nil)
	body := readBody(t, r)
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "session has ended") || !strings.Contains(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
	r = e.do("GET", "/api/targets", "bogus-cookie-value-that-is-not-a-session-0000000", map[string]string{"Accept": "application/json"}, nil)
	if body := readBody(t, r); r.StatusCode != http.StatusUnauthorized || strings.TrimSpace(body) != `{"error":"auth_required"}` {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
	if _, _, code := dialWS(t, e.addr(), "/ws/client", http.Header{"Origin": {"http://" + e.addr()}}); code != http.StatusUnauthorized {
		t.Fatalf("WebSocket: %d", code)
	}
	if r := e.do("POST", "/auth/logout", "", map[string]string{"Origin": "http://" + e.addr()}, nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	e.hub.mu.Lock()
	n := len(e.hub.requests)
	e.hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d unauthenticated requests reached the hub", n)
	}
}

// After a hub restart the daemon logs in again for a valid session,
// transparently.
func TestReloginAfterHubRestart(t *testing.T) {
	e := newTestEnv(t, nil)
	cookie := e.enter(nil)
	e.hub.restart()
	e.clock.Advance(time.Minute)
	if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, readBody(t, r))
	}
	if n := e.hub.loginCount(); n != 2 {
		t.Fatalf("logins %d", n)
	}
	// /auth/session answers 200 {"authenticated":false} after a restart.
	e.hub.restart()
	e.clock.Advance(time.Minute)
	r := e.do("GET", "/auth/session", cookie, nil, nil)
	if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, `"authenticated":true`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// A WebSocket upgrade is repeated too.
	e.hub.restart()
	e.clock.Advance(time.Minute)
	conn, br, code := dialWS(t, e.addr(), "/ws/client", http.Header{"Cookie": {SessionCookie + "=" + cookie}, "Origin": {"http://" + e.addr()}})
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("WebSocket after restart: %d", code)
	}
	defer conn.Close()
	echoOnce(t, conn, br, "x")
	if n := e.hub.loginCount(); n != 4 {
		t.Fatalf("logins %d", n)
	}
}

// A refused hub login is never retried in a loop.
func TestNoLoginLoop(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  string
	}{{http.StatusUnauthorized, ""}, {http.StatusTooManyRequests, "60"}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			e := newTestEnv(t, nil)
			cookie := e.enter(nil)
			e.hub.restart()
			e.hub.set(func(h *fakeHub) { h.loginStatus, h.retryAfter = tc.status, tc.retry })
			e.clock.Advance(time.Minute)
			for i := 0; i < 5; i++ {
				if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != http.StatusUnauthorized {
					t.Fatalf("request %d: %d", i, r.StatusCode)
				}
				e.clock.Advance(10 * time.Second)
			}
			if n := e.hub.loginCount(); n != 2 {
				t.Fatalf("logins %d, want the entry and one attempt", n)
			}
			// New entries are refused at once, without a login.
			ses, _ := e.s.CreateSession(context.Background(), e.acct, nil)
			if r := e.do("GET", ses.Path, "", nil, nil); r.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("entry: %d", r.StatusCode)
			}
			if n := e.hub.loginCount(); n != 2 {
				t.Fatalf("logins %d", n)
			}
			if tc.status == http.StatusTooManyRequests {
				// The pause ends with the hub's Retry-After.
				e.hub.set(func(h *fakeHub) { h.loginStatus = 0 })
				e.clock.Advance(time.Minute)
				if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != 200 {
					t.Fatalf("after the pause: %d", r.StatusCode)
				}
			}
		})
	}
}

// Sessions end when their account changes (checked on every request and
// by the sweep, which closes open streams), when they expire, at sign-out
// and when the listener closes.
func TestSessionEnd(t *testing.T) {
	ctx := context.Background()
	t.Run("sweep closes streams", func(t *testing.T) {
		e := newTestEnv(t, nil)
		cookie := e.enter(nil)
		conn, br, code := dialWS(t, e.addr(), "/ws/client", http.Header{"Cookie": {SessionCookie + "=" + cookie}, "Origin": {"http://" + e.addr()}})
		if code != http.StatusSwitchingProtocols {
			t.Fatal(code)
		}
		defer conn.Close()
		echoOnce(t, conn, br, "before")
		if err := e.st.BumpTokenVersion(ctx, e.acct.ID); err != nil {
			t.Fatal(err)
		}
		e.s.sweepSessions(ctx)
		waitClosed(t, conn, br)
		if e.s.sessions.count() != 0 {
			t.Fatal("session kept")
		}
		select {
		case <-e.hub.streamClosed:
		case <-time.After(5 * time.Second):
			t.Fatal("hub stream not closed")
		}
		if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("after the sweep: %d", r.StatusCode)
		}
	})
	t.Run("checked per request", func(t *testing.T) {
		e := newTestEnv(t, nil)
		cookie := e.enter(nil)
		no := false
		admin, _ := e.st.CreateAccount(ctx, store.NewAccount{Username: "root", PasswordHash: "x", Role: store.RoleAdmin, Approved: true})
		if _, err := e.st.UpdateAccount(ctx, admin.ID, e.acct.ID, store.AccountUpdate{Approved: &no}); err != nil {
			t.Fatal(err)
		}
		if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unapproved: %d", r.StatusCode)
		}
		if e.s.sessions.count() != 0 {
			t.Fatal("session kept")
		}
	})
	t.Run("expiry", func(t *testing.T) {
		e := newTestEnv(t, nil)
		cookie := e.enter(nil)
		e.clock.Advance(SessionMaxAge - time.Minute)
		if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != 200 {
			t.Fatalf("before expiry: %d", r.StatusCode)
		}
		e.clock.Advance(2 * time.Minute)
		if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("after expiry: %d", r.StatusCode)
		}
	})
	t.Run("sign-out", func(t *testing.T) {
		e := newTestEnv(t, nil)
		cookie := e.enter(nil)
		if r := e.do("POST", "/auth/logout", cookie, nil, nil); r.StatusCode != http.StatusForbidden {
			t.Fatalf("logout without Origin: %d", r.StatusCode)
		}
		r := e.do("POST", "/auth/logout", cookie, map[string]string{"Origin": "http://" + e.addr()}, nil)
		if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, `"authenticated":false`) {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
		cleared := false
		for _, c := range r.Cookies() {
			if c.Name == SessionCookie && c.MaxAge < 0 {
				cleared = true
			}
		}
		e.hub.mu.Lock()
		logouts := len(e.hub.logouts)
		e.hub.mu.Unlock()
		if !cleared || logouts != 1 {
			t.Fatalf("cleared %v, hub logouts %d", cleared, logouts)
		}
		if r := e.do("GET", "/api/targets", cookie, nil, nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("after sign-out: %d", r.StatusCode)
		}
	})
	t.Run("listener close", func(t *testing.T) {
		e := newTestEnv(t, nil)
		cookie := e.enter(nil)
		conn, br, code := dialWS(t, e.addr(), "/ws/client", http.Header{"Cookie": {SessionCookie + "=" + cookie}, "Origin": {"http://" + e.addr()}})
		if code != http.StatusSwitchingProtocols {
			t.Fatal(code)
		}
		defer conn.Close()
		e.s.Close()
		waitClosed(t, conn, br)
	})
}
