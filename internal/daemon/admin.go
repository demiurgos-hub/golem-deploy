package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/datadir"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// ErrUsernameExists is returned by CreateAdmin for a taken username.
var ErrUsernameExists = errors.New("an account with that username already exists")

// ErrNoSuchAccount is returned by ResetPassword for an unknown username.
var ErrNoSuchAccount = errors.New("no account with that username")

// openForAdmin opens the store for the admin CLI. It never takes the
// serve lock (the commands work while the daemon runs). create may
// initialise a new or empty data directory (so the first admin can be
// created before the first serve); other commands require an initialised
// one.
func openForAdmin(ctx context.Context, dataDir string, initialise bool) (*store.Store, error) {
	open := datadir.Open
	if initialise {
		if err := checkDataDirPath(dataDir); err != nil {
			return nil, err
		}
		open = datadir.Prepare
	}
	layout, err := open(dataDir)
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, layout.DB())
}

// CreateAdmin creates an approved admin account. It is the local, offline
// way to bootstrap the first admin (or recover access).
func CreateAdmin(ctx context.Context, dataDir, username, password string) (store.Account, error) {
	return createAdmin(ctx, dataDir, username, password, auth.DefaultBcryptCost)
}

func createAdmin(ctx context.Context, dataDir, username, password string, cost int) (store.Account, error) {
	name, err := auth.NormalizeUsername(username)
	if err != nil {
		return store.Account{}, err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return store.Account{}, err
	}
	h, err := auth.NewHasher(cost)
	if err != nil {
		return store.Account{}, err
	}
	hash, err := h.Hash(password)
	if err != nil {
		return store.Account{}, err
	}
	st, err := openForAdmin(ctx, dataDir, true)
	if err != nil {
		return store.Account{}, err
	}
	defer st.Close()
	acct, err := st.CreateAccount(ctx, store.NewAccount{Username: name, PasswordHash: hash, Role: store.RoleAdmin, Approved: true})
	if errors.Is(err, store.ErrConflict) {
		return store.Account{}, ErrUsernameExists
	}
	return acct, err
}

// ResetPassword sets a new password for username, ends all of its sessions
// and deletes all of its API tokens (it is the recovery action for a
// compromised account). It returns the number of tokens deleted.
func ResetPassword(ctx context.Context, dataDir, username, password string) (tokensRevoked int, err error) {
	return resetPassword(ctx, dataDir, username, password, auth.DefaultBcryptCost)
}

func resetPassword(ctx context.Context, dataDir, username, password string, cost int) (int, error) {
	if err := auth.ValidatePassword(password); err != nil {
		return 0, err
	}
	h, err := auth.NewHasher(cost)
	if err != nil {
		return 0, err
	}
	hash, err := h.Hash(password)
	if err != nil {
		return 0, err
	}
	st, err := openForAdmin(ctx, dataDir, false)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	acct, err := st.GetAccountByUsername(ctx, auth.LoginKey(username))
	if errors.Is(err, store.ErrNotFound) {
		return 0, ErrNoSuchAccount
	}
	if err != nil {
		return 0, err
	}
	_, revoked, err := st.SetPassword(ctx, acct.ID, hash, true)
	if err != nil {
		return 0, fmt.Errorf("set password: %w", err)
	}
	return revoked, nil
}
