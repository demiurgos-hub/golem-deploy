package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupAdmin(name string) NewAccount {
	return NewAccount{Username: name, PasswordHash: "hash", Role: RoleAdmin, Approved: true}
}

func countSetupTokens(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM setup_token`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSetupTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if req, err := s.SetupRequired(ctx); err != nil || !req {
		t.Fatalf("fresh store: required %v, %v", req, err)
	}
	if ok, err := s.CheckSetupToken(ctx, "a"); err != nil || ok {
		t.Fatalf("no token: %v %v", ok, err)
	}
	// A pending developer account does not count as an admin.
	mustAccount(t, s, "pending", RoleDeveloper, false)
	mustAccount(t, s, "unapproved-admin", RoleAdmin, false)

	if err := s.SetSetupToken(ctx, "a", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A new token replaces the previous one.
	if err := s.SetSetupToken(ctx, "b", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := countSetupTokens(t, s); n != 1 {
		t.Fatalf("%d setup tokens", n)
	}
	if ok, _ := s.CheckSetupToken(ctx, "a"); ok {
		t.Fatal("replaced token still valid")
	}
	if ok, _ := s.CheckSetupToken(ctx, "b"); !ok {
		t.Fatal("current token not valid")
	}
	if _, err := s.CompleteSetup(ctx, "a", setupAdmin("root")); !errors.Is(err, ErrSetupInvalid) {
		t.Fatalf("old token: %v", err)
	}
	// A taken username keeps the token.
	if _, err := s.CompleteSetup(ctx, "b", setupAdmin("PENDING")); !errors.Is(err, ErrConflict) {
		t.Fatalf("taken username: %v", err)
	}
	if ok, _ := s.CheckSetupToken(ctx, "b"); !ok {
		t.Fatal("conflict consumed the token")
	}
	if _, err := s.CompleteSetup(ctx, "b", NewAccount{Username: "dev", PasswordHash: "h", Role: RoleDeveloper, Approved: true}); err == nil {
		t.Fatal("setup created a developer")
	}

	acct, err := s.CompleteSetup(ctx, "b", setupAdmin("root"))
	if err != nil || !acct.IsApprovedAdmin() || acct.Username != "root" {
		t.Fatalf("complete: %+v %v", acct, err)
	}
	if n := countSetupTokens(t, s); n != 0 {
		t.Fatal("token not consumed")
	}
	if _, err := s.CompleteSetup(ctx, "b", setupAdmin("root2")); !errors.Is(err, ErrSetupInvalid) {
		t.Fatalf("reuse: %v", err)
	}
	if req, _ := s.SetupRequired(ctx); req {
		t.Fatal("setup still required")
	}
	if err := s.SetSetupToken(ctx, "c", now.Add(time.Hour)); !errors.Is(err, ErrAdminExists) {
		t.Fatalf("token with an admin: %v", err)
	}
}

func TestSetupTokenExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetSetupToken(ctx, "a", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if ok, _ := s.CheckSetupToken(ctx, "a"); ok {
		t.Fatal("expired token valid")
	}
	if _, err := s.CompleteSetup(ctx, "a", setupAdmin("root")); !errors.Is(err, ErrSetupInvalid) {
		t.Fatalf("expired: %v", err)
	}
}

// An approved admin created or promoted another way makes the token useless
// and deletes it.
func TestSetupTokenDeletedByOtherAdmins(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	exp := time.Now().Add(time.Hour)

	if err := s.SetSetupToken(ctx, "a", exp); err != nil {
		t.Fatal(err)
	}
	mustAccount(t, s, "dev", RoleDeveloper, true)
	if n := countSetupTokens(t, s); n != 1 {
		t.Fatal("a developer account deleted the token")
	}
	mustAccount(t, s, "root", RoleAdmin, true)
	if n := countSetupTokens(t, s); n != 0 {
		t.Fatal("admin create kept the token")
	}

	s2 := openTest(t)
	dev := mustAccount(t, s2, "dev", RoleAdmin, false)
	if err := s2.SetSetupToken(ctx, "a", exp); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s2.UpdateAccount(ctx, 0, dev.ID, AccountUpdate{Approved: &yes}); err != nil {
		t.Fatal(err)
	}
	if n := countSetupTokens(t, s2); n != 0 {
		t.Fatal("approving an admin kept the token")
	}
}

func TestCompleteSetupConcurrent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.SetSetupToken(ctx, "a", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.CompleteSetup(ctx, "a", setupAdmin("root"+string(rune('a'+i))))
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrSetupInvalid):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	c, err := s.CountAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok != 1 || c.Total != 1 {
		t.Fatalf("%d successes, %d accounts", ok, c.Total)
	}
}

// The admin check is part of the transaction, not only the deletion of the
// token: an approved admin that appeared without deleting it still blocks
// the setup.
func TestCompleteSetupChecksForAdmins(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.SetSetupToken(ctx, "a", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO accounts (username, password_hash, role, approved, token_version, created_at)
		VALUES ('other', 'h', 'admin', 1, 1, '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.CheckSetupToken(ctx, "a"); ok {
		t.Fatal("token valid with an admin")
	}
	if _, err := s.CompleteSetup(ctx, "a", setupAdmin("root")); !errors.Is(err, ErrSetupInvalid) {
		t.Fatalf("with an admin: %v", err)
	}
}

func TestSetupTokenRefusedWithoutAPendingToken(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// The stand-in values are never accepted, not even when sent as the
	// hash.
	for _, h := range []string{noSetupToken.hash, "", strings.Repeat("0", 64)} {
		if ok, err := s.CheckSetupToken(ctx, h); err != nil || ok {
			t.Fatalf("no token, hash %q: %v %v", h, ok, err)
		}
		if _, err := s.CompleteSetup(ctx, h, setupAdmin("root")); !errors.Is(err, ErrSetupInvalid) {
			t.Fatalf("complete without a token: %v", err)
		}
	}
	if _, err := parseTime(noSetupToken.expires); err != nil {
		t.Fatal(err)
	}
	if len(noSetupToken.hash) != 64 {
		t.Fatal("the stand-in hash must have the length of a real one")
	}
}
