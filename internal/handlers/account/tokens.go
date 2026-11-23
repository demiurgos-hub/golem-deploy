package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// TokensHandler lists (GET) or creates (POST) API tokens for the session user.
func TokensHandler(w http.ResponseWriter, r *http.Request) {
	identity, ok := middleware.RequireSession(w, r)
	if !ok {
		return
	}

	switch r.Method {
	case http.MethodGet:
		listTokens(w, identity.Username)
	case http.MethodPost:
		createToken(w, r, identity.Username)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

func listTokens(w http.ResponseWriter, username string) {
	records, err := database.ListAPITokensByUser(username)
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, api.ListAPITokensResponse{
			Status: "error",
			Detail: "Failed to list tokens",
		})
		return
	}

	tokens := make([]api.APIToken, 0, len(records))
	for _, rec := range records {
		tokens = append(tokens, api.APIToken{
			ID:        rec.ID,
			Name:      rec.Name,
			Prefix:    rec.Prefix,
			CreatedAt: rec.CreatedAt,
		})
	}

	utils.WriteJSON(w, http.StatusOK, api.ListAPITokensResponse{
		Status: "success",
		Tokens: tokens,
	})
}

func createToken(w http.ResponseWriter, r *http.Request, username string) {
	var req api.CreateAPITokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, api.CreateAPITokenResponse{
			Status: "error",
			Detail: "Invalid JSON request",
		})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		utils.WriteJSON(w, http.StatusBadRequest, api.CreateAPITokenResponse{
			Status: "error",
			Detail: "Token name is required",
		})
		return
	}

	plaintext, rec, err := database.CreateAPIToken(username, name)
	if err != nil {
		if errors.Is(err, database.ErrAccountNotEligible) {
			utils.WriteJSON(w, http.StatusForbidden, api.CreateAPITokenResponse{
				Status: "error",
				Detail: "Account not found or not approved",
			})
			return
		}
		utils.WriteJSON(w, http.StatusInternalServerError, api.CreateAPITokenResponse{
			Status: "error",
			Detail: "Failed to create token",
		})
		return
	}

	utils.WriteJSON(w, http.StatusCreated, api.CreateAPITokenResponse{
		Status:    "success",
		Token:     plaintext,
		ID:        rec.ID,
		Name:      rec.Name,
		Prefix:    rec.Prefix,
		CreatedAt: rec.CreatedAt,
	})
}

// RevokeTokenHandler revokes an API token owned by the session user.
func RevokeTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	identity, ok := middleware.RequireSession(w, r)
	if !ok {
		return
	}

	var req api.RevokeAPITokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if req.ID <= 0 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Token id is required",
		})
		return
	}

	if err := database.RevokeAPIToken(req.ID, identity.Username); err != nil {
		if strings.Contains(err.Error(), "token not found") {
			utils.WriteJSON(w, http.StatusNotFound, map[string]string{
				"status": "error",
				"detail": "Token not found",
			})
			return
		}
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": "Failed to revoke token",
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "Token revoked",
	})
}
