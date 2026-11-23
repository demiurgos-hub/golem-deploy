package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SwitchBranchOptions contains options for switching branches
type SwitchBranchOptions struct {
	Branch   string
	AuthType string // "none", "https", "ssh"
	Username string
	Password string
	SSHKey   string
}

// SwitchBranch switches to a branch, creating a local branch that tracks the remote branch
func SwitchBranch(repoName string, opts SwitchBranchOptions) (string, error) {
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

	// First, check if the repository was cloned with --single-branch
	// If so, we need to reconfigure it to fetch all branches
	fmt.Printf("Checking if repository is configured for single-branch...\n")

	// Check the fetch refspec configuration
	configCheckCmd := exec.Command("git", "config", "--get", "remote.origin.fetch")
	configCheckCmd.Dir = repoPath
	configOutput, _ := configCheckCmd.CombinedOutput()
	fetchRefspec := strings.TrimSpace(string(configOutput))

	// If the refspec is for a single branch, update it to fetch all branches
	if strings.Contains(fetchRefspec, "refs/heads/") && !strings.Contains(fetchRefspec, "refs/heads/*") {
		fmt.Printf("Repository was cloned with --single-branch, updating configuration to fetch all branches...\n")

		// Update the fetch refspec to get all branches
		configSetCmd := exec.Command("git", "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
		configSetCmd.Dir = repoPath
		if configSetErr := configSetCmd.Run(); configSetErr != nil {
			fmt.Printf("Warning: Failed to update fetch configuration: %v\n", configSetErr)
		} else {
			fmt.Printf("Successfully updated fetch configuration to track all branches\n")
		}
	}

	// Configure git to prevent false modifications (in case repo was cloned before this fix)
	if err := ensureGitConfig(repoPath); err != nil {
		fmt.Printf("Warning: Failed to configure git settings: %v\n", err)
	}

	// Now fetch all branches from origin
	fmt.Printf("Fetching all branches from remote...\n")
	fetchCmd := exec.Command("git", "fetch", "origin", "--prune")
	fetchCmd.Dir = repoPath
	fetchCmd.Env = env

	fetchOutput, err := fetchCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git fetch failed: %w\nOutput: %s", err, string(fetchOutput))
	}
	fmt.Printf("Fetch output: %s\n", string(fetchOutput))

	// Check if the branch exists locally
	checkLocalCmd := exec.Command("git", "rev-parse", "--verify", opts.Branch)
	checkLocalCmd.Dir = repoPath
	localBranchExists := checkLocalCmd.Run() == nil

	// Check if the branch exists on remote using git branch -r
	// This is more reliable than git rev-parse after fetch
	checkRemoteCmd := exec.Command("git", "branch", "-r")
	checkRemoteCmd.Dir = repoPath
	remoteBranchesOutput, err := checkRemoteCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to list remote branches: %w\nOutput: %s", err, string(remoteBranchesOutput))
	}

	// Check if origin/<branch> exists in the list of remote branches
	remoteBranchName := fmt.Sprintf("origin/%s", opts.Branch)
	remoteBranchExists := false
	for _, line := range strings.Split(string(remoteBranchesOutput), "\n") {
		trimmedLine := strings.TrimSpace(line)
		// Remote branches are listed as "  origin/branch" or "origin/branch"
		if trimmedLine == remoteBranchName || strings.HasSuffix(trimmedLine, remoteBranchName) {
			remoteBranchExists = true
			break
		}
	}

	if !remoteBranchExists {
		return "", fmt.Errorf("branch '%s' does not exist on remote. Available remote branches:\n%s", opts.Branch, string(remoteBranchesOutput))
	}

	// Switch to the branch
	var switchCmd *exec.Cmd
	if localBranchExists {
		// Local branch exists, just switch to it
		fmt.Printf("Switching to existing local branch '%s'...\n", opts.Branch)
		switchCmd = exec.Command("git", "checkout", opts.Branch)
	} else {
		// Local branch doesn't exist, create it tracking the remote branch
		fmt.Printf("Creating local branch '%s' tracking remote branch 'origin/%s'...\n", opts.Branch, opts.Branch)
		switchCmd = exec.Command("git", "checkout", "-b", opts.Branch, "--track", fmt.Sprintf("origin/%s", opts.Branch))
	}

	switchCmd.Dir = repoPath
	switchCmd.Env = env

	switchOutput, err := switchCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git checkout failed: %w\nOutput: %s", err, string(switchOutput))
	}

	// Pull the latest changes for this branch
	fmt.Printf("Pulling latest changes for branch '%s'...\n", opts.Branch)
	pullCmd := exec.Command("git", "pull", "origin", opts.Branch)
	pullCmd.Dir = repoPath
	pullCmd.Env = env

	pullOutput, err := pullCmd.CombinedOutput()
	if err != nil {
		// Check if it's "already up to date"
		if strings.Contains(string(pullOutput), "Already up to date") ||
			strings.Contains(string(pullOutput), "Already up-to-date") {
			// Not an error, just no changes
		} else {
			return "", fmt.Errorf("git pull failed: %w\nOutput: %s", err, string(pullOutput))
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

	fmt.Printf("Successfully switched to branch '%s' (commit: %s)\n", opts.Branch, commitHash[:7])
	return commitHash, nil
}

// ensureGitConfig ensures git configuration is set correctly to prevent false modifications
func ensureGitConfig(repoPath string) error {
	// Set core.filemode to false
	configCmd := exec.Command("git", "config", "core.filemode", "false")
	configCmd.Dir = repoPath
	if output, err := configCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set core.filemode: %w\nOutput: %s", err, string(output))
	}

	// Set core.autocrlf to false
	configCmd = exec.Command("git", "config", "core.autocrlf", "false")
	configCmd.Dir = repoPath
	if output, err := configCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set core.autocrlf: %w\nOutput: %s", err, string(output))
	}

	fmt.Printf("Git configuration updated: core.filemode=false, core.autocrlf=false\n")
	return nil
}

// handleLFSAfterPull ensures LFS files are properly checked out after a pull/switch
func handleLFSAfterPull(repoPath, authType, sshKeyFile string) error {
	// Check if git-lfs is installed
	if _, err := exec.LookPath("git-lfs"); err != nil {
		return fmt.Errorf("git-lfs command not found (this is OK if the repo doesn't use LFS)")
	}

	// Ensure Git LFS is installed in this repository
	installCmd := exec.Command("git", "lfs", "install", "--local", "--skip-smudge")
	installCmd.Dir = repoPath
	if installOutput, err := installCmd.CombinedOutput(); err != nil {
		fmt.Printf("Warning: git lfs install failed: %v\nOutput: %s\n", err, string(installOutput))
	}

	// Set up environment for SSH if needed
	env := os.Environ()
	if authType == "ssh" && sshKeyFile != "" {
		env = append(env,
			fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=no", sshKeyFile),
		)
	}

	// Pull LFS files
	pullCmd := exec.Command("git", "lfs", "pull")
	pullCmd.Dir = repoPath
	pullCmd.Env = env
	if pullOutput, err := pullCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git lfs pull failed: %w\nOutput: %s", err, string(pullOutput))
	}

	// Explicitly checkout LFS files to replace any pointer files
	checkoutCmd := exec.Command("git", "lfs", "checkout")
	checkoutCmd.Dir = repoPath
	if checkoutOutput, err := checkoutCmd.CombinedOutput(); err != nil {
		fmt.Printf("Warning: git lfs checkout had issues: %v\nOutput: %s\n", err, string(checkoutOutput))
	}

	// Check if there are any modifications and reset if needed
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

	fmt.Printf("Git LFS files handled successfully\n")
	return nil
}
