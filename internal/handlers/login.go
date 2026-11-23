package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/auth"
	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// LoginHandler handles user authentication and returns a JWT token
func LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req api.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, api.LoginResponse{
			Status: "error",
			Detail: "Invalid JSON request",
		})
		return
	}

	// Validate required fields
	if req.Username == "" || req.Password == "" {
		utils.WriteJSON(w, http.StatusBadRequest, api.LoginResponse{
			Status: "error",
			Detail: "Username and password are required",
		})
		return
	}

	// Validate credentials
	valid, accessLevel, err := database.ValidateCredentials(req.Username, req.Password)
	if err != nil {
		// Check if it's an unapproved account error
		if err.Error() == "account not approved" {
			utils.WriteJSON(w, http.StatusForbidden, api.LoginResponse{
				Status: "error",
				Detail: "Account not approved. Please contact an administrator.",
			})
			return
		}
		utils.WriteJSON(w, http.StatusInternalServerError, api.LoginResponse{
			Status: "error",
			Detail: "Internal server error",
		})
		return
	}

	if !valid {
		utils.WriteJSON(w, http.StatusUnauthorized, api.LoginResponse{
			Status: "error",
			Detail: "Invalid username or password",
		})
		return
	}

	// Generate JWT token
	token, err := auth.GenerateToken(req.Username, accessLevel, middleware.JWTSecretKey)
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, api.LoginResponse{
			Status: "error",
			Detail: "Failed to generate token",
		})
		return
	}

	// Return token
	utils.WriteJSON(w, http.StatusOK, api.LoginResponse{
		Status: "success",
		Token:  token,
	})
}
