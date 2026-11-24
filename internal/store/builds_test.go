package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStaticAddonsAndBuildReferences(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.CreateProject(ctx, Project{Name: "arena", RepoURL: "https://x/y.git"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.StaticAddons == nil || len(p.StaticAddons) != 0 {
		t.Fatalf("new project add-ons %#v", p.StaticAddons)
	}
	addons := []StaticAddon{{Name: "maps.zip", Target: "assets"}, {Name: "o'brien.zip", Target: "."}}
	p, err = s.UpdateProjectRemote(ctx, p.ID, ProjectRemoteUpdate{StaticAddons: &addons})
	if err != nil || !reflect.DeepEqual(p.StaticAddons, addons) {
		t.Fatalf("%+v %v", p.StaticAddons, err)
	}
	other, _ := s.CreateProject(ctx, Project{Name: "other", RepoURL: "https://x/z.git"}, nil)
	_, _ = s.UpdateProjectRemote(ctx, other.ID, ProjectRemoteUpdate{StaticAddons: &[]StaticAddon{{Name: "maps.zip", Target: "x"}}})
	for name, want := range map[string][]string{"maps.zip": {"arena", "other"}, "o'brien.zip": {"arena"}, "none.zip": nil} {
		got, err := s.ProjectsUsingStatic(ctx, name)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	// A URL-only update keeps the add-ons.
	url := "https://x/w.git"
	if p, _ = s.UpdateProjectRemote(ctx, p.ID, ProjectRemoteUpdate{RepoURL: &url}); len(p.StaticAddons) != 2 {
		t.Fatalf("%+v", p.StaticAddons)
	}

	b, _ := s.CreateBuild(ctx, NewBuild{ProjectID: p.ID})
	if _, err := s.DeleteBuildUnused(ctx, b.ID); !errors.Is(err, ErrActive) {
		t.Fatalf("active: %v", err)
	}
	_ = s.FinishBuild(ctx, b.ID, BuildResult{Status: BuildStatusSuccess, ImageID: "sha256:1"})
	sv, _ := s.CreateServer(ctx, Server{Name: "s"}, nil)
	if _, err := s.UpdateServer(ctx, sv.ID, ServerUpdate{BuildID: &b.ID}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.ProjectBuildServerIDs(ctx, p.ID); !reflect.DeepEqual(ids, []int64{sv.ID}) {
		t.Fatalf("%v", ids)
	}
	if _, err := s.DeleteBuildUnused(ctx, b.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("in use: %v", err)
	}
	if err := s.DeleteProjectUnused(ctx, p.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("project in use: %v", err)
	}
	_ = s.DeleteServer(ctx, sv.ID)
	if err := s.DeleteProjectUnused(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBuild(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("builds did not cascade")
	}
	if err := s.DeleteProjectUnused(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := s.DeleteBuildUnused(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}
