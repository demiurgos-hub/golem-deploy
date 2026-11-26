package build_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// TestIntegrationBuildModuleInSubdirectory builds the fixture game laid out
// like a monorepo: golem.yaml, the schemas and the web client at the
// repository root, the Go module (go.mod, go.sum, cmd/ and the generated
// code) in server/, selected with server.module.
func TestIntegrationBuildModuleInSubdirectory(t *testing.T) {
	requireDocker(t)
	dir := t.TempDir()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	p, err := st.CreateProject(ctx, store.Project{Name: "gdm3b-mod-" + hex.EncodeToString(suffix),
		RepoURL: "https://git.example.com/x.git", Branch: "main", LastCommit: strings.Repeat("4d", 20)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	repos := filepath.Join(dir, "repos")
	checkout := filepath.Join(repos, "1")
	copyTree(t, filepath.Join("testdata", "game"), checkout)
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(checkout, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.mod", "go.sum", "cmd"} {
		if err := os.Rename(filepath.Join(checkout, name), filepath.Join(checkout, "server", name)); err != nil {
			t.Fatal(err)
		}
	}
	golemYAML, err := os.ReadFile(filepath.Join(checkout, "golem.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.ReplaceAll(string(golemYAML), "out: internal/generated", "out: server/internal/generated")
	if strings.Count(moved, "server/internal/generated") != 2 {
		t.Fatalf("fixture golem.yaml changed shape:\n%s", golemYAML)
	}
	if err := os.WriteFile(filepath.Join(checkout, "golem.yaml"), []byte(moved), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, build.ConfigFile), []byte("server:\n  module: server\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dc := docker.New(docker.Options{InstanceID: dockertest.IntegrationInstanceID, BuildConfigDir: filepath.Join(dir, "docker-build")})
	svc := build.New(build.Options{Store: st, Docker: dc, InstanceID: dockertest.IntegrationInstanceID, RepoDir: repos,
		TempDir: filepath.Join(dir, "tmp"), HeadCommit: func(context.Context, string) (string, error) { return p.LastCommit, nil }})
	b, err := st.CreateBuild(ctx, store.NewBuild{ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		spec := docker.BuildSpec{ProjectName: p.Name, ProjectID: p.ID, Commit: p.LastCommit, BuildID: b.ID, EngineVersion: "v0.4.0-rc.1"}
		if err := dc.RemoveBuildOutput(context.Background(), spec); err != nil && !errors.Is(err, docker.ErrNotFound) {
			t.Errorf("cleanup module fixture: %v", err)
		}
	})
	out := &lockedBuf{}
	runErr := svc.Run(ctx, b.ID, out, nil)
	got, err := st.GetBuild(ctx, b.ID)
	if err != nil || runErr != nil || got.Status != store.BuildStatusSuccess {
		t.Fatalf("module build %+v: %v, %v\n%s", got, err, runErr, out.String())
	}
	if !strings.Contains(out.String(), "WORKDIR /src/server") || !strings.Contains(out.String(), "Go module: server (server.module)") {
		t.Fatalf("build log does not show the module directory:\n%s", out.String())
	}
}
