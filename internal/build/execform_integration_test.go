package build

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// pinnedBusybox matches the docker package's integration tests.
const pinnedBusybox = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// TestIntegrationExecFormRoundTrip builds an image whose ENTRYPOINT is
// rendered by execForm from hostile words and proves Docker's parser turns
// it into exactly those words: no extra instruction, no shell form
// fallback, no variable expansion.
func TestIntegrationExecFormRoundTrip(t *testing.T) {
	if os.Getenv("GOLEM_DEPLOY_DOCKER_TESTS") != "1" {
		t.Skip("set GOLEM_DEPLOY_DOCKER_TESTS=1 to run tests against a real Docker daemon")
	}
	words := []string{"/bin/echo", `it's "quoted"`, "$HOME", "${PATH}", `back\slash`, `\`, "]", `"],["sh","-c","id"]`,
		"#not a comment", "&& rm -rf /", "unicodé ✓", ""}
	if err := checkWords("test", words, true); err != nil {
		t.Fatal(err)
	}
	df := "FROM " + pinnedBusybox + "\nRUN " + execForm("/bin/echo", "$HOME", `"],["x"]`) + "\nENTRYPOINT " + execForm(words...) + "\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := "golem-deploy/gdm3b-execform:test"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "--progress", "plain", "--provenance=false", "--tag", tag, "--", dir)
	cmd.Env = append(os.Environ(), "BUILDX_GIT_INFO=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	defer func() { _ = exec.Command("docker", "image", "rm", "--force", tag).Run() }()
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Entrypoint}}|{{json .Config.Cmd}}", "--", tag).Output()
	if err != nil {
		t.Fatal(err)
	}
	ep, cmdPart, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	var got []string
	if err := json.Unmarshal([]byte(ep), &got); err != nil {
		t.Fatalf("entrypoint %s: %v", ep, err)
	}
	if !slices.Equal(got, words) {
		t.Fatalf("entrypoint\n%q\nwant\n%q", got, words)
	}
	if cmdPart != "null" {
		t.Fatalf("unexpected CMD %s", cmdPart)
	}
}
