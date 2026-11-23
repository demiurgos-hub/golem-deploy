package repo

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/demiurgos-hub/golem-deploy/internal/database"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/utils"
)

// AddRequest represents the request body for adding a repository
type AddRequest struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	AuthType string `json:"auth_type"` // "none", "https", "ssh"
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	SSHKey   string `json:"ssh_key,omitempty"`
}

// AddHandler handles adding a new repository
func AddHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.ErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.ErrorResponse(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.Name == "" {
		utils.ErrorResponse(w, "Repository name is required", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		utils.ErrorResponse(w, "Repository URL is required", http.StatusBadRequest)
		return
	}
	if req.Branch == "" {
		req.Branch = "main"
	}
	if req.AuthType == "" {
		req.AuthType = "none"
	}

	// Validate auth type
	if req.AuthType != "none" && req.AuthType != "https" && req.AuthType != "ssh" {
		utils.ErrorResponse(w, "Invalid auth_type. Must be 'none', 'https', or 'ssh'", http.StatusBadRequest)
		return
	}

	// Validate credentials based on auth type
	if req.AuthType == "https" && (req.Username == "" || req.Password == "") {
		utils.ErrorResponse(w, "Username and password required for HTTPS authentication", http.StatusBadRequest)
		return
	}
	if req.AuthType == "ssh" && req.SSHKey == "" {
		utils.ErrorResponse(w, "SSH key required for SSH authentication", http.StatusBadRequest)
		return
	}

	// Create repository record
	repo := &database.Repository{
		Name:     req.Name,
		URL:      req.URL,
		Branch:   req.Branch,
		AuthType: req.AuthType,
		Username: req.Username,
		Password: req.Password,
		SSHKey:   req.SSHKey,
	}

	// Set status to cloning
	repo.Status = "cloning"

	// Add to database
	id, err := database.AddRepository(repo)
	if err != nil {
		utils.ErrorResponse(w, fmt.Sprintf("Failed to add repository: %v", err), http.StatusInternalServerError)
		return
	}

	// Clone the repository in the background
	go func() {
		cloneOpts := git.CloneOptions{
			URL:      req.URL,
			Branch:   req.Branch,
			AuthType: req.AuthType,
			Username: req.Username,
			Password: req.Password,
			SSHKey:   req.SSHKey,
		}

		commitHash, err := git.Clone(req.Name, cloneOpts)
		if err != nil {
			// If clone fails, update status to error
			database.UpdateRepositoryStatus(int(id), "error")
			fmt.Printf("Error cloning repository %s: %v\n", req.Name, err)
			return
		}

		// Update the database with the commit hash and set status to idle
		if err := database.UpdateRepositoryPullInfo(int(id), commitHash); err != nil {
			fmt.Printf("Warning: Failed to update commit hash: %v\n", err)
		}
		database.UpdateRepositoryStatus(int(id), "idle")
		fmt.Printf("Successfully cloned repository %s\n", req.Name)
	}()

	utils.SuccessResponse(w, map[string]interface{}{
		"message": "Repository added, cloning started in background",
		"id":      id,
	})
}
