package database

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrAccountNotEligible is returned when a token operation requires a live approved account.
var ErrAccountNotEligible = errors.New("account not found or not approved")

const apiTokenPrefixLen = 12

// APITokenRecord is a stored API token without the plaintext secret.
type APITokenRecord struct {
	ID        int
	Username  string
	Name      string
	Prefix    string
	CreatedAt string
}

// HashAPIToken returns the SHA-256 hex digest of a plaintext API token.
func HashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func generateAPITokenSecret() (plaintext, hash, prefix string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("failed to generate token secret: %w", err)
	}
	plaintext = "gdp_" + hex.EncodeToString(raw)
	hash = HashAPIToken(plaintext)
	prefix = plaintext
	if len(prefix) > apiTokenPrefixLen {
		prefix = prefix[:apiTokenPrefixLen]
	}
	return plaintext, hash, prefix, nil
}

// CreateAPIToken stores a hashed token for username and returns the plaintext once.
func CreateAPIToken(username, name string) (plaintext string, rec APITokenRecord, err error) {
	name = strings.TrimSpace(name)
	if username == "" || name == "" {
		return "", APITokenRecord{}, fmt.Errorf("username and name are required")
	}

	ok, err := IsApprovedAccount(username)
	if err != nil {
		return "", APITokenRecord{}, err
	}
	if !ok {
		return "", APITokenRecord{}, ErrAccountNotEligible
	}

	plaintext, hash, prefix, err := generateAPITokenSecret()
	if err != nil {
		return "", APITokenRecord{}, err
	}

	result, err := DB.Exec(
		"INSERT INTO api_tokens (username, name, token_hash, prefix) VALUES (?, ?, ?, ?)",
		username, name, hash, prefix,
	)
	if err != nil {
		return "", APITokenRecord{}, fmt.Errorf("failed to insert api token: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return "", APITokenRecord{}, fmt.Errorf("failed to get api token id: %w", err)
	}

	rec = APITokenRecord{
		ID:       int(id),
		Username: username,
		Name:     name,
		Prefix:   prefix,
	}
	err = DB.QueryRow("SELECT created_at FROM api_tokens WHERE id = ?", id).Scan(&rec.CreatedAt)
	if err != nil {
		return "", APITokenRecord{}, fmt.Errorf("failed to load api token: %w", err)
	}

	return plaintext, rec, nil
}

// ListAPITokensByUser returns tokens for a username without hashes or secrets.
func ListAPITokensByUser(username string) ([]APITokenRecord, error) {
	rows, err := DB.Query(
		"SELECT id, username, name, prefix, created_at FROM api_tokens WHERE username = ? ORDER BY created_at DESC",
		username,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query api tokens: %w", err)
	}
	defer rows.Close()

	tokens := make([]APITokenRecord, 0)
	for rows.Next() {
		var rec APITokenRecord
		if err := rows.Scan(&rec.ID, &rec.Username, &rec.Name, &rec.Prefix, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan api token: %w", err)
		}
		tokens = append(tokens, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating api tokens: %w", err)
	}
	return tokens, nil
}

// LookupAPITokenByHash returns the token for a SHA-256 hex digest, or nil if absent.
func LookupAPITokenByHash(hash string) (*APITokenRecord, error) {
	var rec APITokenRecord
	err := DB.QueryRow(
		"SELECT id, username, name, prefix, created_at FROM api_tokens WHERE token_hash = ?",
		hash,
	).Scan(&rec.ID, &rec.Username, &rec.Name, &rec.Prefix, &rec.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to lookup api token: %w", err)
	}
	return &rec, nil
}

// RevokeAPIToken deletes a token owned by username.
func RevokeAPIToken(id int, username string) error {
	result, err := DB.Exec("DELETE FROM api_tokens WHERE id = ? AND username = ?", id, username)
	if err != nil {
		return fmt.Errorf("failed to revoke api token: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("token not found")
	}
	return nil
}
