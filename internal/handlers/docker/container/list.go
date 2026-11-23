package container

import (
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// ListHandler handles requests to list all running docker containers
func ListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	// List running containers
	output, err := docker.ListRunningContainers()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "success",
		"containers": output,
	})
}
