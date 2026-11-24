package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Temporary names of a replacement in progress (same directory, so the
// renames are atomic).
const (
	pendingCert = CertFile + ".new"
	pendingKey  = KeyFile + ".new"
)

// generate creates a new self-signed pair for host.
func (m *Manager) generate(host string, otherHosts ...string) ([]byte, []byte, Info, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, Info{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, Info{}, err
	}
	dns, ips := selfSignedNames(append([]string{host}, otherHosts...)...)
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
		return nil, nil, Info{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, Info{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, Info{}, err
	}
	info := describe(leaf, 1)
	info.Mode = ModeSelfSigned
	return certPEM, keyPEM, info, nil
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
func (m *Manager) install(dir string, certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("certs: refusing to install an invalid pair: %w", err)
	}
	if err := m.prepareDir(dir); err != nil {
		return err
	}
	if err := m.recoverPending(dir); err != nil {
		return err
	}
	keyMode := os.FileMode(0o600)
	if filepath.Dir(dir) == m.serversDir() {
		keyMode = 0o640
	}
	if err := m.writeFile(dir, pendingKey, keyPEM, keyMode); err != nil {
		return err
	}
	if err := m.writeFile(dir, pendingCert, certPEM, 0o644); err != nil {
		_ = os.Remove(filepath.Join(dir, pendingKey))
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, pendingKey), filepath.Join(dir, KeyFile)); err != nil {
		return fmt.Errorf("install certificate key: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, pendingCert), filepath.Join(dir, CertFile)); err != nil {
		return fmt.Errorf("install certificate: %w", err)
	}
	return syncDir(dir)
}

// writeFile writes name in the directory through a uniquely named
// temporary file and renames it to name.
func (m *Manager) writeFile(dir, name string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(dir, ".tmp-*")
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
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	ok = true
	return nil
}

func (m *Manager) prepareDir(dir string) error {
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("certificate path %s is not a directory", dir)
	}
	if err := m.setPerm(dir, 0o750); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}

// recoverPending completes an interrupted replacement or discards an
// unfinished stage. The caller holds the manager lock.
func (m *Manager) recoverPending(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	cert, err := os.ReadFile(filepath.Join(dir, pendingCert))
	if errors.Is(err, os.ErrNotExist) {
		return removeIfPresent(filepath.Join(dir, pendingKey))
	}
	if err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(dir, pendingKey))
	hasPendingKey := err == nil
	if errors.Is(err, os.ErrNotExist) {
		key, err = os.ReadFile(filepath.Join(dir, KeyFile))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return errors.Join(removeIfPresent(filepath.Join(dir, pendingCert)), removeIfPresent(filepath.Join(dir, pendingKey)))
	}
	if hasPendingKey {
		if err := os.Rename(filepath.Join(dir, pendingKey), filepath.Join(dir, KeyFile)); err != nil {
			return err
		}
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	if err := os.Rename(filepath.Join(dir, pendingCert), filepath.Join(dir, CertFile)); err != nil {
		return err
	}
	return syncDir(dir)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Manager) copyPair(src, dst string) error {
	chain, err := os.ReadFile(filepath.Join(src, CertFile))
	if err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(src, KeyFile))
	if err != nil {
		return err
	}
	return m.install(dst, chain, key)
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
