package gameadmintest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FakeGamePackage is the import path of the fake game program.
const FakeGamePackage = "github.com/demiurgos-hub/golem-deploy/internal/gameadmin/gameadmintest/fakegame"

func run(t testing.TB, dir string, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// BuildFakeGameImage compiles the fake game statically for the Docker
// daemon's Linux architecture, builds a FROM scratch image (with an empty
// /data owned by uid 65532, like generated images) tagged tag with labels
// (key=value), removes it when the test ends and returns its immutable id.
// It needs the go and docker CLIs.
func BuildFakeGameImage(t testing.TB, tag string, labels ...string) string {
	t.Helper()
	arch := run(t, "", nil, "docker", "version", "--format", "{{.Server.Arch}}")
	dir := t.TempDir()
	run(t, "", []string{"GOOS=linux", "GOARCH=" + arch, "CGO_ENABLED=0"}, "go", "build", "-trimpath", "-o", filepath.Join(dir, "fakegame"), FakeGamePackage)
	dockerfile := "FROM scratch\nCOPY fakegame /fakegame\nCOPY --chown=65532:65532 --chmod=0700 data /data\nENTRYPOINT [\"/fakegame\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{"build", "--quiet", "--tag", tag}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	run(t, dir, nil, "docker", append(args, ".")...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "image", "rm", "--force", tag).Run()
	})
	return run(t, "", nil, "docker", "image", "inspect", "--format", "{{.Id}}", tag)
}
