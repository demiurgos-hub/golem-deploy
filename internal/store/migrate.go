package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Migrations are embedded SQL files named NNNN_description.sql. Each is
// applied once, in version order, inside its own transaction together with
// its schema_migrations row. Never edit an applied migration; add a new file.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	out := make([]migration, 0, len(names))
	seen := map[int]string{}
	for _, n := range names {
		base := path.Base(n)
		prefix, _, ok := strings.Cut(base, "_")
		if !ok || len(prefix) != 4 {
			return nil, fmt.Errorf("migration %q: name must be NNNN_description.sql", base)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: invalid version", base)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, base, v)
		}
		seen[v] = base
		body, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: base, sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (s *Store) migrate(ctx context.Context) error {
	return s.migrateFS(ctx, migrationFS)
}

func (s *Store) migrateFS(ctx context.Context, fsys fs.FS) error {
	migs, err := loadMigrations(fsys)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	latest := 0
	if len(migs) > 0 {
		latest = migs[len(migs)-1].version
	}
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); upgrade golem-deploy", current, latest)
	}

	for _, m := range migs {
		if m.version <= current {
			continue
		}
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			// Re-check inside the write transaction in case another process
			// (e.g. "golem-deploy admin" next to a running daemon) migrated.
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
				m.version, m.name, formatTime(s.nowUTC()))
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
