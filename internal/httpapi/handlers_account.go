package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// maxTokensPerAccount bounds API tokens per account.
const maxTokensPerAccount = 50

// handleMe returns the caller's account and how it authenticated. It is
// reachable with an API token so that a token can identify itself.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r)
	a := id.Account
	out := apitypes.CurrentAccount{
		ID:        a.ID,
		Username:  a.Username,
		Role:      apitypes.Role(a.Role),
		Approved:  a.Approved,
		CreatedAt: a.CreatedAt.UTC(),
		Auth:      apitypes.AuthKindSession,
	}
	if id.Kind == KindAPIToken {
		t := toAPIToken(id.Token)
		out.Auth, out.APIToken = apitypes.AuthKindAPIToken, &t
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleChangePassword verifies the current password (counted against the
// login limits per IP and per username), stores the new one, ends every
// session (token_version bump), deletes the account's API tokens unless
// revoke_tokens is false, and returns a fresh session.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) error {
	id := identityFrom(r)
	var req apitypes.ChangePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		return invalidField("new_password", err.Error())
	}
	if err := s.verifyCurrentPassword(r, id.Account, req.CurrentPassword); err != nil {
		return err
	}
	hash, err := s.hashPassword(r.Context(), req.NewPassword)
	if err != nil {
		return err
	}
	revoke := req.RevokeTokens == nil || *req.RevokeTokens
	acct, revoked, err := s.store.SetPassword(r.Context(), id.Account.ID, hash, revoke)
	if err != nil {
		return internal(err)
	}
	s.logger.Info("password changed", "request_id", requestIDFrom(r.Context()), "account_id", acct.ID, "tokens_revoked", revoked)
	sess, err := s.sessionResponse(acct, s.now())
	if err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, apitypes.ChangePasswordResponse{
		Token:            sess.Token,
		ExpiresAt:        sess.ExpiresAt,
		SessionExpiresAt: sess.SessionExpiresAt,
		Account:          sess.Account,
		TokensRevoked:    revoked,
	})
	return nil
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) error {
	return s.writeTokenList(w, r, identityFrom(r).Account.ID)
}

func (s *Server) writeTokenList(w http.ResponseWriter, r *http.Request, accountID int64) error {
	toks, err := s.store.ListAPITokens(r.Context(), accountID)
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
	// A stolen session alone must not be enough to mint a long-lived
	// credential.
	if err := s.verifyCurrentPassword(r, id.Account, req.CurrentPassword); err != nil {
		return err
	}
	secret, hash, prefix, err := auth.NewAPIToken()
	if err != nil {
		return internal(err)
	}
	tok, err := s.store.CreateAPIToken(r.Context(), store.NewAPIToken{
		AccountID: id.Account.ID, Name: name, TokenHash: hash, Prefix: prefix, ExpiresAt: req.ExpiresAt,
		MaxPerAccount: maxTokensPerAccount,
	})
	if errors.Is(err, store.ErrLimit) {
		return conflict(apitypes.ErrorCodeConflict, "too many API tokens; revoke unused ones first")
	}
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
