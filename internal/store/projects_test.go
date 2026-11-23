package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestProjectTargetedUpdates(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p, err := s.CreateProject(ctx, Project{Name: "game", RepoURL: "https://h/x", AuthType: "https", AuthUsername: "u", Status: "cloning"},
		func(int64) ([]byte, error) { return []byte("sealed"), nil })
	if err != nil {
		t.Fatal(err)
	}

	pulled := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := s.SetProjectCheckout(ctx, p.ID, ProjectCheckout{Branch: "main", LastCommit: "abc", LastPulledAt: &pulled, Status: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "main" || got.LastCommit != "abc" || got.LastPulledAt == nil || !got.LastPulledAt.Equal(pulled) || got.Status != "idle" ||
		got.RepoURL != "https://h/x" || !bytes.Equal(got.AuthSecret, []byte("sealed")) {
		t.Fatalf("after checkout update %+v", got)
	}

	url := "https://h/y"
	got, err = s.UpdateProjectRemote(ctx, p.ID, ProjectRemoteUpdate{RepoURL: &url})
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoURL != url || got.AuthType != "https" || !bytes.Equal(got.AuthSecret, []byte("sealed")) || got.LastCommit != "abc" {
		t.Fatalf("after url update %+v", got)
	}
	got, err = s.UpdateProjectRemote(ctx, p.ID, ProjectRemoteUpdate{Auth: &ProjectAuthUpdate{Type: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthType != "none" || got.AuthUsername != "" || got.AuthSecret != nil || got.RepoURL != url || got.Branch != "main" {
		t.Fatalf("after auth clear %+v", got)
	}
	if _, err := s.UpdateProjectRemote(ctx, p.ID, ProjectRemoteUpdate{Auth: &ProjectAuthUpdate{Type: "token"}}); err == nil {
		t.Fatal("invalid auth type accepted")
	}
	if _, err := s.SetProjectCheckout(ctx, 999, ProjectCheckout{Status: "idle"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := s.UpdateProjectRemote(ctx, 999, ProjectRemoteUpdate{RepoURL: &url}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}
}
