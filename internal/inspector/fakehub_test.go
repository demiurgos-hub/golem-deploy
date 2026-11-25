package inspector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/demiurgos-hub/golem-deploy/internal/netpolicy"
	"github.com/demiurgos-hub/golem-deploy/internal/secrets"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// seen is one request the fake hub received.
type seen struct {
	Method, Path, Host string
	Header             http.Header
}

// fakeHub mimics the Golem Inspector hub's HTTP surface: login with the
// password whose bcrypt hash it is given, server-side sessions, the origin
// check, targets, and WebSocket endpoints that echo raw bytes after the
// upgrade.
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	hash        func() string
	agentToken  func() string
	sessions    map[string]bool
	logins      int
	logouts     []string
	loginStatus int
	retryAfter  string
	targets     []map[string]any
	requests    []seen
	// static, when set, answers GET and HEAD of the static UI.
	static       http.HandlerFunc
	openStreams  atomic.Int32
	streamClosed chan struct{}
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{t: t, sessions: map[string]bool{}, streamClosed: make(chan struct{}, 64),
		targets: []map[string]any{}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) port() int {
	u, _ := url.Parse(h.srv.URL)
	p, _ := strconv.Atoi(u.Port())
	return p
}

func (h *fakeHub) set(fn func(*fakeHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

// restart drops every session, like a restarted hub.
func (h *fakeHub) restart() { h.set(func(h *fakeHub) { h.sessions = map[string]bool{} }) }

func (h *fakeHub) loginCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logins
}

func (h *fakeHub) last(path string) (seen, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.requests) - 1; i >= 0; i-- {
		if h.requests[i].Path == path {
			return h.requests[i], true
		}
	}
	return seen{}, false
}

func writeHubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *fakeHub) authed(r *http.Request) (string, bool) {
	c, err := r.Cookie(hubCookie)
	if err != nil {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return c.Value, h.sessions[c.Value]
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, seen{Method: r.Method, Path: r.URL.Path, Host: r.Host, Header: r.Header.Clone()})
	h.mu.Unlock()
	// Every answer tries to set a cookie; none may reach a browser.
	http.SetCookie(w, &http.Cookie{Name: "hub_tracker", Value: "x", Path: "/"})
	originOK := r.Header.Get("Origin") == HubOrigin
	switch r.URL.Path {
	case "/ready":
		writeHubJSON(w, 200, map[string]bool{"ready": true})
	case "/auth/login":
		if !originOK {
			writeHubJSON(w, 403, map[string]string{"error": "origin_not_allowed"})
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.logins++
		status, retry := h.loginStatus, h.retryAfter
		hash := ""
		if h.hash != nil {
			hash = h.hash()
		}
		h.mu.Unlock()
		if status != 0 {
			if retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			writeHubJSON(w, status, map[string]string{"error": "refused"})
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
			writeHubJSON(w, 401, map[string]string{"error": "invalid_credentials"})
			return
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		sid := hex.EncodeToString(b)
		h.mu.Lock()
		h.sessions[sid] = true
		h.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: hubCookie, Value: sid, Path: "/", HttpOnly: true, Secure: true})
		writeHubJSON(w, 200, map[string]bool{"authenticated": true})
	case "/auth/logout":
		if !originOK {
			writeHubJSON(w, 403, map[string]string{"error": "origin_not_allowed"})
			return
		}
		sid, ok := h.authed(r)
		if !ok {
			writeHubJSON(w, 401, map[string]string{"error": "auth_required"})
			return
		}
		h.mu.Lock()
		delete(h.sessions, sid)
		h.logouts = append(h.logouts, sid)
		h.mu.Unlock()
		writeHubJSON(w, 200, map[string]bool{"authenticated": false})
	case "/auth/session":
		_, ok := h.authed(r)
		writeHubJSON(w, 200, map[string]bool{"authenticated": ok, "authDisabled": false})
	case "/api/targets":
		if _, ok := h.authed(r); !ok {
			writeHubJSON(w, 401, map[string]string{"error": "auth_required"})
			return
		}
		h.mu.Lock()
		list := h.targets
		h.mu.Unlock()
		writeHubJSON(w, 200, map[string]any{"targets": list})
	case "/ws/client":
		if !originOK {
			writeHubJSON(w, 403, map[string]string{"error": "origin_not_allowed"})
			return
		}
		if _, ok := h.authed(r); !ok {
			writeHubJSON(w, 401, map[string]string{"error": "auth_required"})
			return
		}
		h.echo(w, r)
	case "/ws/agent":
		want := ""
		if h.agentToken != nil {
			want = h.agentToken()
		}
		if r.Header.Get("Authorization") != "Bearer "+want || want == "" {
			writeHubJSON(w, 401, map[string]string{"error": "auth_failed"})
			return
		}
		h.echo(w, r)
	default:
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if _, ok := h.authed(r); !ok {
				writeHubJSON(w, 401, map[string]string{"error": "auth_required"})
				return
			}
			body, _ := io.ReadAll(r.Body)
			writeHubJSON(w, 200, map[string]string{"echo": string(body)})
			return
		}
		h.mu.Lock()
		static := h.static
		h.mu.Unlock()
		if static != nil {
			static(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, fakeDoc)
	}
}

// fakeDoc is the fake hub's UI document.
const fakeDoc = `<!doctype html><html lang="en"><head><meta charset="UTF-8"><title>Golem Inspector</title>` +
	`<script type="module" src="/assets/index.js"></script></head><body>inspector ui</body></html>`

// echo completes a WebSocket upgrade and echoes raw bytes until EOF.
func (h *fakeHub) echo(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeHubJSON(w, 400, map[string]string{"error": "upgrade_required"})
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	h.openStreams.Add(1)
	defer func() {
		h.openStreams.Add(-1)
		_ = conn.Close()
		h.streamClosed <- struct{}{}
	}()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x\r\n\r\n")
	_ = brw.Flush()
	buf := make([]byte, 1024)
	for {
		n, err := brw.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testEnv is a Service wired to a fake hub, with a real store.
type testEnv struct {
	t     *testing.T
	s     *Service
	st    *store.Store
	hub   *fakeHub
	clock *fakeClock
	acct  store.Account
	logs  *syncBuffer
	// keys are the session keys the env obtained; none may reach the hub
	// or the log.
	keys []string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func openStore(t *testing.T) (*store.Store, *secrets.Box) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := secrets.New(bytes.Repeat([]byte{9}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return st, box
}

// newTestEnv returns a running Service (listener open, hub known) talking
// to a fake hub. mut adjusts the configuration before New.
func newTestEnv(t *testing.T, mut func(*Config)) *testEnv {
	t.Helper()
	st, box := openStore(t)
	clock := &fakeClock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	cfg := Config{Store: st, Secrets: box, Policy: netpolicy.New(nil, nil, nil), Listen: "127.0.0.1:0", GOOS: "linux",
		Now: clock.Now, BcryptCost: bcrypt.MinCost, Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if mut != nil {
		mut(&cfg)
	}
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	sec, err := newHubSecrets(1, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hub := newFakeHub(t)
	hub.hash = func() string { return sec.PasswordHash }
	hub.agentToken = func() string { return sec.AgentToken }
	s.mu.Lock()
	s.enabled, s.sec, s.hubPort, s.hubContainer = true, sec, hub.port(), strings.Repeat("a", 64)
	s.mu.Unlock()
	s.sniffTimeout = 300 * time.Millisecond
	if err := s.openListener(); err != nil {
		t.Fatal(err)
	}
	acct, err := st.CreateAccount(context.Background(), store.NewAccount{Username: "dev", PasswordHash: "x", Role: store.RoleDeveloper, Approved: true})
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, s: s, st: st, hub: hub, clock: clock, acct: acct, logs: logs}
	t.Cleanup(func() {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		for _, key := range e.keys {
			for _, req := range hub.requests {
				for name, vs := range req.Header {
					for _, v := range vs {
						if strings.Contains(v, key) {
							t.Errorf("a session key reached the hub in %s of %s", name, req.Path)
						}
					}
				}
			}
			if strings.Contains(logs.String(), key) {
				t.Error("a session key reached the daemon log")
			}
		}
	})
	return e
}

// addr is the listener's host:port.
func (e *testEnv) addr() string { return "127.0.0.1:" + strconv.Itoa(e.s.port()) }

func (e *testEnv) base() string { return "http://" + e.addr() }

// client never follows redirects and keeps no cookies.
func plainClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
}

// ticketOf returns the ticket in the fragment of an entry path.
func ticketOf(t *testing.T, entryPath string) string {
	t.Helper()
	tok, ok := strings.CutPrefix(entryPath, EnterPath+"#t=")
	if !ok || len(tok) != 43 {
		t.Fatalf("entry path %q", entryPath)
	}
	return tok
}

// postTicket sends the ticket exchange of enter.js: POST SessionPath with
// JSON and origin. hdr adds or replaces headers ("" removes one; "Host"
// sets the Host).
func postTicket(t *testing.T, client *http.Client, base, ticket, origin string, hdr map[string]string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"ticket": ticket})
	req, err := http.NewRequest("POST", base+SessionPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	for k, v := range hdr {
		switch {
		case strings.EqualFold(k, "Host"):
			req.Host = v
		case v == "":
			req.Header.Del(k)
		default:
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Errorf("the ticket exchange set a cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	return resp
}

// entryOf decodes a successful ticket exchange.
func entryOf(t *testing.T, resp *http.Response) sessionEntry {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	var v sessionEntry
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &v) != nil || len(v.Key) != 43 {
		t.Fatalf("exchange: %d %s", resp.StatusCode, raw)
	}
	return v
}

// origin is the Origin the env's browser sends.
func (e *testEnv) origin() string {
	if e.s.cfg.PublicURL != "" {
		return e.s.cfg.PublicURL
	}
	return "http://" + e.addr()
}

// exchange posts a ticket from the env's origin.
func (e *testEnv) exchange(ticket string, hdr map[string]string) *http.Response {
	e.t.Helper()
	return postTicket(e.t, plainClient(), e.base(), ticket, e.origin(), hdr)
}

// enter creates a ticket for the env's account and exchanges it; it
// returns the session key.
func (e *testEnv) enter(serverID *int64) string {
	e.t.Helper()
	ses, err := e.s.CreateSession(context.Background(), e.acct, serverID)
	if err != nil {
		e.t.Fatal(err)
	}
	key := entryOf(e.t, e.exchange(ticketOf(e.t, ses.Path), nil)).Key
	e.keys = append(e.keys, key)
	return key
}

// wsAuth returns the headers of a browser WebSocket with the boot
// script's subprotocols for key and the env's origin.
func (e *testEnv) wsAuth(key string) http.Header {
	return http.Header{"Sec-WebSocket-Protocol": {WebSocketProtocol + ", " + WebSocketKeyPrefix + key}, "Origin": {e.origin()}}
}

// do sends a request with the session key as a bearer header (when not
// empty). Every answer of the listener is checked for Set-Cookie.
func (e *testEnv) do(method, path, key string, hdr map[string]string, body io.Reader) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.base()+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := plainClient().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		e.t.Errorf("%s %s set a cookie: %v", method, path, resp.Header.Values("Set-Cookie"))
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// dialWS opens a raw WebSocket upgrade on path and returns the connection
// after a 101, or the status code otherwise.
func dialWS(t *testing.T, addr, path string, hdr http.Header) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	conn, br, resp := dialWSResponse(t, addr, path, hdr)
	return conn, br, resp.StatusCode
}

// dialWSResponse is dialWS with the whole response. No answer may set a
// cookie.
func dialWSResponse(t *testing.T, addr, path string, hdr http.Header) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("GET " + path + " HTTP/1.1\r\nHost: " + addr + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n")
	for k, vs := range hdr {
		for _, v := range vs {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Errorf("WebSocket %s set a cookie", path)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, nil, resp
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, br, resp
}

// echoOnce writes msg and expects it back.
func echoOnce(t *testing.T, conn net.Conn, br *bufio.Reader, msg string) {
	t.Helper()
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != msg {
		t.Fatalf("echo %q %v", got, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// waitClosed expects the connection to be closed by the other side.
func waitClosed(t *testing.T, conn net.Conn, br *bufio.Reader) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := br.ReadByte()
	if err == nil {
		t.Fatal("stream still open")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("stream not closed in time")
	}
}

func e2ePolicy() *netpolicy.Policy { return netpolicy.New(nil, nil, nil) }
