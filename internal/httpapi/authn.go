package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// Access is the minimum identity a route requires. Every route declares one
// in the route table; there is no default.
type Access int

// Access levels.
const (
	// AccessPublic needs no credentials.
	AccessPublic Access = iota + 1
	// AccessDeveloper needs any authenticated identity: a session of any role
	// or an API token.
	AccessDeveloper
	// AccessSession needs a session JWT (any role); API tokens get 403
	// session_required.
	AccessSession
	// AccessAdmin needs a session JWT of an admin account. API tokens never
	// qualify, even when owned by an admin.
	AccessAdmin
)

func (a Access) String() string {
	switch a {
	case AccessPublic:
		return "public"
	case AccessDeveloper:
		return "developer"
	case AccessSession:
		return "session"
	case AccessAdmin:
		return "admin"
	}
	return "unset"
}

// IdentityKind tells how a request authenticated.
type IdentityKind int

// Identity kinds.
const (
	KindSession IdentityKind = iota + 1
	KindAPIToken
)

// Identity is the authenticated principal of a request, loaded fresh from
// the database on every request.
type Identity struct {
	Account store.Account
	Kind    IdentityKind
	Session auth.Session   // valid when Kind == KindSession
	TokenID int64          // valid when Kind == KindAPIToken
	Token   store.APIToken // valid when Kind == KindAPIToken
}

// Role is the effective role: API tokens are capped at developer.
func (id *Identity) Role() store.Role {
	if id.Kind != KindSession {
		return store.RoleDeveloper
	}
	return id.Account.Role
}

// IsAdmin reports whether the identity is an admin session.
func (id *Identity) IsAdmin() bool { return id != nil && id.Role() == store.RoleAdmin }

func identityFrom(r *http.Request) *Identity {
	id, _ := r.Context().Value(identityKey).(*Identity)
	return id
}

// tokenTouchInterval bounds how often last_used_at is written per token.
const tokenTouchInterval = time.Minute

// authenticate resolves the bearer credential of r. Any failure yields the
// same 401 so callers learn nothing about why.
func (s *Server) authenticate(r *http.Request) (*Identity, error) {
	h := r.Header.Get("Authorization")
	scheme, cred, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || cred == "" {
		return nil, errUnauthenticated
	}
	cred = strings.TrimSpace(cred)
	ctx := r.Context()

	if strings.HasPrefix(cred, auth.APITokenPrefix) {
		if !auth.LooksLikeAPIToken(cred) {
			return nil, errUnauthenticated
		}
		tok, err := s.store.GetAPITokenByHash(ctx, auth.HashAPIToken(cred))
		if err != nil {
			return nil, authLookupError(err)
		}
		now := s.now()
		if tok.Expired(now) {
			return nil, errUnauthenticated
		}
		acct, err := s.store.GetAccount(ctx, tok.AccountID)
		if err != nil {
			return nil, authLookupError(err)
		}
		if !acct.Approved {
			return nil, errUnauthenticated
		}
		if tok.LastUsedAt == nil || now.Sub(*tok.LastUsedAt) >= tokenTouchInterval {
			if err := s.store.TouchAPIToken(ctx, tok.ID, now, tokenTouchInterval); err != nil {
				s.logger.Warn("record api token use failed", "request_id", requestIDFrom(ctx), "err", err)
			}
		}
		return &Identity{Account: acct, Kind: KindAPIToken, TokenID: tok.ID, Token: tok}, nil
	}

	sess, err := s.sessions.Parse(cred)
	if err != nil {
		return nil, errUnauthenticated
	}
	acct, err := s.store.GetAccount(ctx, sess.AccountID)
	if err != nil {
		return nil, authLookupError(err)
	}
	if !acct.Approved || acct.TokenVersion != sess.Ver {
		return nil, errUnauthenticated
	}
	return &Identity{Account: acct, Kind: KindSession, Session: sess}, nil
}

func authLookupError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return errUnauthenticated
	}
	return internal(err)
}

// authorize enforces access for a route before its handler runs.
func (s *Server) authorize(access Access, next handlerFunc) handlerFunc {
	if access == AccessPublic {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := s.authenticate(r)
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && ae.status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="golem-deploy"`)
			}
			return err
		}
		if ri, ok := r.Context().Value(reqInfoKey).(*reqInfo); ok {
			ri.accountID = id.Account.ID
		}
		switch access {
		case AccessSession:
			if id.Kind != KindSession {
				return errSessionRequired
			}
		case AccessAdmin:
			if id.Kind != KindSession {
				return errForbidden
			}
			if id.Role() != store.RoleAdmin {
				return errForbidden
			}
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	}
}
