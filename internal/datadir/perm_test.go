package datadir

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
}

// TestPrepareNeverChmodsExistingDirectory: pointing --data-dir at an
// existing, shared directory must not silently change its mode; a loose one
// is refused with the command to fix it, a private one is used as is.
func TestPrepareNeverChmodsExistingDirectory(t *testing.T) {
	skipOnWindows(t)
	dir := filepath.Join(t.TempDir(), "home dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // defeat the umask
		t.Fatal(err)
	}
	_, err := Prepare(dir)
	if err == nil || !strings.Contains(err.Error(), "chmod 700 '"+dir+"'") {
		t.Fatalf("loose existing dir: %v", err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode changed to %v", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("refused directory was written to: %v", entries)
	}

	// An existing private directory with a loose subdirectory: used, and the
	// subdirectory is left alone (the root already protects it).
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "static")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(sub); info.Mode().Perm() != 0o755 {
		t.Fatalf("existing subdirectory mode changed to %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(l.KeysDir()); info.Mode().Perm() != 0o700 {
		t.Fatalf("new subdirectory mode %v", info.Mode().Perm())
	}
}

// TestPrepareCreatesPrivateDatabaseFiles: the database file is created 0600
// before SQLite opens it, and SQLite's -wal and -shm files inherit the mode.
func TestPrepareCreatesPrivateDatabaseFiles(t *testing.T) {
	l, err := Prepare(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(l.DB())
	if err != nil {
		t.Fatalf("database file not created: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode %v", info.Mode().Perm())
	}
	st, err := store.Open(context.Background(), l.DB())
	if err != nil {
		t.Fatalf("open pre-created database: %v", err)
	}
	defer st.Close()
	if _, err := st.CreateAccount(context.Background(), store.NewAccount{Username: "alice", PasswordHash: "h", Role: store.RoleAdmin, Approved: true}); err != nil {
		t.Fatal(err)
	}
	// A second Prepare leaves the existing database alone.
	if _, err := Prepare(l.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAccountByUsername(context.Background(), "alice"); err != nil {
		t.Fatalf("data lost: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(l.DB() + suffix)
		if err != nil {
			t.Fatalf("%s: %v", suffix, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("golem-deploy.db%s has mode %v", suffix, info.Mode().Perm())
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/srv/golem-deploy": "/srv/golem-deploy",
		"/home/a b":         "'/home/a b'",
		"/x/it's":           `'/x/it'\''s'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
