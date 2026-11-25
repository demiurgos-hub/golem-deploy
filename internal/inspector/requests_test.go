package inspector

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// A relay of the hub's own pages that started before a turn-off ends with
// it, like sessions and agent relays.
func TestTurnOffEndsPageRelays(t *testing.T) {
	e := newTestEnv(t, nil)
	started, ended := make(chan struct{}), make(chan struct{})
	e.hub.set(func(h *fakeHub) {
		h.static = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/css")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "body{")
			_ = http.NewResponseController(w).Flush()
			close(started)
			<-r.Context().Done()
			close(ended)
		}
	})
	go func() {
		resp, err := plainClient().Get(e.base() + "/assets/app.css")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the page request did not reach the hub")
	}
	if n := e.s.pages.count(); n != 1 {
		t.Fatalf("%d page relays registered", n)
	}
	if err := e.s.storeEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := e.s.pages.count(); n != 0 {
		t.Fatalf("%d page relays after the turn-off", n)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the page relay outlived the turn-off")
	}
}

// A session pushed out of the store is ended before it leaves it, so a
// drain at that moment cannot miss its streams.
func TestEvictedSessionEndsInStore(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	st := newSessionStore(clock.Now)
	_, first, _, _ := st.create(1, 0, "s0")
	ctx, _, release, ok := first.track(context.Background(), clock.Now())
	if !ok {
		t.Fatal("track")
	}
	defer release()
	for i := 1; i < maxSessionsPerAccount; i++ {
		clock.Advance(time.Second)
		if _, _, ev, _ := st.create(1, 0, ""); len(ev) != 0 {
			t.Fatal("evicted early")
		}
	}
	clock.Advance(time.Second)
	_, _, ev, _ := st.create(1, 0, "new")
	if len(ev) != 1 || ev[0] != "s0" {
		t.Fatalf("evicted %v", ev)
	}
	if ctx.Err() == nil || !first.isEnded() {
		t.Fatal("the session left the store before it ended")
	}
}

// A WebSocket upgrade the hub does not complete has a deadline: a hub that
// answers something else than 101 and stalls frees the client connection
// and its handler when it passes.
func TestHandshakeDeadline(t *testing.T) {
	e := newTestEnv(t, nil, func(s *Service) { s.handshakeTimeout = 200 * time.Millisecond })
	key := e.enter(nil)
	e.hub.set(func(h *fakeHub) { h.wsStall = true })
	ln := e.listener()
	conn, err := net.DialTimeout("tcp", e.addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var b strings.Builder
	b.WriteString("GET /ws/client HTTP/1.1\r\nHost: " + e.addr() + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n")
	for k, vs := range e.wsAuth(key) {
		for _, v := range vs {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatal(err)
	}
	// The client stays connected and reads: the daemon must end it.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err == nil {
		if resp.StatusCode == http.StatusSwitchingProtocols {
			t.Fatal("switched")
		}
		_, err = io.Copy(io.Discard, resp.Body)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the failed handshake kept the connection open")
	}
	waitFor(t, "the handler to end", func() bool { return ln.tr.running() == 0 })
}

// A 101 whose Sec-WebSocket-Accept does not answer the request's key is
// refused (502), for browsers and for agents.
func TestWebSocketAccept(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.AgentHost = "127.0.0.1" })
	key := e.enter(nil)
	p := 7001
	sv, _ := e.st.CreateServer(context.Background(), store.Server{Name: "arena", Port: &p, Inspector: true}, nil)
	tok, _ := e.s.AgentToken(sv.ID)
	e.hub.set(func(h *fakeHub) { h.acceptValue = wsAccept("another key") })
	if _, _, code := dialWS(t, e.addr(), "/ws/client", e.wsAuth(key)); code != http.StatusBadGateway {
		t.Fatalf("browser: %d", code)
	}
	if _, _, code := dialWS(t, e.addr(), "/ws/agent", http.Header{"Authorization": {"Bearer " + tok}}); code != http.StatusBadGateway {
		t.Fatalf("agent: %d", code)
	}
	e.hub.set(func(h *fakeHub) { h.acceptValue = "" })
	conn, _, code := dialWS(t, e.addr(), "/ws/client", e.wsAuth(key))
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("valid accept: %d", code)
	}
	_ = conn.Close()
}

// A tracked request's context ends at one absolute instant computed from
// the session's expiry, which the caller also uses for the write deadline.
func TestTrackDeadline(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	st := newSessionStore(clock.Now)
	_, s, _, _ := st.create(1, 0, "")
	clock.Advance(SessionMaxAge - time.Minute)
	ctx, expiry, release, ok := s.track(context.Background(), clock.Now())
	if !ok {
		t.Fatal("track")
	}
	defer release()
	d, has := ctx.Deadline()
	if !has || !d.Equal(expiry) {
		t.Fatalf("deadline %v, expiry %v", d, expiry)
	}
	if left := time.Until(expiry); left > time.Minute || left < 50*time.Second {
		t.Fatalf("expiry in %v", left)
	}
}
