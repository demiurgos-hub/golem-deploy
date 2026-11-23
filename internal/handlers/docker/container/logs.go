package container

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// LogsRequest represents the JSON request body for getting container logs
type LogsRequest struct {
	ContainerName string `json:"container_name"`
}

// LogsHandler handles requests to get logs from a docker container
func LogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req LogsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if req.ContainerName == "" {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "container_name is required",
		})
		return
	}

	// Get container logs
	logs, err := docker.GetContainerLogs(req.ContainerName)
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	// Set headers for file download
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-logs.txt", req.ContainerName))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(logs))
}
