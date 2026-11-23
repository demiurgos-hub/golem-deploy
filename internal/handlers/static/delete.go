package static

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// DeleteRequest represents the JSON request body for deleting static files
type DeleteRequest struct {
	FileNames []string `json:"file_names"`
}

// DeleteHandler handles requests to delete static files
func DeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request
	var req DeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "Invalid JSON request",
		})
		return
	}

	if len(req.FileNames) == 0 {
		utils.WriteJSON(w, http.StatusBadRequest, map[string]string{
			"status": "error",
			"detail": "file_names is required and must not be empty",
		})
		return
	}

	deletedFiles := make([]string, 0)
	errors := make([]string, 0)

	for _, fileName := range req.FileNames {
		// Validate filename (prevent path traversal)
		if filepath.Base(fileName) != fileName {
			errors = append(errors, fmt.Sprintf("%s: invalid filename", fileName))
			continue
		}

		// Validate and get safe path with path traversal protection
		absFilePath, err := utils.ValidatePathWithinDirectory(utils.StaticDir, fileName)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: invalid file path", fileName))
			continue
		}

		// Check if file exists
		if _, err := os.Stat(absFilePath); os.IsNotExist(err) {
			errors = append(errors, fmt.Sprintf("%s: file not found", fileName))
			continue
		}

		// Delete the file
		if err := os.Remove(absFilePath); err != nil {
			errors = append(errors, fmt.Sprintf("%s: failed to delete - %v", fileName, err))
			continue
		}

		deletedFiles = append(deletedFiles, fileName)
	}

	// Determine response status
	statusCode := http.StatusOK
	status := "success"
	if len(deletedFiles) == 0 {
		statusCode = http.StatusBadRequest
		status = "error"
	} else if len(errors) > 0 {
		status = "partial"
	}

	utils.WriteJSON(w, statusCode, map[string]interface{}{
		"status":  status,
		"deleted": deletedFiles,
		"errors":  errors,
		"count":   len(deletedFiles),
	})
}
