package account

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// CreateRequest represents the JSON request body for creating accounts
type CreateRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	AccessLevel int    `json:"access_level"` // 0=admin, 1=developer
	Approved    bool   `json:"approved"`
}

// CreateHandler handles creating new user accounts (public endpoint for registration)
func CreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	// Validate required fields
	if req.Username == "" || req.Password == "" {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Username and password are required",
		})
		return
	}

	// Validate password length
	if len(req.Password) < 8 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Password must be at least 8 characters long",
		})
		return
	}

	// Public registration always creates an unapproved developer.
	// Client-supplied access_level and approved are ignored.
	if err := database.AddAccount(req.Username, req.Password, 1, false); err != nil {
		// Check if it's a duplicate username error
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			utils.WriteJSON(w, http.StatusConflict, map[string]string{
				"status": "error",
				"detail": "Username already exists",
			})
			return
		}
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": "Failed to create account",
		})
		return
	}

	// Return success
	utils.WriteJSON(w, http.StatusCreated, map[string]string{
		"status":  "success",
		"message": fmt.Sprintf("Account '%s' created successfully", req.Username),
	})
}
