package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dirState records every file below dir with its size, time and content
// hash.
func dirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if fi.IsDir() {
			out[rel] = "dir"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("%s %d %x", fi.ModTime().Format(time.RFC3339Nano), fi.Size(), sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameState(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("files changed: before %v, after %v", keys(before), keys(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("%s changed", k)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestReadInstanceID(t *testing.T) {
	ctx := context.Background()
	// Characters that have a meaning in a URI.
	dir := filepath.Join(t.TempDir(), "a b#c%d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstanceID(ctx, path); !errors.Is(err, ErrNoInstanceID) {
		t.Fatalf("before the first serve: %v, want ErrNoInstanceID", err)
	}
	want, err := s.InstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// While the daemon has the database open.
	got, err := ReadInstanceID(ctx, path)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// With the daemon stopped: nothing is created or changed.
	before := dirState(t, dir)
	got, err = ReadInstanceID(ctx, path)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	sameState(t, before, dirState(t, dir))
}

func TestReadInstanceIDWithoutID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Missing file: nothing is created.
	missing := filepath.Join(dir, "missing.db")
	if _, err := ReadInstanceID(ctx, missing); !errors.Is(err, ErrNoInstanceID) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the database file was created: %v", err)
	}

	// An empty file, as datadir.Prepare leaves it before the first open:
	// no tables are created.
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before := dirState(t, dir)
	if _, err := ReadInstanceID(ctx, empty); !errors.Is(err, ErrNoInstanceID) {
		t.Fatalf("empty: %v", err)
	}
	sameState(t, before, dirState(t, dir))

	// A migrated database without an id stays without one.
	migrated := filepath.Join(dir, "migrated.db")
	s, err := Open(ctx, migrated)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before = dirState(t, dir)
	if _, err := ReadInstanceID(ctx, migrated); !errors.Is(err, ErrNoInstanceID) {
		t.Fatalf("migrated: %v", err)
	}
	sameState(t, before, dirState(t, dir))

	// Not a database at all.
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("this is not a SQLite database, just some text long enough"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstanceID(ctx, junk); err == nil || errors.Is(err, ErrNoInstanceID) {
		t.Fatalf("junk: %v, want an error", err)
	}
}

// A database copied or restored with its -wal file but without the -shm
// file is read from private copies: nothing appears next to it.
func TestReadInstanceIDWALWithoutSHM(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src.db")
	s, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.InstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Copy while the store is open, so the id is still in the -wal file.
	dir := t.TempDir()
	path := filepath.Join(dir, "golem-deploy.db")
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+suffix, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the copied -wal file is missing or empty: %v", err)
	}

	before := dirState(t, dir)
	got, err := ReadInstanceID(ctx, path)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
	sameState(t, before, dirState(t, dir))
}
