// Package netpolicy is the network-facing request policy the daemon's
// listeners share: the Host header allowlist (DNS rebinding defence) and
// client address resolution behind trusted reverse proxies. The panel
// listener (internal/httpapi) and the Inspector listener
// (internal/inspector) use the same Policy value.
package netpolicy

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

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
		if ip, err := netip.ParseAddr(h); err == nil && ip.Zone() == "" {
			out = append(out, ip.Unmap().String())
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

// Policy is the Host allowlist and the trusted proxy list. It is immutable
// and safe for concurrent use.
type Policy struct {
	publicHosts map[string]bool
	trusted     []netip.Prefix
	// panelHost reports whether a host is a panel host derived from the
	// game host; consulted only when no public host is configured.
	panelHost func(string) bool
}

// New returns a Policy. publicHosts must be normalised
// (NormalizePublicHosts). panelHost, when not nil, is asked about host names
// while no public host is configured (the panel certificate's hosts).
func New(publicHosts []string, trusted []netip.Prefix, panelHost func(string) bool) *Policy {
	p := &Policy{publicHosts: map[string]bool{}, trusted: append([]netip.Prefix(nil), trusted...), panelHost: panelHost}
	for _, h := range publicHosts {
		p.publicHosts[h] = true
	}
	return p
}

// HostAllowed reports whether the Host header value may be served: IP
// literals and "localhost" with any port, or a configured public host name.
// An empty Host is refused.
func (p *Policy) HostAllowed(host string) bool {
	h := Hostname(host)
	if h == "" {
		return false
	}
	if ip, err := netip.ParseAddr(h); err == nil && ip.Zone() == "" {
		return true
	}
	return h == "localhost" || p.publicHosts[h] ||
		(len(p.publicHosts) == 0 && p.panelHost != nil && p.panelHost(h))
}

// Hostname returns the host part of a Host header value, lower-cased,
// without brackets and trailing dot.
func Hostname(host string) string {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// ClientIP returns the rate-limiting key of the request's client: an IPv4
// address or an IPv6 /64. Only when the TCP peer is a trusted proxy is
// X-Forwarded-For consulted, taking the right-most entry that is not itself
// a trusted proxy; entries to its left are client-controlled and ignored.
func (p *Policy) ClientIP(r *http.Request) string {
	peer, ok := ParsePeer(r.RemoteAddr)
	if !ok {
		return PeerKey(r.RemoteAddr)
	}
	if p != nil && p.trustedAddr(peer) {
		if a, ok := p.forwardedClient(r.Header.Values("X-Forwarded-For")); ok {
			peer = a
		}
	}
	return addrKey(peer)
}

func (p *Policy) trustedAddr(a netip.Addr) bool {
	for _, t := range p.trusted {
		if t.Contains(a) {
			return true
		}
	}
	return false
}

// forwardedClient walks X-Forwarded-For from the right. An unparseable entry
// where the trusted chain should be makes it fall back to the peer.
func (p *Policy) forwardedClient(values []string) (netip.Addr, bool) {
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
		if !p.trustedAddr(a) {
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

// ParsePeer parses the address part of a RemoteAddr (IPv4-mapped addresses
// unmapped, zone dropped).
func ParsePeer(remote string) (netip.Addr, bool) {
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

// PeerKey is the key of the TCP peer, ignoring forwarding headers.
func PeerKey(remote string) string {
	if a, ok := ParsePeer(remote); ok {
		return addrKey(a)
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}
