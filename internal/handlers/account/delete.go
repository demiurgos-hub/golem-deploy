package account

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// DeleteRequest represents the JSON request body for account deletion
type DeleteRequest struct {
	Username string `json:"username"`
}

// DeleteHandler handles account deletion (requires admin session)
func DeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := middleware.RequireAdmin(w, r); !ok {
		return
	}

	var req DeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if req.Username == "" {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Username is required",
		})
		return
	}

	accountCount, err := database.CountAccounts()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": "Failed to check account count",
		})
		return
	}

	if accountCount <= 1 {
		utils.WriteJSON(w, http.StatusForbidden, map[string]string{
			"status": "error",
			"detail": "Cannot delete the last account. At least one account must exist.",
		})
		return
	}

	if err := database.DeleteAccount(req.Username); err != nil {
		if strings.Contains(err.Error(), "user not found") {
			utils.WriteJSON(w, http.StatusNotFound, map[string]string{
				"status": "error",
				"detail": "User not found",
			})
			return
		}
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": "Failed to delete account",
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "Account deleted successfully",
	})
}
