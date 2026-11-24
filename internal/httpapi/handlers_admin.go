package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return internal(err)
	}
	projects, err := s.store.CountProjects(ctx)
	if err != nil {
		return internal(err)
	}
	servers, err := s.store.CountServers(ctx)
	if err != nil {
		return internal(err)
	}
	active, err := s.store.CountActiveJobs(ctx)
	if err != nil {
		return internal(err)
	}
	accts, err := s.store.CountAccounts(ctx)
	if err != nil {
		return internal(err)
	}
	reachable := s.docker != nil && s.docker.Reachable(ctx)
	writeJSON(w, http.StatusOK, apitypes.Status{
		Version:             s.cfg.Version,
		DockerReachable:     reachable,
		RegistrationEnabled: settings.RegistrationEnabled,
		Counts: apitypes.StatusCounts{
			Projects:        projects,
			Servers:         servers,
			ActiveJobs:      active,
			PendingAccounts: accts.Pending,
		},
	})
	return nil
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) error {
	st, err := s.store.GetSettings(r.Context())
	if err != nil {
		return internal(err)
	}
	return s.writeSettings(w, r, st)
}

// effectiveGameHost is the game host servers use ("" when none).
func (s *Server) effectiveGameHost(ctx context.Context) (string, error) {
	if s.servers != nil {
		return s.servers.GameHost(ctx)
	}
	st, err := s.store.GetSettings(ctx)
	return st.GameHost, err
}

func (s *Server) writeSettings(w http.ResponseWriter, r *http.Request, st store.Settings) error {
	host, err := s.effectiveGameHost(r.Context())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, apitypes.Settings{RegistrationEnabled: st.RegistrationEnabled, GameHost: st.GameHost, EffectiveGameHost: host})
	return nil
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) error {
	var req apitypes.UpdateSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	upd := store.SettingsUpdate{RegistrationEnabled: req.RegistrationEnabled}
	if req.GameHost != nil {
		host, err := certs.ValidateHost(*req.GameHost)
		if err != nil {
			return invalidField("game_host", "game_host "+err.Error()+" (no scheme, port or path)")
		}
		upd.GameHost = &host
	}
	st, err := s.store.UpdateSettings(r.Context(), upd)
	if err != nil {
		return internal(err)
	}
	s.logger.Info("settings updated", "request_id", requestIDFrom(r.Context()), "account_id", identityFrom(r).Account.ID,
		"game_host_changed", req.GameHost != nil)
	if req.GameHost != nil && s.certs != nil {
		// A self-signed certificate follows the game host; a failure is
		// retried hourly and before every server start.
		host, err := s.effectiveGameHost(r.Context())
		if err == nil {
			_, err = s.certs.Ensure(r.Context(), host)
		}
		if err != nil && !errors.Is(err, certs.ErrExpired) && !errors.Is(err, certs.ErrNoCertificate) {
			s.logger.Warn("could not update the game certificate for the new game host", "request_id", requestIDFrom(r.Context()), "err", err)
		}
	}
	return s.writeSettings(w, r, st)
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) error {
	accts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		return internal(err)
	}
	out := apitypes.AccountList{Items: make([]apitypes.Account, 0, len(accts))}
	for _, a := range accts {
		out.Items = append(out.Items, toAPIAccount(a))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func parseRole(field string, r apitypes.Role) (store.Role, error) {
	role := store.Role(r)
	if !role.Valid() {
		return "", invalidField(field, `role must be "admin" or "developer"`)
	}
	return role, nil
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) error {
	var req apitypes.CreateAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	username, err := auth.NormalizeUsername(req.Username)
	if err != nil {
		return invalidField("username", err.Error())
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		return invalidField("password", err.Error())
	}
	role, err := parseRole("role", req.Role)
	if err != nil {
		return err
	}
	hash, err := s.hashPassword(r.Context(), req.Password)
	if err != nil {
		return err
	}
	acct, err := s.store.CreateAccount(r.Context(), store.NewAccount{Username: username, PasswordHash: hash, Role: role, Approved: req.Approved})
	if errors.Is(err, store.ErrConflict) {
		return conflict(apitypes.ErrorCodeUsernameTaken, "username is already taken")
	}
	if err != nil {
		return internal(err)
	}
	s.logger.Info("account created", "request_id", requestIDFrom(r.Context()), "by", identityFrom(r).Account.ID, "account_id", acct.ID, "role", acct.Role)
	writeJSON(w, http.StatusCreated, toAPIAccount(acct))
	return nil
}

func accountChangeError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return notFound("account")
	case errors.Is(err, store.ErrLastAdmin):
		return conflict(apitypes.ErrorCodeLastAdmin, "at least one approved admin account must remain")
	case errors.Is(err, store.ErrSelf):
		return conflict(apitypes.ErrorCodeCannotModifySelf, "you cannot delete, demote or unapprove your own account")
	}
	return internal(err)
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) error {
	aid, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req apitypes.UpdateAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	var upd store.AccountUpdate
	if req.Role != nil {
		role, err := parseRole("role", *req.Role)
		if err != nil {
			return err
		}
		upd.Role = &role
	}
	upd.Approved = req.Approved
	actor := identityFrom(r).Account.ID
	acct, err := s.store.UpdateAccount(r.Context(), actor, aid, upd)
	if err != nil {
		return accountChangeError(err)
	}
	s.logger.Info("account updated", "request_id", requestIDFrom(r.Context()), "by", actor, "account_id", acct.ID, "role", acct.Role, "approved", acct.Approved)
	writeJSON(w, http.StatusOK, toAPIAccount(acct))
	return nil
}

// handleListAccountTokens lists another account's API tokens (metadata
// only; secrets and hashes are never stored or returned).
func (s *Server) handleListAccountTokens(w http.ResponseWriter, r *http.Request) error {
	aid, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if _, err := s.store.GetAccount(r.Context(), aid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound("account")
		}
		return internal(err)
	}
	return s.writeTokenList(w, r, aid)
}

// handleDeleteAccountToken revokes one API token of another account.
func (s *Server) handleDeleteAccountToken(w http.ResponseWriter, r *http.Request) error {
	aid, err := pathID(r, "id")
	if err != nil {
		return err
	}
	tid, err := pathID(r, "token_id")
	if err != nil {
		return err
	}
	err = s.store.DeleteAPIToken(r.Context(), aid, tid)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("token")
	}
	if err != nil {
		return internal(err)
	}
	s.logger.Info("api token revoked by admin", "request_id", requestIDFrom(r.Context()), "by", identityFrom(r).Account.ID, "account_id", aid, "token_id", tid)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) error {
	aid, err := pathID(r, "id")
	if err != nil {
		return err
	}
	actor := identityFrom(r).Account.ID
	if err := s.store.DeleteAccount(r.Context(), actor, aid); err != nil {
		return accountChangeError(err)
	}
	s.logger.Info("account deleted", "request_id", requestIDFrom(r.Context()), "by", actor, "account_id", aid)
	w.WriteHeader(http.StatusNoContent)
	return nil
}
