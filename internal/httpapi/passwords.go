package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// This file is the only place in the HTTP layer that runs bcrypt (a source
// test enforces it): every hash and comparison takes a slot from s.hashSlots,
// and every password check is counted against the attempt limits before it
// runs.

var (
	errInvalidCredentials   = newError(http.StatusUnauthorized, apitypes.ErrorCodeInvalidCredentials, "invalid username or password")
	errInvalidCurrentPasswd = func() *apiError {
		e := newError(http.StatusBadRequest, apitypes.ErrorCodeInvalidCurrentPassword, "current password is incorrect")
		e.field = "current_password"
		return e
	}()
)

// rateLimited is a 429 with Retry-After rounded up to whole seconds.
func rateLimited(retry time.Duration) *apiError {
	e := newError(http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited, "too many attempts; try again later")
	e.header = http.Header{"Retry-After": {retryAfter(retry)}}
	return e
}

// tooManyConcurrent is a 429 for an in-flight cap.
func tooManyConcurrent(message string) *apiError {
	e := newError(http.StatusTooManyRequests, apitypes.ErrorCodeRateLimited, message)
	e.header = http.Header{"Retry-After": {"1"}}
	return e
}

// errBusy is the 503 for a request that could not get a bcrypt slot.
func errBusy() *apiError {
	e := newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "the server is busy; try again in a moment")
	e.header = http.Header{"Retry-After": {"2"}}
	return e
}

func retryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}

// slotError maps a failure to get a bcrypt slot.
func slotError(err error) error {
	if errors.Is(err, auth.ErrBusy) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errBusy()
	}
	return internal(err)
}

// hashPassword validates and hashes pw under a bcrypt slot.
func (s *Server) hashPassword(ctx context.Context, pw string) (string, error) {
	release, err := s.hashSlots.Acquire(ctx)
	if err != nil {
		return "", slotError(err)
	}
	defer release()
	h, err := s.hasher.Hash(pw)
	if err != nil {
		return "", internal(err)
	}
	return h, nil
}

// comparePassword checks pw against acct's hash under a bcrypt slot; with a
// nil acct it runs the dummy comparison so unknown users cost the same.
func (s *Server) comparePassword(ctx context.Context, acct *store.Account, pw string) (bool, error) {
	release, err := s.hashSlots.Acquire(ctx)
	if err != nil {
		return false, slotError(err)
	}
	defer release()
	if acct == nil {
		s.hasher.Dummy()
		return false, nil
	}
	return s.hasher.Verify(acct.PasswordHash, pw), nil
}

// attempt is one password check counted against the client-IP limit and,
// unless the client address is known-good for the username, the username
// limit. Failures stay counted; succeed or refund settle it otherwise.
type attempt struct {
	s        *Server
	ip, user string
	ipRes    *auth.Reservation
	userRes  *auth.Reservation
}

// reserveAttempt counts an attempt for (ip, user) before any expensive work,
// or returns a 429 with the longest wait.
func (s *Server) reserveAttempt(ip, user string) (*attempt, error) {
	ipRes, ipWait, ok := s.loginIP.Reserve(ip)
	if !ok {
		return nil, rateLimited(ipWait)
	}
	a := &attempt{s: s, ip: ip, user: user, ipRes: ipRes}
	if !s.knownIPs.Contains(user, ip) {
		userRes, userWait, ok := s.loginUser.Reserve(user)
		if !ok {
			ipRes.Refund()
			return nil, rateLimited(userWait)
		}
		a.userRes = userRes
	}
	return a, nil
}

// refund un-counts an attempt that never reached the password check.
func (a *attempt) refund() {
	a.ipRes.Refund()
	a.userRes.Refund()
}

// succeed records a correct password: the attempt does not count against
// the IP, the username's failures are cleared and the address becomes
// known-good for the username.
func (a *attempt) succeed() {
	a.ipRes.Refund()
	a.userRes.Reset()
	a.s.knownIPs.Add(a.user, a.ip)
}

// verifyCurrentPassword re-checks the password of an authenticated account
// (password change, API token creation) under the same limits as login.
func (s *Server) verifyCurrentPassword(r *http.Request, acct store.Account, pw string) error {
	if pw == "" {
		return invalidField("current_password", "current_password is required")
	}
	a, err := s.reserveAttempt(clientIP(r), auth.LoginKey(acct.Username))
	if err != nil {
		return err
	}
	ok, err := s.comparePassword(r.Context(), &acct, pw)
	if err != nil {
		a.refund()
		return err
	}
	if !ok {
		return errInvalidCurrentPasswd
	}
	a.succeed()
	return nil
}

// login checks a username and password. It returns the account on success
// and the identical errInvalidCredentials for an unknown user, a wrong
// password and an unapproved account, with a bcrypt comparison on every
// path. The same limits apply whether or not the username exists.
func (s *Server) login(r *http.Request, username, password string) (store.Account, error) {
	ip, user := clientIP(r), auth.LoginKey(username)
	a, err := s.reserveAttempt(ip, user)
	if err != nil {
		return store.Account{}, err
	}
	releaseIP, ok := s.loginFlightIP.Acquire(ip)
	if !ok {
		a.refund()
		return store.Account{}, tooManyConcurrent("too many concurrent logins from your address; try again in a moment")
	}
	defer releaseIP()
	releaseUser, ok := s.loginFlightU.Acquire(user)
	if !ok {
		a.refund()
		return store.Account{}, tooManyConcurrent("another login for this username is in progress; try again in a moment")
	}
	defer releaseUser()

	acct, err := s.store.GetAccountByUsername(r.Context(), user)
	var target *store.Account
	switch {
	case err == nil:
		target = &acct
	case errors.Is(err, store.ErrNotFound):
	default:
		a.refund()
		return store.Account{}, internal(err)
	}
	match, err := s.comparePassword(r.Context(), target, password)
	if err != nil {
		a.refund()
		return store.Account{}, err
	}
	if target == nil || !match || !acct.Approved {
		return store.Account{}, errInvalidCredentials
	}
	a.succeed()
	return acct, nil
}
