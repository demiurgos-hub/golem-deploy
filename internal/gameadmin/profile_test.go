package gameadmin_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest"
)

func TestProfile(t *testing.T) {
	ctx := context.Background()
	s := gameadmintest.New(t)
	s.Set(func(s *gameadmintest.Server) { s.ExpectToken = func() string { return token } })
	c := client(t, s.Port)
	body, err := c.Profile(ctx, "cpu", 2)
	if err != nil || !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		t.Fatalf("% x %v", body, err)
	}
	if body, err := c.Profile(ctx, "heap", 0); err != nil || len(body) == 0 {
		t.Fatalf("% x %v", body, err)
	}
	auth, paths := s.Requests()
	if strings.Join(paths, " ") != "/profile/cpu?seconds=2 /profile/heap" {
		t.Fatalf("paths %q", paths)
	}
	for _, a := range auth {
		if a != "Bearer "+token {
			t.Fatalf("auth %q", a)
		}
	}
}

func TestProfileStatuses(t *testing.T) {
	ctx := context.Background()
	s := gameadmintest.New(t)
	c := client(t, s.Port)
	for status, want := range map[int]error{
		http.StatusNotFound:            gameadmin.ErrProfilingUnsupported,
		http.StatusConflict:            gameadmin.ErrProfileInProgress,
		http.StatusServiceUnavailable:  gameadmin.ErrNotReady,
		http.StatusBadRequest:          gameadmin.ErrBadResponse,
		http.StatusInternalServerError: gameadmin.ErrBadResponse,
	} {
		s.Set(func(s *gameadmintest.Server) { s.ProfileStatus = status })
		if _, err := c.Profile(ctx, "cpu", 1); !errors.Is(err, want) {
			t.Errorf("status %d: %v, want %v", status, err, want)
		}
	}
	s.Set(func(s *gameadmintest.Server) {
		s.ProfileStatus = 0
		s.ExpectToken = func() string { return "another-token-0123456789" }
	})
	if _, err := c.Profile(ctx, "heap", 0); !errors.Is(err, gameadmin.ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
}

func TestProfileRefusesHostileAnswers(t *testing.T) {
	ctx := context.Background()
	s := gameadmintest.New(t)
	c := client(t, s.Port)
	for name, set := range map[string]func(*gameadmintest.Server){
		"json":     func(s *gameadmintest.Server) { s.ProfileContentType = "application/json" },
		"html":     func(s *gameadmintest.Server) { s.ProfileContentType = "text/html" },
		"empty":    func(s *gameadmintest.Server) { s.ProfileBody = []byte{} },
		"huge":     func(s *gameadmintest.Server) { s.ProfileBody = make([]byte, gameadmin.MaxProfileBytes+1) },
		"token":    func(s *gameadmintest.Server) { s.ProfileBody = []byte("trace " + token + " end") },
		"redirect": func(s *gameadmintest.Server) { s.ProfileStatus = http.StatusFound },
	} {
		s.Set(func(s *gameadmintest.Server) {
			s.ProfileStatus, s.ProfileBody, s.ProfileContentType = 0, nil, ""
			set(s)
		})
		body, err := c.Profile(ctx, "trace", 1)
		if !errors.Is(err, gameadmin.ErrBadResponse) || body != nil {
			t.Errorf("%s: %d bytes, %v", name, len(body), err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("%s: error leaks the token: %v", name, err)
		}
	}
}

func TestProfileRejectsBadRequests(t *testing.T) {
	ctx := context.Background()
	s := gameadmintest.New(t)
	c := client(t, s.Port)
	for _, tc := range []struct {
		kind    string
		seconds int
	}{{"CPU", 1}, {"../ready", 0}, {"", 0}, {"cpu", -1}, {"cpu", 3601}} {
		if _, err := c.Profile(ctx, tc.kind, tc.seconds); err == nil {
			t.Errorf("Profile(%q, %d) accepted", tc.kind, tc.seconds)
		}
	}
	session, err := gameadmin.NewSession(s.Port, token)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Profile(ctx, "heap", 0); err == nil {
		t.Fatal("a session client captured a profile")
	}
	if _, paths := s.Requests(); len(paths) != 0 {
		t.Fatalf("rejected requests reached the game: %q", paths)
	}
}
