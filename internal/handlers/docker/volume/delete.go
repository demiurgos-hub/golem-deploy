package volume

import (
	"encoding/json"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
	"github.com/demiurgos-hub/golem-deploy/internal/volumes"
)

// DeleteRequest represents the JSON request body for deleting volumes
type DeleteRequest struct {
	VolumeNames []string `json:"volume_names"`
}

// DeleteHandler handles requests to delete volumes
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

	if len(req.VolumeNames) == 0 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "volume_names is required and must not be empty",
		})
		return
	}

	// Delete each volume
	for _, volumeName := range req.VolumeNames {
		if err := volumes.DeleteVolume(volumeName); err != nil {
			utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
				"status": "error",
				"detail": err.Error(),
			})
			return
		}
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "success",
		"message":       "Volumes deleted successfully",
		"deleted_count": len(req.VolumeNames),
	})
}
