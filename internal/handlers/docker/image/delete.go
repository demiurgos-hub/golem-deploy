package image

import (
	"encoding/json"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// DeleteRequest represents the JSON request body for deleting images
type DeleteRequest struct {
	ImageIDs []string `json:"image_ids"`
}

// DeleteHandler handles requests to delete docker images
func DeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req DeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if len(req.ImageIDs) == 0 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "image_ids is required and must not be empty",
		})
		return
	}

	// Delete the images
	if err := docker.DeleteImages(req.ImageIDs); err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "success",
		"message": "Images deleted successfully",
	})
}
