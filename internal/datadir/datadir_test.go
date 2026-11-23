package datadir

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrepareCreatesLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	l, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(l.Root) {
		t.Fatalf("root not absolute: %s", l.Root)
	}
	for _, d := range []string{l.Root, l.KeysDir(), l.Repositories(), l.Static(), l.JobLogs(), l.Tmp()} {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v, want 0700", d, info.Mode().Perm())
		}
	}
	if filepath.Dir(l.DB()) != l.Root || filepath.Dir(l.JWTKey()) != l.KeysDir() || filepath.Dir(l.SecretsKey()) != l.KeysDir() {
		t.Fatal("unexpected file locations")
	}
	if _, err := Prepare(""); err == nil {
		t.Fatal("empty root accepted")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k", "jwt.key")
	k1, err := LoadOrCreateKey(path, 32)
	if err != nil || len(k1) != 32 {
		t.Fatalf("create: %v len %d", err, len(k1))
	}
	k2, err := LoadOrCreateKey(path, 32)
	if err != nil || string(k1) != string(k2) {
		t.Fatalf("reload: %v", err)
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	if _, err := LoadOrCreateKey(path, 16); err == nil {
		t.Fatal("wrong size accepted")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateKey(path, 32); err == nil {
			t.Fatal("world-readable key accepted")
		}
	}
	// No temp files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}
