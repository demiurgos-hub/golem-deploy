// Package certs manages the game TLS files on their per-server read-only
// mounts. Self-signed servers have independent keys. In custom mode each
// mount receives a copy of the administrator's pair.
package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
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

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
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
	ListServerIDs(ctx context.Context) ([]int64, error)
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

// APIInfo maps an installed leaf to its public metadata.
func (i Info) APIInfo(host string) *apitypes.CertificateInfo {
	if i.Fingerprint == "" {
		return nil
	}
	return &apitypes.CertificateInfo{
		Subject: i.Subject, Issuer: i.Issuer,
		DNSNames: append([]string{}, i.DNSNames...), IPAddresses: append([]string{}, i.IPAddresses...),
		NotBefore: i.NotBefore.UTC(), NotAfter: i.NotAfter.UTC(),
		FingerprintSHA256: i.Fingerprint, SelfSigned: i.SelfSigned, ChainLength: i.ChainLength,
		CoversGameHost: i.Covers(host),
	}
}

// Manager owns the certificate directory. It is safe for concurrent use;
// changes are serialised.
type Manager struct {
	dir      string // canonical custom pair (also read on upgrades)
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
	if err := syncDir(filepath.Dir(filepath.Dir(dir))); err != nil {
		return nil, err
	}
	if err := m.prepareDir(dir); err != nil {
		return nil, err
	}
	if err := m.openFiles(context.Background()); err != nil {
		return nil, err
	}
	return m, nil
}

// ServerDir is the stable bind mount source used in both modes.
func (m *Manager) ServerDir(id int64) string {
	return filepath.Join(m.serversDir(), fmt.Sprint(id))
}

func (m *Manager) serversDir() string {
	return filepath.Join(filepath.Dir(m.dir), "servers")
}

// GroupAdd returns the supplementary groups a server container needs to
// read the files (nil when none are needed, e.g. on Windows hosts).
func (m *Manager) GroupAdd() []int { return slices.Clone(m.groupAdd) }

// Mode returns the configured mode after completing any committed change.
func (m *Manager) Mode(ctx context.Context) (Mode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state(ctx)
}

// Current returns certificate metadata for the configured mode. Pass 0 for
// the installation view, which only describes a custom certificate.
func (m *Manager) Current(ctx context.Context, id int64) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mode, err := m.state(ctx)
	if err != nil {
		return Info{}, err
	}
	dir := m.dir
	if mode == ModeSelfSigned {
		if id == 0 {
			return Info{Mode: mode}, nil
		}
		dir = m.ServerDir(id)
	}
	info, err := load(dir)
	info.Mode = mode
	if errors.Is(err, ErrNoCertificate) {
		return Info{Mode: mode}, nil
	}
	return info, err
}

// Ensure prepares a server's mount and returns the selected certificate.
// In custom mode it never generates a missing pair.
func (m *Manager) Ensure(ctx context.Context, id int64, gameHost func(context.Context) (string, error)) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mode, err := m.state(ctx)
	if err != nil {
		return Info{}, err
	}
	host, err := gameHost(ctx)
	if err != nil {
		return Info{}, err
	}
	dir := m.ServerDir(id)
	if mode == ModeCustom {
		info, err := load(m.dir)
		info.Mode = mode
		if err != nil {
			return info, err
		}
		if !m.now().Before(info.NotAfter) {
			return info, ErrExpired
		}
		if m.now().Before(info.NotBefore) {
			return info, ErrNoCertificate
		}
		if current, err := load(dir); err == nil && current.Fingerprint == info.Fingerprint {
			return info, nil
		}
		return info, m.copyPair(m.dir, dir)
	}
	return m.ensureSelfSigned(dir, host)
}

func (m *Manager) ensureSelfSigned(dir, host string) (Info, error) {
	if err := m.recoverPending(dir); err != nil {
		return Info{}, err
	}
	info, err := load(dir)
	if err == nil && m.selfSignedOK(info, host) {
		info.Mode = ModeSelfSigned
		return info, nil
	}
	if err != nil && !errors.Is(err, ErrNoCertificate) {
		return Info{}, err
	}
	chain, key, info, err := m.generate(host)
	if err != nil {
		return Info{}, err
	}
	return info, m.install(dir, chain, key)
}

// UseSelfSigned regenerates a distinct pair for every server.
func (m *Manager) UseSelfSigned(ctx context.Context, gameHost func(context.Context) (string, error)) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info := Info{Mode: ModeSelfSigned}
	return info, m.change(ctx, ModeSelfSigned, func(stage string, ids []int64) error {
		host, err := gameHost(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			chain, key, _, err := m.generate(host)
			if err != nil {
				return err
			}
			if err := m.install(filepath.Join(stage, fmt.Sprint(id)), chain, key); err != nil {
				return err
			}
		}
		return nil
	})
}

// UseCustom validates and installs the administrator's shared pair.
func (m *Manager) UseCustom(ctx context.Context, chainPEM, keyPEM []byte) (Info, error) {
	chain, key, err := ValidateCustom(chainPEM, keyPEM, m.now())
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.change(ctx, ModeCustom, func(stage string, ids []int64) error {
		if err := m.install(filepath.Join(stage, "custom"), chain, key); err != nil {
			return err
		}
		for _, id := range ids {
			if err := os.Mkdir(filepath.Join(stage, fmt.Sprint(id)), 0o700); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return Info{}, err
	}
	info, err := load(m.dir)
	info.Mode = ModeCustom
	return info, err
}

// RemoveServer removes the mount after the container is gone, then
// deletes the row while holding the lock used to enumerate servers for
// certificate replacements.
func (m *Manager) RemoveServer(ctx context.Context, id int64, deleteRow func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.state(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(m.ServerDir(id)); err != nil {
		return err
	}
	if err := syncDir(m.serversDir()); err != nil {
		return err
	}
	return deleteRow()
}

// Maintain checks and rotates existing certificates hourly.
func (m *Manager) Maintain(ctx context.Context, host func(context.Context) (string, error)) {
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx, host)
		}
	}
}

// Check updates existing self-signed pairs for the current host, or warns
// when the custom certificate is missing or approaching expiry.
func (m *Manager) Check(ctx context.Context, host func(context.Context) (string, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx, host); err != nil {
		m.logger.Warn("game certificate check failed", "err", err)
	}
}

func (m *Manager) check(ctx context.Context, host func(context.Context) (string, error)) error {
	mode, err := m.state(ctx)
	if err != nil {
		return err
	}
	if mode == ModeCustom {
		info, err := load(m.dir)
		if err != nil {
			return err
		}
		if left := info.NotAfter.Sub(m.now()); left <= 0 {
			m.logger.Error("the custom game certificate has expired; upload a new one", "not_after", info.NotAfter)
		} else if left < customWarnBefore {
			m.logger.Warn("the custom game certificate expires soon; upload a renewed one", "not_after", info.NotAfter)
		}
		return nil
	}
	h, err := host(ctx)
	if err != nil {
		return err
	}
	ids, err := m.settings.ListServerIDs(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := m.ServerDir(id)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue // first start or an explicit regeneration creates it
		}
		if _, err := m.ensureSelfSigned(dir, h); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// selfSignedOK reports whether info is a usable generated certificate for
// host.
func (m *Manager) selfSignedOK(info Info, host string) bool {
	now := m.now()
	if !info.SelfSigned || info.NotBefore.After(now) || info.NotAfter.Sub(now) < RenewBefore {
		return false
	}
	key, ok := info.leaf.PublicKey.(*ecdsa.PublicKey)
	if info.NotAfter.Sub(info.NotBefore) > SelfSignedValidity || !ok || key.Curve != elliptic.P256() {
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
func load(dir string) (Info, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFile))
	if errors.Is(err, os.ErrNotExist) {
		return Info{}, ErrNoCertificate
	}
	if err != nil {
		return Info{}, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, KeyFile))
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
