package handlers

import (
	"fmt"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// StatusHandler reports whether the docker CLI is available on the host.
func StatusHandler(w http.ResponseWriter, r *http.Request) {
	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"docker_installed": fmt.Sprintf("%v", docker.CheckDockerInstalled()),
	})
}
