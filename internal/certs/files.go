package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Temporary names of a replacement in progress (same directory, so the
// renames are atomic).
const (
	pendingCert = CertFile + ".new"
	pendingKey  = KeyFile + ".new"
)

// generate creates and installs a self-signed certificate for host.
func (m *Manager) generate(host, reason string) (Info, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Info{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return Info{}, err
	}
	dns, ips := selfSignedNames(host)
	cn := host
	if cn == "" {
		cn = "localhost"
	}
	notBefore := m.now().Add(-backdate).UTC().Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Golem Deploy self-signed"}},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(SelfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
	}
	for _, s := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(s))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Info{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Info{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := m.install(certPEM, keyPEM); err != nil {
		return Info{}, err
	}
	info, err := m.load()
	if err != nil {
		return Info{}, err
	}
	info.Mode = ModeSelfSigned
	m.logger.Info("self-signed game certificate generated", "reason", reason, "host", host, "fingerprint", info.Fingerprint,
		"not_after", info.NotAfter)
	return info, nil
}

// install replaces the pair. Both files are first written completely
// (fsynced, with their final mode and group) under temporary names, then
// renamed over the old ones: the key first, the certificate last. Each
// rename is atomic, so a reader never sees a partly written file; between
// the two renames the directory holds the new key with the old
// certificate, a mismatched pair that the engine's reload (and its startup
// load) rejects, keeping the previous certificate and retrying on the next
// poll until the certificate rename completes. A crash between the renames
// is completed by recoverPending on the next start.
func (m *Manager) install(certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("certs: refusing to install an invalid pair: %w", err)
	}
	if err := m.writeFile(pendingKey, keyPEM, 0o640); err != nil {
		return err
	}
	if err := m.writeFile(pendingCert, certPEM, 0o644); err != nil {
		_ = os.Remove(filepath.Join(m.dir, pendingKey))
		return err
	}
	if err := os.Rename(filepath.Join(m.dir, pendingKey), filepath.Join(m.dir, KeyFile)); err != nil {
		return fmt.Errorf("install certificate key: %w", err)
	}
	if err := os.Rename(filepath.Join(m.dir, pendingCert), filepath.Join(m.dir, CertFile)); err != nil {
		return fmt.Errorf("install certificate: %w", err)
	}
	syncDir(m.dir)
	return nil
}

// writeFile writes name in the directory through a uniquely named
// temporary file and renames it to name.
func (m *Manager) writeFile(name string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(m.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := m.setPermFile(f, mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, name)); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	ok = true
	return nil
}

// recoverPending finishes a replacement interrupted between its renames,
// or discards one interrupted while writing, and removes stray temporary
// files.
func (m *Manager) recoverPending() {
	entries, _ := os.ReadDir(m.dir)
	for _, e := range entries {
		if len(e.Name()) > 5 && e.Name()[:5] == ".tmp-" {
			_ = os.Remove(filepath.Join(m.dir, e.Name()))
		}
	}
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(m.dir, n))
		if err != nil {
			return nil
		}
		return b
	}
	cert := read(pendingCert)
	if cert == nil {
		// Interrupted before the certificate was written: the installed
		// pair was never touched.
		_ = os.Remove(filepath.Join(m.dir, pendingKey))
		return
	}
	key := read(pendingKey)
	keyName := pendingKey
	if key == nil {
		key, keyName = read(KeyFile), KeyFile // the key was already renamed
	}
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		_ = os.Remove(filepath.Join(m.dir, pendingCert))
		_ = os.Remove(filepath.Join(m.dir, pendingKey))
		return
	}
	if keyName == pendingKey {
		if err := os.Rename(filepath.Join(m.dir, pendingKey), filepath.Join(m.dir, KeyFile)); err != nil {
			m.logger.Warn("could not complete an interrupted certificate replacement", "err", err)
			return
		}
	}
	if err := os.Rename(filepath.Join(m.dir, pendingCert), filepath.Join(m.dir, CertFile)); err != nil {
		m.logger.Warn("could not complete an interrupted certificate replacement", "err", err)
		return
	}
	m.logger.Info("completed a certificate replacement interrupted by a restart")
}

// setPerm sets the mode and group of an existing path.
func (m *Manager) setPerm(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set mode of %s: %w", path, err)
	}
	return chownGroup(path, m.fileGID)
}

func (m *Manager) setPermFile(f *os.File, mode os.FileMode) error {
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("set mode of %s: %w", f.Name(), err)
	}
	return chownGroup(f.Name(), m.fileGID)
}
