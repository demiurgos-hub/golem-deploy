package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoInstanceID is returned by ReadInstanceID when the database has no
// instance id yet. serve creates it when it starts.
var ErrNoInstanceID = errors.New("store: no instance id yet")

// ReadInstanceID returns the instance id stored in the database file at
// path. It changes nothing: the file is opened read-only, no migration runs
// and no id is generated. It works while the daemon has the database open.
func ReadInstanceID(ctx context.Context, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	fi, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoInstanceID
	}
	if err != nil {
		return "", fmt.Errorf("stat database: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("the database is not a regular file")
	}
	// While the daemon runs, or after it stopped without closing the
	// database, newer pages can be in the -wal file: open read-only with
	// the usual locking, which uses the daemon's -wal and -shm files. Without
	// a -wal file the database file holds everything; open it as immutable
	// so that SQLite creates no -wal or -shm file. The instance id never
	// changes once written, so a daemon that starts meanwhile does not
	// change the answer.
	query := "mode=ro&immutable=1"
	if _, err := os.Lstat(abs + "-wal"); err == nil {
		query = "mode=ro&_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite", readOnlyDSN(abs, query))
	if err != nil {
		return "", fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// A database that was created but never migrated has no settings
	// table yet.
	var tables int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'settings'`).Scan(&tables)
	if err != nil {
		return "", mapErr("read instance id", err)
	}
	if tables == 0 {
		return "", ErrNoInstanceID
	}
	var v string
	err = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, SettingInstanceID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstanceID
	}
	if err != nil {
		return "", mapErr("read instance id", err)
	}
	if !instanceIDRE.MatchString(v) {
		return "", errors.New("instance id in settings is malformed")
	}
	return v, nil
}

// readOnlyDSN is a SQLite URI for the file at abs with query. SQLite
// applies its URI parameters (mode, immutable) only to names that start
// with "file:".
func readOnlyDSN(abs, query string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a Windows drive letter: file:///C:/...
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: query}
	return u.String()
}
