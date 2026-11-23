package store

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
)

// TestF1_IDsAreNeverReused deletes the newest row of every table keyed by an
// id and checks that the next insert gets a fresh id (AUTOINCREMENT), since
// sessions, job logs, job subjects and checkouts are keyed by these ids.
func TestF1_IDsAreNeverReused(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	admin := mustAccount(t, s, "root", RoleAdmin, true)

	tables := []struct {
		name   string
		create func(i int) int64
		del    func(id int64)
	}{
		{"accounts", func(i int) int64 {
			return mustAccount(t, s, "user"+strconv.Itoa(i), RoleDeveloper, true).ID
		}, func(id int64) {
			if err := s.DeleteAccount(ctx, admin.ID, id); err != nil {
				t.Fatal(err)
			}
		}},
		{"api_tokens", func(i int) int64 {
			tok, err := s.CreateAPIToken(ctx, NewAPIToken{AccountID: admin.ID, Name: "t", TokenHash: "h" + strconv.Itoa(i), Prefix: "gdp_x"})
			if err != nil {
				t.Fatal(err)
			}
			return tok.ID
		}, func(id int64) {
			if err := s.DeleteAPIToken(ctx, admin.ID, id); err != nil {
				t.Fatal(err)
			}
		}},
		{"projects", func(i int) int64 {
			p, err := s.CreateProject(ctx, Project{Name: "p" + strconv.Itoa(i), RepoURL: "x"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return p.ID
		}, func(id int64) {
			if err := s.DeleteProject(ctx, id); err != nil {
				t.Fatal(err)
			}
		}},
		{"servers", func(i int) int64 {
			sv, err := s.CreateServer(ctx, Server{Name: "s" + strconv.Itoa(i), Image: "i"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return sv.ID
		}, func(id int64) {
			if err := s.DeleteServer(ctx, id); err != nil {
				t.Fatal(err)
			}
		}},
		{"jobs", func(i int) int64 {
			j, err := s.CreateJob(ctx, NewJob{Kind: "k", SubjectType: "project", SubjectID: strconv.Itoa(i)})
			if err != nil {
				t.Fatal(err)
			}
			return j.ID
		}, func(id int64) {
			// The daemon does not delete jobs today; future history pruning
			// must not lead to reuse either.
			if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tb := range tables {
		t.Run(tb.name, func(t *testing.T) {
			first := tb.create(1)
			tb.del(first)
			second := tb.create(2)
			if second <= first {
				t.Fatalf("%s: id %d reused after deleting %d", tb.name, second, first)
			}
		})
	}
}

// TestF1_TokenVersionIsRandomPerRow checks that accounts do not start at a
// predictable token_version and that a bump at the top of the range wraps
// to 0 instead of overflowing.
func TestF1_TokenVersionIsRandomPerRow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	seen := map[int64]bool{}
	for i := 0; i < 8; i++ {
		a := mustAccount(t, s, "u"+strconv.Itoa(i), RoleDeveloper, true)
		if a.TokenVersion < 0 {
			t.Fatalf("negative token_version %d", a.TokenVersion)
		}
		seen[a.TokenVersion] = true
	}
	if len(seen) < 8 {
		t.Fatalf("token versions repeat: %v", seen)
	}
	a, _ := s.GetAccountByUsername(ctx, "u0")
	if _, err := s.db.ExecContext(ctx, `UPDATE accounts SET token_version = 9223372036854775807 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.BumpTokenVersion(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetAccount(ctx, a.ID); err != nil || got.TokenVersion != 0 {
		t.Fatalf("wrap: token_version %d, err %v", got.TokenVersion, err)
	}
}

// TestF2_TokenCapIsAtomic creates tokens concurrently against a small cap.
func TestF2_TokenCapIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a := mustAccount(t, s, "alice", RoleDeveloper, true)
	const limit, workers = 5, 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, limited := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateAPIToken(ctx, NewAPIToken{AccountID: a.ID, Name: "t", TokenHash: "h" + strconv.Itoa(i), Prefix: "gdp_x", MaxPerAccount: limit})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrLimit):
				limited++
			default:
				t.Errorf("create: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != limit || limited != workers-limit {
		t.Fatalf("created %d, limited %d; want %d and %d", ok, limited, limit, workers-limit)
	}
}

// TestF5_StoreRevokesTokens covers the store side of token revocation:
// a password reset with revocation and un-approval delete tokens (and
// re-approval does not bring them back); a role change keeps them.
func TestF5_StoreRevokesTokens(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustAccount(t, s, "root", RoleAdmin, true)
	d := mustAccount(t, s, "dev", RoleDeveloper, true)
	mk := func(h string) {
		t.Helper()
		if _, err := s.CreateAPIToken(ctx, NewAPIToken{AccountID: d.ID, Name: "t", TokenHash: h, Prefix: "gdp_x"}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		l, err := s.ListAPITokens(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(l)
	}
	mk("a")
	mk("b")
	if _, n, err := s.SetPassword(ctx, d.ID, "x", false); err != nil || n != 0 || count() != 2 {
		t.Fatalf("keep tokens: %v %d %d", err, n, count())
	}
	if _, n, err := s.SetPassword(ctx, d.ID, "y", true); err != nil || n != 2 || count() != 0 {
		t.Fatalf("revoke tokens: %v %d %d", err, n, count())
	}
	mk("c")
	admin := RoleAdmin
	if _, err := s.UpdateAccount(ctx, 0, d.ID, AccountUpdate{Role: &admin}); err != nil || count() != 1 {
		t.Fatalf("role change: %v %d", err, count())
	}
	no, yes := false, true
	if _, err := s.UpdateAccount(ctx, 0, d.ID, AccountUpdate{Approved: &no}); err != nil || count() != 0 {
		t.Fatalf("unapprove: %v %d", err, count())
	}
	if _, err := s.UpdateAccount(ctx, 0, d.ID, AccountUpdate{Approved: &yes}); err != nil || count() != 0 {
		t.Fatalf("re-approve: %v %d", err, count())
	}
	if _, _, err := s.SetPassword(ctx, 9999, "z", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
}
