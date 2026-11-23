// Package datadir defines the layout of the daemon's data directory and
// creates it with restrictive permissions. All runtime state lives below the
// data directory; nothing is written relative to the working directory.
package datadir

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Layout holds absolute paths inside a data directory.
type Layout struct {
	Root string
}

// DB is the SQLite database file.
func (l Layout) DB() string { return filepath.Join(l.Root, "golem-deploy.db") }

// KeysDir holds key files.
func (l Layout) KeysDir() string { return filepath.Join(l.Root, "keys") }

// JWTKey is the session-signing key file.
func (l Layout) JWTKey() string { return filepath.Join(l.KeysDir(), "jwt.key") }

// SecretsKey is the at-rest encryption key file.
func (l Layout) SecretsKey() string { return filepath.Join(l.KeysDir(), "secrets.key") }

// Repositories holds project checkouts.
func (l Layout) Repositories() string { return filepath.Join(l.Root, "repositories") }

// Static is the static file store.
func (l Layout) Static() string { return filepath.Join(l.Root, "static") }

// JobLogs holds one log file per job.
func (l Layout) JobLogs() string { return filepath.Join(l.Root, "logs", "jobs") }

// Tmp is scratch space on the same filesystem as the other directories.
func (l Layout) Tmp() string { return filepath.Join(l.Root, "tmp") }

// Prepare resolves root to an absolute path, creates missing directories of
// the layout with mode 0700 and creates the database file with mode 0600 if
// it does not exist (so SQLite's -wal and -shm files inherit that mode).
//
// It never changes the mode of an existing directory: the operator may have
// pointed --data-dir at a directory they use for other things. Instead, on
// Unix, an existing data directory that is accessible by group or others is
// refused with the chmod command to run.
func Prepare(root string) (Layout, error) {
	if root == "" {
		return Layout{}, errors.New("data directory must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve data directory: %w", err)
	}
	l := Layout{Root: abs}
	info, err := os.Stat(abs)
	switch {
	case err == nil:
		if !info.IsDir() {
			return Layout{}, fmt.Errorf("data directory %s is not a directory", abs)
		}
		if err := checkPrivate(abs, info); err != nil {
			return Layout{}, err
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return Layout{}, fmt.Errorf("create %s: %w", abs, err)
		}
	default:
		return Layout{}, fmt.Errorf("stat data directory: %w", err)
	}
	for _, d := range []string{l.KeysDir(), l.Repositories(), l.Static(), filepath.Join(l.Root, "logs"), l.JobLogs(), l.Tmp()} {
		if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return Layout{}, fmt.Errorf("create %s: %w", d, err)
		}
	}
	f, err := os.OpenFile(l.DB(), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return Layout{}, fmt.Errorf("create database file: %w", err)
		}
	case !errors.Is(err, fs.ErrExist):
		return Layout{}, fmt.Errorf("create database file: %w", err)
	}
	return l, nil
}

// checkPrivate refuses, on Unix, an existing data directory that group or
// others can access.
func checkPrivate(path string, info fs.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("data directory %s has mode %04o, which lets other users read the daemon's keys and database; "+
			"restrict it with: chmod 700 %s (or point --data-dir at a new directory, which is created with mode 0700)",
			path, perm, shellQuote(path))
	}
	return nil
}

// shellQuote quotes p for a POSIX shell when it contains anything unusual.
func shellQuote(p string) string {
	if p != "" && strings.IndexFunc(p, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:", r))
	}) < 0 {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// LoadOrCreateKey returns the size-byte key stored raw in path, creating it
// from crypto/rand with mode 0600 if it does not exist. On Unix it refuses a
// key file readable or writable by group or others.
func LoadOrCreateKey(path string, size int) ([]byte, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createKey(path, size)
	}
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat key %s: %w", path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("key file %s has mode %v; it must not be accessible by group or others (chmod 600)", path, info.Mode().Perm())
		}
	}
	if len(key) != size {
		return nil, fmt.Errorf("key file %s has %d bytes, want %d; restore it from backup or remove it to generate a new one (invalidates what it protects)", path, len(key), size)
	}
	return key, nil
}

func createKey(path string, size int) ([]byte, error) {
	key := make([]byte, size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	// Write to a temporary file and link it into place so a crash never
	// leaves a truncated key, and a concurrent creator never gets replaced.
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return nil, fmt.Errorf("create key: %w", err)
	}
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return LoadOrCreateKey(path, size)
		}
		return nil, fmt.Errorf("install key %s: %w", path, err)
	}
	return key, nil
}
