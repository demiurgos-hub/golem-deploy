package volume

import (
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
	"github.com/demiurgos-hub/golem-deploy/internal/volumes"
)

// OrphanedHandler handles requests to list orphaned volumes
func OrphanedHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	orphanedVols, err := volumes.ListOrphanedVolumes()
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status":           "success",
		"orphaned_volumes": orphanedVols,
		"count":            len(orphanedVols),
	})
}
