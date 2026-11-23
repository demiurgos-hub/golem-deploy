package volume

import (
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
	"github.com/demiurgos-hub/golem-deploy/internal/volumes"
)

// InfoHandler handles requests to get information about a specific volume
func InfoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	// Get container_name from query parameter
	containerName := r.URL.Query().Get("container_name")
	if containerName == "" {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "container_name query parameter is required",
		})
		return
	}

	// Get volume name for this container
	volumeName := volumes.GetVolumeForContainer(containerName)

	// Check if volume exists
	if !volumes.VolumeExists(volumeName) {
		utils.WriteJSON(w, http.StatusNotFound, map[string]string{
			"status": "error",
			"detail": "Volume not found for this container",
		})
		return
	}

	// Get volume info
	info, err := volumes.GetVolumeInfo(volumeName)
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"volume": info,
	})
}
