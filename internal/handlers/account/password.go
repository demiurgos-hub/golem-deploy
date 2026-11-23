package account

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// PasswordRequest represents the JSON request body for password change
type PasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// PasswordHandler handles password change (requires session JWT)
func PasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	identity, ok := middleware.RequireSession(w, r)
	if !ok {
		return
	}

	var req PasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if req.OldPassword == "" || req.NewPassword == "" {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Old password and new password are required",
		})
		return
	}

	if len(req.NewPassword) < 8 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "New password must be at least 8 characters long",
		})
		return
	}

	if err := database.UpdatePassword(identity.Username, req.OldPassword, req.NewPassword); err != nil {
		if strings.Contains(err.Error(), "invalid old password") {
			utils.WriteJSON(w, http.StatusUnauthorized, map[string]string{
				"status": "error",
				"detail": "Invalid old password",
			})
			return
		}
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": "Failed to update password",
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "Password updated successfully",
	})
}
