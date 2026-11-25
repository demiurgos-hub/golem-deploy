package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// setupTokenRawBytes is the entropy of a setup token (golem-deploy admin
// setup-link).
const setupTokenRawBytes = 32

// NewSetupToken returns a fresh one-time setup token (32 random bytes,
// base64url without padding) and its storage hash.
func NewSetupToken() (token, hash string, err error) {
	raw := make([]byte, setupTokenRawBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate setup token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashSetupToken(token), nil
}

// HashSetupToken returns the hex SHA-256 of a setup token. The token has 256
// bits of entropy, so a fast hash is sufficient.
func HashSetupToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
