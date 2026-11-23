package account

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/middleware"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// ApproveRequest represents the JSON request body for account approval/deletion
type ApproveRequest struct {
	Username string `json:"username"`
	Approve  bool   `json:"approve"`
}

// ApproveHandler handles account approval and deletion (requires admin session)
func ApproveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := middleware.RequireAdmin(w, r); !ok {
		return
	}

	var req ApproveRequest
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

	if req.Approve {
		if err := database.ApproveAccount(req.Username); err != nil {
			if strings.Contains(err.Error(), "not found") {
				utils.WriteJSON(w, http.StatusNotFound, map[string]string{
					"status": "error",
					"detail": "Account not found",
				})
				return
			}
			utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
				"status": "error",
				"detail": "Failed to approve account",
			})
			return
		}

		utils.WriteJSON(w, http.StatusOK, map[string]string{
			"status":  "success",
			"message": "Account approved successfully",
		})
		return
	}

	if err := database.DeleteAccount(req.Username); err != nil {
		if strings.Contains(err.Error(), "not found") {
			utils.WriteJSON(w, http.StatusNotFound, map[string]string{
				"status": "error",
				"detail": "Account not found",
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

// ListUnapprovedHandler returns list of unapproved accounts (requires admin session)
func ListUnapprovedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := middleware.RequireAdmin(w, r); !ok {
		return
	}

	accounts, err := database.GetUnapprovedAccounts()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": "Failed to retrieve unapproved accounts",
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "success",
		"accounts": accounts,
	})
}

// ListAllHandler returns list of all accounts except the first admin (requires admin session)
func ListAllHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := middleware.RequireAdmin(w, r); !ok {
		return
	}

	accounts, err := database.GetAllAccounts()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": "Failed to retrieve accounts",
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "success",
		"accounts": accounts,
	})
}
