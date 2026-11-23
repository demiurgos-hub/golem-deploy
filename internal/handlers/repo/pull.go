package repo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// PullRequest represents the request body for pulling a repository
type PullRequest struct {
	RepoID int `json:"repo_id"`
}

// PullHandler handles pulling the latest changes from a repository
func PullHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.ErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req PullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.ErrorResponse(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.RepoID == 0 {
		utils.ErrorResponse(w, "Repository ID is required", http.StatusBadRequest)
		return
	}

	// Get repository from database
	repo, err := database.GetRepository(req.RepoID)
	if err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to get repository: %v", err), http.StatusNotFound)
		return
	}

	// Check if already pulling/cloning
	if repo.Status == "pulling" || repo.Status == "cloning" {
		utils.ErrorResponse(w, "Repository is already being updated", http.StatusConflict)
		return
	}

	// Determine if we need to clone or pull
	needsClone := !git.RepositoryExists(repo.Name)
	action := "pulling"
	if needsClone {
		action = "cloning"
	}

	// Update status to show operation in progress
	if err := database.UpdateRepositoryStatus(req.RepoID, action); err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to update status: %v", err), http.StatusInternalServerError)
		return
	}

	jobID, err := launchRepoJob(req.RepoID, database.RepoJobKindPull, repo.Status, func() (string, error) {
		var commitHash string
		var err error

		if needsClone {
			cloneOpts := git.CloneOptions{
				URL:      repo.URL,
				Branch:   repo.Branch,
				AuthType: repo.AuthType,
				Username: repo.Username,
				Password: repo.Password,
				SSHKey:   repo.SSHKey,
			}

			commitHash, err = git.Clone(repo.Name, cloneOpts)
		} else {
			pullOpts := git.PullOptions{
				Branch:   repo.Branch,
				AuthType: repo.AuthType,
				Username: repo.Username,
				Password: repo.Password,
				SSHKey:   repo.SSHKey,
			}

			commitHash, err = git.Pull(repo.Name, pullOpts)
		}

		if err != nil {
			fmt.Printf("Error during %s for repo %s: %v\n", action, repo.Name, err)
			database.UpdateRepositoryStatus(req.RepoID, "error")
			return "", err
		}

		if err := database.UpdateRepositoryPullInfo(req.RepoID, commitHash); err != nil {
			fmt.Printf("Warning: Failed to update commit hash for repo %s: %v\n", repo.Name, err)
		}
		if err := database.UpdateRepositoryStatus(req.RepoID, "idle"); err != nil {
			fmt.Printf("Warning: Failed to update status for repo %s: %v\n", repo.Name, err)
		}

		fmt.Printf("Successfully completed %s for repo %s\n", action, repo.Name)
		return "", nil
	})
	if err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to create job: %v", err), http.StatusInternalServerError)
		return
	}

	utils.SuccessResponse(w, map[string]interface{}{
		"message": fmt.Sprintf("Repository %s started", action),
		"action":  action,
		"status":  action,
		"job_id":  jobID,
	})
}

// DeleteHandler handles deleting a repository
func DeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		utils.ErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get repo_id from query parameter
	repoIDStr := r.URL.Query().Get("repo_id")
	if repoIDStr == "" {
		utils.ErrorResponse(w, "Repository ID is required", http.StatusBadRequest)
		return
	}

	repoID, err := strconv.Atoi(repoIDStr)
	if err != nil {
		utils.ErrorResponse(w, "Invalid repository ID", http.StatusBadRequest)
		return
	}

	// Get repository from database to get the name
	repo, err := database.GetRepository(repoID)
	if err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to get repository: %v", err), http.StatusNotFound)
		return
	}

	// Delete from filesystem
	if git.RepositoryExists(repo.Name) {
		if err := git.DeleteRepository(repo.Name); err != nil {
			// Log the error but continue with database deletion
			fmt.Printf("Warning: Failed to delete repository directory: %v\n", err)
		}
	}

	// Delete from database
	if err := database.DeleteRepository(repoID); err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to delete repository: %v", err), http.StatusInternalServerError)
		return
	}

	utils.SuccessResponse(w, map[string]interface{}{
		"message": "Repository deleted successfully",
	})
}
