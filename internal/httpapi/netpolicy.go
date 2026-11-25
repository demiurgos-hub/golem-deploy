package httpapi

import (
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/netpolicy"
)

// This file applies the network-facing request policy (internal/netpolicy):
// the Host header allowlist (DNS rebinding defence) and client address
// resolution behind trusted reverse proxies. The Inspector listener shares
// the same netpolicy.Policy.

// NormalizePublicHosts validates and normalises --public-host values (see
// netpolicy.NormalizePublicHosts).
func NormalizePublicHosts(in []string) ([]string, error) { return netpolicy.NormalizePublicHosts(in) }

// ParseTrustedProxies parses --trusted-proxy values (see
// netpolicy.ParseTrustedProxies).
func ParseTrustedProxies(in []string) ([]netip.Prefix, error) {
	return netpolicy.ParseTrustedProxies(in)
}

// hostAllowed reports whether the Host header value may be served (see
// netpolicy.Policy.HostAllowed).
func (s *Server) hostAllowed(host string) bool { return s.policy.HostAllowed(host) }

func requestHostname(host string) string { return netpolicy.Hostname(host) }

var errBadHost = newError(http.StatusMisdirectedRequest, apitypes.ErrorCodeBadHost,
	"this host name is not served here; open the panel by IP address, by localhost or by a name configured with --public-host")

// sampledLog emits at most one line per interval and counts the rest.
type sampledLog struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

const badHostLogInterval = time.Minute

func (s *Server) logBadHost(r *http.Request, reqID string) {
	l := &s.badHostLog
	l.mu.Lock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < badHostLogInterval {
		l.suppressed++
		l.mu.Unlock()
		return
	}
	suppressed := l.suppressed
	l.last, l.suppressed = now, 0
	l.mu.Unlock()
	s.logger.Warn("rejected request for unknown host (configure --public-host if this name is legitimate)",
		"request_id", reqID, "host", clip(r.Host, 100), "remote", clientIP(r), "suppressed_since_last", suppressed)
}

// resolveClientIP returns the rate-limiting key of the request's client
// (see netpolicy.Policy.ClientIP).
func (s *Server) resolveClientIP(r *http.Request) string { return s.policy.ClientIP(r) }

// peerKey is the key of the TCP peer, ignoring forwarding headers.
func peerKey(remote string) string { return netpolicy.PeerKey(remote) }
