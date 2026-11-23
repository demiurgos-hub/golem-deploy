package static

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// DownloadHandler handles requests to download static files
func DownloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	// Get file_name from query parameter
	fileName := r.URL.Query().Get("file_name")
	if fileName == "" {
		http.Error(w, "file_name query parameter is required", http.StatusBadRequest)
		return
	}

	// Validate filename (prevent path traversal)
	if filepath.Base(fileName) != fileName {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	// Validate and get safe path with path traversal protection
	absFilePath, err := utils.ValidatePathWithinDirectory(utils.StaticDir, fileName)
	if err != nil {
		http.Error(w, "invalid file path", http.StatusBadRequest)
		return
	}

	// Check if file exists
	if _, err := os.Stat(absFilePath); os.IsNotExist(err) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	// Open the file
	file, err := os.Open(absFilePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to open file: %v", err), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Get file info
	fileInfo, err := file.Stat()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get file info: %v", err), http.StatusInternalServerError)
		return
	}

	// Set headers for file download
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))

	// Stream the file to the response
	http.ServeContent(w, r, fileName, fileInfo.ModTime(), file)
}
