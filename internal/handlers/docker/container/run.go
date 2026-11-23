package container

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// RunResult is the outcome of starting a container.
type RunResult struct {
	ContainerID   string
	ContainerName string
	ImageName     string
}

// RunFromRequest starts a container from a run request. Containers currently
// run without a data volume.
func RunFromRequest(req api.RunContainerRequest) (*RunResult, error) {
	imageName := req.ImageName
	if imageName == "" {
		return nil, fmt.Errorf("image_name required")
	}

	containerName := req.ContainerName
	if containerName == "" {
		baseImageName := strings.Split(imageName, ":")[0]
		baseImageName = strings.ReplaceAll(baseImageName, "/", "-")
		containerName = fmt.Sprintf("%s-container", baseImageName)
	}

	envVars := make(map[string]string)

	if req.EnvVars != nil {
		for key, value := range req.EnvVars {
			envVars[key] = value
			fmt.Printf("Adding custom environment variable: %s=%s\n", key, value)
		}
	}

	containerID, err := docker.RunContainer(imageName, containerName, req.Port, envVars, req.Label)
	if err != nil {
		return nil, err
	}

	return &RunResult{
		ContainerID:   containerID,
		ContainerName: containerName,
		ImageName:     imageName,
	}, nil
}

// RunHandler handles requests to run a docker container from an existing image
func RunHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req api.RunContainerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if req.ImageName == "" {
		utils.WriteJSON(w, http.StatusNotFound, map[string]string{
			"status": "error",
			"detail": "image_name required",
		})
		return
	}

	result, err := RunFromRequest(req)
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	responseData := map[string]interface{}{
		"status":         "success",
		"container_id":   result.ContainerID,
		"container_name": result.ContainerName,
		"image_name":     result.ImageName,
		"message":        "Container running successfully",
	}

	utils.WriteJSON(w, http.StatusOK, responseData)
}
