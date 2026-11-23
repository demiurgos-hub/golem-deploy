package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

func (s *Server) sessionResponse(acct store.Account, loginAt time.Time) (apitypes.SessionResponse, error) {
	tok, sess, err := s.sessions.Issue(acct.ID, acct.TokenVersion, loginAt)
	if err != nil {
		return apitypes.SessionResponse{}, err
	}
	return apitypes.SessionResponse{
		Token:            tok,
		ExpiresAt:        sess.ExpiresAt,
		SessionExpiresAt: sess.SessionExpiresAt,
		Account:          toAPIAccount(acct),
	}, nil
}

// handleAuthOptions tells the login page whether self-registration is open.
// It is public and reveals nothing else.
func (s *Server) handleAuthOptions(w http.ResponseWriter, r *http.Request) error {
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, apitypes.AuthOptions{RegistrationEnabled: settings.RegistrationEnabled})
	return nil
}

// handleLogin: identical 401 for unknown user, wrong password and unapproved
// account, with a bcrypt comparison on every path. Attempts are counted
// against the client IP and the submitted username before the password is
// checked (see Server.login).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	var req apitypes.LoginRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Username == "" {
		return invalidField("username", "username is required")
	}
	if req.Password == "" {
		return invalidField("password", "password is required")
	}
	acct, err := s.login(r, req.Username, req.Password)
	if err != nil {
		return err
	}
	resp, err := s.sessionResponse(acct, s.now())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// handleRegister counts every attempt against the client IP before doing
// any work.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) error {
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		return internal(err)
	}
	if !settings.RegistrationEnabled {
		return newError(http.StatusForbidden, apitypes.ErrorCodeRegistrationDisabled, "self-registration is disabled; ask an administrator for an account")
	}
	res, wait, ok := s.registerIP.Reserve(clientIP(r))
	if !ok {
		return rateLimited(wait)
	}

	var req apitypes.RegisterRequest
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
	hash, err := s.hashPassword(r.Context(), req.Password)
	if err != nil {
		res.Refund() // the server was busy; do not charge the client
		return err
	}
	acct, err := s.store.CreateAccount(r.Context(), store.NewAccount{Username: username, PasswordHash: hash, Role: store.RoleDeveloper, Approved: false})
	if errors.Is(err, store.ErrConflict) {
		return conflict(apitypes.ErrorCodeUsernameTaken, "username is already taken")
	}
	if err != nil {
		return internal(err)
	}
	s.logger.Info("account registered", "request_id", requestIDFrom(r.Context()), "account_id", acct.ID)
	writeJSON(w, http.StatusCreated, toAPIAccount(acct))
	return nil
}

// handleLogout ends every session of the account (token_version bump).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r)
	if err := s.store.BumpTokenVersion(r.Context(), id.Account.ID); err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// handleRefresh issues a new token for the same login. The identity was
// re-validated against the database by the auth layer; the absolute session
// lifetime is carried over through LoginAt.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r)
	resp, err := s.sessionResponse(id.Account, id.Session.LoginAt)
	if errors.Is(err, auth.ErrInvalidSession) {
		return errUnauthenticated
	}
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}
