package certs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type memSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSettings) GetSetting(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *memSettings) SetSetting(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newManager(t *testing.T) (*Manager, *clock, *memSettings) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	st := &memSettings{m: map[string]string{}}
	m, err := Open(Options{Dir: filepath.Join(t.TempDir(), "certs", "game"), Settings: st, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return m, c, st
}

func TestSelfSignedGenerationAndRotation(t *testing.T) {
	ctx := context.Background()
	m, c, _ := newManager(t)
	if _, err := m.Current(ctx); !errors.Is(err, ErrNoCertificate) {
		t.Fatalf("got %v", err)
	}
	info, err := m.Ensure(ctx, "play.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ModeSelfSigned || !info.SelfSigned || info.ChainLength != 1 ||
		!slices.Equal(sortedCopy(info.DNSNames), []string{"localhost", "play.example.com"}) ||
		!slices.Equal(sortedCopy(info.IPAddresses), []string{"127.0.0.1", "::1"}) {
		t.Fatalf("%+v", info)
	}
	if v := info.NotAfter.Sub(info.NotBefore); v != SelfSignedValidity || v > 14*24*time.Hour {
		t.Fatalf("validity %s", v)
	}
	if !info.Covers("play.example.com") || !info.Covers("127.0.0.1") || info.Covers("other.example.com") {
		t.Fatal("covers")
	}
	// The key is ECDSA P-256 (required for serverCertificateHashes).
	pair, err := tls.LoadX509KeyPair(filepath.Join(m.Dir(), CertFile), filepath.Join(m.Dir(), KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok || k.Curve != elliptic.P256() {
		t.Fatalf("key %T", pair.PrivateKey)
	}
	// Unchanged host and enough validity: kept.
	again, err := m.Ensure(ctx, "play.example.com")
	if err != nil || again.Fingerprint != info.Fingerprint {
		t.Fatalf("regenerated without reason: %v", err)
	}
	// Host change: regenerated for the new host (an IP).
	byIP, err := m.Ensure(ctx, "203.0.113.7")
	if err != nil || byIP.Fingerprint == info.Fingerprint || !byIP.Covers("203.0.113.7") || byIP.Covers("play.example.com") {
		t.Fatalf("%+v %v", byIP, err)
	}
	// Rotation once less than RenewBefore is left.
	c.add(SelfSignedValidity - RenewBefore - 2*time.Hour)
	if kept, _ := m.Ensure(ctx, "203.0.113.7"); kept.Fingerprint != byIP.Fingerprint {
		t.Fatal("rotated too early")
	}
	c.add(2 * time.Hour)
	rotated, err := m.Ensure(ctx, "203.0.113.7")
	if err != nil || rotated.Fingerprint == byIP.Fingerprint || !rotated.NotBefore.Before(c.now()) {
		t.Fatalf("not rotated: %+v %v", rotated, err)
	}
	// No host: localhost only.
	local, err := m.Ensure(ctx, "")
	if err != nil || !slices.Equal(sortedCopy(local.DNSNames), []string{"localhost"}) {
		t.Fatalf("%+v %v", local, err)
	}
	// A damaged pair is replaced.
	if err := os.WriteFile(filepath.Join(m.Dir(), KeyFile), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fixed, err := m.Ensure(ctx, ""); err != nil || fixed.Fingerprint == local.Fingerprint {
		t.Fatalf("damaged pair kept: %v", err)
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and groups are not used on Windows hosts")
	}
	m, _, _ := newManager(t)
	if _, err := m.Ensure(context.Background(), "play.example.com"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"": 0o750 | os.ModeDir, KeyFile: 0o640, CertFile: 0o644} {
		fi, err := os.Stat(filepath.Join(m.Dir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != want {
			t.Errorf("%q mode %v, want %v", name, fi.Mode(), want)
		}
	}
	if os.Getegid() == 0 {
		if m.GroupAdd() != nil {
			t.Fatal("group 0 handed to containers")
		}
	} else if !slices.Equal(m.GroupAdd(), []int{os.Getegid()}) {
		t.Fatalf("group add %v", m.GroupAdd())
	}
	entries, _ := os.ReadDir(m.Dir())
	if len(entries) != 2 {
		t.Fatalf("leftover files: %v", entries)
	}
}

type keyKind int

const (
	ecP256 keyKind = iota
	rsa2048
	rsa1024
	edKey
)

func newKey(t *testing.T, k keyKind) crypto.Signer {
	t.Helper()
	var key crypto.Signer
	var err error
	switch k {
	case ecP256:
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case rsa2048:
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	case rsa1024:
		key, err = rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // testing the rejection
	case edKey:
		_, key, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type certOpts struct {
	notBefore, notAfter time.Time
	eku                 []x509.ExtKeyUsage
	isCA                bool
}

func makeCert(t *testing.T, key crypto.Signer, parent *x509.Certificate, parentKey crypto.Signer, o certOpts) (*x509.Certificate, []byte) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "play.example.com"},
		NotBefore:    o.notBefore, NotAfter: o.notAfter,
		DNSNames:              []string{"play.example.com"},
		ExtKeyUsage:           o.eku,
		BasicConstraintsValid: true, IsCA: o.isCA,
	}
	if o.isCA {
		tmpl.KeyUsage = x509.KeyUsageCertSign
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, key.Public(), parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func keyPEM(t *testing.T, key crypto.Signer, kind string) []byte {
	t.Helper()
	var der []byte
	var err error
	switch kind {
	case "PRIVATE KEY":
		der, err = x509.MarshalPKCS8PrivateKey(key)
	case "RSA PRIVATE KEY":
		der = x509.MarshalPKCS1PrivateKey(key.(*rsa.PrivateKey))
	case "EC PRIVATE KEY":
		der, err = x509.MarshalECPrivateKey(key.(*ecdsa.PrivateKey))
	}
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

func TestCustomCertificate(t *testing.T) {
	ctx := context.Background()
	m, c, st := newManager(t)
	now := c.now()
	valid := certOpts{notBefore: now.Add(-time.Hour), notAfter: now.Add(90 * 24 * time.Hour), eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}

	caKey := newKey(t, ecP256)
	ca, caPEM := makeCert(t, caKey, nil, nil, certOpts{notBefore: now.Add(-time.Hour), notAfter: now.Add(365 * 24 * time.Hour), isCA: true})
	leafKey := newKey(t, rsa2048)
	_, leafPEM := makeCert(t, leafKey, ca, caKey, valid)
	chain := append(append([]byte{}, leafPEM...), caPEM...)

	info, err := m.UseCustom(ctx, chain, keyPEM(t, leafKey, "RSA PRIVATE KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ModeCustom || info.SelfSigned || info.ChainLength != 2 || !info.Covers("play.example.com") || st.m[SettingMode] != "custom" {
		t.Fatalf("%+v", info)
	}
	// The key is stored normalised as PKCS#8 and never with other modes.
	stored, _ := os.ReadFile(filepath.Join(m.Dir(), KeyFile))
	if !strings.HasPrefix(string(stored), "-----BEGIN PRIVATE KEY-----") {
		t.Fatalf("stored key %.40q", stored)
	}
	// Ensure never replaces a custom certificate, whatever the host.
	if again, err := m.Ensure(ctx, "other.example.com"); err != nil || again.Fingerprint != info.Fingerprint {
		t.Fatalf("custom certificate changed: %v", err)
	}
	c.add(91 * 24 * time.Hour)
	if _, err := m.Ensure(ctx, "play.example.com"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired custom certificate: %v", err)
	}
	// Back to self-signed.
	ss, err := m.UseSelfSigned(ctx, "play.example.com")
	if err != nil || ss.Mode != ModeSelfSigned || !ss.SelfSigned || st.m[SettingMode] != "self_signed" {
		t.Fatalf("%+v %v", ss, err)
	}
	// A custom certificate that disappeared is reported, not regenerated.
	ecKey := newKey(t, ecP256)
	_, ecPEM := makeCert(t, ecKey, nil, nil, certOpts{notBefore: c.now().Add(-time.Hour), notAfter: c.now().Add(30 * 24 * time.Hour)})
	if _, err := m.UseCustom(ctx, ecPEM, keyPEM(t, ecKey, "EC PRIVATE KEY")); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(m.Dir(), CertFile))
	if _, err := m.Ensure(ctx, "play.example.com"); !errors.Is(err, ErrNoCertificate) {
		t.Fatalf("got %v", err)
	}
}

func TestValidateCustomRejects(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	valid := certOpts{notBefore: now.Add(-time.Hour), notAfter: now.Add(30 * 24 * time.Hour)}
	ec := newKey(t, ecP256)
	_, ecCert := makeCert(t, ec, nil, nil, valid)
	ecKeyPEM := keyPEM(t, ec, "PRIVATE KEY")
	other := newKey(t, ecP256)
	weak := newKey(t, rsa1024)
	_, weakCert := makeCert(t, weak, nil, nil, valid)
	ed := newKey(t, edKey)
	_, edCert := makeCert(t, ed, nil, nil, valid)
	_, expired := makeCert(t, ec, nil, nil, certOpts{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-time.Hour)})
	_, future := makeCert(t, ec, nil, nil, certOpts{notBefore: now.Add(time.Hour), notAfter: now.Add(48 * time.Hour)})
	_, clientOnly := makeCert(t, ec, nil, nil, certOpts{notBefore: valid.notBefore, notAfter: valid.notAfter, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	encrypted := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{1, 2, 3}})
	legacyEncrypted := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: []byte{1}})
	big := []byte(strings.Repeat("A", MaxChainBytes+1))

	for i, tc := range []struct {
		chain, key []byte
		field      string
		msg        string
	}{
		{nil, ecKeyPEM, FieldCertificate, "required"},
		{ecCert, nil, FieldPrivateKey, "required"},
		{big, ecKeyPEM, FieldCertificate, "larger"},
		{ecCert, []byte(strings.Repeat("A", MaxKeyBytes+1)), FieldPrivateKey, "larger"},
		{[]byte("not pem"), ecKeyPEM, FieldCertificate, "not a PEM block"},
		{append(append([]byte{}, ecCert...), "trailing"...), ecKeyPEM, FieldCertificate, "not a PEM block"},
		{ecKeyPEM, ecKeyPEM, FieldCertificate, "only contain CERTIFICATE"},
		{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2}}), ecKeyPEM, FieldCertificate, "cannot be parsed"},
		{[]byte(strings.Repeat(string(ecCert), MaxChainCerts+1)), ecKeyPEM, FieldCertificate, "more than"},
		{ecCert, keyPEM(t, other, "PRIVATE KEY"), FieldPrivateKey, "does not belong"},
		{weakCert, keyPEM(t, weak, "PRIVATE KEY"), FieldPrivateKey, "at least 2048"},
		{edCert, keyPEM(t, ed, "PRIVATE KEY"), FieldPrivateKey, "RSA or ECDSA"},
		{expired, ecKeyPEM, FieldCertificate, "expired"},
		{future, ecKeyPEM, FieldCertificate, "not valid before"},
		{clientOnly, ecKeyPEM, FieldCertificate, "extended key usage"},
		{ecCert, encrypted, FieldPrivateKey, "encrypted"},
		{ecCert, legacyEncrypted, FieldPrivateKey, "encrypted"},
		{ecCert, append(append([]byte{}, ecKeyPEM...), ecKeyPEM...), FieldPrivateKey, "exactly one"},
		{ecCert, []byte("junk"), FieldPrivateKey, "no PEM private key"},
	} {
		_, _, err := ValidateCustom(tc.chain, tc.key, now)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != tc.field || !strings.Contains(ve.Msg, tc.msg) {
			t.Errorf("case %d: got %v, want %s ~ %q", i, err, tc.field, tc.msg)
			continue
		}
		if strings.Contains(ve.Msg, "PRIVATE KEY-----") || strings.Contains(ve.Msg, string(ecKeyPEM[30:60])) {
			t.Errorf("case %d: key material in message %q", i, ve.Msg)
		}
	}
	// An EC PARAMETERS block before an EC key (openssl output) is fine.
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{6, 8, 42, 134, 72, 206, 61, 3, 1, 7}})
	if _, _, err := ValidateCustom(ecCert, append(params, keyPEM(t, ec, "EC PRIVATE KEY")...), now); err != nil {
		t.Fatal(err)
	}
}

// A replacement interrupted between its renames is completed on the next
// Open; one interrupted while writing is discarded.
func TestRecoverPending(t *testing.T) {
	ctx := context.Background()
	m, c, st := newManager(t)
	old, err := m.Ensure(ctx, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Prepare a complete new pair under the pending names.
	other, _, _ := newManager(t)
	newInfo, _ := other.Ensure(ctx, "b.example.com")
	newCert, _ := os.ReadFile(filepath.Join(other.Dir(), CertFile))
	newKey, _ := os.ReadFile(filepath.Join(other.Dir(), KeyFile))
	reopen := func() Info {
		t.Helper()
		m2, err := Open(Options{Dir: m.Dir(), Settings: st, Now: c.now})
		if err != nil {
			t.Fatal(err)
		}
		info, err := m2.Current(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}

	// Crash after the key rename: the new key is installed, the new
	// certificate still pending.
	_ = os.WriteFile(filepath.Join(m.Dir(), KeyFile), newKey, 0o640)
	_ = os.WriteFile(filepath.Join(m.Dir(), pendingCert), newCert, 0o644)
	if got := reopen(); got.Fingerprint != newInfo.Fingerprint {
		t.Fatal("interrupted replacement not completed")
	}
	// Crash while writing the certificate: the pending key is discarded.
	_ = os.WriteFile(filepath.Join(m.Dir(), pendingKey), []byte("partial"), 0o640)
	if got := reopen(); got.Fingerprint != newInfo.Fingerprint {
		t.Fatal("installed pair changed")
	}
	// A pending pair that does not match is discarded.
	_ = os.WriteFile(filepath.Join(m.Dir(), pendingKey), newKey, 0o640)
	oldCert, _ := makeCertPEM(t)
	_ = os.WriteFile(filepath.Join(m.Dir(), pendingCert), oldCert, 0o644)
	_ = os.WriteFile(filepath.Join(m.Dir(), ".tmp-123"), []byte("x"), 0o600)
	if got := reopen(); got.Fingerprint != newInfo.Fingerprint || got.Fingerprint == old.Fingerprint {
		t.Fatal("mismatched pending pair installed")
	}
	if entries, _ := os.ReadDir(m.Dir()); len(entries) != 2 {
		t.Fatalf("leftovers %v", entries)
	}
}

func makeCertPEM(t *testing.T) ([]byte, crypto.Signer) {
	k := newKey(t, ecP256)
	_, p := makeCert(t, k, nil, nil, certOpts{notBefore: time.Now().Add(-time.Hour), notAfter: time.Now().Add(time.Hour)})
	return p, k
}

func TestValidateHost(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"Play.Example.COM.": "play.example.com",
		"gamebox":           "gamebox",
		"203.0.113.7":       "203.0.113.7",
		"::ffff:10.0.0.1":   "10.0.0.1",
		"2001:db8::1":       "2001:db8::1",
		" a-b.example ":     "a-b.example",
	} {
		if got, err := ValidateHost(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"https://x.example", "x.example:7000", "[::1]", "fe80::1%eth0", "-x.example", "x-.example",
		"a..b", "*.example.com", "under_score.example", strings.Repeat("a", 64) + ".example", "0.0.0.0", "::", "1.2.3.999", "x.example/a",
		"ünï.example", "a b"} {
		if got, err := ValidateHost(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	if Origin("2001:db8::1", 7001) != "https://[2001:db8::1]:7001" || Origin("play.example.com", 7001) != "https://play.example.com:7001" {
		t.Fatal("origin")
	}
}
