package volume

import (
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
	"github.com/demiurgos-hub/golem-deploy/internal/volumes"
)

// ListHandler handles requests to list all volumes
func ListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	vols, err := volumes.ListAllVolumes()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "success",
		"volumes": vols,
	})
}
