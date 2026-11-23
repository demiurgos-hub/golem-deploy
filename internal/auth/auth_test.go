package auth

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock { return &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

func TestPasswordPolicy(t *testing.T) {
	tests := []struct {
		pw   string
		want error
	}{
		{"", ErrPasswordTooShort},
		{strings.Repeat("a", 11), ErrPasswordTooShort},
		{strings.Repeat("a", 12), nil},
		{strings.Repeat("a", 72), nil},
		{strings.Repeat("a", 73), ErrPasswordTooLong},
		{strings.Repeat("é", 36), nil},                      // 72 bytes
		{strings.Repeat("é", 36) + "x", ErrPasswordTooLong}, // 73 bytes: limit is bytes, not runes
	}
	for _, tt := range tests {
		if got := ValidatePassword(tt.pw); got != tt.want {
			t.Errorf("ValidatePassword(len %d) = %v, want %v", len(tt.pw), got, tt.want)
		}
	}
}

func TestUsernamePolicy(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"alice", "alice", true},
		{"  Alice ", "alice", true},
		{"a.b-c_d", "a.b-c_d", true},
		{"abc", "abc", true},
		{"0day", "0day", true},
		{strings.Repeat("a", 32), strings.Repeat("a", 32), true},
		{"ab", "", false},
		{strings.Repeat("a", 33), "", false},
		{".alice", "", false},
		{"-alice", "", false},
		{"al ice", "", false},
		{"alice@x", "", false},
		{"ålice", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, err := NormalizeUsername(tt.in)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("NormalizeUsername(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestHasher(t *testing.T) {
	if DefaultBcryptCost < 12 {
		t.Fatalf("DefaultBcryptCost %d < 12", DefaultBcryptCost)
	}
	h, err := NewHasher(bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := h.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Verify(hash, "correct horse battery") || h.Verify(hash, "wrong horse battery") {
		t.Fatal("verify")
	}
	if _, err := h.Hash("short"); err != ErrPasswordTooShort {
		t.Fatalf("short: %v", err)
	}
	if _, err := h.Hash(strings.Repeat("a", 100)); err != ErrPasswordTooLong {
		t.Fatalf("long: %v", err)
	}
	// Over-long input never matches, even if its first 72 bytes would.
	long := strings.Repeat("b", 72)
	hl, _ := h.Hash(long)
	if h.Verify(hl, long+"extra") {
		t.Fatal("over-long password accepted via truncation")
	}
	h.Dummy()
}

func newSessions(t *testing.T, c *clock) *Sessions {
	t.Helper()
	s, err := NewSessions(bytes.Repeat([]byte{1}, JWTKeySize), c.Now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionIssueParse(t *testing.T) {
	c := newClock()
	s := newSessions(t, c)
	tok, sess, err := s.Issue(42, 3, c.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !sess.ExpiresAt.Equal(c.Now().Add(SessionTTL)) || !sess.SessionExpiresAt.Equal(c.Now().Add(SessionMaxAge)) {
		t.Fatalf("expiry %+v", sess)
	}
	got, err := s.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != 42 || got.Ver != 3 || !got.LoginAt.Equal(c.Now()) {
		t.Fatalf("parsed %+v", got)
	}
	c.Add(SessionTTL + time.Minute)
	if _, err := s.Parse(tok); err != ErrInvalidSession {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestSessionAbsoluteLifetime(t *testing.T) {
	c := newClock()
	s := newSessions(t, c)
	loginAt := c.Now()
	// Refresh every 11 hours, as a client would.
	var tok string
	var err error
	for {
		var sess Session
		tok, sess, err = s.Issue(1, 0, loginAt)
		if err != nil {
			break
		}
		if sess.ExpiresAt.After(loginAt.Add(SessionMaxAge)) {
			t.Fatalf("expiry %v beyond absolute lifetime", sess.ExpiresAt)
		}
		if _, perr := s.Parse(tok); perr != nil {
			t.Fatalf("fresh token rejected at %v: %v", c.Now().Sub(loginAt), perr)
		}
		c.Add(11 * time.Hour)
	}
	if c.Now().Sub(loginAt) < SessionMaxAge {
		t.Fatalf("refresh refused too early (%v): %v", c.Now().Sub(loginAt), err)
	}
	if _, err := s.Parse(tok); err != ErrInvalidSession {
		t.Fatal("token valid past absolute lifetime")
	}
}

func TestSessionRejectsForgedTokens(t *testing.T) {
	c := newClock()
	s := newSessions(t, c)
	key := bytes.Repeat([]byte{1}, JWTKeySize)
	now := c.Now()
	valid := func() SessionClaims {
		return SessionClaims{
			Ver: 0, SidIat: now.Unix(),
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: SessionIssuer, Subject: "1", Audience: jwt.ClaimStrings{SessionAudience},
				IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			},
		}
	}
	sign := func(m jwt.SigningMethod, k any, cl SessionClaims) string {
		tok, err := jwt.NewWithClaims(m, cl).SignedString(k)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	// Sanity: the hand-built valid token parses.
	if _, err := s.Parse(sign(jwt.SigningMethodHS256, key, valid())); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	mod := func(f func(*SessionClaims)) SessionClaims { c := valid(); f(&c); return c }
	cases := map[string]string{
		"HS384":          sign(jwt.SigningMethodHS384, key, valid()),
		"HS512":          sign(jwt.SigningMethodHS512, key, valid()),
		"none":           sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid()),
		"wrong key":      sign(jwt.SigningMethodHS256, bytes.Repeat([]byte{2}, JWTKeySize), valid()),
		"wrong issuer":   sign(jwt.SigningMethodHS256, key, mod(func(c *SessionClaims) { c.Issuer = "x" })),
		"wrong audience": sign(jwt.SigningMethodHS256, key, mod(func(c *SessionClaims) { c.Audience = jwt.ClaimStrings{"x"} })),
		"no expiry":      sign(jwt.SigningMethodHS256, key, mod(func(c *SessionClaims) { c.ExpiresAt = nil })),
		"bad subject":    sign(jwt.SigningMethodHS256, key, mod(func(c *SessionClaims) { c.Subject = "admin" })),
		"no sid_iat":     sign(jwt.SigningMethodHS256, key, mod(func(c *SessionClaims) { c.SidIat = 0 })),
		"future iat":     sign(jwt.SigningMethodHS256, key, mod(func(c *SessionClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Hour)) })),
		"past absolute":  sign(jwt.SigningMethodHS256, key, mod(func(c *SessionClaims) { c.SidIat = now.Add(-SessionMaxAge - time.Second).Unix() })),
		"garbage":        "not-a-jwt",
		"empty":          "",
	}
	for name, tok := range cases {
		if _, err := s.Parse(tok); err != ErrInvalidSession {
			t.Errorf("%s: got %v, want ErrInvalidSession", name, err)
		}
	}
	if _, err := NewSessions([]byte("short"), nil); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestAPITokenFormat(t *testing.T) {
	secret, hash, prefix, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeAPIToken(secret) || !strings.HasPrefix(secret, APITokenPrefix) || len(secret) != 68 {
		t.Fatalf("secret shape %q", secret)
	}
	if hash != HashAPIToken(secret) || len(hash) != 64 || strings.Contains(hash, secret[4:]) {
		t.Fatal("hash")
	}
	if !strings.HasPrefix(secret, prefix) || len(prefix) != 12 {
		t.Fatalf("prefix %q", prefix)
	}
	s2, _, _, _ := NewAPIToken()
	if s2 == secret {
		t.Fatal("tokens repeat")
	}
	for _, bad := range []string{"gdp_", "gdp_xyz", secret + "0", "abc" + secret[3:], "gdp_" + strings.Repeat("z", 64)} {
		if LooksLikeAPIToken(bad) {
			t.Errorf("LooksLikeAPIToken(%q) = true", bad)
		}
	}
}

func TestLimiterTripsAndRecovers(t *testing.T) {
	c := newClock()
	l := NewLimiter(5, 15*time.Minute, 100, c.Now)
	for i := 0; i < 5; i++ {
		if ok, _ := l.Check("k"); !ok {
			t.Fatalf("blocked after %d events", i)
		}
		l.Add("k")
	}
	ok, wait := l.Check("k")
	if ok || wait <= 0 || wait > 15*time.Minute {
		t.Fatalf("not blocked after 5 events: %v %v", ok, wait)
	}
	if ok, _ := l.Check("other"); !ok {
		t.Fatal("unrelated key blocked")
	}
	c.Add(15 * time.Minute)
	if ok, _ := l.Check("k"); !ok {
		t.Fatal("not recovered after window")
	}
	l.Add("k")
	l.Reset("k")
	if l.Len() != 0 {
		t.Fatal("reset did not forget key")
	}
}

func TestLimiterBoundedMemory(t *testing.T) {
	c := newClock()
	l := NewLimiter(5, time.Minute, 50, c.Now)
	for i := 0; i < 1000; i++ {
		l.Add("k" + strconv.Itoa(i))
		c.Add(time.Millisecond)
	}
	if l.Len() > 50 {
		t.Fatalf("limiter holds %d keys, cap 50", l.Len())
	}
	// Newest keys survive eviction.
	for i := 0; i < 5; i++ {
		l.Add("k999")
	}
	if ok, _ := l.Check("k999"); ok {
		t.Fatal("recent key evicted")
	}
}

func TestLoginKeyBounded(t *testing.T) {
	if got := LoginKey("  BoB "); got != "bob" {
		t.Fatalf("LoginKey = %q", got)
	}
	if len(LoginKey(strings.Repeat("x", 1000))) != 64 {
		t.Fatal("LoginKey not bounded")
	}
}
