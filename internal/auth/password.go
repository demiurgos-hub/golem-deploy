// Package auth holds credential primitives: password and username policy,
// bcrypt hashing, session JWTs, API token generation and hashing, and the
// login rate limiter. It has no knowledge of HTTP or the database.
package auth

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// Password policy, in bytes of UTF-8. bcrypt ignores input past 72 bytes, so
// longer passwords are rejected instead of silently truncated.
const (
	MinPasswordBytes = 12
	MaxPasswordBytes = 72
)

// DefaultBcryptCost is the production bcrypt cost (at least 12).
const DefaultBcryptCost = 12

// Policy errors. Their messages are safe to show to users.
var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d bytes", MinPasswordBytes)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	ErrInvalidUsername  = errors.New("username must be 3-32 characters: lower-case letters, digits, '.', '_' or '-', starting with a letter or digit")
)

// ValidatePassword enforces the password length policy.
func ValidatePassword(pw string) error {
	switch {
	case len(pw) < MinPasswordBytes:
		return ErrPasswordTooShort
	case len(pw) > MaxPasswordBytes:
		return ErrPasswordTooLong
	}
	return nil
}

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

// NormalizeUsername trims and lower-cases u and validates it against
// ^[a-z0-9][a-z0-9._-]{2,31}$.
func NormalizeUsername(u string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(u))
	if !usernameRE.MatchString(n) {
		return "", ErrInvalidUsername
	}
	return n, nil
}

// LoginKey normalises a username for rate limiting and lookup without
// validating it, bounded in length so arbitrary input cannot bloat memory.
func LoginKey(u string) string {
	n := strings.ToLower(strings.TrimSpace(u))
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// Hasher hashes and verifies passwords with bcrypt.
type Hasher struct {
	cost      int
	dummyOnce sync.Once
	dummyHash []byte
}

// NewHasher returns a Hasher with the given bcrypt cost. Production code
// uses DefaultBcryptCost; tests may use bcrypt.MinCost for speed.
func NewHasher(cost int) (*Hasher, error) {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return nil, fmt.Errorf("invalid bcrypt cost %d", cost)
	}
	return &Hasher{cost: cost}, nil
}

// Cost returns the configured bcrypt cost.
func (h *Hasher) Cost() int { return h.cost }

// Hash validates pw against the policy and returns its bcrypt hash.
func (h *Hasher) Hash(pw string) (string, error) {
	if err := ValidatePassword(pw); err != nil {
		return "", err
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), h.cost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(b), nil
}

// Verify reports whether pw matches hash. Over-long input is never accepted
// but still costs one bcrypt comparison.
func (h *Hasher) Verify(hash, pw string) bool {
	if len(pw) > MaxPasswordBytes {
		h.Dummy()
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// Dummy performs a bcrypt comparison against a fixed hash so that a login
// for an unknown user costs the same as one for a known user. The fixed hash
// is computed on first use; servers call Dummy once at startup to warm it.
func (h *Hasher) Dummy() {
	h.dummyOnce.Do(func() {
		h.dummyHash, _ = bcrypt.GenerateFromPassword([]byte("golem-deploy-dummy-password"), h.cost)
	})
	_ = bcrypt.CompareHashAndPassword(h.dummyHash, []byte("not-the-password"))
}
