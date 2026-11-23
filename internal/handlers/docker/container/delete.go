package container

import (
	"encoding/json"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
	"github.com/demiurgos-hub/golem-deploy/internal/volumes"
)

// DeleteRequest represents the JSON request body for deleting containers
type DeleteRequest struct {
	ContainerNames []string `json:"container_names"`
	DeleteSaves    bool     `json:"delete_saves,omitempty"` // If true, also delete associated volumes
}

// DeleteHandler handles requests to delete docker containers
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

	if len(req.ContainerNames) == 0 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "container_names is required and must not be empty",
		})
		return
	}

	// Delete each container
	deletedVolumes := []string{}
	for _, containerName := range req.ContainerNames {
		if err := docker.DeleteContainer(containerName, req.DeleteSaves); err != nil {
			utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
				"status": "error",
				"detail": err.Error(),
			})
			return
		}
		if req.DeleteSaves {
			deletedVolumes = append(deletedVolumes, volumes.GetVolumeForContainer(containerName))
		}
	}

	responseData := map[string]interface{}{
		"status":  "success",
		"message": "Containers deleted successfully",
	}

	if req.DeleteSaves && len(deletedVolumes) > 0 {
		responseData["deleted_volumes"] = deletedVolumes
	}

	utils.WriteJSON(w, http.StatusOK, responseData)
}
