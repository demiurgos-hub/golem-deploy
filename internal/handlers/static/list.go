package static

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/demiurgos-hub/golem-deploy/internal/api"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// ListHandler lists all available static files
func ListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	// Read all files in the static directory
	entries, err := os.ReadDir(utils.StaticDir)
	if err != nil {
		utils.WriteJSON(w, http.StatusInternalServerError, map[string]string{
			"status": "error",
			"detail": fmt.Sprintf("Failed to read static directory: %v", err),
		})
		return
	}

	staticFiles := make([]api.StaticFile, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()

		// Get file info
		filePath := filepath.Join(utils.StaticDir, filename)
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			continue
		}

		// Get file extension
		ext := filepath.Ext(filename)

		staticFiles = append(staticFiles, api.StaticFile{
			Name:      filename,
			Size:      fileInfo.Size(),
			Extension: ext,
		})
	}

	utils.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"status": "success",
		"files":  staticFiles,
		"count":  len(staticFiles),
	})
}
