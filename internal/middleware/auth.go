package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// JWTSecretKey holds the secret key for JWT signing and validation
var JWTSecretKey string

type identityContextKey struct{}

var identityKey = identityContextKey{}

// SetJWTSecret sets the JWT secret key
func SetJWTSecret(secret string) {
	JWTSecretKey = secret
}

// AuthMiddleware validates a Bearer JWT or hashed API token and stores Identity.
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			utils.WriteJSON(w, http.StatusUnauthorized, map[string]string{
				"status": "error",
				"detail": "Authorization header required",
			})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			utils.WriteJSON(w, http.StatusUnauthorized, map[string]string{
				"status": "error",
				"detail": "Invalid Authorization header format. Expected: Bearer <token>",
			})
			return
		}

		identity, err := authenticateBearer(parts[1])
		if err != nil {
			utils.WriteJSON(w, http.StatusUnauthorized, map[string]string{
				"status": "error",
				"detail": "Invalid or expired token",
			})
			return
		}

		ctx := context.WithValue(r.Context(), identityKey, identity)
		next(w, r.WithContext(ctx))
	}
}

func authenticateBearer(tokenString string) (auth.Identity, error) {
	if strings.HasPrefix(tokenString, "gdp_") {
		rec, err := database.LookupAPITokenByHash(database.HashAPIToken(tokenString))
		if err != nil {
			return auth.Identity{}, err
		}
		if rec == nil {
			return auth.Identity{}, errInvalidAPIToken
		}
		ok, err := database.IsApprovedAccount(rec.Username)
		if err != nil {
			return auth.Identity{}, err
		}
		if !ok {
			return auth.Identity{}, errInvalidAPIToken
		}
		return auth.Identity{
			Username:    rec.Username,
			AccessLevel: 1,
			Kind:        auth.KindAPI,
		}, nil
	}

	claims, err := auth.ValidateToken(tokenString, JWTSecretKey)
	if err != nil {
		return auth.Identity{}, err
	}
	return auth.Identity{
		Username:    claims.Username,
		AccessLevel: claims.AccessLevel,
		Kind:        auth.KindSession,
	}, nil
}

// IdentityFromRequest returns the Identity stored by AuthMiddleware.
func IdentityFromRequest(r *http.Request) (auth.Identity, bool) {
	identity, ok := r.Context().Value(identityKey).(auth.Identity)
	return identity, ok
}

// RequireSession requires a session JWT identity. API tokens are rejected.
func RequireSession(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	identity, ok := IdentityFromRequest(r)
	if !ok {
		utils.WriteJSON(w, http.StatusUnauthorized, map[string]string{
			"status": "error",
			"detail": "Authorization required",
		})
		return auth.Identity{}, false
	}
	if identity.Kind != auth.KindSession {
		utils.WriteJSON(w, http.StatusForbidden, map[string]string{
			"status": "error",
			"detail": "Session authentication required",
		})
		return auth.Identity{}, false
	}
	return identity, true
}

// RequireAdmin requires a session JWT with access_level 0.
func RequireAdmin(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	identity, ok := RequireSession(w, r)
	if !ok {
		return auth.Identity{}, false
	}
	if identity.AccessLevel != 0 {
		utils.WriteJSON(w, http.StatusForbidden, map[string]string{
			"status": "error",
			"detail": "Admin access required",
		})
		return auth.Identity{}, false
	}
	return identity, true
}
