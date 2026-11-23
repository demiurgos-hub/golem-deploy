package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// API token format: "gdp_" + 64 hex characters (32 random bytes).
const (
	APITokenPrefix   = "gdp_"
	apiTokenRawBytes = 32
	// apiTokenDisplayLen is how much of the secret is kept as a visible
	// prefix: "gdp_" + 8 hex characters (32 bits, not enough to help guess the
	// remaining 224).
	apiTokenDisplayLen = len(APITokenPrefix) + 8
)

// NewAPIToken returns a fresh token secret, its storage hash and its display
// prefix.
func NewAPIToken() (secret, hash, prefix string, err error) {
	raw := make([]byte, apiTokenRawBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("generate api token: %w", err)
	}
	secret = APITokenPrefix + hex.EncodeToString(raw)
	return secret, HashAPIToken(secret), secret[:apiTokenDisplayLen], nil
}

// HashAPIToken returns the hex SHA-256 of a token secret. The secret has 256
// bits of entropy, so a fast hash is sufficient.
func HashAPIToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// LooksLikeAPIToken reports whether a bearer value has the API token shape.
func LooksLikeAPIToken(bearer string) bool {
	if !strings.HasPrefix(bearer, APITokenPrefix) || len(bearer) != len(APITokenPrefix)+2*apiTokenRawBytes {
		return false
	}
	_, err := hex.DecodeString(bearer[len(APITokenPrefix):])
	return err == nil
}
