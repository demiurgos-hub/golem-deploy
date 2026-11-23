package datadir

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestG8_PrepareRequiresDedicatedDirectory: Prepare initialises a new or
// empty directory (writing the marker), reuses a marked one, and refuses a
// non-empty directory it did not initialise without writing to it.
func TestG8_PrepareRequiresDedicatedDirectory(t *testing.T) {
	base := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// (a) does not exist.
	fresh := filepath.Join(base, "fresh")
	l, err := Prepare(fresh)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(l.Marker())
	if err != nil || !strings.HasPrefix(string(marker), "format=1\n") {
		t.Fatalf("marker: %v %q", err, marker)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(l.Marker()); fi.Mode().Perm() != 0o600 {
			t.Fatalf("marker mode %v", fi.Mode().Perm())
		}
	}
	// (c) previously initialised, also with extra content of its own.
	if err := os.WriteFile(filepath.Join(fresh, "repositories", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(fresh); err != nil {
		t.Fatalf("marked directory refused: %v", err)
	}

	// (b) empty, or only lost+found.
	if _, err := Prepare(mk("empty")); err != nil {
		t.Fatal(err)
	}
	lf := mk("volume")
	if err := os.Mkdir(filepath.Join(lf, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(lf); err != nil {
		t.Fatalf("volume root refused: %v", err)
	}

	// Anything else is refused, and left untouched.
	for name, setup := range map[string]func(string){
		"home dir":      func(d string) { _ = os.WriteFile(filepath.Join(d, ".bashrc"), nil, 0o600) },
		"tmp only":      func(d string) { _ = os.MkdirAll(filepath.Join(d, "tmp", "git-123"), 0o700) },
		"repos only":    func(d string) { _ = os.MkdirAll(filepath.Join(d, "repositories", "1"), 0o700) },
		"marker is dir": func(d string) { _ = os.Mkdir(filepath.Join(d, MarkerName), 0o700) },
		"newer format":  func(d string) { _ = os.WriteFile(filepath.Join(d, MarkerName), []byte("format=99\n"), 0o600) },
		"garbage":       func(d string) { _ = os.WriteFile(filepath.Join(d, MarkerName), []byte("hello\n"), 0o600) },
	} {
		d := mk(name)
		setup(d)
		before, _ := os.ReadDir(d)
		_, err := Prepare(d)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), d) {
			t.Errorf("%s: error does not name the directory: %v", name, err)
		}
		if after, _ := os.ReadDir(d); len(after) != len(before) {
			t.Errorf("%s: refused directory was written to", name)
		}
	}
	d := mk("unrelated")
	_ = os.WriteFile(filepath.Join(d, "notes.txt"), nil, 0o600)
	if _, err := Prepare(d); err == nil || !strings.Contains(err.Error(), "must be empty or previously initialised by Golem Deploy") {
		t.Fatalf("unrelated: %v", err)
	}

	// A data directory from before the marker existed (only layout
	// entries, with the database) is adopted.
	legacy := mk("legacy")
	for _, sub := range []string{"keys", "repositories/3", "tmp/git-1", "logs/jobs", "static"} {
		_ = os.MkdirAll(filepath.Join(legacy, filepath.FromSlash(sub)), 0o700)
	}
	_ = os.WriteFile(filepath.Join(legacy, "golem-deploy.db"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(legacy, "known_hosts"), nil, 0o600)
	if l, err := Prepare(legacy); err != nil {
		t.Fatalf("legacy: %v", err)
	} else if _, err := os.Stat(l.Marker()); err != nil {
		t.Fatal("legacy directory not marked")
	}
}

// TestG8_OpenRequiresMarker: Open (used by admin reset-password) never
// initialises anything.
func TestG8_OpenRequiresMarker(t *testing.T) {
	base := t.TempDir()
	if _, err := Open(filepath.Join(base, "missing")); err == nil {
		t.Fatal("missing dir opened")
	}
	if _, err := os.Stat(filepath.Join(base, "missing")); !os.IsNotExist(err) {
		t.Fatal("Open created the directory")
	}
	empty := filepath.Join(base, "empty")
	_ = os.Mkdir(empty, 0o700)
	if _, err := Open(empty); err == nil {
		t.Fatal("unmarked dir opened")
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Fatal("Open wrote to an unmarked directory")
	}
	l, err := Prepare(filepath.Join(base, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Open(l.Root); err != nil || got != l {
		t.Fatalf("open: %v %v", got, err)
	}
}

// TestG4_LockIsExclusive: a second lock on the same data directory fails
// with ErrLocked naming the directory, until the first is released.
func TestG4_LockIsExclusive(t *testing.T) {
	l, err := Prepare(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := AcquireLock(l)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AcquireLock(l)
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), l.Root) {
		t.Fatalf("second lock: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := AcquireLock(l)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	_ = again.Release()
}

// TestG4_LockHelper holds the lock in a child process for
// TestG4_LockAcrossProcesses until its stdin closes or it is killed.
func TestG4_LockHelper(t *testing.T) {
	dir := os.Getenv("GOLEM_TEST_LOCK_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	lk, err := AcquireLock(Layout{Root: dir})
	if err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(3)
	}
	os.Stdout.WriteString("locked\n")
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}

// TestG4_LockAcrossProcesses: the lock excludes another process and is
// released by the operating system when its holder dies.
func TestG4_LockAcrossProcesses(t *testing.T) {
	l, err := Prepare(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestG4_LockHelper$")
	cmd.Env = append(os.Environ(), "GOLEM_TEST_LOCK_DIR="+l.Root)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if line != "locked\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("helper: %q", line)
	}
	if _, err := AcquireLock(l); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock held by another process was granted: %v", err)
	}
	// Kill the holder without letting it release.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lk, err := AcquireLock(l)
		if err == nil {
			_ = lk.Release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock not released after the holder died: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
