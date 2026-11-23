package docker

import "os/exec"

// CheckDockerInstalled reports whether the docker CLI is installed and runnable.
func CheckDockerInstalled() bool {
	return exec.Command("docker", "--version").Run() == nil
}
