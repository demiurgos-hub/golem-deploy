package inspector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Nothing that started before a rotation, a turn-off or a shutdown
// registers after the drain: a ticket exchange, an agent relay and a new
// entry link all check the generation under the lock that registers them.
func TestAdmissionGenerations(t *testing.T) {
	ctx := context.Background()
	t.Run("ticket exchange and rotation", func(t *testing.T) {
		e := newTestEnv(t, nil)
		ses, err := e.s.CreateSession(ctx, e.acct, nil)
		if err != nil {
			t.Fatal(err)
		}
		e.s.hooks.afterEntryLogin = func() {
			if _, err := e.s.ensureSecrets(ctx, io.Discard, true); err != nil {
				t.Error(err)
			}
		}
		r := e.exchange(ticketOf(t, ses.Path), nil)
		if b := readBody(t, r); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(b, "not_running") {
			t.Fatalf("%d %s", r.StatusCode, b)
		}
		if n := e.s.sessions.count(); n != 0 {
			t.Fatalf("%d sessions after the rotation", n)
		}
	})
	t.Run("agent and turn-off", func(t *testing.T) {
		e := newTestEnv(t, func(c *Config) { c.AgentHost = "127.0.0.1" })
		p := 7001
		sv, _ := e.st.CreateServer(ctx, store.Server{Name: "arena", Port: &p, Inspector: true}, nil)
		tok, _ := e.s.AgentToken(sv.ID)
		e.s.hooks.afterAgentCheck = e.s.endAdmission
		if _, _, code := dialWS(t, e.addr(), "/ws/agent", http.Header{"Authorization": {"Bearer " + tok}}); code != http.StatusServiceUnavailable {
			t.Fatalf("relay after the turn-off: %d", code)
		}
		if n := e.s.agents.count(); n != 0 {
			t.Fatalf("%d relays after the turn-off", n)
		}
	})
	t.Run("entry link and shutdown", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.s.hooks.beforeTicket = e.s.endAdmission
		if _, err := e.s.CreateSession(ctx, e.acct, nil); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("%v", err)
		}
		e.s.tickets.mu.Lock()
		n := len(e.s.tickets.m)
		e.s.tickets.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d tickets after the drain", n)
		}
	})
}

// A proxied request reads its body before the session is checked against
// its account; a session that ended meanwhile never starts a request.
func TestSessionCheckedAfterBody(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	e.s.hooks.afterKeyCheck = func() {
		if err := e.st.BumpTokenVersion(ctx, e.acct.ID); err != nil {
			t.Error(err)
		}
	}
	r := e.do("POST", "/api/x", key, map[string]string{"Origin": e.origin()}, strings.NewReader(`{"a":1}`))
	if r.StatusCode != http.StatusUnauthorized || r.Header.Get(SessionHeader) != "ended" {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	if _, ok := e.hub.last("/api/x"); ok {
		t.Fatal("the request reached the hub")
	}
}

// Sign-out ends the session at once, before the hub's logout answers.
func TestSignOutEndsFirst(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	conn, br, code := dialWS(t, e.addr(), "/ws/client", e.wsAuth(key))
	if code != http.StatusSwitchingProtocols {
		t.Fatal(code)
	}
	defer conn.Close()
	gate, seen := make(chan struct{}), make(chan struct{}, 1)
	e.hub.set(func(h *fakeHub) { h.logoutGate, h.logoutSeen = gate, seen })
	var wg sync.WaitGroup
	var status int
	wg.Go(func() {
		req, _ := http.NewRequest("POST", e.base()+"/auth/logout", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Origin", e.origin())
		if resp, err := plainClient().Do(req); err == nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
	})
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no logout at the hub")
	}
	waitClosed(t, conn, br)
	if r := e.do("GET", "/api/targets", key, nil, nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("key still valid during the hub logout: %d", r.StatusCode)
	}
	close(gate)
	wg.Wait()
	if status != http.StatusOK {
		t.Fatalf("sign-out: %d", status)
	}
}

// An open stream ends when its session expires.
func TestStreamEndsAtExpiry(t *testing.T) {
	e := newTestEnv(t, nil)
	key := e.enter(nil)
	e.clock.Advance(SessionMaxAge - 500*time.Millisecond)
	conn, br, code := dialWS(t, e.addr(), "/ws/client", e.wsAuth(key))
	if code != http.StatusSwitchingProtocols {
		t.Fatal(code)
	}
	defer conn.Close()
	echoOnce(t, conn, br, "x")
	waitClosed(t, conn, br)
}

// The sweep closes the relays of servers whose switch is off or that were
// deleted; forwarded agent requests are refused.
func TestAgentSweepAndForwarding(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t, func(c *Config) { c.AgentHost = "127.0.0.1" })
	p1, p2, p3 := 7001, 7002, 7003
	off, _ := e.st.CreateServer(ctx, store.Server{Name: "off", Port: &p1, Inspector: true}, nil)
	gone, _ := e.st.CreateServer(ctx, store.Server{Name: "gone", Port: &p2, Inspector: true}, nil)
	kept, _ := e.st.CreateServer(ctx, store.Server{Name: "kept", Port: &p3, Inspector: true}, nil)
	dial := func(id int64, extra http.Header) (int, func()) {
		tok, _ := e.s.AgentToken(id)
		hdr := http.Header{"Authorization": {"Bearer " + tok}}
		for k, v := range extra {
			hdr[k] = v
		}
		conn, br, code := dialWS(t, e.addr(), "/ws/agent", hdr)
		if conn == nil {
			return code, nil
		}
		t.Cleanup(func() { _ = conn.Close() })
		return code, func() { waitClosed(t, conn, br) }
	}
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "Via"} {
		if code, _ := dial(kept.ID, http.Header{h: {"for=192.0.2.1"}}); code != http.StatusForbidden {
			t.Errorf("%s: %d", h, code)
		}
	}
	_, offClosed := dial(off.ID, nil)
	_, goneClosed := dial(gone.ID, nil)
	code, _ := dial(kept.ID, nil)
	if offClosed == nil || goneClosed == nil || code != http.StatusSwitchingProtocols {
		t.Fatal("relays not opened")
	}
	no := false
	if _, err := e.st.UpdateServer(ctx, off.ID, store.ServerUpdate{Inspector: &no}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteServer(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	e.s.sweepAgents(ctx)
	offClosed()
	goneClosed()
	if n := e.s.agents.count(); n != 1 {
		t.Fatalf("relays %d", n)
	}
}

// Hub logins run one at a time: concurrent callers do not all try while
// the hub limits logins.
func TestHubLoginsSerialised(t *testing.T) {
	e := newTestEnv(t, nil)
	gate := make(chan struct{})
	e.hub.set(func(h *fakeHub) { h.loginStatus, h.retryAfter, h.loginGate = http.StatusTooManyRequests, "60", gate })
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Go(func() {
			if _, err := e.s.hubLogin(context.Background(), e.s.hubNow()); err == nil {
				t.Error("login succeeded")
			}
		})
	}
	waitFor(t, "a login at the hub", func() bool { return e.hub.loginCount() >= 1 })
	time.Sleep(100 * time.Millisecond) // the other callers wait for the first
	close(gate)
	wg.Wait()
	if n := e.hub.loginCount(); n != 1 {
		t.Fatalf("logins %d", n)
	}
}

// A hold belongs to a hub generation and is never shortened.
func TestLoginGuard(t *testing.T) {
	var g loginGuard
	now := time.Unix(1000, 0)
	g.record("a/1", now, &rateLimitedError{retryAfter: time.Minute})
	g.record("a/1", now, &rateLimitedError{retryAfter: time.Second})
	if held, cause := g.blocked("a/1", now.Add(30*time.Second)); !held || !errors.Is(cause, errHubRateLimited) {
		t.Fatal("a later 429 shortened the hold")
	}
	if held, _ := g.blocked("a/2", now); held {
		t.Fatal("the hold of another generation applies")
	}
	if held, _ := g.blocked("a/1", now.Add(time.Minute)); held {
		t.Fatal("held after Retry-After")
	}
	g.record("a/1", now, errHubCredentials)
	if held, cause := g.blocked("a/1", now.Add(24*time.Hour)); !held || !errors.Is(cause, errHubCredentials) {
		t.Fatal("a refused password does not hold")
	}
	g.record("b/1", now, nil)
	if held, _ := g.blocked("a/1", now); held {
		t.Fatal("the hold survived a new generation")
	}
}

// The hub port is used only for a verified running hub container: a
// stopped container drops it at once, the password goes out only after
// Docker confirmed the port, and a transport error drops the port until
// Docker confirms it again.
func TestHubPortValidity(t *testing.T) {
	ctx := context.Background()
	t.Run("stopped container", func(t *testing.T) {
		e := newTestEnv(t, nil)
		e.hub.set(func(h *fakeHub) { h.stopped = true })
		e.s.refreshHub(ctx)
		if p := e.s.hubNow().port; p != 0 {
			t.Fatalf("port %d kept for a stopped hub", p)
		}
		if r := e.do("GET", "/", "", nil, nil); r.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("UI while stopped: %d", r.StatusCode)
		}
		e.hub.set(func(h *fakeHub) { h.stopped = false })
		e.clock.Advance(time.Minute)
		waitFor(t, "the port to come back", func() bool {
			e.s.view()
			return e.s.hubNow().port != 0
		})
		if r := e.do("GET", "/", "", nil, nil); r.StatusCode != http.StatusOK {
			t.Fatalf("UI after the hub runs again: %d", r.StatusCode)
		}
	})
	t.Run("login only after Docker confirms the port", func(t *testing.T) {
		e := newTestEnv(t, nil)
		impostor := newFakeHub(t)
		e.s.mu.Lock()
		e.s.hubPort = impostor.port() // another program took the port
		e.s.mu.Unlock()
		ses, _ := e.s.CreateSession(ctx, e.acct, nil)
		if r := e.exchange(ticketOf(t, ses.Path), nil); r.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("exchange: %d", r.StatusCode)
		}
		if impostor.loginCount() != 0 {
			t.Fatal("the password went to another program")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		e := newTestEnv(t, nil)
		dead := newFakeHub(t)
		deadPort := dead.port()
		dead.srv.Close()
		gate := make(chan struct{})
		e.hub.set(func(h *fakeHub) { h.publish, h.inspectGate = deadPort, gate })
		e.s.mu.Lock()
		e.s.hubPort = deadPort
		e.s.mu.Unlock()
		if r := e.do("GET", "/", "", nil, nil); r.StatusCode != http.StatusBadGateway {
			t.Fatalf("UI: %d", r.StatusCode)
		}
		if p := e.s.hubNow().port; p != 0 {
			t.Fatalf("port %d kept after a transport error", p)
		}
		e.hub.set(func(h *fakeHub) { h.publish = 0 })
		close(gate)
		waitFor(t, "Docker's port", func() bool { return e.s.hubNow().port == e.hub.port() })
	})
}
