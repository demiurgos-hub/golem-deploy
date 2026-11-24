// Package certs owns the installation's game certificate: one TLS
// certificate and key, stored as tls.crt and tls.key in the data
// directory's certs/game/ and bind-mounted read-only into every server
// container, where the engine serves it for WebTransport and the HTTPS
// sidecar and reloads it when the files change.
//
// The certificate has a mode, stored in the settings table:
//
//   - self_signed (the default): the daemon generates an ECDSA P-256
//     certificate valid for 13 days (WebTransport serverCertificateHashes
//     pinning accepts at most 14) for the game host, localhost and the
//     loopback addresses. It is regenerated when the game host changes and
//     rotated when less than RenewBefore of its validity remains (Ensure,
//     called at startup, hourly by Maintain and before every server start).
//   - custom: an admin uploads a PEM chain and key (UseCustom), which are
//     validated and stored as given; the daemon never changes them.
//
// A certificate manager (ACME) is meant to become a third mode on top of
// the same files: it writes through install, and Ensure renews it.
//
// Files are replaced atomically and in a fixed order (see install), so a
// running engine either keeps the old pair or loads the new one. The
// container user (uid 65532) reads them through the group: on Unix the
// directory is 0750 and the key 0640, owned by the daemon's group, and the
// container gets that group as a supplementary group (GroupAdd); when the
// daemon runs with group 0 the files get group 65532 (the container's own
// group) instead, so no container is ever given group 0.
package certs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Mode is how the game certificate is obtained.
type Mode string

// Modes.
const (
	ModeSelfSigned Mode = "self_signed"
	ModeCustom     Mode = "custom"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeSelfSigned || m == ModeCustom }

// File names inside the certificate directory, and the paths the engine
// finds them at inside every server container.
const (
	CertFile = "tls.crt"
	KeyFile  = "tls.key"
	// ContainerDir is where the directory is mounted (read-only) in
	// server containers.
	ContainerDir = "/run/golem-deploy/tls"
)

// Self-signed certificate parameters.
const (
	// SelfSignedValidity is the total validity of a generated certificate
	// (NotAfter - NotBefore). WebTransport certificate hash pinning requires
	// at most 14 days.
	SelfSignedValidity = 13 * 24 * time.Hour
	// backdate moves NotBefore into the past to absorb small clock skew
	// between the host and players; it counts against the validity.
	backdate = time.Hour
	// RenewBefore: a self-signed certificate with less validity left is
	// replaced.
	RenewBefore = 5 * 24 * time.Hour
	// CheckInterval is how often Maintain runs Ensure.
	CheckInterval = time.Hour
	// customWarnBefore: Maintain warns about a custom certificate expiring
	// within this time.
	customWarnBefore = 14 * 24 * time.Hour
)

// ContainerGID is the primary group of the container user (distroless
// "nonroot"); it gets the files when the daemon's own group is 0.
const ContainerGID = 65532

// Errors.
var (
	// ErrNoCertificate: no valid certificate is installed (a custom
	// certificate that was never uploaded or is damaged).
	ErrNoCertificate = errors.New("certs: no valid game certificate is installed")
	// ErrExpired: the custom certificate has expired.
	ErrExpired = errors.New("certs: the game certificate has expired")
)

// SettingMode is the settings key holding the mode ("" = self_signed).
const SettingMode = "game_certificate_mode"

// Settings persists the mode (the store's settings table).
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Options configures Open.
type Options struct {
	// Dir is the certificate directory (<data>/certs/game). Its parent is
	// created with mode 0700 when missing.
	Dir      string
	Settings Settings
	Logger   *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Info describes the installed certificate (the leaf of the chain).
type Info struct {
	Mode        Mode
	Subject     string
	Issuer      string
	DNSNames    []string
	IPAddresses []string
	NotBefore   time.Time
	NotAfter    time.Time
	// Fingerprint is the lower-case hex SHA-256 of the leaf's DER encoding
	// (the value WebTransport serverCertificateHashes pins).
	Fingerprint string
	// SelfSigned reports whether the leaf is signed by its own key.
	SelfSigned  bool
	ChainLength int

	leaf *x509.Certificate
}

// Covers reports whether the leaf is valid for host (a DNS name or IP).
func (i Info) Covers(host string) bool {
	if i.leaf == nil || host == "" {
		return false
	}
	return i.leaf.VerifyHostname(host) == nil
}

// Manager owns the certificate directory. It is safe for concurrent use;
// changes are serialised.
type Manager struct {
	dir      string
	settings Settings
	logger   *slog.Logger
	now      func() time.Time
	fileGID  int // group of the files (-1: leave as created)
	groupAdd []int

	mu sync.Mutex
}

// Open prepares the directory (creating it, fixing its mode and group and
// completing or discarding a replacement interrupted by a crash) and
// returns a Manager. It does not generate a certificate; call Ensure.
func Open(o Options) (*Manager, error) {
	if o.Dir == "" || o.Settings == nil {
		return nil, errors.New("certs: Dir and Settings are required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, settings: o.Settings, logger: o.Logger, now: o.Now}
	m.fileGID, m.groupAdd = groupPolicy()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("create certificate directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create certificate directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("certificate directory %s is not a directory", dir)
	}
	if err := m.setPerm(dir, 0o750); err != nil {
		return nil, err
	}
	m.recoverPending()
	return m, nil
}

// Dir returns the absolute certificate directory (the bind mount source).
func (m *Manager) Dir() string { return m.dir }

// GroupAdd returns the supplementary groups a server container needs to
// read the files (nil when none are needed, e.g. on Windows hosts).
func (m *Manager) GroupAdd() []int { return slices.Clone(m.groupAdd) }

// Mode returns the configured mode.
func (m *Manager) Mode(ctx context.Context) (Mode, error) {
	v, ok, err := m.settings.GetSetting(ctx, SettingMode)
	if err != nil {
		return "", err
	}
	if !ok || v == "" {
		return ModeSelfSigned, nil
	}
	if !Mode(v).Valid() {
		return "", fmt.Errorf("certs: unknown certificate mode %q in settings", v)
	}
	return Mode(v), nil
}

// Current returns the installed certificate without changing anything
// (ErrNoCertificate when none is installed).
func (m *Manager) Current(ctx context.Context) (Info, error) {
	mode, err := m.Mode(ctx)
	if err != nil {
		return Info{}, err
	}
	info, err := m.load()
	if err != nil {
		return Info{}, err
	}
	info.Mode = mode
	return info, nil
}

// Ensure makes sure a usable certificate for host is installed and returns
// it. In self_signed mode it generates one when none is installed, when
// the installed one is not self-signed, does not name exactly host,
// localhost and the loopback addresses, is not yet valid or has less than
// RenewBefore left. In custom mode it changes nothing and reports
// ErrNoCertificate or ErrExpired.
func (m *Manager) Ensure(ctx context.Context, host string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mode, err := m.Mode(ctx)
	if err != nil {
		return Info{}, err
	}
	info, lerr := m.load()
	if mode == ModeCustom {
		if lerr != nil {
			return Info{}, lerr
		}
		info.Mode = mode
		if !m.now().Before(info.NotAfter) {
			return info, ErrExpired
		}
		return info, nil
	}
	if lerr == nil && m.selfSignedOK(info, host) {
		info.Mode = mode
		return info, nil
	}
	reason := "none installed"
	if lerr == nil {
		reason = "host or validity changed"
	}
	return m.generate(host, reason)
}

// UseSelfSigned switches to self_signed mode and installs a freshly
// generated certificate for host.
func (m *Manager) UseSelfSigned(ctx context.Context, host string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, err := m.generate(host, "requested")
	if err != nil {
		return Info{}, err
	}
	if err := m.settings.SetSetting(ctx, SettingMode, string(ModeSelfSigned)); err != nil {
		return Info{}, err
	}
	return info, nil
}

// UseCustom validates an uploaded PEM chain (leaf first) and private key,
// installs them and switches to custom mode. Validation failures are
// *ValidationError.
func (m *Manager) UseCustom(ctx context.Context, chainPEM, keyPEM []byte) (Info, error) {
	chain, key, err := ValidateCustom(chainPEM, keyPEM, m.now())
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.install(chain, key); err != nil {
		return Info{}, err
	}
	if err := m.settings.SetSetting(ctx, SettingMode, string(ModeCustom)); err != nil {
		return Info{}, err
	}
	info, err := m.load()
	if err != nil {
		return Info{}, err
	}
	info.Mode = ModeCustom
	m.logger.Info("custom game certificate installed", "fingerprint", info.Fingerprint, "not_after", info.NotAfter)
	return info, nil
}

// Maintain runs Ensure for the current game host every CheckInterval until
// ctx is done (call Ensure once at startup first). Failures are logged.
func (m *Manager) Maintain(ctx context.Context, host func(context.Context) (string, error)) {
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.Check(ctx, host)
	}
}

// Check runs Ensure once for the current game host and logs the outcome.
func (m *Manager) Check(ctx context.Context, host func(context.Context) (string, error)) {
	h, err := host(ctx)
	if err != nil {
		m.logger.Warn("could not read the game host for the certificate check", "err", err)
		return
	}
	info, err := m.Ensure(ctx, h)
	switch {
	case errors.Is(err, ErrExpired):
		m.logger.Error("the custom game certificate has expired; upload a new one (PUT /api/v1/certificates/game)", "not_after", info.NotAfter)
	case err != nil:
		m.logger.Error("game certificate check failed", "err", err)
	case info.Mode == ModeCustom && info.NotAfter.Sub(m.now()) < customWarnBefore:
		m.logger.Warn("the custom game certificate expires soon; upload a renewed one", "not_after", info.NotAfter)
	}
}

// selfSignedOK reports whether info is a usable generated certificate for
// host.
func (m *Manager) selfSignedOK(info Info, host string) bool {
	now := m.now()
	if !info.SelfSigned || info.NotBefore.After(now) || info.NotAfter.Sub(now) < RenewBefore {
		return false
	}
	if info.NotAfter.Sub(info.NotBefore) > 14*24*time.Hour {
		return false
	}
	dns, ips := selfSignedNames(host)
	return slices.Equal(sortedCopy(info.DNSNames), sortedCopy(dns)) && slices.Equal(sortedCopy(info.IPAddresses), sortedCopy(ips))
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// load reads and parses the installed pair.
func (m *Manager) load() (Info, error) {
	certPEM, err := os.ReadFile(filepath.Join(m.dir, CertFile))
	if errors.Is(err, os.ErrNotExist) {
		return Info{}, ErrNoCertificate
	}
	if err != nil {
		return Info{}, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(m.dir, KeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return Info{}, ErrNoCertificate
	}
	if err != nil {
		return Info{}, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) == 0 {
		return Info{}, fmt.Errorf("%w: the installed files do not form a valid pair", ErrNoCertificate)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Info{}, fmt.Errorf("%w: %w", ErrNoCertificate, err)
	}
	return describe(leaf, len(pair.Certificate)), nil
}

func describe(leaf *x509.Certificate, chainLen int) Info {
	sum := sha256.Sum256(leaf.Raw)
	info := Info{
		Subject:     clean(leaf.Subject.String(), 256),
		Issuer:      clean(leaf.Issuer.String(), 256),
		DNSNames:    []string{},
		IPAddresses: []string{},
		NotBefore:   leaf.NotBefore.UTC(),
		NotAfter:    leaf.NotAfter.UTC(),
		Fingerprint: hex.EncodeToString(sum[:]),
		ChainLength: chainLen,
		leaf:        leaf,
	}
	for i, n := range leaf.DNSNames {
		if i == 100 {
			break
		}
		info.DNSNames = append(info.DNSNames, clean(n, 253))
	}
	for i, ip := range leaf.IPAddresses {
		if i == 100 {
			break
		}
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}
	info.SelfSigned = bytes.Equal(leaf.RawSubject, leaf.RawIssuer) &&
		leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
	return info
}

// clean bounds s and replaces control and format characters (it is shown
// to users).
func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "\ufffd")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			r = '\ufffd'
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// selfSignedNames returns the SANs of a generated certificate for host.
func selfSignedNames(host string) (dns, ips []string) {
	dns = []string{"localhost"}
	ips = []string{"127.0.0.1", "::1"}
	if host == "" {
		return dns, ips
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		s := ip.Unmap().String()
		if !slices.Contains(ips, s) {
			ips = append(ips, s)
		}
		return dns, ips
	}
	if !slices.Contains(dns, host) {
		dns = append(dns, host)
	}
	return dns, ips
}

// ValidateHost checks and normalises a game host: a DNS name (letters,
// digits and hyphens in dot-separated labels, lower-cased, without a
// trailing dot) or an IP address (without zone or brackets). The empty
// string is returned unchanged (no host configured).
func ValidateHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", nil
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if ip.Zone() != "" {
			return "", errors.New("an IP address with a zone cannot be used")
		}
		if ip.IsUnspecified() || ip.IsMulticast() {
			return "", errors.New("the address must be one players can connect to")
		}
		return ip.Unmap().String(), nil
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if len(h) == 0 || len(h) > 253 {
		return "", errors.New("must be a DNS name or an IP address")
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("must be a DNS name or an IP address")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", errors.New("must be a DNS name or an IP address")
			}
		}
	}
	// An all-numeric name would be read as an IPv4 address by clients.
	if last := h[strings.LastIndexByte(h, '.')+1:]; strings.Trim(last, "0123456789") == "" {
		return "", errors.New("must be a DNS name or an IP address")
	}
	return h, nil
}

// URLHost returns host formatted for a URL authority (IPv6 in brackets).
func URLHost(host string) string {
	if ip, err := netip.ParseAddr(host); err == nil && ip.Is6() {
		return "[" + ip.String() + "]"
	}
	return host
}

// hostPort joins host and port for URLs.
func hostPort(host string, port int) string {
	return net.JoinHostPort(host, fmt.Sprint(port))
}

// Origin returns the https origin of host and port, e.g.
// "https://play.example.com:7001".
func Origin(host string, port int) string {
	return "https://" + hostPort(host, port)
}
