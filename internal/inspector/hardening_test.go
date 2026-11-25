package inspector

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// rawRequest sends raw bytes on a new connection and returns it with a
// reader for the answers.
func rawRequest(t *testing.T, addr, raw string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return c, bufio.NewReader(c)
}

// The body of a GET is never relayed or parsed as the next request: the
// request is refused and the connection closes.
func TestGetWithBody(t *testing.T) {
	e := newTestEnv(t, nil)
	host := "Host: " + e.addr() + "\r\n"
	smuggled := "GET /smuggled HTTP/1.1\r\n" + host + "\r\n"
	for name, framing := range map[string]string{
		"content length": "Content-Length: " + strconv.Itoa(len(smuggled)) + "\r\n\r\n" + smuggled,
		"chunked":        "Transfer-Encoding: chunked\r\n\r\n" + strconv.FormatInt(int64(len(smuggled)), 16) + "\r\n" + smuggled + "\r\n0\r\n\r\n",
	} {
		_, br := rawRequest(t, e.addr(), "GET / HTTP/1.1\r\n"+host+framing+"GET /second HTTP/1.1\r\n"+host+"\r\n")
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusBadRequest || !resp.Close {
			t.Fatalf("%s: %d close=%v", name, resp.StatusCode, resp.Close)
		}
		if next, err := http.ReadResponse(br, nil); err == nil {
			t.Fatalf("%s: a second request was served: %d", name, next.StatusCode)
		}
	}
	for _, p := range e.hub.paths() {
		if p == "/" || p == "/smuggled" || p == "/second" {
			t.Fatalf("%s reached the hub", p)
		}
	}
}

// Every request has read and write deadlines from its start: a body that
// never completes does not hold the connection, a refused request with a
// large announced body is closed at once, and the entry limiter answers
// before the body is read.
func TestRequestDeadlines(t *testing.T) {
	e := newTestEnv(t, nil, func(s *Service) { s.reqTimeout = 500 * time.Millisecond })
	key := e.enter(nil)
	closedSoon := func(name string, c net.Conn, br *bufio.Reader, within time.Duration) {
		t.Helper()
		start := time.Now()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := io.Copy(io.Discard, br)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("%s: the connection stayed open", name)
		}
		if d := time.Since(start); d > within {
			t.Fatalf("%s: closed after %s", name, d)
		}
	}
	head := func(method, path string, extra ...string) string {
		return method + " " + path + " HTTP/1.1\r\nHost: " + e.addr() + "\r\nOrigin: " + e.origin() + "\r\nContent-Type: application/json\r\n" +
			strings.Join(extra, "") + "Content-Length: 100000\r\n\r\n0123456789"
	}
	c, br := rawRequest(t, e.addr(), head("POST", "/api/x", "Authorization: Bearer "+key+"\r\n"))
	closedSoon("unfinished body", c, br, 3*time.Second)
	c, br = rawRequest(t, e.addr(), head("POST", "/_golem-deploy/other"))
	closedSoon("refused with a body", c, br, 3*time.Second)
	for i := 0; i < entryFailuresPerWindow; i++ {
		if r := e.exchange(strings.Repeat("x", 43), nil); r.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	c, br = rawRequest(t, e.addr(), head("POST", SessionPath))
	_ = c.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || !resp.Close {
		t.Fatalf("limited exchange: %v %v", resp, err)
	}
	for _, p := range e.hub.paths() {
		if p == "/api/x" {
			t.Fatal("an unfinished body was relayed")
		}
	}
}

// The listener holds at most maxConns connections; OPTIONS * reaches the
// handlers (405) instead of Go's general handler.
func TestConnectionCap(t *testing.T) {
	e := newTestEnv(t, nil, func(s *Service) { s.maxConns = 3 })
	var held []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", e.addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		held = append(held, c)
	}
	waitFor(t, "three open connections", func() bool { return e.listener().limit.open() == 3 })
	extra, err := net.Dial("tcp", e.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil {
		t.Fatal("a fourth connection was served")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("a fourth connection was kept open")
	}
	_ = held[0].Close()
	waitFor(t, "a free slot", func() bool { return e.listener().limit.open() == 2 })
	if line := writeRaw(t, e.addr(), "OPTIONS * HTTP/1.1\r\nHost: "+e.addr()+"\r\n\r\n"); !strings.Contains(line, " 405 ") {
		t.Fatalf("OPTIONS *: %q", line)
	}
}

// Go answers an unsupported Expect itself (417) before any handler runs and
// then reads the announced body. The server timeouts bound that too, on a
// plaintext listener and on both sides of a TLS listener, and the
// connection slot is freed.
func TestExpectationFailedFreesSlot(t *testing.T) {
	cert := selfSigned(t)
	withTLS := func(c *Config) {
		c.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }}
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		tls  bool
	}{{"plaintext listener", nil, false}, {"TLS side", withTLS, true}, {"plaintext side of TLS", withTLS, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t, tc.mut, func(s *Service) { s.reqTimeout = 500 * time.Millisecond })
			c, err := net.Dial("tcp", e.addr())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if tc.tls {
				c = tls.Client(c, &tls.Config{InsecureSkipVerify: true})
			}
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(c, "POST /ws/agent HTTP/1.1\r\nHost: "+e.addr()+"\r\nExpect: something-else\r\nContent-Length: 100\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, nil)
			if err != nil || resp.StatusCode != http.StatusExpectationFailed {
				t.Fatalf("%v %v", resp, err)
			}
			start := time.Now()
			_, err = io.Copy(io.Discard, br)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the connection stayed open after the 417")
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("closed after %s", d)
			}
			waitFor(t, "the connection slot", func() bool { return e.listener().limit.open() == 0 })
		})
	}
}

// Closing the listener also closes connections that have not sent their
// first byte yet.
func TestSnifferCloseClosesWaiting(t *testing.T) {
	cert := selfSigned(t)
	e := newTestEnv(t, func(c *Config) {
		c.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }}
	}, func(s *Service) { s.sniffTimeout = time.Minute })
	silent, err := net.Dial("tcp", e.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	waitFor(t, "the silent connection", func() bool { return e.listener().limit.open() == 1 })
	start := time.Now()
	e.s.closeListener()
	_ = silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("data on a silent connection")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the waiting connection was not closed")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("closed after %s", d)
	}
}

// Only WebSocket upgrades under /ws/ are relayed; other upgrades and
// announced trailers are refused before the hub, and the hub's
// informational responses and trailers never reach the browser.
func TestUpgradesAndTrailers(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	before := len(e.hub.paths())
	h := "Host: " + e.addr() + "\r\n"
	for name, raw := range map[string]string{
		"h2c on the UI": "GET / HTTP/1.1\r\n" + h + "Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAARAAAAAAAIAAAAA\r\n\r\n",
		"h2c on /ws": "GET /ws/client HTTP/1.1\r\n" + h + "Authorization: Bearer " + key + "\r\nOrigin: " + e.origin() +
			"\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n",
		"upgrade header alone": "GET / HTTP/1.1\r\n" + h + "Upgrade: websocket\r\n\r\n",
		"trailer": "POST /api/x HTTP/1.1\r\n" + h + "Authorization: Bearer " + key + "\r\nOrigin: " + e.origin() +
			"\r\nTrailer: X-Late\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\nX-Late: 1\r\n\r\n",
	} {
		if line := writeRaw(t, e.addr(), raw); !strings.Contains(line, " 400 ") {
			t.Errorf("%s: %q", name, line)
		}
	}
	for _, p := range []string{"/api/targets", "/auth/session", EnterPath} {
		if _, _, code := dialWS(t, e.addr(), p, e.wsAuth(key)); code != http.StatusBadRequest {
			t.Errorf("WebSocket on %s: %d", p, code)
		}
	}
	if n := len(e.hub.paths()); n != before {
		t.Fatalf("%d refused requests reached the hub", n-before)
	}
	e.hub.set(func(h *fakeHub) {
		h.static = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Link", "</assets/x.js>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Del("Link")
			w.Header().Set("Content-Type", "text/css")
			w.Header().Set("Trailer", "X-Hub-Trailer")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "body{}")
			w.Header().Set("X-Hub-Trailer", "secret")
		}
	})
	_, br := rawRequest(t, e.addr(), "GET /app.css HTTP/1.1\r\n"+h+"\r\n")
	line, _ := br.ReadString('\n')
	if !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Fatalf("first status line %q", line)
	}
	r := e.do("GET", "/app.css", "", nil, nil)
	if b := readBody(t, r); b != "body{}" || len(r.Trailer) != 0 || r.Header.Get("Trailer") != "" {
		t.Fatalf("%q trailer %v header %v", b, r.Trailer, r.Header)
	}
}

// A relayed stream whose client stops reading and sending is closed on
// both sides when its session ends or the agent relays close, and its
// connection slot is freed; closing the listener waits for the relays.
func TestSilentClientStreams(t *testing.T) {
	t.Run("browser", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		ln := e.listener()
		conn, _, code := dialWS(t, e.addr(), "/ws/flood", e.wsAuth(key))
		if code != http.StatusSwitchingProtocols {
			t.Fatal(code)
		}
		defer conn.Close()
		sess := e.session(key)
		waitFor(t, "the flood", func() bool { return e.hub.openStreams.Load() == 1 })
		time.Sleep(300 * time.Millisecond) // the relay now blocks writing to the silent client
		open := ln.limit.open()
		e.s.endSession(sess)
		waitFor(t, "the relay to end", func() bool { return sess.streamCount() == 0 && ln.tr.streams() == 0 && ln.tr.running() == 0 })
		waitFor(t, "the connection slot", func() bool { return ln.limit.open() == open-1 })
		waitFor(t, "the hub side", func() bool { return e.hub.openStreams.Load() == 0 })
	})
	t.Run("agent", func(t *testing.T) {
		e := newTestEnv(t, func(c *Config) { c.AgentHost = "127.0.0.1" })
		e.hub.set(func(h *fakeHub) { h.agentFlood = true })
		p := 7001
		sv, _ := e.st.CreateServer(context.Background(), store.Server{Name: "arena", Port: &p, Inspector: true}, nil)
		tok, _ := e.s.AgentToken(sv.ID)
		ln := e.listener()
		conn, _, code := dialWS(t, e.addr(), "/ws/agent", http.Header{"Authorization": {"Bearer " + tok}})
		if code != http.StatusSwitchingProtocols {
			t.Fatal(code)
		}
		defer conn.Close()
		waitFor(t, "the flood", func() bool { return e.hub.openStreams.Load() == 1 })
		time.Sleep(300 * time.Millisecond)
		e.s.agents.closeAll()
		waitFor(t, "the relay to end", func() bool { return ln.tr.streams() == 0 && ln.tr.running() == 0 && ln.limit.open() == 0 })
	})
	t.Run("shutdown", func(t *testing.T) {
		e := newTestEnv(t, nil)
		key := e.enter(nil)
		ln := e.listener()
		conn, _, code := dialWS(t, e.addr(), "/ws/flood", e.wsAuth(key))
		if code != http.StatusSwitchingProtocols {
			t.Fatal(code)
		}
		defer conn.Close()
		waitFor(t, "the flood", func() bool { return e.hub.openStreams.Load() == 1 })
		time.Sleep(300 * time.Millisecond)
		e.s.Close()
		if n, m := ln.tr.running(), ln.tr.streams(); n != 0 || m != 0 {
			t.Fatalf("after Close: %d handlers, %d streams", n, m)
		}
	})
}

// A 101 of the hub that does not answer the WebSocket upgrade with a
// WebSocket is refused (502) and the hub's connection is closed, for
// browsers and for agents.
func TestUnexpectedSwitch(t *testing.T) {
	check := func(t *testing.T, e *testEnv, path string, hdr http.Header) {
		t.Helper()
		e.hub.set(func(h *fakeHub) { h.badUpgrade = true })
		if _, _, code := dialWS(t, e.addr(), path, hdr); code != http.StatusBadGateway {
			t.Fatalf("%s: %d", path, code)
		}
		select {
		case <-e.hub.streamClosed:
		case <-time.After(5 * time.Second):
			t.Fatal("the hub's connection was kept open")
		}
	}
	t.Run("browser", func(t *testing.T) {
		e := newTestEnv(t, nil)
		check(t, e, "/ws/client", e.wsAuth(e.enter(nil)))
	})
	t.Run("agent", func(t *testing.T) {
		e := newTestEnv(t, func(c *Config) { c.AgentHost = "127.0.0.1" })
		p := 7001
		sv, _ := e.st.CreateServer(context.Background(), store.Server{Name: "arena", Port: &p, Inspector: true}, nil)
		tok, _ := e.s.AgentToken(sv.ID)
		check(t, e, "/ws/agent", http.Header{"Authorization": {"Bearer " + tok}})
	})
}

// A proxied answer to a client that stopped reading is never written after
// the session expired: the handler ends at the expiry, not after the
// request timeout. Sign-out refuses a request with a body.
func TestWritesEndWithSession(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	if r := e.do("POST", "/auth/logout", key, map[string]string{"Origin": e.origin()}, strings.NewReader("{}")); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("sign-out with a body: %d", r.StatusCode)
	}
	if r := e.do("GET", "/api/targets", key, nil, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("the session ended on a refused sign-out: %d", r.StatusCode)
	}
	chunk := strings.Repeat("x", 64<<10)
	e.hub.set(func(h *fakeHub) {
		h.static = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			for i := 0; i < 4096; i++ {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
			}
		}
	})
	e.clock.Advance(SessionMaxAge - 700*time.Millisecond)
	ln := e.listener()
	rawRequest(t, e.addr(), "GET /api/big HTTP/1.1\r\nHost: "+e.addr()+"\r\nAuthorization: Bearer "+key+"\r\n\r\n") // never read
	waitFor(t, "the request to start", func() bool { return ln.tr.running() == 1 })
	waitFor(t, "the request to end at the expiry", func() bool { return ln.tr.running() == 0 })
}

// A request whose session ends while the hub has not answered yet gets an
// answer (401, session ended), never an empty 200.
func TestCancelledRequestAnswers(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	gate := make(chan struct{})
	arrived := make(chan struct{}, 1)
	e.hub.set(func(h *fakeHub) {
		h.static = func(w http.ResponseWriter, r *http.Request) {
			arrived <- struct{}{}
			select {
			case <-gate:
			case <-r.Context().Done():
			}
		}
	})
	defer close(gate)
	sess := e.session(key)
	go func() {
		<-arrived
		e.s.endSession(sess)
	}()
	r := e.do("GET", "/api/slow", key, nil, nil)
	if b := readBody(t, r); r.StatusCode != http.StatusUnauthorized || r.Header.Get(SessionHeader) != "ended" {
		t.Fatalf("%d %q", r.StatusCode, b)
	}
}

// An /auth/session answer of the hub too large to check is a 502, never a
// cut body.
func TestOversizedSessionAnswer(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	e.hub.set(func(h *fakeHub) { h.sessionPad = maxSessionAnswer })
	r := e.do("GET", "/auth/session", key, nil, nil)
	if b := readBody(t, r); r.StatusCode != http.StatusBadGateway || strings.Contains(b, "authenticated") {
		t.Fatalf("%d %q", r.StatusCode, b)
	}
}

// Origins are compared in canonical form, and a request with more than one
// Origin header is refused.
func TestOriginCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"https://[2001:DB8:0:0::1]:443": "https://[2001:db8::1]",
		"HTTP://Example.COM:80":         "http://example.com",
		"http://127.0.0.1:8444":         "http://127.0.0.1:8444",
		"https://[::ffff:10.0.0.1]":     "https://[::ffff:10.0.0.1]",
	} {
		if got, err := canonicalOrigin(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"null", "", "https://", "https://x.example/p", "file:///x", "https://u@x.example", "https://[fe80::1%25eth0]"} {
		if got, err := canonicalOrigin(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	if got, err := ValidatePublicURL("https://[2001:DB8:0:0::1]:443/"); err != nil || got != "https://[2001:db8::1]" {
		t.Fatalf("%q %v", got, err)
	}
	e := newTestEnv(t, func(c *Config) { c.PublicURL = "https://[2001:db8::1]" })
	key := e.enter(nil)
	if r := e.do("POST", "/api/x", key, map[string]string{"Origin": "https://[2001:0db8:0::1]:443"}, strings.NewReader("{}")); r.StatusCode != 200 {
		t.Fatalf("other spelling of the public origin: %d", r.StatusCode)
	}
	raw := "POST /api/x HTTP/1.1\r\nHost: " + e.addr() + "\r\nAuthorization: Bearer " + key + "\r\nOrigin: https://[2001:db8::1]\r\n" +
		"Origin: https://evil.example\r\nContent-Length: 2\r\n\r\n{}"
	if line := writeRaw(t, e.addr(), raw); !strings.Contains(line, " 403 ") {
		t.Fatalf("two Origin headers: %q", line)
	}
}

// The boot script goes only after a <head> tag that leads the document.
func TestHeadEnd(t *testing.T) {
	for doc, want := range map[string]string{
		"<head>x": "<head>",
		"\xef\xbb\xbf <!doctype html>\n<html><head>":                                                           "\xef\xbb\xbf <!doctype html>\n<html><head>",
		"<!-- a --><!DOCTYPE html><!-- b --><HTML lang=\"a>b\" data-x='>'><!-- <head> --><HEAD data-y=\"<\">x": "<!-- a --><!DOCTYPE html><!-- b --><HTML lang=\"a>b\" data-x='>'><!-- <head> --><HEAD data-y=\"<\">",
		"<html>\n  <head\n>": "<html>\n  <head\n>",
	} {
		if at := headEnd([]byte(doc)); at < 0 || doc[:at] != want {
			t.Errorf("%q: %d", doc, at)
		}
	}
	for _, doc := range []string{"", "<p>hi</p><head>", "<html><body><head>", "<!-- <head> -->", "<!-- open <head>", "<header>",
		"<html><title>x</title><head>", "x<head>", "<!doctype html><!doctype html><head>", "<html lang=\"x><head>", "<head lang=\"x>"} {
		if at := headEnd([]byte(doc)); at >= 0 {
			t.Errorf("%q accepted at %d", doc, at)
		}
	}
}

// Concurrent refreshes share one call, and a call that just completed is
// not repeated.
func TestFlight(t *testing.T) {
	var f flight
	clock := &fakeClock{t: time.Unix(1000, 0)}
	release := make(chan struct{})
	started := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	fn := func() {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { f.do(context.Background(), clock.Now, time.Second, fn) })
	<-started
	for i := 0; i < 5; i++ {
		wg.Go(func() { f.do(context.Background(), clock.Now, time.Second, fn) })
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	if f.do(context.Background(), clock.Now, time.Second, fn) {
		t.Fatal("repeated within minAge")
	}
	clock.Advance(time.Second)
	if !f.do(context.Background(), clock.Now, time.Second, fn) || calls != 2 {
		t.Fatalf("not repeated after minAge: %d", calls)
	}
}
