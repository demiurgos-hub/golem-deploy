// Package ports owns the host port range game servers are published on:
// parsing and validating the configured range, choosing free ports and
// probing the host for listeners.
package ports

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

// DefaultRange is the default --port-range.
const DefaultRange = "7000-7099"

// Bounds of a valid range.
const (
	// MinPort: ports below 1024 are privileged and host services live there.
	MinPort = 1024
	// EphemeralStart is the start of the Linux default ephemeral range
	// (net.ipv4.ip_local_port_range); the kernel hands those ports to
	// outgoing connections, so they must not be published.
	EphemeralStart = 32768
)

// unsafePorts are ports browsers refuse to connect to ("restricted" or
// "unsafe" ports): Chromium's kRestrictedPorts in net/base/port_util.cc and
// Firefox's gBadPortList in netwerk/base/nsIOService.cpp, limited to
// MinPort..EphemeralStart. Web game clients cannot reach a server on them,
// so they are skipped inside the range.
func unsafePorts() []int {
	return []int{1719, 1720, 1723, 2049, 3659, 4045, 4190, 5060, 5061, 6000, 6566, 6665, 6666, 6667, 6668, 6669, 6679, 6697, 10080}
}

// IsUnsafe reports whether browsers refuse port p.
func IsUnsafe(p int) bool { return slices.Contains(unsafePorts(), p) }

// Range is an inclusive port range.
type Range struct {
	Lo, Hi int
}

// ErrOutOfRange is returned for a port outside the range (or unsafe).
var ErrOutOfRange = errors.New("port is outside the configured port range")

// ErrExhausted means every port of the range is taken.
var ErrExhausted = errors.New("no free port left in the configured port range")

func (r Range) String() string { return strconv.Itoa(r.Lo) + "-" + strconv.Itoa(r.Hi) }

// Parse parses "LO-HI" (or a single port).
func Parse(s string) (Range, error) {
	lo, hi, found := strings.Cut(strings.TrimSpace(s), "-")
	if !found {
		hi = lo
	}
	l, err1 := strconv.Atoi(strings.TrimSpace(lo))
	h, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil {
		return Range{}, fmt.Errorf("port range %q: want LO-HI, e.g. %s", s, DefaultRange)
	}
	return Range{Lo: l, Hi: h}, nil
}

// Validate checks the range against the rules: within
// [MinPort, EphemeralStart), LO <= HI, not containing the panel's own
// listen port (0 when unknown) and holding at least one usable port.
func (r Range) Validate(panelPort int) error {
	switch {
	case r.Lo > r.Hi:
		return fmt.Errorf("port range %s: start is after end", r)
	case r.Lo < MinPort:
		return fmt.Errorf("port range %s: ports below %d are not allowed", r, MinPort)
	case r.Hi >= EphemeralStart:
		return fmt.Errorf("port range %s: ports from %d up are the Linux ephemeral range and are not allowed", r, EphemeralStart)
	case panelPort != 0 && r.Contains(panelPort):
		return fmt.Errorf("port range %s contains the panel's own listen port %d", r, panelPort)
	}
	for p := r.Lo; p <= r.Hi; p++ {
		if !IsUnsafe(p) {
			return nil
		}
	}
	return fmt.Errorf("port range %s has no usable port (all are browser-unsafe)", r)
}

// Contains reports whether p is inside the range.
func (r Range) Contains(p int) bool { return p >= r.Lo && p <= r.Hi }

// Allowed reports whether p may be assigned: inside the range and not a
// browser-unsafe port.
func (r Range) Allowed(p int) bool { return r.Contains(p) && !IsUnsafe(p) }

// Lowest returns the lowest allowed port not in used.
func (r Range) Lowest(used map[int]bool) (int, error) {
	for p := r.Lo; p <= r.Hi; p++ {
		if r.Allowed(p) && !used[p] {
			return p, nil
		}
	}
	return 0, ErrExhausted
}

// Overlaps reports whether the ranges share a port.
func (r Range) Overlaps(o Range) bool { return r.Lo <= o.Hi && o.Lo <= r.Hi }

// EphemeralRangeFile is where Linux exposes the ephemeral port range.
const EphemeralRangeFile = "/proc/sys/net/ipv4/ip_local_port_range"

// ParseEphemeralRange parses the content of EphemeralRangeFile ("LO\tHI").
func ParseEphemeralRange(content string) (Range, error) {
	f := strings.Fields(content)
	if len(f) != 2 {
		return Range{}, fmt.Errorf("unexpected ephemeral port range %q", strings.TrimSpace(content))
	}
	lo, err1 := strconv.Atoi(f[0])
	hi, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return Range{}, fmt.Errorf("unexpected ephemeral port range %q", strings.TrimSpace(content))
	}
	return Range{Lo: lo, Hi: hi}, nil
}

// EphemeralOverlap reads the host's ephemeral port range (Linux only; ok
// is false elsewhere or when it cannot be read) and reports whether r
// overlaps it. The kernel hands those ports to outgoing connections (and
// Docker assigns them to dynamically published ports), so a game server
// port inside it can be taken at any time. The fixed bounds of Validate
// assume the default range (32768-60999); a host configured with a lower
// start needs this check.
func EphemeralOverlap(r Range, read func(string) ([]byte, error)) (eph Range, overlap, ok bool) {
	b, err := read(EphemeralRangeFile)
	if err != nil {
		return Range{}, false, false
	}
	eph, err = ParseEphemeralRange(string(b))
	if err != nil {
		return Range{}, false, false
	}
	return eph, r.Overlaps(eph), true
}

// ErrBusy means something on the host listens on the port.
var ErrBusy = errors.New("port is in use on the host")

// ProbeFree checks that nothing on the host holds port p by binding it for
// TCP and UDP on bindAddr (empty: all interfaces) and releasing it.
func ProbeFree(bindAddr string, p int) error {
	addr := net.JoinHostPort(bindAddr, strconv.Itoa(p))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp %s: %w", addr, ErrBusy)
	}
	_ = ln.Close()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("udp %s: %w", addr, ErrBusy)
	}
	_ = pc.Close()
	return nil
}
