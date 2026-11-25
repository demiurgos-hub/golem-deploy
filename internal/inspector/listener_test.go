package inspector

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// With TLS, one port serves browsers over TLS (the panel's certificate)
// and only the agent endpoint over plaintext, told apart by the first
// byte; a client that sends nothing is dropped without blocking others.
func TestListenerTLSAndPlaintext(t *testing.T) {
	cert := selfSigned(t)
	e := newTestEnv(t, func(c *Config) {
		c.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }}
	})
	// A silent client first.
	silent, err := net.Dial("tcp", e.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := tlsClient.Get("https://" + e.addr() + "/api/targets")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "auth_required") || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Fatalf("TLS: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	// Entering over TLS: the exchange needs the https origin, and the
	// key, not a cookie, authenticates.
	ses, _ := e.s.CreateSession(context.Background(), e.acct, nil)
	if r := postTicket(t, tlsClient, "https://"+e.addr(), ticketOf(t, ses.Path), "http://"+e.addr(), nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("http origin over TLS: %d", r.StatusCode)
	}
	key := entryOf(t, postTicket(t, tlsClient, "https://"+e.addr(), ticketOf(t, ses.Path), "https://"+e.addr(), nil)).Key
	e.keys = append(e.keys, key)
	// The expected Origin is https here.
	req, _ := http.NewRequest("POST", "https://"+e.addr()+"/api/x", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Origin", "https://"+e.addr())
	if resp, err := tlsClient.Do(req); err != nil || resp.StatusCode != 200 || len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("https origin: %v %v", resp, err)
	}
	// Plaintext: only the agent endpoint.
	for path, want := range map[string]int{"/": http.StatusNotFound, "/api/targets": http.StatusNotFound, EnterPath: http.StatusNotFound,
		bootJSPath: http.StatusNotFound, "/ws/agent": http.StatusBadRequest} {
		r := e.do("GET", path, key, nil, nil)
		if r.StatusCode != want {
			t.Errorf("plaintext %s: %d, want %d", path, r.StatusCode, want)
		}
	}
	// /ws/agent is not an agent endpoint on the TLS side.
	resp, err = tlsClient.Get("https://" + e.addr() + "/ws/agent")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("TLS /ws/agent: %d", resp.StatusCode)
	}
	// The silent client is dropped after the first-byte timeout.
	_ = silent.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("silent connection got data")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("silent connection was not closed")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("closed too late")
	}
}

// A plaintext panel gets a plaintext Inspector listener that serves both
// browsers and agents; the Host policy applies to browsers.
func TestListenerPlaintextHostPolicy(t *testing.T) {
	e := newTestEnv(t, nil)
	if r := e.do("GET", "/api/targets", "", map[string]string{"Host": "evil.example"}, nil); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("bad host: %d", r.StatusCode)
	}
	for _, h := range []string{"localhost:1", "[::1]:2", "10.0.0.1"} {
		if r := e.do("GET", "/api/targets", "", map[string]string{"Host": h}, nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("host %s: %d", h, r.StatusCode)
		}
	}
	if r := e.do("GET", "/ws/agent", "", map[string]string{"Host": "host.docker.internal:8444"}, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("agent endpoint host: %d", r.StatusCode)
	}
}

func TestBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	_, err = openListener(addr, nil, http.NotFoundHandler(), http.NotFoundHandler(), time.Second, maxConns, nil)
	if err == nil {
		t.Fatal("bound a busy port")
	}
	msg := bindError(addr, err)
	if !strings.Contains(msg, "in use") || strings.Contains(msg, "127.0.0.1") {
		t.Fatalf("%q", msg)
	}
}

// The agent endpoint relays a game's agent with the hub's own token, only
// from an allowed peer, only with the server's token, only for a server
// with the switch on, and within the connection limits; a server keeps one
// relay.
func TestAgentRelay(t *testing.T) {
	// A loopback peer is allowed on Linux only with an explicit agent host.
	e := newTestEnv(t, func(c *Config) { c.AgentHost = "127.0.0.1" })
	ctx := context.Background()
	p1, p2, p3 := 7001, 7002, 7003
	arena, _ := e.st.CreateServer(ctx, store.Server{Name: "arena", Port: &p1, Inspector: true}, nil)
	other, _ := e.st.CreateServer(ctx, store.Server{Name: "other", Port: &p2, Inspector: true}, nil)
	off, _ := e.st.CreateServer(ctx, store.Server{Name: "off", Port: &p3}, nil)
	tok := func(id int64) string {
		v, err := e.s.AgentToken(id)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	bearer := func(v string) http.Header {
		return http.Header{"Authorization": {"Bearer " + v}, "Cookie": {"a=b"}, "X-Real-Ip": {"6.6.6.6"}, "Origin": {"http://x"}}
	}
	cases := map[string]struct {
		hdr  http.Header
		want int
	}{
		"no token":       {http.Header{}, http.StatusUnauthorized},
		"hub's token":    {bearer(e.s.sec.AgentToken), http.StatusUnauthorized},
		"forged":         {bearer(AgentTokenPrefix + strconv.FormatInt(arena.ID, 10) + "_" + strings.Repeat("0", 64)), http.StatusUnauthorized},
		"switch off":     {bearer(tok(off.ID)), http.StatusForbidden},
		"unknown server": {bearer(tok(999)), http.StatusForbidden},
	}
	for name, c := range cases {
		if _, _, code := dialWS(t, e.addr(), "/ws/agent", c.hdr); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	a1, br1, code := dialWS(t, e.addr(), "/ws/agent", bearer(tok(arena.ID)))
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("relay: %d", code)
	}
	defer a1.Close()
	echoOnce(t, a1, br1, "register")
	got, _ := e.hub.last("/ws/agent")
	if got.Header.Get("Authorization") != "Bearer "+e.s.sec.AgentToken || got.Header.Get("Cookie") != "" ||
		got.Header.Get("X-Real-Ip") != "" || got.Header.Get("Origin") != "" || got.Host != hubHost {
		t.Fatalf("headers at the hub: %v %q", got.Header, got.Host)
	}
	// A second connection of the same server replaces the first.
	a2, br2, code := dialWS(t, e.addr(), "/ws/agent", bearer(tok(arena.ID)))
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("second: %d", code)
	}
	defer a2.Close()
	waitClosed(t, a1, br1)
	echoOnce(t, a2, br2, "still here")
	b1, _, code := dialWS(t, e.addr(), "/ws/agent", bearer(tok(other.ID)))
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("other server: %d", code)
	}
	defer b1.Close()
	if n := e.s.agents.count(); n != 2 {
		t.Fatalf("relayed %d", n)
	}
	// Turning the Inspector off (or rotating) closes relayed agents.
	e.s.agents.closeAll()
	waitClosed(t, a2, br2)
	if e.s.agents.count() != 0 {
		deadline := time.Now().Add(5 * time.Second)
		for e.s.agents.count() != 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if e.s.agents.count() != 0 {
			t.Fatalf("slots not released: %d", e.s.agents.count())
		}
	}
	if !strings.Contains(e.logs.String(), "refused an Inspector agent connection") || strings.Contains(e.logs.String(), tok(arena.ID)) {
		t.Fatal("refusal log")
	}
}

// Peers inside the managed network's subnets may use the agent endpoint;
// loopback peers only off Linux (Docker Desktop) or with an explicit
// --inspector-agent-host.
func TestAgentPeer(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		goos, agentHost string
		loopback        bool
	}{{"linux", "", false}, {"linux", "172.30.0.1", true}, {"windows", "", true}, {"darwin", "", true}} {
		e := newTestEnv(t, func(c *Config) { c.GOOS, c.AgentHost = tc.goos, tc.agentHost })
		e.s.netMu.Lock()
		e.s.netAddrs = docker.NetworkAddresses{Subnets: []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}}
		e.s.netAt = e.clock.Now()
		e.s.netMu.Unlock()
		for peer, want := range map[string]bool{"127.0.0.1": tc.loopback, "::1": tc.loopback, "172.30.4.5": true, "172.31.0.1": false,
			"10.0.0.1": false, "2001:db8::1": false} {
			if got := e.s.peerAllowed(ctx, netip.MustParseAddr(peer)); got != want {
				t.Errorf("%s/%q %s: %v", tc.goos, tc.agentHost, peer, got)
			}
		}
	}
}

// The agent address follows the listener and the platform.
func TestAgentHost(t *testing.T) {
	ctx := context.Background()
	gw := docker.NetworkAddresses{Gateways: []netip.Addr{netip.MustParseAddr("fd00::1"), netip.MustParseAddr("172.30.0.1")}}
	for _, tc := range []struct {
		listen, goos, override string
		host                   string
		reachable, known       bool
	}{
		{":8444", "linux", "", "172.30.0.1", true, true},
		{"0.0.0.0:8444", "linux", "", "172.30.0.1", true, true},
		{"[::]:8444", "linux", "", "172.30.0.1", true, true},
		{"127.0.0.1:8444", "linux", "", "", false, false},
		{"localhost:8444", "linux", "", "", false, false},
		{"192.0.2.10:8444", "linux", "", "192.0.2.10", true, true},
		{"127.0.0.1:8444", "windows", "", "host.docker.internal", true, true},
		{":8444", "darwin", "", "host.docker.internal", true, true},
		{"127.0.0.1:8444", "linux", "10.1.1.1", "10.1.1.1", true, true},
	} {
		st, box := openStore(t)
		s, err := New(ctx, Config{Store: st, Secrets: box, Policy: e2ePolicy(), Listen: tc.listen, GOOS: tc.goos, AgentHost: tc.override})
		if err != nil {
			t.Fatal(err)
		}
		s.netAddrs, s.netAt = gw, time.Now()
		host, reachable, known := s.agentHost(ctx)
		if host != tc.host || reachable != tc.reachable || known != tc.known {
			t.Errorf("%s on %s: %q %v %v", tc.listen, tc.goos, host, reachable, known)
		}
	}
	// Without the network the gateway is not known yet.
	st, box := openStore(t)
	s, _ := New(ctx, Config{Store: st, Secrets: box, Policy: e2ePolicy(), Listen: ":8444", GOOS: "linux"})
	if _, reachable, known := s.agentHost(ctx); !reachable || known {
		t.Fatalf("no network: %v %v", reachable, known)
	}
	// Agent: only while enabled, with secrets and an address.
	e := newTestEnv(t, func(c *Config) { c.AgentHost = "172.30.0.1" })
	a, ok := e.s.Agent(ctx)
	if !ok || a.HubURL != "ws://172.30.0.1:"+strconv.Itoa(e.s.port())+"/ws/agent" || a.Generation != 1 {
		t.Fatalf("%+v %v", a, ok)
	}
	e.s.mu.Lock()
	e.s.enabled = false
	e.s.mu.Unlock()
	if _, ok := e.s.Agent(ctx); ok {
		t.Fatal("agent while off")
	}
}

// writeRaw sends raw bytes and reads the status line.
func writeRaw(t *testing.T, addr, raw string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, raw)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

// Plaintext requests on the TLS side or TLS on a plaintext listener are
// not served as something else.
func TestListenerNoConfusion(t *testing.T) {
	e := newTestEnv(t, nil)
	if line := writeRaw(t, e.addr(), "\x16\x03\x01\x00\x05hello"); strings.Contains(line, "200") {
		t.Fatalf("TLS bytes on plaintext: %q", line)
	}
}
