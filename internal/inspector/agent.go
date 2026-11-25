package inspector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/netpolicy"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Agent relay limits.
const (
	// MaxAgents bounds the relayed agent connections. A server keeps one
	// relay: a new valid connection of the same server closes the previous
	// one.
	MaxAgents = 256
	// networkCacheTTL is how long the managed network's addresses are
	// reused; a peer outside them triggers a refresh at most this often.
	networkCacheTTL = 30 * time.Second
)

// agentRelay is one relayed agent connection.
type agentRelay struct {
	cancel context.CancelFunc
}

// agentRegistry tracks relayed agent connections, one per server, so
// turning the Inspector off, rotating its secrets, turning a server's
// switch off or deleting the server closes them.
type agentRegistry struct {
	mu       sync.Mutex
	byServer map[int64]*agentRelay
}

func newAgentRegistry() *agentRegistry {
	return &agentRegistry{byServer: map[int64]*agentRelay{}}
}

// acquire registers the relay of server id and closes the server's
// previous relay; ctx is cancelled by closeAll, closeIf or the next relay
// of the same server. ok is false when MaxAgents other servers are relayed.
func (a *agentRegistry) acquire(parent context.Context, id int64) (context.Context, func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.byServer[id]
	if prev == nil && len(a.byServer) >= MaxAgents {
		return nil, nil, false
	}
	if prev != nil {
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	r := &agentRelay{cancel: cancel}
	a.byServer[id] = r
	return ctx, func() {
		cancel()
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.byServer[id] == r {
			delete(a.byServer, id)
		}
	}, true
}

// closeAll cancels every relayed connection.
func (a *agentRegistry) closeAll() {
	a.closeIf(func(int64) bool { return true })
}

// closeIf cancels the relays of the servers drop reports and returns how
// many it closed.
func (a *agentRegistry) closeIf(drop func(id int64) bool) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for id, r := range a.byServer {
		if drop(id) {
			r.cancel()
			delete(a.byServer, id)
			n++
		}
	}
	return n
}

func (a *agentRegistry) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.byServer)
}

// isWebSocket reports whether r asks for a WebSocket upgrade.
func isWebSocket(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// asksUpgrade reports whether r asks for any protocol upgrade.
func asksUpgrade(r *http.Request) bool {
	return len(r.Header.Values("Upgrade")) > 0 || headerHasToken(r.Header, "Connection", "upgrade")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// networkAddrs returns the managed network's addresses, cached; refresh
// forces a new query when the cache is older than networkCacheTTL/6.
// Concurrent refreshes share one Docker call; no lock is held during it.
// The answer of a query that started before the cache was dropped or
// forgotten is discarded.
func (s *Service) networkAddrs(ctx context.Context, refresh bool) (docker.NetworkAddresses, error) {
	cached := func() (bool, docker.NetworkAddresses, error) {
		s.netMu.Lock()
		defer s.netMu.Unlock()
		age := s.cfg.Now().Sub(s.netAt)
		fresh := !s.netAt.IsZero() && age < networkCacheTTL && !(refresh && age > networkCacheTTL/6)
		return fresh, s.netAddrs, s.netErr
	}
	if ok, addrs, err := cached(); ok {
		return addrs, err
	}
	if s.netQuery == nil {
		return docker.NetworkAddresses{}, docker.ErrUnavailable
	}
	s.netFlight.do(ctx, s.cfg.Now, 0, func() {
		s.netMu.Lock()
		gen := s.netGen
		s.netMu.Unlock()
		qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		addrs, err := s.netQuery(qctx)
		now := s.cfg.Now()
		s.netMu.Lock()
		defer s.netMu.Unlock()
		if s.netGen != gen {
			return // dropped or forgotten meanwhile: this answer is older
		}
		transient := err != nil && !errors.Is(err, docker.ErrNotFound) && !errors.Is(err, docker.ErrNotManaged) && !errors.Is(err, docker.ErrNotOurs)
		if transient && s.netErr == nil && len(s.netAddrs.Gateways)+len(s.netAddrs.Subnets) > 0 {
			// Docker did not answer: keep the last known addresses (the
			// agent address and restart_required must not flicker) and ask
			// again soon. A network that is not managed or not ours drops
			// them at once.
			s.netAt = now.Add(-networkCacheTTL + 5*time.Second)
			return
		}
		s.netAddrs, s.netErr, s.netAt = addrs, err, now
		if err != nil {
			// Retry sooner after a failure.
			s.netAt = now.Add(-networkCacheTTL + 5*time.Second)
		}
	})
	s.netMu.Lock()
	defer s.netMu.Unlock()
	return s.netAddrs, s.netErr
}

// forgetNetwork makes the next use of the managed network's addresses ask
// Docker again.
func (s *Service) forgetNetwork() {
	s.netMu.Lock()
	s.netAt = time.Time{}
	s.netGen++
	s.netMu.Unlock()
}

// dropNetwork forgets the managed network's addresses at once: Docker
// reported the network as not managed or not ours (err). Docker is asked
// again after a short while.
func (s *Service) dropNetwork(err error) {
	s.netMu.Lock()
	s.netAddrs, s.netErr = docker.NetworkAddresses{}, err
	s.netAt = s.cfg.Now().Add(-networkCacheTTL + 5*time.Second)
	s.netGen++
	s.netMu.Unlock()
}

// peerAllowed reports whether an agent connection from peer may be relayed:
// an address inside a subnet of the managed network, or a loopback address
// where agents arrive from loopback: on Docker Desktop (not Linux), or on
// Linux only when --inspector-agent-host was set explicitly (a reverse
// proxy on the host would otherwise turn remote clients into loopback
// peers).
func (s *Service) peerAllowed(ctx context.Context, peer netip.Addr) bool {
	if peer.IsLoopback() {
		return s.cfg.GOOS != "linux" || s.cfg.AgentHost != ""
	}
	for _, refresh := range []bool{false, true} {
		addrs, err := s.networkAddrs(ctx, refresh)
		if err == nil {
			for _, p := range addrs.Subnets {
				if p.Contains(peer) {
					return true
				}
			}
		}
	}
	return false
}

// agentHandler serves the plaintext side of a TLS listener: only the agent
// endpoint.
func (s *Service) agentHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.prepare(w, r) {
			return
		}
		if r.URL.Path != "/ws/agent" {
			reject(w, r, http.StatusNotFound, "not_found")
			return
		}
		s.handleAgent(w, r)
	})
}

// forwardingHeaders mark a request that came through a proxy; agents
// connect directly.
var forwardingHeaders = []string{"Forwarded", "X-Forwarded-For", "Via"}

// handleAgent relays a game's Inspector agent to the hub. The request must
// come directly (no forwarding headers) from an allowed peer, the token
// must be the server's own agent token, and the server must exist with its
// Inspector switch on. The hub sees the hub's own agent token instead.
func (s *Service) handleAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !isWebSocket(r) {
		reject(w, r, http.StatusBadRequest, "websocket_required")
		return
	}
	for _, h := range forwardingHeaders {
		if len(r.Header.Values(h)) > 0 {
			s.logAgentRefusal("forwarded request", 0, r)
			reject(w, r, http.StatusForbidden, "forbidden")
			return
		}
	}
	peer, ok := netpolicy.ParsePeer(r.RemoteAddr)
	if !ok || !s.peerAllowed(r.Context(), peer) {
		s.logAgentRefusal("peer not allowed", 0, r)
		reject(w, r, http.StatusForbidden, "forbidden")
		return
	}
	v, serving := s.view()
	if !serving {
		reject(w, r, http.StatusServiceUnavailable, "hub_unavailable")
		return
	}
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	id, ok := verifyAgentToken(v.sec.TokenKey, strings.TrimSpace(tok))
	if !strings.EqualFold(scheme, "Bearer") || !ok {
		s.logAgentRefusal("invalid token", 0, r)
		reject(w, r, http.StatusUnauthorized, "auth_failed")
		return
	}
	sv, err := s.cfg.Store.GetServer(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.logAgentRefusal("unknown server", id, r)
		reject(w, r, http.StatusForbidden, "forbidden")
		return
	case err != nil:
		reject(w, r, http.StatusServiceUnavailable, "unavailable")
		return
	case !sv.Inspector:
		s.logAgentRefusal("the server's Inspector switch is off", id, r)
		reject(w, r, http.StatusForbidden, "forbidden")
		return
	}
	if s.hooks.afterAgentCheck != nil {
		s.hooks.afterAgentCheck()
	}
	var ctx context.Context
	var release func()
	full := false
	admitted := s.admit(v.gen, func() bool {
		var ok bool
		ctx, release, ok = s.agents.acquire(r.Context(), id)
		full = !ok
		return ok
	})
	switch {
	case full:
		reject(w, r, http.StatusTooManyRequests, "connection_limit")
		return
	case !admitted:
		reject(w, r, http.StatusServiceUnavailable, "hub_unavailable")
		return
	}
	defer release()
	ctx = context.WithValue(ctx, agentKey{}, agentInfo{view: v})
	s.agentProxy.ServeHTTP(newProxyWriter(w, ctx), r.WithContext(ctx))
}

type agentKey struct{}

// agentInfo is the hub snapshot one relay uses.
type agentInfo struct {
	view hubView
}

func (s *Service) logAgentRefusal(reason string, id int64, r *http.Request) {
	s.agentLog.Do(func() {
		s.logger.Warn("refused an Inspector agent connection (logged once per minute)", "reason", reason, "server_id", id,
			"remote", netpolicy.PeerKey(r.RemoteAddr))
	})
}

// sweepAgents closes the relays of servers that no longer exist or whose
// Inspector switch is off.
func (s *Service) sweepAgents(ctx context.Context) {
	if s.agents.count() == 0 {
		return
	}
	rows, err := s.cfg.Store.ListServers(ctx)
	if err != nil {
		return
	}
	on := map[int64]bool{}
	for _, r := range rows {
		if r.Inspector {
			on[r.ID] = true
		}
	}
	if n := s.agents.closeIf(func(id int64) bool { return !on[id] }); n > 0 {
		s.logger.Info("closed Inspector agent relays of servers that were deleted or whose Inspector switch is off", "relays", n)
	}
}

// newAgentProxy relays agent WebSockets to the hub's loopback port.
func (s *Service) newAgentProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			info, _ := pr.In.Context().Value(agentKey{}).(agentInfo)
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(info.view.port)})
			pr.Out.Host = hubHost
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Set("Authorization", "Bearer "+info.view.sec.AgentToken)
		},
		Transport: &agentTransport{s: s, base: newHubTransport()},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if resp.StatusCode == http.StatusSwitchingProtocols && !webSocketAnswer(resp) {
				return errBadUpgrade
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				s.logger.Debug("inspector agent relay failed", "err", err)
			}
			hubError(w, http.StatusBadGateway, "hub_unavailable")
		},
		ErrorLog: s.proxyLog,
	}
}

// agentTransport sends the hub's agent token only to a hub port Docker
// confirmed within hubCheckInterval, and drops the port on a transport
// error.
type agentTransport struct {
	s    *Service
	base http.RoundTripper
}

func (t *agentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	info, _ := req.Context().Value(agentKey{}).(agentInfo)
	if !t.s.freshHub(req.Context(), info.view) {
		return nil, errHubUnverified
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, t.s.hubUnreachable(req.Context(), info.view.port, err)
	}
	return resp, nil
}

// strippedHeaders never reach the hub from browsers or agents.
var strippedHeaders = []string{"Cookie", "Authorization", "Proxy-Authorization", "Forwarded", "X-Forwarded-For",
	"X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-Ip", "Via"}
