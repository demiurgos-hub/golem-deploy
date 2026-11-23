package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"time"
)

// Role is an account role as stored.
type Role string

// Roles.
const (
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleDeveloper }

// Account is a stored account.
type Account struct {
	ID           int64
	Username     string
	PasswordHash string `json:"-"`
	Role         Role
	Approved     bool
	// TokenVersion is embedded in session JWTs; bumping it ends every
	// session of the account. It starts at a random non-negative 63-bit
	// value, so a session can never match a different (e.g. recreated)
	// account row.
	TokenVersion int64
	CreatedAt    time.Time
}

// IsApprovedAdmin reports whether the account counts towards the
// last-admin invariant.
func (a Account) IsApprovedAdmin() bool { return a.Role == RoleAdmin && a.Approved }

// NewAccount is the input of CreateAccount. Username must already be
// normalised and PasswordHash computed by the caller.
type NewAccount struct {
	Username     string
	PasswordHash string `json:"-"`
	Role         Role
	Approved     bool
}

// AccountUpdate changes role and/or approval. Nil fields are unchanged.
type AccountUpdate struct {
	Role     *Role
	Approved *bool
}

const accountCols = `id, username, password_hash, role, approved, token_version, created_at`

func scanAccount(r scanner) (Account, error) {
	var a Account
	var created string
	if err := r.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.Approved, &a.TokenVersion, &created); err != nil {
		return Account{}, err
	}
	t, err := parseTime(created)
	if err != nil {
		return Account{}, err
	}
	a.CreatedAt = t
	return a, nil
}

// nextTokenVersion is the SQL expression for an incremented token_version.
// It wraps to 0 instead of overflowing into a REAL.
const nextTokenVersion = `(CASE WHEN token_version >= 9223372036854775807 THEN 0 ELSE token_version + 1 END)`

// randomTokenVersion returns a uniformly random non-negative int64.
func randomTokenVersion() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("generate token version: %w", err)
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 1), nil
}

// CreateAccount inserts an account. ErrConflict means the username exists
// (case-insensitively). It is the only way accounts are created (HTTP admin
// create, self-registration and the local CLI all use it), so every account
// starts with a random token_version.
func (s *Store) CreateAccount(ctx context.Context, in NewAccount) (Account, error) {
	if !in.Role.Valid() {
		return Account{}, fmt.Errorf("create account: invalid role %q", in.Role)
	}
	ver, err := randomTokenVersion()
	if err != nil {
		return Account{}, err
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO accounts (username, password_hash, role, approved, token_version, created_at)
		 VALUES (?, ?, ?, ?, ?, ?) RETURNING `+accountCols,
		in.Username, in.PasswordHash, in.Role, in.Approved, ver, formatTime(s.nowUTC()))
	a, err := scanAccount(row)
	if err != nil {
		return Account{}, mapErr("create account", err)
	}
	return a, nil
}

// GetAccount returns the account with id, or ErrNotFound.
func (s *Store) GetAccount(ctx context.Context, id int64) (Account, error) {
	a, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
	if err != nil {
		return Account{}, mapErr("get account", err)
	}
	return a, nil
}

// GetAccountByUsername looks up an account case-insensitively.
func (s *Store) GetAccountByUsername(ctx context.Context, username string) (Account, error) {
	a, err := scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE username = ?`, username))
	if err != nil {
		return Account{}, mapErr("get account", err)
	}
	return a, nil
}

// ListAccounts returns all accounts ordered by username.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY username`)
	if err != nil {
		return nil, mapErr("list accounts", err)
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, mapErr("list accounts", err)
		}
		out = append(out, a)
	}
	return out, mapErr("list accounts", rows.Err())
}

// AccountCounts summarises the accounts table.
type AccountCounts struct {
	Total          int
	ApprovedAdmins int
	Pending        int
}

// CountAccounts returns account counters.
func (s *Store) CountAccounts(ctx context.Context) (AccountCounts, error) {
	var c AccountCounts
	err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(role = 'admin' AND approved = 1), 0),
		COALESCE(SUM(approved = 0), 0)
		FROM accounts`).Scan(&c.Total, &c.ApprovedAdmins, &c.Pending)
	if err != nil {
		return AccountCounts{}, mapErr("count accounts", err)
	}
	return c, nil
}

func countApprovedAdmins(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE role = 'admin' AND approved = 1`).Scan(&n)
	return n, err
}

// UpdateAccount applies upd to account id on behalf of actorID (0 for the
// local CLI, which is not subject to the self rule). Within one transaction it
// refuses to leave zero approved admins (ErrLastAdmin) and refuses an admin
// demoting or unapproving themselves (ErrSelf). A role change or un-approval
// bumps token_version, ending the account's sessions; un-approval also
// deletes the account's API tokens, so re-approval does not revive them.
func (s *Store) UpdateAccount(ctx context.Context, actorID, id int64, upd AccountUpdate) (Account, error) {
	if upd.Role != nil && !upd.Role.Valid() {
		return Account{}, fmt.Errorf("update account: invalid role %q", *upd.Role)
	}
	var out Account
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
		if err != nil {
			return mapErr("update account", err)
		}
		next := cur
		if upd.Role != nil {
			next.Role = *upd.Role
		}
		if upd.Approved != nil {
			next.Approved = *upd.Approved
		}
		roleChanged := next.Role != cur.Role
		unapproved := cur.Approved && !next.Approved
		if !roleChanged && next.Approved == cur.Approved {
			out = cur
			return nil
		}
		if actorID == id && (roleChanged || unapproved) {
			return ErrSelf
		}
		if cur.IsApprovedAdmin() && !next.IsApprovedAdmin() {
			n, err := countApprovedAdmins(ctx, tx)
			if err != nil {
				return mapErr("update account", err)
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		if unapproved {
			if _, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE account_id = ?`, id); err != nil {
				return mapErr("update account", err)
			}
		}
		ver := `token_version`
		if roleChanged || unapproved {
			ver = nextTokenVersion
		}
		out, err = scanAccount(tx.QueryRowContext(ctx,
			`UPDATE accounts SET role = ?, approved = ?, token_version = `+ver+`
			 WHERE id = ? RETURNING `+accountCols,
			next.Role, next.Approved, id))
		return mapErr("update account", err)
	})
	return out, err
}

// DeleteAccount deletes account id on behalf of actorID (0 for the local
// CLI). It refuses self-deletion (ErrSelf) and deleting the last approved
// admin (ErrLastAdmin). API tokens cascade; jobs keep a NULL creator.
func (s *Store) DeleteAccount(ctx context.Context, actorID, id int64) error {
	if actorID == id {
		return ErrSelf
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
		if err != nil {
			return mapErr("delete account", err)
		}
		if cur.IsApprovedAdmin() {
			n, err := countApprovedAdmins(ctx, tx)
			if err != nil {
				return mapErr("delete account", err)
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
		if err != nil {
			return mapErr("delete account", err)
		}
		return checkAffected("delete account", res)
	})
}

// SetPassword stores a new password hash and bumps token_version, ending all
// sessions. With revokeTokens it also deletes every API token of the account
// in the same transaction. It returns the updated account and the number of
// tokens deleted.
func (s *Store) SetPassword(ctx context.Context, id int64, passwordHash string, revokeTokens bool) (Account, int, error) {
	var out Account
	var revoked int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = scanAccount(tx.QueryRowContext(ctx,
			`UPDATE accounts SET password_hash = ?, token_version = `+nextTokenVersion+`
			 WHERE id = ? RETURNING `+accountCols, passwordHash, id))
		if err != nil {
			return mapErr("set password", err)
		}
		if !revokeTokens {
			return nil
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE account_id = ?`, id)
		if err != nil {
			return mapErr("set password", err)
		}
		revoked, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return Account{}, 0, err
	}
	return out, int(revoked), nil
}

// BumpTokenVersion ends every session of account id (logout).
func (s *Store) BumpTokenVersion(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE accounts SET token_version = `+nextTokenVersion+` WHERE id = ?`, id)
	if err != nil {
		return mapErr("bump token version", err)
	}
	return checkAffected("bump token version", res)
}
