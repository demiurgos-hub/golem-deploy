package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CloneOptions contains options for cloning a repository
type CloneOptions struct {
	URL      string
	Branch   string
	AuthType string // "none", "https", "ssh"
	Username string
	Password string
	SSHKey   string
}

// GetRepoPath returns the local path where a repository should be stored
func GetRepoPath(repoName string) string {
	return filepath.Join("repositories", repoName)
}

// Clone clones a Git repository to the local filesystem using native git
func Clone(repoName string, opts CloneOptions) (string, error) {
	repoPath := GetRepoPath(repoName)

	// Check if repository already exists
	if _, err := os.Stat(repoPath); err == nil {
		return "", fmt.Errorf("repository directory already exists: %s", repoPath)
	}

	// Ensure repositories directory exists
	if err := os.MkdirAll("repositories", 0755); err != nil {
		return "", fmt.Errorf("failed to create repositories directory: %w", err)
	}

	// Check if git is installed
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git command not found. Please install git: %w", err)
	}

	// Build git clone command
	args := []string{"clone"}

	// Add branch specification if provided (without --single-branch so all branches are available)
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
	}

	// Handle authentication and build URL
	cloneURL := opts.URL
	var sshKeyFile string
	var err error

	switch opts.AuthType {
	case "https":
		// Embed credentials in URL for HTTPS
		if opts.Username != "" && opts.Password != "" {
			cloneURL, err = embedCredentials(opts.URL, opts.Username, opts.Password)
			if err != nil {
				return "", fmt.Errorf("failed to embed credentials in URL: %w", err)
			}
		}

	case "ssh":
		// Create temporary SSH key file
		if opts.SSHKey != "" {
			sshKeyFile, err = createTempSSHKey(opts.SSHKey)
			if err != nil {
				return "", fmt.Errorf("failed to create SSH key file: %w", err)
			}
			defer os.Remove(sshKeyFile)
		}

	case "none":
		// No authentication needed
	}

	args = append(args, cloneURL, repoPath)

	// Execute git clone
	cmd := exec.Command("git", args...)

	// Set SSH key if using SSH authentication
	if opts.AuthType == "ssh" && sshKeyFile != "" {
		cmd.Env = append(os.Environ(),
			fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=no", sshKeyFile),
		)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git clone failed: %w\nOutput: %s", err, string(output))
	}

	// Configure git settings (filemode, autocrlf)
	if err := configureGitSettings(repoPath); err != nil {
		return "", fmt.Errorf("failed to configure git settings: %w", err)
	}

	// Install and pull Git LFS files if LFS is being used
	if err := setupLFS(repoPath, opts.AuthType, sshKeyFile); err != nil {
		// Log warning but don't fail - repository might not use LFS
		fmt.Printf("Warning: Git LFS setup failed (repo might not use LFS): %v\n", err)
	}

	// Get the current commit hash
	commitHash, err := GetCurrentCommit(repoName)
	if err != nil {
		return "", fmt.Errorf("failed to get commit hash: %w", err)
	}

	fmt.Printf("Successfully cloned repository %s (commit: %s)\n", repoName, commitHash[:7])
	return commitHash, nil
}

// embedCredentials embeds username and password into a Git URL
func embedCredentials(url, username, password string) (string, error) {
	// Handle HTTPS URLs
	if strings.HasPrefix(url, "https://") {
		// Extract the part after https://
		remainder := strings.TrimPrefix(url, "https://")
		// Embed credentials
		return fmt.Sprintf("https://%s:%s@%s", username, password, remainder), nil
	}

	// Handle HTTP URLs
	if strings.HasPrefix(url, "http://") {
		remainder := strings.TrimPrefix(url, "http://")
		return fmt.Sprintf("http://%s:%s@%s", username, password, remainder), nil
	}

	return "", fmt.Errorf("URL must start with http:// or https:// for credential embedding")
}

// createTempSSHKey creates a temporary file with the SSH private key
func createTempSSHKey(sshKey string) (string, error) {
	// Create temp file
	tmpFile, err := os.CreateTemp("", "golem-deploy-ssh-key-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	keyPath := tmpFile.Name()

	// Write SSH key
	if _, err := tmpFile.WriteString(sshKey); err != nil {
		tmpFile.Close()
		os.Remove(keyPath)
		return "", fmt.Errorf("failed to write SSH key: %w", err)
	}

	tmpFile.Close()

	// Set proper permissions (0600 - read/write for owner only)
	if err := os.Chmod(keyPath, 0600); err != nil {
		os.Remove(keyPath)
		return "", fmt.Errorf("failed to set key file permissions: %w", err)
	}

	return keyPath, nil
}

// RepositoryExists checks if a repository directory exists locally
func RepositoryExists(repoName string) bool {
	repoPath := GetRepoPath(repoName)
	_, err := os.Stat(repoPath)
	return err == nil
}

// DeleteRepository deletes a repository directory from the local filesystem
func DeleteRepository(repoName string) error {
	repoPath := GetRepoPath(repoName)
	if err := os.RemoveAll(repoPath); err != nil {
		return fmt.Errorf("failed to delete repository directory: %w", err)
	}
	return nil
}

// configureGitSettings sets git configuration to prevent false modifications
func configureGitSettings(repoPath string) error {
	// Disable file mode tracking (permission changes) - important for cross-platform repos
	configCmd := exec.Command("git", "config", "core.filemode", "false")
	configCmd.Dir = repoPath
	if output, err := configCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set core.filemode: %w\nOutput: %s", err, string(output))
	}

	// Set autocrlf to false to prevent automatic line ending conversion
	// This keeps line endings as-is (LF), preventing false modifications
	configCmd = exec.Command("git", "config", "core.autocrlf", "false")
	configCmd.Dir = repoPath
	if output, err := configCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set core.autocrlf: %w\nOutput: %s", err, string(output))
	}

	fmt.Printf("Git repository configured: core.filemode=false, core.autocrlf=false\n")
	return nil
}

// setupLFS configures and pulls Git LFS files
func setupLFS(repoPath, authType, sshKeyFile string) error {
	// Check if git-lfs is installed
	if _, err := exec.LookPath("git-lfs"); err != nil {
		return fmt.Errorf("git-lfs command not found (this is OK if the repo doesn't use LFS)")
	}

	// Install Git LFS hooks and filters in this repository
	// This is CRITICAL - without this, LFS pointers won't be replaced with actual file content
	installCmd := exec.Command("git", "lfs", "install", "--local", "--skip-smudge")
	installCmd.Dir = repoPath
	if installOutput, err := installCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git lfs install failed: %w\nOutput: %s", err, string(installOutput))
	}
	fmt.Printf("Git LFS installed in repository\n")

	// Pull LFS files
	env := os.Environ()
	if authType == "ssh" && sshKeyFile != "" {
		env = append(env,
			fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=no", sshKeyFile),
		)
	}

	pullCmd := exec.Command("git", "lfs", "pull")
	pullCmd.Dir = repoPath
	pullCmd.Env = env

	if pullOutput, err := pullCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git lfs pull failed: %w\nOutput: %s", err, string(pullOutput))
	}

	fmt.Printf("Git LFS files pulled successfully\n")

	// Use 'git lfs checkout' to ensure all LFS files are properly checked out
	// This replaces any LFS pointer files with actual content
	checkoutCmd := exec.Command("git", "lfs", "checkout")
	checkoutCmd.Dir = repoPath
	if checkoutOutput, err := checkoutCmd.CombinedOutput(); err != nil {
		fmt.Printf("Warning: git lfs checkout had issues: %v\nOutput: %s\n", err, string(checkoutOutput))
	}

	// Now that LFS files are in place, do a final status check to see if anything is modified
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = repoPath
	if statusOutput, err := statusCmd.CombinedOutput(); err == nil {
		if len(statusOutput) > 0 {
			fmt.Printf("Warning: Repository has modifications after LFS checkout:\n%s\n", string(statusOutput))
			// Reset to clean working directory
			resetCmd := exec.Command("git", "reset", "--hard", "HEAD")
			resetCmd.Dir = repoPath
			if resetOutput, resetErr := resetCmd.CombinedOutput(); resetErr != nil {
				fmt.Printf("Warning: Failed to reset: %v\nOutput: %s\n", resetErr, string(resetOutput))
			} else {
				fmt.Printf("Repository reset to clean state\n")
			}
		}
	}

	return nil
}
