// Package secrets encrypts values stored at rest (Git credentials, server
// environment) with AES-256-GCM.
//
// Ciphertext format: version byte (1) || 12-byte random nonce || GCM output.
// The associated data is the version byte followed by a purpose string such
// as "projects.auth_secret:12", which binds a ciphertext to one column of one
// row: a blob copied to another row or column fails to open.
//
// Errors never include plaintext or key material.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"

	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
)

// KeySize is the key length in bytes (AES-256).
const KeySize = 32

const version1 byte = 1

// ErrDecrypt is returned for any ciphertext that cannot be opened: wrong
// key, wrong purpose, tampering, truncation or unknown version.
var ErrDecrypt = errors.New("secrets: cannot decrypt value")

// Box seals and opens values with one key. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// New returns a Box using key, which must be KeySize bytes.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: key must be %d bytes", KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &Box{aead: aead}, nil
}

func associatedData(v byte, purpose string) []byte {
	ad := make([]byte, 0, 1+len(purpose))
	ad = append(ad, v)
	return append(ad, purpose...)
}

// Seal encrypts plaintext for purpose. purpose must not be empty.
func (b *Box) Seal(purpose string, plaintext []byte) ([]byte, error) {
	if purpose == "" {
		return nil, errors.New("secrets: empty purpose")
	}
	ns := b.aead.NonceSize()
	out := make([]byte, 1+ns, 1+ns+len(plaintext)+b.aead.Overhead())
	out[0] = version1
	if _, err := rand.Read(out[1 : 1+ns]); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return b.aead.Seal(out, out[1:1+ns], plaintext, associatedData(version1, purpose)), nil
}

// Open decrypts a value produced by Seal with the same purpose.
func (b *Box) Open(purpose string, ciphertext []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if purpose == "" || len(ciphertext) < 1+ns+b.aead.Overhead() || ciphertext[0] != version1 {
		return nil, ErrDecrypt
	}
	pt, err := b.aead.Open(nil, ciphertext[1:1+ns], ciphertext[1+ns:], associatedData(version1, purpose))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// ProjectAuthPurpose is the purpose for projects.auth_secret of row id.
func ProjectAuthPurpose(id int64) string {
	return "projects.auth_secret:" + strconv.FormatInt(id, 10)
}

// ServerEnvPurpose is the purpose for servers.env of row id.
func ServerEnvPurpose(id int64) string {
	return "servers.env:" + strconv.FormatInt(id, 10)
}

// LoadOrCreate returns a Box for the key file at path, creating a random key
// with mode 0600 on first use.
func LoadOrCreate(path string) (*Box, error) {
	key, err := datadir.LoadOrCreateKey(path, KeySize)
	if err != nil {
		return nil, err
	}
	return New(key)
}
