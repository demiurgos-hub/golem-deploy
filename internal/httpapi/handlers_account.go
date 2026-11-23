package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// maxTokensPerAccount bounds API tokens per account.
const maxTokensPerAccount = 50

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, toAPIAccount(identityFrom(r).Account))
	return nil
}

// handleChangePassword verifies the current password, stores the new one,
// ends every session (token_version bump) and returns a fresh session.
// Wrong current passwords count against the login rate limit for the user.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r)
	var req apitypes.ChangePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	userKey := auth.LoginKey(id.Account.Username)
	if err := checkLimits(func() (bool, time.Duration) { return s.loginUser.Check(userKey) }); err != nil {
		return err
	}
	if !s.hasher.Verify(id.Account.PasswordHash, req.CurrentPassword) {
		s.loginUser.Add(userKey)
		e := newError(http.StatusBadRequest, apitypes.ErrorCodeInvalidCurrentPassword, "current password is incorrect")
		e.field = "current_password"
		return e
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		return invalidField("new_password", err.Error())
	}
	hash, err := s.hasher.Hash(req.NewPassword)
	if err != nil {
		return internal(err)
	}
	acct, err := s.store.SetPassword(r.Context(), id.Account.ID, hash)
	if err != nil {
		return internal(err)
	}
	s.logger.Info("password changed", "request_id", requestIDFrom(r.Context()), "account_id", acct.ID)
	resp, err := s.sessionResponse(acct, s.now())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) error {
	toks, err := s.store.ListAPITokens(r.Context(), identityFrom(r).Account.ID)
	if err != nil {
		return internal(err)
	}
	out := apitypes.APITokenList{Items: make([]apitypes.APIToken, 0, len(toks))}
	for _, t := range toks {
		out.Items = append(out.Items, toAPIToken(t))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func validTokenName(name string) bool {
	n := utf8.RuneCountInString(name)
	if n < 1 || n > 64 || !utf8.ValidString(name) {
		return false
	}
	return strings.IndexFunc(name, unicode.IsControl) < 0
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r)
	var req apitypes.CreateAPITokenRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	name := strings.TrimSpace(req.Name)
	if !validTokenName(name) {
		return invalidField("name", "name must be 1-64 printable characters")
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(s.now()) {
		return invalidField("expires_at", "expires_at must be in the future")
	}
	existing, err := s.store.ListAPITokens(r.Context(), id.Account.ID)
	if err != nil {
		return internal(err)
	}
	if len(existing) >= maxTokensPerAccount {
		return conflict(apitypes.ErrorCodeConflict, "too many API tokens; revoke unused ones first")
	}
	secret, hash, prefix, err := auth.NewAPIToken()
	if err != nil {
		return internal(err)
	}
	tok, err := s.store.CreateAPIToken(r.Context(), store.NewAPIToken{
		AccountID: id.Account.ID, Name: name, TokenHash: hash, Prefix: prefix, ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		return internal(err)
	}
	s.logger.Info("api token created", "request_id", requestIDFrom(r.Context()), "account_id", id.Account.ID, "token_id", tok.ID)
	writeJSON(w, http.StatusCreated, apitypes.CreateAPITokenResponse{Token: toAPIToken(tok), Secret: secret})
	return nil
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) error {
	tid, err := pathID(r, "id")
	if err != nil {
		return err
	}
	err = s.store.DeleteAPIToken(r.Context(), identityFrom(r).Account.ID, tid)
	if errors.Is(err, store.ErrNotFound) {
		return notFound("token")
	}
	if err != nil {
		return internal(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
