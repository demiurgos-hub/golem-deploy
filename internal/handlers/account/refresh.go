package account

import (
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// RefreshHandler issues a new session JWT. API tokens cannot refresh.
func RefreshHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	identity, ok := middleware.RequireSession(w, r)
	if !ok {
		return
	}

	newToken, err := auth.GenerateToken(identity.Username, identity.AccessLevel, middleware.JWTSecretKey)
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, api.RefreshResponse{
			Status: "error",
			Detail: "Failed to generate new token",
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, api.RefreshResponse{
		Status: "success",
		Token:  newToken,
	})
}
