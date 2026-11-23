package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PullOptions contains options for pulling a repository
type PullOptions struct {
	Branch   string
	AuthType string // "none", "https", "ssh"
	Username string
	Password string
	SSHKey   string
}

// Pull pulls the latest changes from a Git repository using native git
func Pull(repoName string, opts PullOptions) (string, error) {
	repoPath := GetRepoPath(repoName)

	// Check if repository exists
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		return "", fmt.Errorf("repository directory does not exist: %s", repoPath)
	}

	// Check if git is installed
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git command not found. Please install git: %w", err)
	}

	// Set up authentication
	var sshKeyFile string
	var err error
	var env []string

	switch opts.AuthType {
	case "https":
		// For HTTPS, we need to configure git credential helper
		if opts.Username != "" && opts.Password != "" {
			// Set credentials via environment variables for this operation
			env = append(os.Environ(),
				fmt.Sprintf("GIT_ASKPASS=%s", createCredentialHelper(opts.Password)),
				fmt.Sprintf("GIT_USERNAME=%s", opts.Username),
			)
		}

	case "ssh":
		// Create temporary SSH key file
		if opts.SSHKey != "" {
			sshKeyFile, err = createTempSSHKey(opts.SSHKey)
			if err != nil {
				return "", fmt.Errorf("failed to create SSH key file: %w", err)
			}
			defer os.Remove(sshKeyFile)

			env = append(os.Environ(),
				fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=no", sshKeyFile),
			)
		}

	case "none":
		env = os.Environ()
	}

	// Ensure git configuration is set correctly (in case repo was cloned before this fix)
	if err := ensureGitConfig(repoPath); err != nil {
		fmt.Printf("Warning: Failed to configure git settings: %v\n", err)
	}

	// Reset any files that might be marked as modified and clean untracked files BEFORE pulling
	fmt.Printf("Cleaning working directory before pull...\n")
	resetCmd := exec.Command("git", "reset", "--hard", "HEAD")
	resetCmd.Dir = repoPath
	if resetOutput, resetErr := resetCmd.CombinedOutput(); resetErr != nil {
		fmt.Printf("Warning: Failed to reset repository before pull: %v\nOutput: %s\n", resetErr, string(resetOutput))
	}

	// Remove untracked files and directories
	cleanCmd := exec.Command("git", "clean", "-f", "-d")
	cleanCmd.Dir = repoPath
	if cleanOutput, cleanErr := cleanCmd.CombinedOutput(); cleanErr != nil {
		fmt.Printf("Warning: Failed to clean repository before pull: %v\nOutput: %s\n", cleanErr, string(cleanOutput))
	} else if len(cleanOutput) > 0 {
		fmt.Printf("Cleaned untracked files: %s\n", string(cleanOutput))
	}

	// Build git pull command
	args := []string{"pull", "origin"}
	if opts.Branch != "" {
		args = append(args, opts.Branch)
	}

	// Execute git pull
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	cmd.Env = env

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if it's "already up to date"
		if strings.Contains(string(output), "Already up to date") ||
			strings.Contains(string(output), "Already up-to-date") {
			// Not an error, just no changes
		} else {
			return "", fmt.Errorf("git pull failed: %w\nOutput: %s", err, string(output))
		}
	}

	// Handle Git LFS files if LFS is being used
	if err := handleLFSAfterPull(repoPath, opts.AuthType, sshKeyFile); err != nil {
		// Log warning but don't fail - repository might not use LFS
		fmt.Printf("Warning: Git LFS handling failed (repo might not use LFS): %v\n", err)
	}

	// Get the current commit hash
	commitHash, err := GetCurrentCommit(repoName)
	if err != nil {
		return "", fmt.Errorf("failed to get commit hash: %w", err)
	}

	fmt.Printf("Successfully pulled repository %s (commit: %s)\n", repoName, commitHash[:7])
	return commitHash, nil
}

// GetCurrentCommit returns the current commit hash of a repository
func GetCurrentCommit(repoName string) (string, error) {
	repoPath := GetRepoPath(repoName)

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoPath

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get commit hash: %w\nOutput: %s", err, string(output))
	}

	commitHash := strings.TrimSpace(string(output))
	return commitHash, nil
}

// createCredentialHelper creates a simple script to provide password
// This is a helper for HTTPS authentication
func createCredentialHelper(password string) string {
	// Return echo command that outputs the password
	// Note: This is a simplified approach. In production, consider using git credential helpers
	return fmt.Sprintf("echo %s", password)
}
