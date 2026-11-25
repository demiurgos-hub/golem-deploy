package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The setup token lets the first admin account be created from a browser
// (golem-deploy admin setup-link). There is at most one; only its SHA-256
// (hex) and expiry are stored. "An admin exists" means at least one approved
// admin account, the same notion as the last-admin invariant.

var (
	// ErrAdminExists is returned by SetSetupToken when an approved admin
	// account exists.
	ErrAdminExists = errors.New("store: an approved admin account exists")
	// ErrSetupInvalid is returned by CompleteSetup when the token is not the
	// current one, has expired or was used, or an approved admin exists.
	ErrSetupInvalid = errors.New("store: setup token is not valid")
)

// SetSetupToken stores hash as the only setup token, replacing any previous
// one. It refuses with ErrAdminExists when an approved admin exists; the
// check and the write share one transaction.
func (s *Store) SetSetupToken(ctx context.Context, hash string, expiresAt time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		n, err := countApprovedAdmins(ctx, tx)
		if err != nil {
			return mapErr("set setup token", err)
		}
		if n > 0 {
			return ErrAdminExists
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO setup_token (id, token_hash, expires_at) VALUES (1, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET token_hash = excluded.token_hash, expires_at = excluded.expires_at`,
			hash, formatTime(expiresAt))
		return mapErr("set setup token", err)
	})
}

// SetupRequired reports whether no approved admin account exists.
func (s *Store) SetupRequired(ctx context.Context) (bool, error) {
	c, err := s.CountAccounts(ctx)
	if err != nil {
		return false, err
	}
	return c.ApprovedAdmins == 0, nil
}

// CheckSetupToken reports whether hash is the current setup token, it has
// not expired and no approved admin exists. It changes nothing; use it to
// decide a request before doing expensive work, then CompleteSetup.
func (s *Store) CheckSetupToken(ctx context.Context, hash string) (bool, error) {
	return s.setupTokenValid(ctx, s.db, hash)
}

// CompleteSetup creates the first admin account from in (which must be an
// approved admin) and deletes the setup token, in one transaction that
// first checks the token as CheckSetupToken does (ErrSetupInvalid). Two
// concurrent calls with the same token create one account. ErrConflict
// means the username exists; the token is then kept.
func (s *Store) CompleteSetup(ctx context.Context, hash string, in NewAccount) (Account, error) {
	if in.Role != RoleAdmin || !in.Approved {
		return Account{}, errors.New("complete setup: the account must be an approved admin")
	}
	var out Account
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ok, err := s.setupTokenValid(ctx, tx, hash)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSetupInvalid
		}
		// insertAccount deletes the token, since the account is an
		// approved admin.
		out, err = s.insertAccount(ctx, tx, in)
		return err
	})
	if err != nil {
		return Account{}, err
	}
	return out, nil
}

// queryRower is satisfied by *sql.DB and *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// noSetupToken stands in for the stored token and its expiry while none is
// pending, so that every refused token takes the same steps: both queries,
// the time parse and the constant-time comparison of 64 bytes. No setup
// token hashes to it (a hash is 64 hex characters).
var noSetupToken = struct{ hash, expires string }{
	hash:    strings.Repeat("x", 64),
	expires: formatTime(time.Unix(0, 0)),
}

// setupTokenValid checks hash against the stored token in constant time.
// Every refusal (an admin exists, no token, wrong token, expired) does the
// same work.
func (s *Store) setupTokenValid(ctx context.Context, q queryRower, hash string) (bool, error) {
	var admins int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE role = 'admin' AND approved = 1`).Scan(&admins)
	if err != nil {
		return false, mapErr("check setup token", err)
	}
	pending := true
	var stored, expires string
	err = q.QueryRowContext(ctx, `SELECT token_hash, expires_at FROM setup_token WHERE id = 1`).Scan(&stored, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		pending, stored, expires = false, noSetupToken.hash, noSetupToken.expires
	} else if err != nil {
		return false, mapErr("check setup token", err)
	}
	exp, err := parseTime(expires)
	if err != nil {
		return false, err
	}
	match := subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) == 1
	return pending && admins == 0 && match && s.nowUTC().Before(exp), nil
}

func deleteSetupToken(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM setup_token`)
	return err
}
