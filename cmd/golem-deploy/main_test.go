package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, stderr := runCLI(t, ""); code != 2 || !strings.Contains(stderr, "Usage:") {
		t.Fatalf("no args: %d %q", code, stderr)
	}
	if code, _, _ := runCLI(t, "", "bogus"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code, out, _ := runCLI(t, "", "version"); code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, _ := runCLI(t, "", "admin"); code != 2 {
		t.Fatalf("admin without subcommand: %d", code)
	}
	if code, _, _ := runCLI(t, "", "admin", "create"); code != 2 {
		t.Fatalf("admin create without username: %d", code)
	}
	if code, _, _ := runCLI(t, "", "serve", "extra"); code != 2 {
		t.Fatalf("serve with positional args: %d", code)
	}
}

func TestAdminCreateFromStdin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	code, out, stderr := runCLI(t, "a-very-good-password\n", "admin", "create", "--username", "Alice", "--password-stdin", "--data-dir", dir)
	if code != 0 || !strings.Contains(out, `"alice"`) {
		t.Fatalf("create: %d %q %q", code, out, stderr)
	}
	if code, _, stderr := runCLI(t, "a-very-good-password\n", "admin", "create", "--username", "alice", "--password-stdin", "--data-dir", dir); code != 1 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("duplicate: %d %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "short\n", "admin", "create", "--username", "bob", "--password-stdin", "--data-dir", dir); code != 1 || !strings.Contains(stderr, "at least") {
		t.Fatalf("short password: %d %q", code, stderr)
	}
	// Without --password-stdin a non-terminal stdin is refused instead of
	// blocking.
	if code, _, stderr := runCLI(t, "x\n", "admin", "create", "--username", "bob", "--data-dir", dir); code != 1 || !strings.Contains(stderr, "--password-stdin") {
		t.Fatalf("non-tty: %d %q", code, stderr)
	}
	if code, out, stderr := runCLI(t, "another-good-password\r\n", "admin", "reset-password", "--username", "alice", "--password-stdin", "--data-dir", dir); code != 0 || !strings.Contains(out, "all of its sessions have been ended and 0 API token(s) deleted") {
		t.Fatalf("reset: %d %q %q", code, out, stderr)
	}
}

func TestServeRejectsBadNetworkFlags(t *testing.T) {
	t.Setenv("GOLEM_DEPLOY_INSECURE_HTTP", "")
	if code, _, stderr := runCLI(t, "", "serve", "--listen", "127.0.0.1:0", "--trusted-proxy", "10.0.0.0/8,nonsense", "--data-dir", t.TempDir()); code != 1 || !strings.Contains(stderr, "invalid trusted proxy") {
		t.Fatalf("bad trusted proxy: %d %q", code, stderr)
	}
	t.Setenv("GOLEM_DEPLOY_PUBLIC_HOST", "https://deploy.example.com")
	if code, _, stderr := runCLI(t, "", "serve", "--listen", "127.0.0.1:0", "--data-dir", t.TempDir()); code != 1 || !strings.Contains(stderr, "invalid public host") {
		t.Fatalf("bad public host from env: %d %q", code, stderr)
	}
	if got := splitList(" a.example, ,b.example,"); len(got) != 2 || got[0] != "a.example" || got[1] != "b.example" {
		t.Fatalf("splitList %q", got)
	}
}

func TestServeRejectsIncompatibleTLSFlags(t *testing.T) {
	t.Setenv("GOLEM_DEPLOY_INSECURE_HTTP", "")
	code, _, stderr := runCLI(t, "", "serve", "--listen", "127.0.0.1:0", "--http-listen", ":80", "--data-dir", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "--http-listen requires the panel to serve HTTPS") {
		t.Fatalf("serve: %d %q", code, stderr)
	}
	t.Setenv("GOLEM_DEPLOY_INSECURE_HTTP", "maybe")
	if code, _, _ := runCLI(t, "", "serve"); code != 2 {
		t.Fatalf("bad env bool: %d", code)
	}
}

// D4/D6: servers get memory and CPU limits and the static store a quota by
// default; 0 stays an explicit "unlimited".
func TestD4_ServeDefaults(t *testing.T) {
	t.Setenv("GOLEM_DEPLOY_SERVER_MEMORY_MB", "")
	t.Setenv("GOLEM_DEPLOY_SERVER_CPUS", "")
	t.Setenv("GOLEM_DEPLOY_STATIC_QUOTA_MB", "")
	_, _, stderr := runCLI(t, "", "serve", "-h")
	for _, want := range []string{"-server-memory-mb int", "(default 2048)", "-server-cpus float", "(default 2)", "-static-quota-mb int", "(default 5120)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("serve -h lacks %q:\n%s", want, stderr)
		}
	}
	t.Setenv("GOLEM_DEPLOY_SERVER_MEMORY_MB", "0")
	if _, _, stderr := runCLI(t, "", "serve", "-h"); strings.Contains(stderr, "(default 2048)") {
		t.Error("explicit 0 replaced by the default")
	}
}

func TestBuildFlags(t *testing.T) {
	t.Setenv("GOLEM_DEPLOY_INSECURE_HTTP", "")
	t.Setenv("GOLEM_DEPLOY_BUILD_TIMEOUT", "")
	t.Setenv("GOLEM_DEPLOY_BUILD_RETENTION", "")
	_, _, stderr := runCLI(t, "", "serve", "-h")
	for _, want := range []string{"-build-timeout duration", "(default 30m0s)", "-build-retention int", "(default 5)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("serve -h lacks %q:\n%s", want, stderr)
		}
	}
	t.Setenv("GOLEM_DEPLOY_BUILD_TIMEOUT", "45m")
	t.Setenv("GOLEM_DEPLOY_BUILD_RETENTION", "9")
	if _, _, stderr := runCLI(t, "", "serve", "-h"); !strings.Contains(stderr, "(default 45m0s)") || !strings.Contains(stderr, "(default 9)") {
		t.Errorf("env fallbacks ignored:\n%s", stderr)
	}
	t.Setenv("GOLEM_DEPLOY_BUILD_TIMEOUT", "soon")
	if code, _, _ := runCLI(t, "", "serve"); code != 2 {
		t.Fatalf("bad env duration: %d", code)
	}
	t.Setenv("GOLEM_DEPLOY_BUILD_TIMEOUT", "")
	if code, _, stderr := runCLI(t, "", "serve", "--listen", "127.0.0.1:0", "--build-timeout", "10s", "--data-dir", t.TempDir()); code != 1 || !strings.Contains(stderr, "--build-timeout") {
		t.Fatalf("short timeout: %d %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, "", "serve", "--listen", "127.0.0.1:0", "--build-retention", "-1", "--data-dir", t.TempDir()); code != 1 || !strings.Contains(stderr, "--build-retention") {
		t.Fatalf("bad retention: %d %q", code, stderr)
	}
}
