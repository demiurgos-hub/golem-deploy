package repo

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// BranchRequest represents the request body for switching a branch
type BranchRequest struct {
	RepoID int    `json:"repo_id"`
	Branch string `json:"branch"`
}

// BranchHandler handles switching repository branches
func BranchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.ErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req BranchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.ErrorResponse(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.RepoID == 0 {
		utils.ErrorResponse(w, "Repository ID is required", http.StatusBadRequest)
		return
	}

	if req.Branch == "" {
		utils.ErrorResponse(w, "Branch name is required", http.StatusBadRequest)
		return
	}

	// Get repository from database
	repo, err := database.GetRepository(req.RepoID)
	if err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to get repository: %v", err), http.StatusNotFound)
		return
	}

	// Check if repository exists locally
	if !git.RepositoryExists(repo.Name) {
		utils.ErrorResponse(w, "Repository not found locally. Please pull the repository first.", http.StatusNotFound)
		return
	}

	// Check if already pulling/cloning
	if repo.Status == "pulling" || repo.Status == "cloning" {
		utils.ErrorResponse(w, fmt.Sprintf("Repository is currently %s", repo.Status), http.StatusConflict)
		return
	}

	// Update status to show operation in progress
	if err := database.UpdateRepositoryStatus(req.RepoID, "pulling"); err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to update status: %v", err), http.StatusInternalServerError)
		return
	}

	// Log branch switch start
	log.Printf("[BRANCH] Switching repository '%s' (ID: %d) to branch '%s'", repo.Name, repo.ID, req.Branch)

	jobID, err := launchRepoJob(req.RepoID, database.RepoJobKindBranch, repo.Status, func() (string, error) {
		switchOpts := git.SwitchBranchOptions{
			Branch:   req.Branch,
			AuthType: repo.AuthType,
			Username: repo.Username,
			Password: repo.Password,
			SSHKey:   repo.SSHKey,
		}

		commitHash, err := git.SwitchBranch(repo.Name, switchOpts)
		if err != nil {
			log.Printf("[BRANCH] Error during branch switch for repo %s: %v", repo.Name, err)
			database.UpdateRepositoryStatus(req.RepoID, "error")
			return "", err
		}

		if err := database.UpdateRepositoryBranch(req.RepoID, req.Branch); err != nil {
			log.Printf("[BRANCH] Warning: Failed to update branch for repo %s: %v", repo.Name, err)
		}
		if err := database.UpdateRepositoryPullInfo(req.RepoID, commitHash); err != nil {
			log.Printf("[BRANCH] Warning: Failed to update commit hash for repo %s: %v", repo.Name, err)
		}
		if err := database.UpdateRepositoryStatus(req.RepoID, "idle"); err != nil {
			log.Printf("[BRANCH] Warning: Failed to update status for repo %s: %v", repo.Name, err)
		}

		log.Printf("[BRANCH] Successfully switched repo %s to branch %s", repo.Name, req.Branch)
		return "", nil
	})
	if err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to create job: %v", err), http.StatusInternalServerError)
		return
	}

	utils.SuccessResponse(w, map[string]interface{}{
		"message": fmt.Sprintf("Branch switch to '%s' started", req.Branch),
		"status":  "pulling",
		"job_id":  jobID,
	})
}
