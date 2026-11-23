package repo

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// calculateDirectorySize calculates the total size of a directory in bytes
func calculateDirectorySize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// formatSize formats bytes into a human-readable string
func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// RepositoryWithSize extends the database Repository with size information
type RepositoryWithSize struct {
	*database.Repository
	RepoSize      string `json:"repo_size"`
	RepoBytesSize int64  `json:"repo_bytes_size"`
}

// ListHandler handles listing all repositories
func ListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		utils.ErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	repos, err := database.ListRepositories()
	if err != nil {
		// Log the actual error for debugging
		fmt.Printf("[ERROR] Failed to list repositories: %v\n", err)
		utils.ErrorResponse(w, fmt.Sprintf("Failed to list repositories: %v", err), http.StatusInternalServerError)
		return
	}

	// Create enhanced repository list with size information
	reposWithSize := make([]RepositoryWithSize, 0, len(repos))
	for _, repo := range repos {
		repoWithSize := RepositoryWithSize{
			Repository: repo,
			RepoSize:   "N/A",
		}

		// Calculate repository size
		repoPath := git.GetRepoPath(repo.Name)
		if _, err := os.Stat(repoPath); err == nil {
			if size, err := calculateDirectorySize(repoPath); err == nil {
				repoWithSize.RepoBytesSize = size
				repoWithSize.RepoSize = formatSize(size)
			}
		}

		// Remove sensitive information before sending
		repoWithSize.Repository.Password = ""
		repoWithSize.Repository.SSHKey = ""

		reposWithSize = append(reposWithSize, repoWithSize)
	}

	utils.SuccessResponse(w, map[string]interface{}{
		"repositories": reposWithSize,
		"count":        len(reposWithSize),
	})
}
