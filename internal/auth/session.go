package auth

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Session JWT parameters.
const (
	SessionIssuer   = "golem-deploy"
	SessionAudience = "golem-deploy-api"
	// SessionTTL is the lifetime of one session token; refresh before it ends.
	SessionTTL = 12 * time.Hour
	// SessionMaxAge is the absolute lifetime of a login, across refreshes.
	SessionMaxAge = 30 * 24 * time.Hour
	// JWTKeySize is the HMAC key length in bytes.
	JWTKeySize = 32
)

// ErrInvalidSession is returned for any token that is not a valid, current
// session token. It deliberately carries no detail.
var ErrInvalidSession = errors.New("invalid session token")

// SessionClaims are the claims of a session JWT. The account id is the
// subject.
type SessionClaims struct {
	// Ver is the account's token_version when the token was issued.
	Ver int64 `json:"ver"`
	// SidIat is when the login that started this chain of refreshes happened
	// (Unix seconds); it bounds the absolute session lifetime.
	SidIat int64 `json:"sid_iat"`
	jwt.RegisteredClaims
}

// Session is a verified session token.
type Session struct {
	AccountID        int64
	Ver              int64
	LoginAt          time.Time
	ExpiresAt        time.Time
	SessionExpiresAt time.Time
}

// Sessions issues and verifies HS256 session JWTs.
type Sessions struct {
	key []byte
	now func() time.Time
}

// NewSessions returns a Sessions using key (JWTKeySize bytes). now may be
// nil for time.Now.
func NewSessions(key []byte, now func() time.Time) (*Sessions, error) {
	if len(key) != JWTKeySize {
		return nil, fmt.Errorf("jwt key must be %d bytes", JWTKeySize)
	}
	if now == nil {
		now = time.Now
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &Sessions{key: k, now: now}, nil
}

// Issue signs a token for accountID at token version ver. loginAt is the
// time of the original login (now for a fresh login; the old token's LoginAt
// on refresh). The expiry is capped by the absolute session lifetime.
func (s *Sessions) Issue(accountID, ver int64, loginAt time.Time) (string, Session, error) {
	now := s.now().UTC().Truncate(time.Second)
	loginAt = loginAt.UTC().Truncate(time.Second)
	hardEnd := loginAt.Add(SessionMaxAge)
	exp := now.Add(SessionTTL)
	if exp.After(hardEnd) {
		exp = hardEnd
	}
	if !exp.After(now) {
		return "", Session{}, ErrInvalidSession
	}
	claims := SessionClaims{
		Ver:    ver,
		SidIat: loginAt.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    SessionIssuer,
			Subject:   strconv.FormatInt(accountID, 10),
			Audience:  jwt.ClaimStrings{SessionAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.key)
	if err != nil {
		return "", Session{}, fmt.Errorf("sign session: %w", err)
	}
	return tok, Session{AccountID: accountID, Ver: ver, LoginAt: loginAt, ExpiresAt: exp, SessionExpiresAt: hardEnd}, nil
}

// Parse verifies a token: HS256 only, issuer, audience, exp/nbf/iat, and the
// absolute session lifetime. It does not check the database; callers must
// compare Ver with the account's current token_version.
func (s *Sessions) Parse(token string) (Session, error) {
	var claims SessionClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return s.key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(SessionIssuer),
		jwt.WithAudience(SessionAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 || claims.SidIat <= 0 || claims.ExpiresAt == nil {
		return Session{}, ErrInvalidSession
	}
	loginAt := time.Unix(claims.SidIat, 0).UTC()
	hardEnd := loginAt.Add(SessionMaxAge)
	if !s.now().Before(hardEnd) {
		return Session{}, ErrInvalidSession
	}
	return Session{
		AccountID:        id,
		Ver:              claims.Ver,
		LoginAt:          loginAt,
		ExpiresAt:        claims.ExpiresAt.UTC(),
		SessionExpiresAt: hardEnd,
	}, nil
}
