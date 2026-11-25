package inspector

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestAgentTokens(t *testing.T) {
	sec, err := newHubSecrets(1, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := newHubSecrets(1, bcrypt.MinCost)
	tok, err := agentToken(sec.TokenKey, 42)
	if err != nil || !strings.HasPrefix(tok, "gdi_42_") || len(tok) != len("gdi_42_")+64 {
		t.Fatalf("%q %v", tok, err)
	}
	if id, ok := verifyAgentToken(sec.TokenKey, tok); !ok || id != 42 {
		t.Fatalf("%d %v", id, ok)
	}
	if _, ok := verifyAgentToken(other.TokenKey, tok); ok {
		t.Fatal("verified with another key")
	}
	tok7, _ := agentToken(sec.TokenKey, 7)
	mac7 := tok7[len("gdi_7_"):]
	for _, bad := range []string{"", "gdi_", "gdi_42", "gdi_42_", "gdi_x_" + mac7, "gdi_042_" + tok[7:], "gdi_-1_" + mac7,
		"gdi_42_" + strings.ToUpper(tok[7:]), "gdi_42_" + tok[7:len(tok)-2], "Gdi_42_" + tok[7:], "gdi_7_" + tok[7:],
		"gdi_42_" + tok[7:] + "00", "gdi_99999999999999999999_" + mac7} {
		if _, ok := verifyAgentToken(sec.TokenKey, bad); ok {
			t.Errorf("%q verified", bad)
		}
	}
	if _, err := agentToken("zz", 1); err == nil {
		t.Fatal("bad key accepted")
	}
	if _, err := agentToken(sec.TokenKey, 0); err == nil {
		t.Fatal("server id 0 accepted")
	}
	if !sec.valid() || sec.Password == "" || bcrypt.CompareHashAndPassword([]byte(sec.PasswordHash), []byte(sec.Password)) != nil {
		t.Fatal("secrets")
	}
}

// Secrets are stored sealed and come back the same; nothing readable is
// in the settings row.
func TestSecretsStorage(t *testing.T) {
	st, box := openStore(t)
	ctx := context.Background()
	if got, err := loadSecrets(ctx, st, box); err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	sec, _ := newHubSecrets(3, bcrypt.MinCost)
	if err := saveSecrets(ctx, st, box, sec); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := st.GetSetting(ctx, "inspector_secrets")
	for _, v := range []string{sec.Password, sec.AgentToken, sec.TokenKey, sec.PasswordHash} {
		if strings.Contains(raw, v) {
			t.Fatal("secret readable in the settings row")
		}
	}
	got, err := loadSecrets(ctx, st, box)
	if err != nil || *got != *sec {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTickets(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	ts := newTicketStore(clock.Now)
	tok, exp, err := ts.issue(1, 5, "arena")
	if err != nil || len(tok) != 43 || !exp.Equal(clock.Now().Add(TicketTTL)) {
		t.Fatalf("%q %v %v", tok, exp, err)
	}
	got, ok := ts.consume(tok)
	if !ok || got.accountID != 1 || got.tokenVersion != 5 || got.target != "arena" {
		t.Fatalf("%+v %v", got, ok)
	}
	if _, ok := ts.consume(tok); ok {
		t.Fatal("used twice")
	}
	tok, _, _ = ts.issue(1, 5, "")
	clock.Advance(TicketTTL)
	if _, ok := ts.consume(tok); ok {
		t.Fatal("expired ticket accepted")
	}
	for i := 0; i < maxTicketsPerAccount; i++ {
		if _, _, err := ts.issue(2, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ts.issue(2, 0, ""); err != errTooManyTickets {
		t.Fatalf("per account: %v", err)
	}
	for i := int64(0); len(ts.m) < maxTickets; i++ {
		if _, _, err := ts.issue(100+i/maxTicketsPerAccount, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ts.issue(99999, 0, ""); err != errTooManyTickets {
		t.Fatalf("total: %v", err)
	}
	clock.Advance(TicketTTL)
	if _, _, err := ts.issue(99999, 0, ""); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 43), strings.Repeat("a", 100)} {
		if _, ok := ts.consume(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSessionStore(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	st := newSessionStore(clock.Now)
	key, s, evicted, err := st.create(1, 7, "hub-1")
	if err != nil || len(key) != 43 || len(evicted) != 0 || s.hubSession() != "hub-1" {
		t.Fatalf("%q %v %v", key, evicted, err)
	}
	if got, _ := st.lookup(key); got != s {
		t.Fatal("lookup")
	}
	ctx, release, ok := s.track(context.Background())
	if !ok || s.streamCount() != 1 {
		t.Fatal("track")
	}
	if sid := s.end(); sid != "hub-1" || ctx.Err() == nil {
		t.Fatalf("end: %q %v", sid, ctx.Err())
	}
	release()
	if s.end() != "" {
		t.Fatal("ended twice")
	}
	if _, _, ok := s.track(context.Background()); ok {
		t.Fatal("tracked after end")
	}
	// Expiry.
	key, _, _, _ = st.create(1, 7, "")
	clock.Advance(SessionMaxAge)
	if got, expired := st.lookup(key); got != nil || expired == nil {
		t.Fatal("expired session returned")
	}
	// The account's oldest session is pushed out beyond the limit.
	var first string
	for i := 0; i < maxSessionsPerAccount; i++ {
		c, _, ev, _ := st.create(3, 0, "s"+strconv.Itoa(i))
		if i == 0 {
			first = c
		}
		if len(ev) != 0 {
			t.Fatal("evicted early")
		}
		clock.Advance(time.Second)
	}
	_, _, ev, _ := st.create(3, 0, "new")
	if len(ev) != 1 || ev[0].hubSession() != "s0" {
		t.Fatalf("evicted %v", ev)
	}
	if got, _ := st.lookup(first); got != nil {
		t.Fatal("evicted session still valid")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct{ value, panel, want string }{
		{"", "127.0.0.1:8000", "127.0.0.1:8444"},
		{"", ":8000", ":8444"},
		{"", "[::1]:8000", "[::1]:8444"},
		{"off", "127.0.0.1:8000", ""},
		{"OFF", "127.0.0.1:8000", ""},
		{"0.0.0.0:9000", "127.0.0.1:8000", "0.0.0.0:9000"},
		{"inspector.example.com:9000", "x", "inspector.example.com:9000"},
	} {
		got, err := ResolveListen(tc.value, tc.panel)
		if err != nil || got != tc.want {
			t.Errorf("%q/%q: %q %v", tc.value, tc.panel, got, err)
		}
	}
	for _, bad := range []string{"9000", "a b:1", "host:99999", "host:x", "[fe80::1%eth0]:1", "host:010"} {
		if got, err := ResolveListen(bad, "127.0.0.1:8000"); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	for in, want := range map[string]string{
		"":                                     "",
		"https://Inspector.Example.com/":       "https://inspector.example.com",
		"https://inspector.example.com:443":    "https://inspector.example.com",
		"http://10.0.0.1:8444":                 "http://10.0.0.1:8444",
		"https://[2001:db8::1]:9443":           "https://[2001:db8::1]:9443",
		"https://inspector.example.com:8443/ ": "https://inspector.example.com:8443",
	} {
		got, err := ValidatePublicURL(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"inspector.example.com", "ftp://x.example", "https://u@x.example", "https://x.example/path",
		"https://x.example?q", "https://x.example#f", "https://x.example:", "https://x.example:0", "https://*.example"} {
		if got, err := ValidatePublicURL(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	for in, want := range map[string]string{"": "", "172.17.0.1": "172.17.0.1", "Host.Docker.Internal": "host.docker.internal", "[::1]": "::1"} {
		if got, err := ValidateAgentHost(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"h:1", "a b", "http://h"} {
		if _, err := ValidateAgentHost(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if ValidateImage(DefaultImage) != nil || ValidateImage("-x") == nil {
		t.Fatal("image validation")
	}
	if PublicHost("https://[2001:db8::1]:9443") != "2001:db8::1" || PublicHost("") != "" {
		t.Fatal("PublicHost")
	}
}
