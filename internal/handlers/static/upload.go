package static

import (
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// UploadHandler handles multiple static file upload requests
func UploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	filenames, err := utils.SaveStaticFiles(r)
	if err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": err.Error(),
		})
		return
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"files":  filenames,
		"count":  len(filenames),
	})
}
