package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
)

// This file holds the network-facing request policy: the Host header
// allowlist (DNS rebinding defence) and client address resolution behind
// trusted reverse proxies.

// NormalizePublicHosts validates and normalises --public-host values: bare
// host names (no scheme, port, path or wildcard), lower-cased, without a
// trailing dot. Empty entries are ignored.
func NormalizePublicHosts(in []string) ([]string, error) {
	var out []string
	for _, raw := range in {
		h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if h == "" {
			continue
		}
		if len(h) > 253 || strings.ContainsAny(h, ":/[]*@ \t") {
			return nil, fmt.Errorf("invalid public host %q: give a bare host name such as deploy.example.com (no scheme, port or path)", raw)
		}
		for _, label := range strings.Split(h, ".") {
			if label == "" || len(label) > 63 || strings.IndexFunc(label, func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
			}) >= 0 {
				return nil, fmt.Errorf("invalid public host %q", raw)
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// ParseTrustedProxies parses --trusted-proxy values: CIDR prefixes, or bare
// addresses meaning a single host. Empty entries are ignored.
func ParseTrustedProxies(in []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, netip.PrefixFrom(p.Addr().Unmap(), unmappedBits(p)).Masked())
			continue
		}
		a, err := netip.ParseAddr(v)
		if err != nil || a.Zone() != "" {
			return nil, fmt.Errorf("invalid trusted proxy %q: give a CIDR such as 10.0.0.0/8 or an address", raw)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// unmappedBits converts the length of an IPv4-mapped IPv6 prefix to IPv4.
func unmappedBits(p netip.Prefix) int {
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		return p.Bits() - 96
	}
	return p.Bits()
}

// hostAllowed reports whether the Host header value may be served: IP
// literals and "localhost" with any port, or a configured public host name.
// An empty Host is refused.
func (s *Server) hostAllowed(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return h == "localhost" || s.publicHosts[h]
}

var errBadHost = newError(http.StatusMisdirectedRequest, apitypes.ErrorCodeBadHost,
	"this host name is not served here; use the address configured with --public-host")

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

// resolveClientIP returns the rate-limiting key of the request's client.
// Only when the TCP peer is a trusted proxy is X-Forwarded-For consulted,
// taking the right-most entry that is not itself a trusted proxy; entries
// to its left are client-controlled and ignored.
func (s *Server) resolveClientIP(r *http.Request) string {
	peer, ok := parsePeer(r.RemoteAddr)
	if !ok {
		return peerKey(r.RemoteAddr)
	}
	if s.trusted(peer) {
		if a, ok := s.forwardedClient(r.Header.Values("X-Forwarded-For")); ok {
			peer = a
		}
	}
	return addrKey(peer)
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.trustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// forwardedClient walks X-Forwarded-For from the right. An unparseable entry
// where the trusted chain should be makes it fall back to the peer.
func (s *Server) forwardedClient(values []string) (netip.Addr, bool) {
	var parts []string
	for _, v := range values {
		parts = append(parts, strings.Split(v, ",")...)
	}
	var last netip.Addr
	found := false
	for i := len(parts) - 1; i >= 0; i-- {
		a, ok := parseForwardedAddr(parts[i])
		if !ok {
			return netip.Addr{}, false
		}
		if !s.trusted(a) {
			return a, true
		}
		last, found = a, true
	}
	return last, found // every entry is a trusted proxy: the left-most is the client
}

func parseForwardedAddr(v string) (netip.Addr, bool) {
	v = strings.TrimSpace(v)
	if a, err := netip.ParseAddr(strings.Trim(v, "[]")); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(v); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	return netip.Addr{}, false
}

func parsePeer(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

// addrKey is an IPv4 address, or an IPv6 /64 (one host commonly controls a
// whole /64).
func addrKey(a netip.Addr) string {
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

// peerKey is the key of the TCP peer, ignoring forwarding headers.
func peerKey(remote string) string {
	if a, ok := parsePeer(remote); ok {
		return addrKey(a)
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}
