package httpapi

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

var errInvalidCredentials = newError(http.StatusUnauthorized, apitypes.ErrorCodeInvalidCredentials, "invalid username or password")

func rateLimited(retry time.Duration) *apiError {
	e := newError(http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited, "too many attempts; try again later")
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	e.header = http.Header{"Retry-After": {strconv.Itoa(secs)}}
	return e
}

// checkLimits returns a 429 if any key is over its limit, with the longest
// wait.
func checkLimits(checks ...func() (bool, time.Duration)) error {
	var wait time.Duration
	blocked := false
	for _, c := range checks {
		if ok, d := c(); !ok {
			blocked = true
			wait = max(wait, d)
		}
	}
	if blocked {
		return rateLimited(wait)
	}
	return nil
}

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

// handleLogin: identical 401 for unknown user, wrong password and unapproved
// account, with a bcrypt comparison on every path; failures count against
// both the client IP and the username.
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
	ip := clientIP(r)
	userKey := auth.LoginKey(req.Username)
	if err := checkLimits(
		func() (bool, time.Duration) { return s.loginIP.Check(ip) },
		func() (bool, time.Duration) { return s.loginUser.Check(userKey) },
	); err != nil {
		return err
	}

	acct, err := s.store.GetAccountByUsername(r.Context(), userKey)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.hasher.Dummy()
	case err != nil:
		return internal(err)
	}
	if err != nil || !s.hasher.Verify(acct.PasswordHash, req.Password) || !acct.Approved {
		s.loginIP.Add(ip)
		s.loginUser.Add(userKey)
		return errInvalidCredentials
	}
	s.loginUser.Reset(userKey)

	resp, err := s.sessionResponse(acct, s.now())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) error {
	settings, err := s.store.GetSettings(r.Context())
	if err != nil {
		return internal(err)
	}
	if !settings.RegistrationEnabled {
		return newError(http.StatusForbidden, apitypes.ErrorCodeRegistrationDisabled, "self-registration is disabled; ask an administrator for an account")
	}
	ip := clientIP(r)
	if err := checkLimits(func() (bool, time.Duration) { return s.registerIP.Check(ip) }); err != nil {
		return err
	}
	s.registerIP.Add(ip)

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
	hash, err := s.hasher.Hash(req.Password)
	if err != nil {
		return internal(err)
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
