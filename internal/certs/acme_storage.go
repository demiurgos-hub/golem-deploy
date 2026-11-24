package certs

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type acmeStorage struct {
	root, directoryHash string
}

type storedACMEPair struct {
	AccountURL string
	Chain, Key []byte
}

func (m *acmeClient) storage(account ACMEAccount) acmeStorage {
	return acmeStorage{m.dir, acmeStorageHash(account.Directory)}
}

func acmeStorageHash(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func accountStorageNames(email string) (string, string) {
	name := acmeStorageHash(strings.ToLower(strings.TrimSpace(email)))
	return name + ".key", name + ".json"
}

func (s acmeStorage) path(kind, name string) string {
	return filepath.Join(s.root, s.directoryHash, kind, name)
}

func (s acmeStorage) read(kind, name string) ([]byte, error) {
	return os.ReadFile(s.path(kind, name))
}

func (s acmeStorage) write(kind, name string, data []byte) error {
	dir := s.root
	for _, child := range []string{"", s.directoryHash, kind} {
		dir = filepath.Join(dir, child)
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("ACME storage path %s is not a directory", dir)
		}
		if info.Mode().Perm() != 0o700 {
			if err := os.Chmod(dir, 0o700); err != nil {
				return err
			}
		}
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	if err := writeFile(dir, name, data, 0o600, -1); err != nil {
		return err
	}
	return syncDir(dir)
}

func (s acmeStorage) removeRegistration(name string) error {
	path := s.path("accounts", name)
	if err := removeIfPresent(path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (s acmeStorage) loadPair(host string) (storedACMEPair, error) {
	var pair storedACMEPair
	data, err := s.read("pairs", acmeStorageHash(host)+".json")
	if err != nil {
		return pair, err
	}
	if json.Unmarshal(data, &pair) != nil || pair.AccountURL == "" {
		return storedACMEPair{}, ErrNoCertificate
	}
	return pair, nil
}

func (s acmeStorage) savePair(host string, pair storedACMEPair) error {
	data, err := json.Marshal(pair)
	if err != nil {
		return err
	}
	return s.write("pairs", acmeStorageHash(host)+".json", data)
}

func privateKeyPEM(key any) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
