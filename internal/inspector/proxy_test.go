package inspector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// The entry page, the ended page and the two scripts are the daemon's own,
// served without a session and with headers that allow nothing beyond what
// they need.
func TestOwnPages(t *testing.T) {
	e := newTestEnv(t, nil)
	for _, method := range []string{"GET", "HEAD"} {
		// The query is ignored: a link of the old form consumes nothing.
		r := e.do(method, EnterPath+"?ticket=x", "", nil, nil)
		body := readBody(t, r)
		csp := r.Header.Get("Content-Security-Policy")
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") || r.Header.Get("Cache-Control") != "no-store" ||
			r.Header.Get("Referrer-Policy") != "no-referrer" || r.Header.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "connect-src 'self'") ||
			!strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe") {
			t.Fatalf("%s entry page: %d %v", method, r.StatusCode, r.Header)
		}
		if method == "GET" && (!strings.Contains(body, `<script src="/_golem-deploy/enter.js"></script>`) || strings.Count(body, "<script") != 1) {
			t.Fatalf("entry page body %s", body)
		}
	}
	r := e.do("GET", EndedPath, "", nil, nil)
	body := readBody(t, r)
	if csp := r.Header.Get("Content-Security-Policy"); r.StatusCode != 200 || !strings.Contains(body, "session has ended") || strings.Contains(body, "<script") ||
		strings.Contains(csp, "script-src") || strings.Contains(csp, "connect-src") || strings.Contains(csp, "unsafe") {
		t.Fatalf("ended page: %d %s %v", r.StatusCode, body, r.Header)
	}
	for p, want := range map[string]string{bootJSPath: bootJS, enterJSPath: enterJS} {
		r := e.do("GET", p, "", nil, nil)
		if b := readBody(t, r); r.StatusCode != 200 || b != want || r.Header.Get("Content-Type") != "text/javascript; charset=utf-8" ||
			r.Header.Get("Cache-Control") != "no-cache" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: %d %v", p, r.StatusCode, r.Header)
		}
		for i := 0; i < len(want); i++ {
			if want[i] >= 0x80 {
				t.Fatalf("%s is not ASCII at byte %d", p, i)
			}
		}
		h := e.do("HEAD", p, "", nil, nil)
		if b := readBody(t, h); h.StatusCode != 200 || b != "" || h.Header.Get("Content-Length") != strconv.Itoa(len(want)) {
			t.Fatalf("HEAD %s: %d %q", p, h.StatusCode, h.Header.Get("Content-Length"))
		}
	}
	for p, want := range map[string]int{EnterPath: 405, EndedPath: 405, bootJSPath: 405, "/_golem-deploy/other": 404, "/_golem-deploy/": 404} {
		if r := e.do("POST", p, "", map[string]string{"Origin": e.origin()}, nil); r.StatusCode != want {
			t.Errorf("POST %s: %d, want %d", p, r.StatusCode, want)
		}
	}
	if r := e.do("GET", SessionPath, "", nil, nil); r.StatusCode != 405 {
		t.Fatalf("GET session: %d", r.StatusCode)
	}
	e.hub.mu.Lock()
	hubRequests := len(e.hub.requests)
	e.hub.mu.Unlock()
	if e.s.sessions.count() != 0 || hubRequests != 0 {
		t.Fatal("the daemon's pages touched sessions or the hub")
	}
}

// A ticket is exchanged once, from the Inspector's origin, as bounded
// JSON, for a session key and the ticket's target; the link carries the
// ticket in its fragment only.
func TestSessionEntry(t *testing.T) {
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
	if strings.ContainsAny(ses.Path, "?&") || ses.Port != e.s.port() || !ses.ExpiresAt.Equal(e.clock.Now().Add(TicketTTL)) {
		t.Fatalf("%+v", ses)
	}
	tok := ticketOf(t, ses.Path)
	// Refusals before the ticket is looked at leave it usable.
	refusals := []struct {
		name string
		hdr  map[string]string
		want int
		code string
	}{
		{"no origin", map[string]string{"Origin": ""}, 403, "origin_not_allowed"},
		{"other origin", map[string]string{"Origin": "http://evil.example"}, 403, "origin_not_allowed"},
		{"https origin on http", map[string]string{"Origin": "https://" + e.addr()}, 403, "origin_not_allowed"},
		{"no content type", map[string]string{"Content-Type": ""}, 415, "unsupported_media_type"},
		{"form", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 415, "unsupported_media_type"},
		{"text", map[string]string{"Content-Type": "text/plain"}, 415, "unsupported_media_type"},
	}
	for _, c := range refusals {
		r := e.exchange(tok, c.hdr)
		if b := readBody(t, r); r.StatusCode != c.want || !strings.Contains(b, c.code) {
			t.Fatalf("%s: %d %s", c.name, r.StatusCode, b)
		}
	}
	big := `{"ticket":"` + strings.Repeat("a", maxSessionBody) + `"}`
	if r := e.do("POST", SessionPath, "", map[string]string{"Origin": e.origin(), "Content-Type": "application/json"}, strings.NewReader(big)); r.StatusCode != 413 {
		t.Fatalf("large body: %d", r.StatusCode)
	}
	if r := e.do("POST", SessionPath, "", map[string]string{"Origin": e.origin(), "Content-Type": "application/json; charset=utf-8"}, strings.NewReader("ticket=x")); r.StatusCode != 400 {
		t.Fatalf("not JSON: %d", r.StatusCode)
	}
	if e.hub.loginCount() != 0 {
		t.Fatal("refused exchanges logged in")
	}
	r := e.exchange(tok, nil)
	if r.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("%v", r.Header)
	}
	got := entryOf(t, r)
	e.keys = append(e.keys, got.Key)
	if got.TargetID != "arena" || e.s.sessions.count() != 1 || e.hub.loginCount() != 1 {
		t.Fatalf("%+v", got)
	}
	if r := e.do("GET", "/api/targets", got.Key, nil, nil); r.StatusCode != 200 {
		t.Fatalf("the key does not work: %d", r.StatusCode)
	}
	if again := e.exchange(tok, nil); again.StatusCode != 403 || !strings.Contains(readBody(t, again), "link_invalid") {
		t.Fatalf("second use: %d", again.StatusCode)
	}
	// Without a target the answer names none.
	ses, _ = e.s.CreateSession(ctx, e.acct, nil)
	r = e.exchange(ticketOf(t, ses.Path), nil)
	if b := readBody(t, r); r.StatusCode != 200 || strings.Contains(b, "target_id") {
		t.Fatalf("%d %s", r.StatusCode, b)
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
	// Expired tickets do not enter.
	ses, _ = e.s.CreateSession(ctx, e.acct, nil)
	e.clock.Advance(TicketTTL)
	if r := e.exchange(ticketOf(t, ses.Path), nil); r.StatusCode != 403 || !strings.Contains(readBody(t, r), "link_invalid") {
		t.Fatalf("expired ticket: %d", r.StatusCode)
	}
	// A ticket of an account whose password changed meanwhile does not.
	ses, _ = e.s.CreateSession(ctx, e.acct, nil)
	if err := e.st.BumpTokenVersion(ctx, e.acct.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.exchange(ticketOf(t, ses.Path), nil); r.StatusCode != 403 || !strings.Contains(readBody(t, r), "account_invalid") {
		t.Fatalf("stale ticket: %d", r.StatusCode)
	}
	// Not running: no ticket, and a ticket issued before does not enter.
	e.acct, _ = e.st.GetAccount(ctx, e.acct.ID)
	ses, _ = e.s.CreateSession(ctx, e.acct, nil)
	e.s.mu.Lock()
	e.s.enabled = false
	e.s.mu.Unlock()
	if r := e.exchange(ticketOf(t, ses.Path), nil); r.StatusCode != 503 || !strings.Contains(readBody(t, r), "not_running") {
		t.Fatalf("not running: %d", r.StatusCode)
	}
	e.s.mu.Lock()
	e.s.enabled = true
	e.s.mu.Unlock()
	e.s.closeListener()
	if _, err := e.s.CreateSession(ctx, e.acct, nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("not running: %v", err)
	}
}

// Failed exchanges are limited per client address.
func TestEnterRateLimit(t *testing.T) {
	e := newTestEnv(t, nil)
	for i := 0; i < entryFailuresPerWindow; i++ {
		if r := e.exchange(strings.Repeat("x", 43), nil); r.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	ses, _ := e.s.CreateSession(context.Background(), e.acct, nil)
	r := e.exchange(ticketOf(t, ses.Path), nil)
	if b := readBody(t, r); r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" || !strings.Contains(b, "too_many_attempts") {
		t.Fatalf("limited: %d %s", r.StatusCode, b)
	}
}

// Proxied requests carry only the session's hub cookie: the browser's
// cookies, credentials, key and forwarding headers are dropped, the Host
// is the hub's, and the hub's Set-Cookie and session header are removed.
func TestProxyHeaders(t *testing.T) {
	e := newTestEnv(t, nil)
	e.hub.set(func(h *fakeHub) { h.targets = []map[string]any{{"targetId": "arena", "connected": true}} })
	key := e.enter(nil)
	req, _ := http.NewRequest("GET", e.base()+"/api/targets", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.AddCookie(&http.Cookie{Name: "golem_deploy_inspector", Value: key})
	req.AddCookie(&http.Cookie{Name: hubCookie, Value: "forged"})
	req.AddCookie(&http.Cookie{Name: "panel", Value: "other"})
	req.Header.Set("Proxy-Authorization", "Basic eDp5")
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
		strings.Contains(cookies[0], "panel") {
		t.Fatalf("cookies at the hub: %q", cookies)
	}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Forwarded-For", "Forwarded", "X-Real-Ip", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("%s reached the hub: %q", h, v)
		}
	}
	if got.Host != hubHost {
		t.Fatalf("host %q", got.Host)
	}
	// A hub cannot end the browser's session by imitating the daemon.
	e.hub.set(func(h *fakeHub) {
		h.static = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(SessionHeader, "ended")
			w.Header().Set("Content-Type", "text/css")
		}
	})
	if r := e.do("GET", "/app.css", "", nil, nil); r.StatusCode != 200 || r.Header.Get(SessionHeader) != "" {
		t.Fatalf("hub session header: %d %v", r.StatusCode, r.Header)
	}
}

// The session key is accepted as a bearer header, or on a WebSocket
// upgrade as the key subprotocol offered with the marker; the answer to
// such an upgrade selects the marker. A cookie authenticates nothing.
func TestSessionCredentials(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	ended := func(name string, r *http.Response) {
		t.Helper()
		if b := readBody(t, r); r.StatusCode != http.StatusUnauthorized || strings.TrimSpace(b) != `{"error":"auth_required"}` ||
			r.Header.Get(SessionHeader) != "ended" {
			t.Errorf("%s: %d %s %v", name, r.StatusCode, b, r.Header)
		}
	}
	if r := e.do("GET", "/api/targets", "", map[string]string{"Authorization": "bearer " + key}, nil); r.StatusCode != 200 {
		t.Fatalf("lower-case scheme: %d", r.StatusCode)
	}
	ended("cookie only", e.do("GET", "/api/targets", "", map[string]string{"Cookie": "golem_deploy_inspector=" + key}, nil))
	ended("unknown key", e.do("GET", "/api/targets", strings.Repeat("A", 43), nil, nil))
	ended("other scheme", e.do("GET", "/api/targets", "", map[string]string{"Authorization": "Basic " + key}, nil))
	ended("subprotocol without an upgrade", e.do("GET", "/api/targets", "", map[string]string{
		"Sec-WebSocket-Protocol": WebSocketProtocol + ", " + WebSocketKeyPrefix + key}, nil))
	own := e.origin()
	conn, br, resp := dialWSResponse(t, e.addr(), "/ws/client", e.wsAuth(key))
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Protocol") != WebSocketProtocol {
		t.Fatalf("subprotocol upgrade: %d %v", resp.StatusCode, resp.Header)
	}
	echoOnce(t, conn, br, "hello")
	conn.Close()
	got, _ := e.hub.last("/ws/client")
	if got.Header.Get("Sec-WebSocket-Protocol") != "" || got.Header.Get("Authorization") != "" || !strings.HasPrefix(got.Header.Get("Cookie"), hubCookie+"=") {
		t.Fatalf("upgrade headers at the hub: %v", got.Header)
	}
	// A bearer header works for an upgrade too; nothing was offered, so
	// nothing is selected.
	conn, _, resp = dialWSResponse(t, e.addr(), "/ws/client", http.Header{"Authorization": {"Bearer " + key}, "Origin": {own}})
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatalf("bearer upgrade: %d %v", resp.StatusCode, resp.Header)
	}
	conn.Close()
	for name, protocols := range map[string]string{
		"key without the marker": WebSocketKeyPrefix + key,
		"marker without a key":   WebSocketProtocol,
		"two keys":               WebSocketProtocol + ", " + WebSocketKeyPrefix + key + ", " + WebSocketKeyPrefix + key,
		"unknown key":            WebSocketProtocol + ", " + WebSocketKeyPrefix + strings.Repeat("A", 43),
	} {
		if _, _, code := dialWS(t, e.addr(), "/ws/client", http.Header{"Sec-WebSocket-Protocol": {protocols}, "Origin": {own}}); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	if _, _, code := dialWS(t, e.addr(), "/ws/client", http.Header{"Cookie": {"golem_deploy_inspector=" + key}, "Origin": {own}}); code != http.StatusUnauthorized {
		t.Errorf("cookie upgrade: %d", code)
	}
}

// WebSocket upgrades and requests other than GET and HEAD need the Origin
// the browser used; it is rewritten to the hub's origin.
func TestOriginRules(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	own := e.origin()
	for _, o := range []string{"", "http://evil.example", "https://" + e.addr(), "null"} {
		hdr := map[string]string{"Content-Type": "application/json"}
		if o != "" {
			hdr["Origin"] = o
		}
		r := e.do("POST", "/api/anything", key, hdr, strings.NewReader(`{}`))
		if r.StatusCode != http.StatusForbidden || !strings.Contains(readBody(t, r), "origin_not_allowed") {
			t.Fatalf("origin %q: %d", o, r.StatusCode)
		}
	}
	r := e.do("POST", "/api/anything", key, map[string]string{"Origin": own}, strings.NewReader(`{"a":1}`))
	if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, `{\"a\":1}`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if got, _ := e.hub.last("/api/anything"); got.Header.Get("Origin") != HubOrigin {
		t.Fatalf("origin at the hub %q", got.Header.Get("Origin"))
	}
	// GET needs no Origin.
	if r := e.do("GET", "/auth/session", key, nil, nil); r.StatusCode != 200 {
		t.Fatalf("GET: %d", r.StatusCode)
	}
	hdr := e.wsAuth(key)
	hdr.Del("Origin")
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
	if r := e.do("POST", "/api/anything", key, map[string]string{"Origin": own}, strings.NewReader(big)); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body: %d", r.StatusCode)
	}
}

// With --inspector-public-url the expected Origin is that URL, and its
// host passes the Host policy.
func TestPublicURL(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.PublicURL = "https://inspector.example.com" })
	ses, _ := e.s.CreateSession(context.Background(), e.acct, nil)
	if ses.PublicURL != "https://inspector.example.com" {
		t.Fatalf("%+v", ses)
	}
	host := map[string]string{"Host": "inspector.example.com"}
	if r := e.exchange(ticketOf(t, ses.Path), map[string]string{"Origin": "http://" + e.addr()}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("exchange from the listener's own origin: %d", r.StatusCode)
	}
	key := e.enter(nil)
	if r := e.do("GET", "/auth/session", key, host, nil); r.StatusCode != 200 {
		t.Fatalf("public host: %d", r.StatusCode)
	}
	if r := e.do("POST", "/api/x", key, map[string]string{"Origin": "http://" + e.addr()}, strings.NewReader("{}")); r.StatusCode != http.StatusForbidden {
		t.Fatalf("own origin with a public URL: %d", r.StatusCode)
	}
	if r := e.do("POST", "/api/x", key, map[string]string{"Origin": "https://inspector.example.com"}, strings.NewReader("{}")); r.StatusCode != 200 {
		t.Fatalf("public origin: %d", r.StatusCode)
	}
	if r := e.do("GET", "/auth/session", key, map[string]string{"Host": "evil.example"}, nil); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("bad host: %d", r.StatusCode)
	}
}

// Login, agent, metrics and health paths never reach the hub from
// browsers, with or without a session.
func TestBlockedPaths(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	own := map[string]string{"Origin": e.origin()}
	for _, p := range []string{"/auth/login", "/metrics", "/health", "/live", "/ready", "/auth/../auth/login", "//metrics", "/x/../ready"} {
		for _, m := range []string{"GET", "POST"} {
			for _, k := range []string{key, ""} {
				if r := e.do(m, p, k, own, nil); r.StatusCode != http.StatusNotFound {
					t.Errorf("%s %s (key %v): %d", m, p, k != "", r.StatusCode)
				}
			}
		}
	}
	for _, p := range []string{"/metrics", "/health", "/live", "/ready"} {
		if _, ok := e.hub.last(p); ok {
			t.Fatalf("%s reached the hub", p)
		}
	}
	if e.hub.loginCount() != 1 {
		t.Fatalf("logins %d, want only the entry", e.hub.loginCount())
	}
}

// Without a session the hub's static UI is relayed with no credentials at
// all, and everything under /api, /auth and /ws is refused before the hub.
func TestWithoutSession(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	for _, k := range []string{"", key} {
		r := e.do("GET", "/", k, map[string]string{"Accept": "text/html", "Cookie": "golem_deploy_inspector=x; " + hubCookie + "=forged"}, nil)
		if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, "inspector ui") || !strings.Contains(b, bootTag) {
			t.Fatalf("static UI: %d %s", r.StatusCode, b)
		}
		got, _ := e.hub.last("/")
		if got.Header.Get("Cookie") != "" || got.Header.Get("Authorization") != "" {
			t.Fatalf("credentials reached the hub with the static UI: %v", got.Header)
		}
	}
	if r := e.do("GET", "/assets/index.js", "", nil, nil); r.StatusCode != 200 {
		t.Fatalf("asset: %d", r.StatusCode)
	}
	e.hub.mu.Lock()
	before := len(e.hub.requests)
	e.hub.mu.Unlock()
	own := e.origin()
	for _, p := range []string{"/api/targets", "/api", "/auth/session", "/ws/client", "/assets/../api/targets", "//api/targets", "/auth/other"} {
		r := e.do("GET", p, "", map[string]string{"Accept": "text/html"}, nil)
		if b := readBody(t, r); r.StatusCode != http.StatusUnauthorized || strings.TrimSpace(b) != `{"error":"auth_required"}` || r.Header.Get(SessionHeader) != "ended" {
			t.Errorf("GET %s: %d %s", p, r.StatusCode, b)
		}
	}
	if r := e.do("POST", "/auth/logout", "", map[string]string{"Origin": own}, nil); r.StatusCode != http.StatusUnauthorized || r.Header.Get(SessionHeader) != "ended" {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if _, _, code := dialWS(t, e.addr(), "/ws/client", http.Header{"Origin": {own}}); code != http.StatusUnauthorized {
		t.Fatalf("WebSocket: %d", code)
	}
	// The static UI is GET and HEAD only, and never an upgrade.
	if r := e.do("POST", "/", "", map[string]string{"Origin": own}, strings.NewReader("x")); r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: %d", r.StatusCode)
	}
	if _, _, code := dialWS(t, e.addr(), "/", e.wsAuth(key)); code != http.StatusBadRequest {
		t.Fatalf("upgrade on the static UI: %d", code)
	}
	e.hub.mu.Lock()
	after := len(e.hub.requests)
	e.hub.mu.Unlock()
	if after != before {
		t.Fatalf("%d refused requests reached the hub", after-before)
	}
}

// Every HTML document from the hub gets the boot script directly after
// its opening <head> tag; a document that cannot carry it is not served.
func TestBootInjection(t *testing.T) {
	e := newTestEnv(t, nil)
	serve := func(fn http.HandlerFunc) { e.hub.set(func(h *fakeHub) { h.static = fn }) }
	doc := func(ctype, body string, hdr ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			for i := 0; i+1 < len(hdr); i += 2 {
				w.Header().Set(hdr[i], hdr[i+1])
			}
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method != "HEAD" {
				_, _ = io.WriteString(w, body)
			}
		}
	}
	// The default document, with the hub's own headers.
	r := e.do("GET", "/", "", map[string]string{"Accept-Encoding": "gzip, br"}, nil)
	b := readBody(t, r)
	want := strings.Replace(fakeDoc, "<head>", "<head>"+bootTag, 1)
	if r.StatusCode != 200 || b != want || r.Header.Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Fatalf("%d %q %q", r.StatusCode, b, r.Header.Get("Content-Length"))
	}
	if got, _ := e.hub.last("/"); got.Header.Get("Accept-Encoding") != "identity" {
		t.Fatalf("Accept-Encoding at the hub %q", got.Header.Get("Accept-Encoding"))
	}
	cases := []struct {
		name, ctype, body, want string
	}{
		{"attributes and case", "text/html", "<!DOCTYPE html><HTML><HEAD lang=en><title>x</title></HEAD></HTML>",
			"<!DOCTYPE html><HTML><HEAD lang=en>" + bootTag + "<title>x</title></HEAD></HTML>"},
		{"header before head", "text/html;charset=utf-8", "<!-- <header> --><head\n>x", "<!-- <header> --><head\n>" + bootTag + "x"},
		{"not HTML", "text/javascript", "document.write('<head>')", "document.write('<head>')"},
	}
	for _, c := range cases {
		serve(doc(c.ctype, c.body))
		r := e.do("GET", "/page", "", nil, nil)
		if b := readBody(t, r); r.StatusCode != 200 || b != c.want || r.Header.Get("Content-Length") != strconv.Itoa(len(c.want)) {
			t.Errorf("%s: %d %q", c.name, r.StatusCode, b)
		}
	}
	// HEAD announces the length of the document as it would be served.
	serve(doc("text/html", "<html><head></head></html>"))
	r = e.do("HEAD", "/", "", nil, nil)
	if r.StatusCode != 200 || r.Header.Get("Content-Length") != strconv.Itoa(len("<html><head></head></html>")+len(bootTag)) {
		t.Fatalf("HEAD: %d %q", r.StatusCode, r.Header.Get("Content-Length"))
	}
	// 304 passes through without a body.
	serve(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusNotModified)
	})
	r = e.do("GET", "/", "", map[string]string{"If-None-Match": `"v1"`}, nil)
	if b := readBody(t, r); r.StatusCode != http.StatusNotModified || b != "" || r.Header.Get("ETag") != `"v1"` {
		t.Fatalf("304: %d %q", r.StatusCode, b)
	}
	// Fail closed.
	failures := map[string]http.HandlerFunc{
		"no head":    doc("text/html", "<html><header>secret</header><body>hubcontent</body></html>"),
		"too large":  doc("text/html", "<html><head></head>"+strings.Repeat("x", maxDocument)),
		"compressed": doc("text/html", "<html><head></head></html>", "Content-Encoding", "gzip"),
		"partial": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "<html><head>")
		},
		"xhtml without head": doc("application/xhtml+xml", "<html xmlns='http://www.w3.org/1999/xhtml'><body/></html>"),
	}
	for name, fn := range failures {
		serve(fn)
		r := e.do("GET", "/", "", nil, nil)
		b := readBody(t, r)
		if r.StatusCode != http.StatusBadGateway || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") || strings.Contains(b, "hubcontent") ||
			strings.Contains(b, "secret") || !strings.Contains(b, "could not be prepared") || r.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: %d %q", name, r.StatusCode, b)
		}
	}
}

// After a hub restart the daemon logs in again for a valid session,
// transparently.
func TestReloginAfterHubRestart(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	e.hub.restart()
	e.clock.Advance(time.Minute)
	if r := e.do("GET", "/api/targets", key, nil, nil); r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, readBody(t, r))
	}
	if n := e.hub.loginCount(); n != 2 {
		t.Fatalf("logins %d", n)
	}
	// /auth/session answers 200 {"authenticated":false} after a restart.
	e.hub.restart()
	e.clock.Advance(time.Minute)
	r := e.do("GET", "/auth/session", key, nil, nil)
	if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, `"authenticated":true`) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// A WebSocket upgrade is repeated too.
	e.hub.restart()
	e.clock.Advance(time.Minute)
	conn, br, resp := dialWSResponse(t, e.addr(), "/ws/client", e.wsAuth(key))
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Protocol") != WebSocketProtocol {
		t.Fatalf("WebSocket after restart: %d %v", resp.StatusCode, resp.Header)
	}
	defer conn.Close()
	echoOnce(t, conn, br, "x")
	if n := e.hub.loginCount(); n != 4 {
		t.Fatalf("logins %d", n)
	}
}

// A refused hub login is never retried in a loop, and the hub's "not
// signed in" answer never reaches the browser: the request fails with 503.
func TestNoLoginLoop(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  string
	}{{http.StatusUnauthorized, ""}, {http.StatusTooManyRequests, "60"}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			e := newTestEnv(t, nil)
			key := e.enter(nil)
			e.hub.restart()
			e.hub.set(func(h *fakeHub) { h.loginStatus, h.retryAfter = tc.status, tc.retry })
			e.clock.Advance(time.Minute)
			for i := 0; i < 5; i++ {
				for _, p := range []string{"/api/targets", "/auth/session"} {
					r := e.do("GET", p, key, nil, nil)
					if b := readBody(t, r); r.StatusCode != http.StatusServiceUnavailable || r.Header.Get(SessionHeader) != "" ||
						strings.Contains(b, "authenticated") {
						// The daemon session is still valid.
						t.Fatalf("request %d %s: %d %v %s", i, p, r.StatusCode, r.Header, b)
					}
				}
				e.clock.Advance(10 * time.Second)
			}
			if n := e.hub.loginCount(); n != 2 {
				t.Fatalf("logins %d, want the entry and one attempt", n)
			}
			// New entries are refused at once, without a login.
			ses, _ := e.s.CreateSession(context.Background(), e.acct, nil)
			if r := e.exchange(ticketOf(t, ses.Path), nil); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(readBody(t, r), `"unavailable"`) {
				t.Fatalf("entry: %d", r.StatusCode)
			}
			if n := e.hub.loginCount(); n != 2 {
				t.Fatalf("logins %d", n)
			}
			if tc.status == http.StatusTooManyRequests {
				// The pause ends with the hub's Retry-After.
				e.hub.set(func(h *fakeHub) { h.loginStatus = 0 })
				e.clock.Advance(time.Minute)
				if r := e.do("GET", "/api/targets", key, nil, nil); r.StatusCode != 200 {
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
	ended := func(t *testing.T, r *http.Response) {
		t.Helper()
		if r.StatusCode != http.StatusUnauthorized || r.Header.Get(SessionHeader) != "ended" {
			t.Fatalf("%d %v", r.StatusCode, r.Header)
		}
	}
	t.Run("sweep closes streams", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		conn, br, code := dialWS(t, e.addr(), "/ws/client", e.wsAuth(key))
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
		ended(t, e.do("GET", "/api/targets", key, nil, nil))
	})
	t.Run("checked per request", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		no := false
		admin, _ := e.st.CreateAccount(ctx, store.NewAccount{Username: "root", PasswordHash: "x", Role: store.RoleAdmin, Approved: true})
		if _, err := e.st.UpdateAccount(ctx, admin.ID, e.acct.ID, store.AccountUpdate{Approved: &no}); err != nil {
			t.Fatal(err)
		}
		ended(t, e.do("GET", "/api/targets", key, nil, nil))
		if e.s.sessions.count() != 0 {
			t.Fatal("session kept")
		}
	})
	t.Run("expiry", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		e.clock.Advance(SessionMaxAge - time.Minute)
		if r := e.do("GET", "/api/targets", key, nil, nil); r.StatusCode != 200 {
			t.Fatalf("before expiry: %d", r.StatusCode)
		}
		e.clock.Advance(2 * time.Minute)
		ended(t, e.do("GET", "/api/targets", key, nil, nil))
	})
	t.Run("sign-out", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		if r := e.do("POST", "/auth/logout", key, nil, nil); r.StatusCode != http.StatusForbidden {
			t.Fatalf("logout without Origin: %d", r.StatusCode)
		}
		r := e.do("POST", "/auth/logout", key, map[string]string{"Origin": e.origin()}, nil)
		if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, `"authenticated":false`) {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
		e.hub.mu.Lock()
		logouts := len(e.hub.logouts)
		e.hub.mu.Unlock()
		if logouts != 1 {
			t.Fatalf("hub logouts %d", logouts)
		}
		ended(t, e.do("GET", "/api/targets", key, nil, nil))
	})
	t.Run("listener close", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		conn, br, code := dialWS(t, e.addr(), "/ws/client", e.wsAuth(key))
		if code != http.StatusSwitchingProtocols {
			t.Fatal(code)
		}
		defer conn.Close()
		e.s.Close()
		waitClosed(t, conn, br)
	})
}
