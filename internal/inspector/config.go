package inspector

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/netpolicy"
)

// ResolveListen turns --inspector-listen into the listener address: empty
// means the host part of the panel's --listen with DefaultPort, ListenOff
// returns "" (the Inspector is unavailable). The host must be empty, an IP
// address or a host name; the port is required.
func ResolveListen(value, panelListen string) (string, error) {
	v := strings.TrimSpace(value)
	if strings.EqualFold(v, ListenOff) {
		return "", nil
	}
	if v == "" {
		host, _, err := net.SplitHostPort(panelListen)
		if err != nil {
			return "", fmt.Errorf("--inspector-listen: cannot derive it from --listen %q: %w", panelListen, err)
		}
		return net.JoinHostPort(host, strconv.Itoa(DefaultPort)), nil
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("--inspector-listen %q: want host:port, :port or %q", value, ListenOff)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("--inspector-listen %q: invalid port", value)
	}
	if host != "" {
		if a, err := netip.ParseAddr(host); err == nil {
			if a.Zone() != "" {
				return "", fmt.Errorf("--inspector-listen %q: zoned addresses are not supported", value)
			}
		} else if hs, err := netpolicy.NormalizePublicHosts([]string{host}); err != nil || len(hs) != 1 {
			return "", fmt.Errorf("--inspector-listen %q: invalid host", value)
		}
	}
	return v, nil
}

// ListenPort returns the port of a resolved listen address (0 when none).
func ListenPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// ValidatePublicURL checks --inspector-public-url: an https or http origin
// without credentials, path, query or fragment. It returns the origin in
// canonical form (canonicalOrigin).
func ValidatePublicURL(raw string) (string, error) {
	v := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if v == "" {
		return "", nil
	}
	o, err := canonicalOrigin(v)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(o)
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err != nil {
		if hs, err := netpolicy.NormalizePublicHosts([]string{host}); err != nil || len(hs) != 1 {
			return "", errBadOrigin
		}
	}
	return o, nil
}

var errBadOrigin = errors.New("--inspector-public-url must be an origin such as https://inspector.example.com (no path, query or credentials)")

// canonicalOrigin returns an http or https origin in the form browsers
// send it: lower-case scheme and host, an IP address in its canonical
// spelling (IPv6 in brackets), no default port, no trailing slash. Origins
// are compared in this form.
func canonicalOrigin(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Host == "" {
		return "", errBadOrigin
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", errBadOrigin
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", errBadOrigin
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, ` %/\@`) {
		return "", errBadOrigin
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return "", errBadOrigin
		}
		host = a.String()
	} else if strings.Contains(host, ":") {
		return "", errBadOrigin
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", errBadOrigin
		}
		if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
			port = ""
		}
	}
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return scheme + "://" + authority, nil
}

// canonicalHost lowers a host name and spells an IP address canonically
// (no brackets).
func canonicalHost(h string) string {
	h = strings.ToLower(strings.Trim(h, "[]"))
	if a, err := netip.ParseAddr(h); err == nil {
		return a.String()
	}
	return h
}

// PublicHost returns the host name of a validated public URL ("" for none).
func PublicHost(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ValidateAgentHost checks --inspector-agent-host: an IP address or a host
// name, without port.
func ValidateAgentHost(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if a, err := netip.ParseAddr(strings.Trim(v, "[]")); err == nil && a.Zone() == "" {
		return a.Unmap().String(), nil
	}
	hs, err := netpolicy.NormalizePublicHosts([]string{v})
	if err != nil || len(hs) != 1 {
		return "", fmt.Errorf("--inspector-agent-host %q must be an IP address or a host name without port", raw)
	}
	return hs[0], nil
}

// ValidateImage checks --inspector-image.
func ValidateImage(ref string) error {
	if err := docker.ValidateHubImageRef(ref); err != nil {
		return fmt.Errorf("--inspector-image %q: %w", ref, err)
	}
	return nil
}
