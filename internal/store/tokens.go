package store

import (
	"context"
	"database/sql"
	"time"
)

// APIToken is a stored API token. Only the SHA-256 hash of the secret is
// kept.
type APIToken struct {
	ID         int64
	AccountID  int64
	Name       string
	TokenHash  string `json:"-"`
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
}

// Expired reports whether the token has expired at now.
func (t APIToken) Expired(now time.Time) bool {
	return t.ExpiresAt != nil && !now.Before(*t.ExpiresAt)
}

// NewAPIToken is the input of CreateAPIToken.
type NewAPIToken struct {
	AccountID int64
	Name      string
	TokenHash string `json:"-"`
	Prefix    string
	ExpiresAt *time.Time
	// MaxPerAccount, when positive, makes CreateAPIToken fail with ErrLimit
	// if the account already has that many tokens. The count and the insert
	// happen in one transaction.
	MaxPerAccount int
}

const tokenCols = `id, account_id, name, token_hash, prefix, created_at, last_used_at, expires_at`

func scanToken(r scanner) (APIToken, error) {
	var t APIToken
	var created string
	var lastUsed, expires sql.NullString
	if err := r.Scan(&t.ID, &t.AccountID, &t.Name, &t.TokenHash, &t.Prefix, &created, &lastUsed, &expires); err != nil {
		return APIToken{}, err
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return APIToken{}, err
	}
	if t.LastUsedAt, err = parseNullTime(lastUsed); err != nil {
		return APIToken{}, err
	}
	if t.ExpiresAt, err = parseNullTime(expires); err != nil {
		return APIToken{}, err
	}
	return t, nil
}

// CreateAPIToken stores a token hash, enforcing in.MaxPerAccount
// atomically (ErrLimit). ErrNotFound is not checked here: the foreign key
// rejects unknown accounts.
func (s *Store) CreateAPIToken(ctx context.Context, in NewAPIToken) (APIToken, error) {
	var t APIToken
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if in.MaxPerAccount > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_tokens WHERE account_id = ?`, in.AccountID).Scan(&n); err != nil {
				return mapErr("create api token", err)
			}
			if n >= in.MaxPerAccount {
				return ErrLimit
			}
		}
		var err error
		t, err = scanToken(tx.QueryRowContext(ctx,
			`INSERT INTO api_tokens (account_id, name, token_hash, prefix, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?) RETURNING `+tokenCols,
			in.AccountID, in.Name, in.TokenHash, in.Prefix, formatTime(s.nowUTC()), formatTimePtr(in.ExpiresAt)))
		return mapErr("create api token", err)
	})
	if err != nil {
		return APIToken{}, err
	}
	return t, nil
}

// ListAPITokens returns the tokens of an account, newest first.
func (s *Store) ListAPITokens(ctx context.Context, accountID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE account_id = ? ORDER BY id DESC`, accountID)
	if err != nil {
		return nil, mapErr("list api tokens", err)
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, mapErr("list api tokens", err)
		}
		out = append(out, t)
	}
	return out, mapErr("list api tokens", rows.Err())
}

// GetAPITokenByHash returns the token with the given hash, or ErrNotFound.
func (s *Store) GetAPITokenByHash(ctx context.Context, hash string) (APIToken, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE token_hash = ?`, hash))
	if err != nil {
		return APIToken{}, mapErr("get api token", err)
	}
	return t, nil
}

// DeleteAPIToken deletes token id if it belongs to accountID.
func (s *Store) DeleteAPIToken(ctx context.Context, accountID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND account_id = ?`, id, accountID)
	if err != nil {
		return mapErr("delete api token", err)
	}
	return checkAffected("delete api token", res)
}

// TouchAPIToken records use of token id at now, writing at most once per
// minimum interval.
func (s *Store) TouchAPIToken(ctx context.Context, id int64, now time.Time, interval time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ?
		 WHERE id = ? AND (last_used_at IS NULL OR last_used_at <= ?)`,
		formatTime(now), id, formatTime(now.Add(-interval)))
	return mapErr("touch api token", err)
}
