// Package store owns the daemon's single SQLite database: connection setup,
// versioned migrations and typed data access. It returns domain values and
// sentinel errors; HTTP mapping belongs to internal/httpapi.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Sentinel errors. Callers use errors.Is.
var (
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a UNIQUE constraint is violated.
	ErrConflict = errors.New("store: conflict")
	// ErrLastAdmin is returned when a change would leave no approved admin.
	ErrLastAdmin = errors.New("store: would remove the last approved admin")
	// ErrSelf is returned when an admin tries to delete, demote or unapprove
	// their own account.
	ErrSelf = errors.New("store: cannot modify own account this way")
	// ErrLimit is returned when a per-account quota (e.g. API tokens) is
	// reached.
	ErrLimit = errors.New("store: limit reached")
	// ErrInUse means the row is referenced: a build, or a project with a
	// build, that a server is configured with.
	ErrInUse = errors.New("store: in use")
	// ErrActive means the row belongs to an operation that is still queued
	// or running (an active build).
	ErrActive = errors.New("store: still active")
	// ErrMissingReference means a written row references a row that does
	// not exist (a foreign key), e.g. a server's build deleted meanwhile.
	ErrMissingReference = errors.New("store: referenced row does not exist")
)

// Store is a handle to the database. It is safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Option configures Open.
type Option func(*Store)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	// The driver strips the query from plain file names and applies these as
	// PRAGMAs on every new connection. _txlock=immediate makes BEGIN take the
	// write lock up front, so read-then-write transactions cannot deadlock and
	// invariant checks inside a transaction are race-free.
	dsn := path + "?_busy_timeout=5000&_foreign_keys=1&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) nowUTC() time.Time { return s.now().UTC() }

// withTx runs fn in a transaction, committing when it returns nil.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// timeLayout is fixed width so stored timestamps sort lexically.
const timeLayout = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(timeLayout, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", v, err)
	}
	return t.UTC(), nil
}

func parseNullTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func int64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

// mapErr translates driver errors into sentinels, wrapping with op.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%s: %w", op, ErrConflict)
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return fmt.Errorf("%s: %w", op, ErrMissingReference)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// checkAffected returns ErrNotFound when res affected no rows.
func checkAffected(op string, res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}
