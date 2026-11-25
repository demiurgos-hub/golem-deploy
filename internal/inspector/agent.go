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
	MaxAgentsPerServer = 2
	MaxAgents          = 256
	// networkCacheTTL is how long the managed network's addresses are
	// reused; a peer outside them triggers a refresh at most this often.
	networkCacheTTL = 30 * time.Second
)

// agentRegistry bounds and tracks relayed agent connections, so turning
// the Inspector off or rotating its secrets closes them.
type agentRegistry struct {
	mu        sync.Mutex
	perServer map[int64]int
	conns     map[uint64]context.CancelFunc
	next      uint64
}

func newAgentRegistry() *agentRegistry {
	return &agentRegistry{perServer: map[int64]int{}, conns: map[uint64]context.CancelFunc{}}
}

// acquire reserves a connection slot for server id; ctx is cancelled by
// closeAll.
func (a *agentRegistry) acquire(parent context.Context, id int64) (context.Context, func(), bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.perServer[id] >= MaxAgentsPerServer || len(a.conns) >= MaxAgents {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	a.next++
	key := a.next
	a.conns[key] = cancel
	a.perServer[id]++
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			a.mu.Lock()
			defer a.mu.Unlock()
			delete(a.conns, key)
			if a.perServer[id]--; a.perServer[id] <= 0 {
				delete(a.perServer, id)
			}
		})
	}, true
}

// closeAll cancels every relayed connection.
func (a *agentRegistry) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cancel := range a.conns {
		cancel()
	}
}

func (a *agentRegistry) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conns)
}

// isWebSocket reports whether r asks for a WebSocket upgrade.
func isWebSocket(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
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
func (s *Service) networkAddrs(ctx context.Context, refresh bool) (docker.NetworkAddresses, error) {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	now := s.cfg.Now()
	age := now.Sub(s.netAt)
	if !s.netAt.IsZero() && (age < networkCacheTTL && !(refresh && age > networkCacheTTL/6)) {
		return s.netAddrs, s.netErr
	}
	if s.cfg.Docker == nil {
		return docker.NetworkAddresses{}, docker.ErrUnavailable
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	addrs, err := s.cfg.Docker.ManagedNetworkAddresses(qctx)
	s.netAddrs, s.netErr, s.netAt = addrs, err, now
	if err != nil {
		// Retry sooner after a failure.
		s.netAt = now.Add(-networkCacheTTL + 5*time.Second)
	}
	return addrs, err
}

func (s *Service) forgetNetwork() {
	s.netMu.Lock()
	s.netAt = time.Time{}
	s.netMu.Unlock()
}

// peerAllowed reports whether an agent connection from peer may be relayed:
// a loopback address or an address inside a subnet of the managed network.
func (s *Service) peerAllowed(ctx context.Context, peer netip.Addr) bool {
	if peer.IsLoopback() {
		return true
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

// agentHandler serves the plaintext side of the listener: only the agent
// endpoint.
func (s *Service) agentHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/agent" {
			hubError(w, http.StatusNotFound, "not_found")
			return
		}
		s.handleAgent(w, r)
	})
}

// handleAgent relays a game's Inspector agent to the hub. The peer must be
// a container on the managed network (or a loopback address), the token
// must be the server's own agent token, and the server must exist with its
// Inspector switch on. The hub sees the hub's own agent token instead.
func (s *Service) handleAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !isWebSocket(r) {
		hubError(w, http.StatusBadRequest, "websocket_required")
		return
	}
	peer, ok := netpolicy.ParsePeer(r.RemoteAddr)
	if !ok || !s.peerAllowed(r.Context(), peer) {
		s.logAgentRefusal("peer outside the Docker network", 0, r)
		hubError(w, http.StatusForbidden, "forbidden")
		return
	}
	sec, port, serving := s.agentTarget()
	if !serving || sec == nil {
		hubError(w, http.StatusServiceUnavailable, "hub_unavailable")
		return
	}
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	id, ok := verifyAgentToken(sec.TokenKey, strings.TrimSpace(tok))
	if !strings.EqualFold(scheme, "Bearer") || !ok {
		s.logAgentRefusal("invalid token", 0, r)
		hubError(w, http.StatusUnauthorized, "auth_failed")
		return
	}
	sv, err := s.cfg.Store.GetServer(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.logAgentRefusal("unknown server", id, r)
		hubError(w, http.StatusForbidden, "forbidden")
		return
	case err != nil:
		hubError(w, http.StatusServiceUnavailable, "unavailable")
		return
	case !sv.Inspector:
		s.logAgentRefusal("the server's Inspector switch is off", id, r)
		hubError(w, http.StatusForbidden, "forbidden")
		return
	}
	ctx, release, ok := s.agents.acquire(r.Context(), id)
	if !ok {
		hubError(w, http.StatusTooManyRequests, "connection_limit")
		return
	}
	defer release()
	ctx = context.WithValue(ctx, agentKey{}, agentInfo{port: port, token: sec.AgentToken})
	s.agentProxy.ServeHTTP(w, r.WithContext(ctx))
}

type agentKey struct{}

type agentInfo struct {
	port  int
	token string
}

// agentTarget returns the secrets and the hub port while the Inspector
// serves.
func (s *Service) agentTarget() (*hubSecrets, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sec, s.hubPort, s.enabled && s.ln != nil && s.hubPort != 0
}

func (s *Service) logAgentRefusal(reason string, id int64, r *http.Request) {
	s.agentLog.Do(func() {
		s.logger.Warn("refused an Inspector agent connection (logged once per minute)", "reason", reason, "server_id", id,
			"remote", netpolicy.PeerKey(r.RemoteAddr))
	})
}

// newAgentProxy relays agent WebSockets to the hub's loopback port.
func (s *Service) newAgentProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			info, _ := pr.In.Context().Value(agentKey{}).(agentInfo)
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(info.port)})
			pr.Out.Host = hubHost
			for _, h := range strippedHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Set("Authorization", "Bearer "+info.token)
		},
		Transport:      newHubTransport(),
		ModifyResponse: func(resp *http.Response) error { resp.Header.Del("Set-Cookie"); return nil },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				s.logger.Debug("inspector agent relay failed", "err", err)
				hubError(w, http.StatusBadGateway, "hub_unavailable")
			}
		},
		ErrorLog: s.proxyLog,
	}
}

// strippedHeaders never reach the hub from browsers or agents.
var strippedHeaders = []string{"Cookie", "Authorization", "Proxy-Authorization", "Forwarded", "X-Forwarded-For",
	"X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-Ip"}
