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
	if code, out, stderr := runCLI(t, "another-good-password\r\n", "admin", "reset-password", "--username", "alice", "--password-stdin", "--data-dir", dir); code != 0 || !strings.Contains(out, "password updated") {
		t.Fatalf("reset: %d %q %q", code, out, stderr)
	}
}

func TestServeRefusesPlaintextPublicBind(t *testing.T) {
	t.Setenv("GOLEM_DEPLOY_INSECURE_HTTP", "")
	code, _, stderr := runCLI(t, "", "serve", "--listen", "0.0.0.0:0", "--data-dir", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "refusing to serve plaintext HTTP") {
		t.Fatalf("serve: %d %q", code, stderr)
	}
	t.Setenv("GOLEM_DEPLOY_INSECURE_HTTP", "maybe")
	if code, _, _ := runCLI(t, "", "serve"); code != 2 {
		t.Fatalf("bad env bool: %d", code)
	}
}
