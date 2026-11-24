// Package datadir defines the layout of the daemon's data directory and
// creates it with restrictive permissions. All runtime state lives below the
// data directory; nothing is written relative to the working directory.
package datadir

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

// KnownHosts is the SSH known_hosts file used for Git remotes.
func (l Layout) KnownHosts() string { return filepath.Join(l.Root, "known_hosts") }

// Static is the static file store.
func (l Layout) Static() string { return filepath.Join(l.Root, "static") }

// JobLogs holds one log file per job.
func (l Layout) JobLogs() string { return filepath.Join(l.Root, "logs", "jobs") }

// GameCerts holds the game certificate (internal/certs); it is bind-mounted
// read-only into every server container.
func (l Layout) GameCerts() string { return filepath.Join(l.Root, "certs", "game") }

// Tmp is scratch space on the same filesystem as the other directories.
func (l Layout) Tmp() string { return filepath.Join(l.Root, "tmp") }

// Marker is the file that identifies a data directory initialised by Golem
// Deploy.
func (l Layout) Marker() string { return filepath.Join(l.Root, MarkerName) }

// LockFile is the file serve locks for as long as it runs.
func (l Layout) LockFile() string { return filepath.Join(l.Root, ".lock") }

// MarkerName is the base name of the data directory marker file.
const MarkerName = ".golem-deploy-data"

// markerFormat is the data directory format written into the marker.
const markerFormat = 1

// legacyEntries are the names a data directory created before the marker
// existed can contain (see checkDedicated).
var legacyEntries = map[string]bool{
	"golem-deploy.db": true, "golem-deploy.db-wal": true, "golem-deploy.db-shm": true, "golem-deploy.db-journal": true,
	"keys": true, "repositories": true, "static": true, "logs": true, "tmp": true, "known_hosts": true, ".lock": true,
}

// Prepare resolves root to an absolute path and initialises or reuses the
// data directory there. The data directory is dedicated to Golem Deploy
// (startup sweeps delete leftovers below it), so Prepare accepts only:
//
//   - a path that does not exist (created with mode 0700),
//   - an existing empty directory (a lone "lost+found", as at the root of
//     a freshly formatted volume, counts as empty),
//   - a directory that contains the marker file (MarkerName) written when
//     it was initialised,
//   - a data directory created before the marker existed: nothing but
//     layout entries, including the database file (the marker is added).
//
// Anything else is refused. The marker (0600, a format version line) is
// written once the layout exists. Prepare creates missing directories of
// the layout with mode 0700 and the database file with mode 0600 if it does
// not exist (so SQLite's -wal and -shm files inherit that mode).
//
// It never changes the mode of an existing directory. Instead, on Unix, an
// existing data directory that is accessible by group or others is refused
// with the chmod command to run.
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
		if err := checkDedicated(l); err != nil {
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
	if err := writeMarker(l); err != nil {
		return Layout{}, err
	}
	return l, nil
}

// Open resolves root and returns the layout of an existing data directory
// initialised by Golem Deploy (it must contain the marker). It creates
// nothing; the admin CLI uses it for commands that need existing data.
func Open(root string) (Layout, error) {
	if root == "" {
		return Layout{}, errors.New("data directory must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve data directory: %w", err)
	}
	l := Layout{Root: abs}
	info, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return Layout{}, fmt.Errorf("data directory %s does not exist; golem-deploy serve or golem-deploy admin create initialises it", abs)
	}
	if err != nil {
		return Layout{}, fmt.Errorf("stat data directory: %w", err)
	}
	if !info.IsDir() {
		return Layout{}, fmt.Errorf("data directory %s is not a directory", abs)
	}
	if err := checkPrivate(abs, info); err != nil {
		return Layout{}, err
	}
	ok, err := hasMarker(l)
	if err != nil {
		return Layout{}, err
	}
	if !ok {
		return Layout{}, fmt.Errorf("%s is not an initialised Golem Deploy data directory (no %s file); check --data-dir", abs, MarkerName)
	}
	return l, nil
}

// checkDedicated accepts an existing data directory that has a valid
// marker, is empty, or holds a data directory created before markers
// existed (only layout entries, including the database file).
func checkDedicated(l Layout) error {
	ok, err := hasMarker(l)
	if err != nil || ok {
		return err
	}
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		return fmt.Errorf("read data directory: %w", err)
	}
	empty, legacy, hasDB := true, true, false
	for _, e := range entries {
		n := e.Name()
		if n != "lost+found" {
			empty = false
		}
		if !legacyEntries[n] {
			legacy = false
		}
		if n == "golem-deploy.db" && e.Type().IsRegular() {
			hasDB = true
		}
	}
	if empty || (legacy && hasDB) {
		return nil
	}
	return fmt.Errorf("data directory %s is not empty and was not initialised by Golem Deploy (it has no %s file); "+
		"the data directory must be empty or previously initialised by Golem Deploy, because the daemon removes leftover files below it; "+
		"point --data-dir at a new or empty directory", l.Root, MarkerName)
}

// hasMarker reports whether the marker exists and is valid. A marker that
// is not a regular file or names an unknown format is an error.
func hasMarker(l Layout) (bool, error) {
	fi, err := os.Lstat(l.Marker())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", l.Marker(), err)
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", l.Marker())
	}
	f, err := os.Open(l.Marker())
	if err != nil {
		return false, fmt.Errorf("read %s: %w", l.Marker(), err)
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := io.ReadFull(f, buf)
	line, _, _ := strings.Cut(string(buf[:n]), "\n")
	v, ok := strings.CutPrefix(strings.TrimSpace(line), "format=")
	format, err := strconv.Atoi(v)
	switch {
	case !ok || err != nil || format < 1:
		return false, fmt.Errorf("%s is malformed; it must start with the line format=%d", l.Marker(), markerFormat)
	case format > markerFormat:
		return false, fmt.Errorf("data directory %s has format %d, written by a newer Golem Deploy (this version supports %d)", l.Root, format, markerFormat)
	}
	return true, nil
}

// writeMarker creates the marker (0600) if it does not exist.
func writeMarker(l Layout) error {
	f, err := os.OpenFile(l.Marker(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		_, err := hasMarker(l)
		return err
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", l.Marker(), err)
	}
	_, werr := fmt.Fprintf(f, "format=%d\n# This is a Golem Deploy data directory. Do not store other files in it.\n", markerFormat)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return fmt.Errorf("write %s: %w", l.Marker(), err)
	}
	return nil
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
