package image

import (
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// ListHandler handles requests to list all docker images
func ListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	// List docker images
	output, err := docker.ListDockerImages()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"images": output,
	})
}
