package docker

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/volumes"
)

// DockerContainer represents a running Docker container
type DockerContainer struct {
	ContainerID string          `json:"container_id"`
	Image       string          `json:"image"`
	Command     string          `json:"command"`
	Created     string          `json:"created"`
	Status      string          `json:"status"`
	Ports       string          `json:"ports"`
	Names       string          `json:"names"`
	Label       string          `json:"label"`
	Stats       *ContainerStats `json:"stats,omitempty"`
}

// ContainerStats represents resource usage statistics for a container
type ContainerStats struct {
	CPUPerc  string `json:"cpu_perc"`
	MemUsage string `json:"mem_usage"`
	MemPerc  string `json:"mem_perc"`
	NetIO    string `json:"net_io"`
	BlockIO  string `json:"block_io"`
	PIDs     string `json:"pids"`
}

// ContainerProcess represents a single process running in a container
type ContainerProcess struct {
	PID     string `json:"pid"`
	User    string `json:"user"`
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	VSZ     string `json:"vsz"`
	RSS     string `json:"rss"`
	TTY     string `json:"tty"`
	Stat    string `json:"stat"`
	Start   string `json:"start"`
	Time    string `json:"time"`
	Command string `json:"command"`
}

// LabelKey is the Docker label used to group containers in the UI.
const LabelKey = "dev.golemengine.deploy.label"

// RunContainer launches a docker container from the specified image
// envVars is a map of environment variables to pass to the container
// label is a custom label for organizing containers - defaults to "default" if empty
func RunContainer(imageName string, containerName string, port string, envVars map[string]string, label string) (string, error) {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return "", fmt.Errorf("docker is not installed")
	}

	// Check if image exists
	checkCmd := exec.Command("docker", "images", "-q", imageName)
	output, err := checkCmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		return "", fmt.Errorf("image not found: %s", imageName)
	}

	fmt.Printf("Launching container '%s' from image '%s'...\n", containerName, imageName)

	// Run the container in detached mode with port mapping
	args := []string{"run", "-d", "--name", containerName}

	// Set label - default to "default" if empty
	if label == "" {
		label = "default"
	}
	args = append(args, "--label", fmt.Sprintf("%s=%s", LabelKey, label))
	fmt.Printf("Setting container label: %s\n", label)

	if port != "" {
		// Support comma-separated port mappings
		ports := strings.Split(port, ",")
		for _, p := range ports {
			p = strings.TrimSpace(p)
			if p != "" {
				args = append(args, "-p", p)
				fmt.Printf("Adding port mapping: %s\n", p)
			}
		}
	}

	// Add environment variables if provided
	for key, value := range envVars {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
		fmt.Printf("Setting environment variable: %s=%s\n", key, value)
	}

	args = append(args, imageName)

	cmd := exec.Command("docker", args...)
	containerID, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run container: %w - %s", err, string(containerID))
	}

	containerIDStr := strings.TrimSpace(string(containerID))
	fmt.Printf("Successfully started container: %s (ID: %s)\n", containerName, containerIDStr)
	return containerIDStr, nil
}

// StopContainer stops a running docker container by name
func StopContainer(containerName string) error {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return fmt.Errorf("docker is not installed")
	}

	fmt.Printf("Stopping container '%s'...\n", containerName)

	cmd := exec.Command("docker", "stop", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to stop container: %w - %s", err, string(output))
	}

	fmt.Printf("Successfully stopped container: %s\n", containerName)
	return nil
}

// StartContainer starts a stopped docker container by name
func StartContainer(containerName string) error {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return fmt.Errorf("docker is not installed")
	}

	fmt.Printf("Starting container '%s'...\n", containerName)

	cmd := exec.Command("docker", "start", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to start container: %w - %s", err, string(output))
	}

	fmt.Printf("Successfully started container: %s\n", containerName)
	return nil
}

// DeleteContainer removes a docker container by name
// If deleteVolume is true, also deletes the associated volume (if it exists)
func DeleteContainer(containerName string, deleteVolume bool) error {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return fmt.Errorf("docker is not installed")
	}

	fmt.Printf("Deleting container '%s'...\n", containerName)

	cmd := exec.Command("docker", "rm", "-f", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete container: %w - %s", err, string(output))
	}

	fmt.Printf("Successfully deleted container: %s\n", containerName)

	// Delete associated volume if requested
	if deleteVolume {
		volumeName := volumes.GetVolumeForContainer(containerName)
		// Check if volume exists
		checkCmd := exec.Command("docker", "volume", "inspect", volumeName)
		if checkOutput, checkErr := checkCmd.CombinedOutput(); checkErr == nil && strings.TrimSpace(string(checkOutput)) != "" {
			fmt.Printf("Deleting associated volume '%s'...\n", volumeName)
			volCmd := exec.Command("docker", "volume", "rm", volumeName)
			if volOutput, volErr := volCmd.CombinedOutput(); volErr != nil {
				// Just log warning, don't fail the container deletion
				fmt.Printf("Warning: failed to delete volume %s: %v - %s\n", volumeName, volErr, string(volOutput))
			} else {
				fmt.Printf("Successfully deleted volume: %s\n", volumeName)
			}
		}
	}

	return nil
}

// ListRunningContainers returns a list of all running docker containers with their stats
func ListRunningContainers() ([]DockerContainer, error) {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return nil, fmt.Errorf("docker is not installed")
	}

	// Use --format to get parseable output including label
	cmd := exec.Command("docker", "ps", "--all", "--format", "{{.ID}}\t{{.Image}}\t{{.Command}}\t{{.CreatedAt}}\t{{.Status}}\t{{.Ports}}\t{{.Names}}\t{{.Label \""+LabelKey+"\"}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		// No containers running
		return []DockerContainer{}, nil
	}

	lines := strings.Split(outputStr, "\n")
	containers := make([]DockerContainer, 0, len(lines))

	// Get stats for all containers
	statsMap, err := GetContainerStats()
	if err != nil {
		// Log error but don't fail - stats are optional
		fmt.Printf("Warning: failed to get container stats: %v\n", err)
		statsMap = make(map[string]*ContainerStats)
	}

	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		// We need at least the basic fields (ID, Image, Names, Label)
		if len(fields) < 8 {
			// Pad with empty strings if needed
			for len(fields) < 8 {
				fields = append(fields, "")
			}
		}

		containerID := fields[0]
		label := fields[7]
		// If label is empty, set to "default" for consistency
		if label == "" {
			label = "default"
		}

		container := DockerContainer{
			ContainerID: containerID,
			Image:       fields[1],
			Command:     fields[2],
			Created:     fields[3],
			Status:      fields[4],
			Ports:       fields[5],
			Names:       fields[6],
			Label:       label,
		}

		// Attach stats if available (only for running containers)
		if stats, ok := statsMap[containerID]; ok {
			container.Stats = stats
		}

		containers = append(containers, container)
	}

	return containers, nil
}

// GetContainerLogs retrieves the logs from a docker container by name or ID
func GetContainerLogs(containerName string) (string, error) {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return "", fmt.Errorf("docker is not installed")
	}

	fmt.Printf("Fetching logs for container '%s'...\n", containerName)

	// Get logs from the container
	cmd := exec.Command("docker", "logs", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get container logs: %w - %s", err, string(output))
	}

	return string(output), nil
}

// GetContainerStats retrieves resource usage statistics for all running containers
func GetContainerStats() (map[string]*ContainerStats, error) {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return nil, fmt.Errorf("docker is not installed")
	}

	// Use docker stats with --no-stream to get a one-time snapshot
	// Format: container_id, cpu%, mem usage, mem%, net i/o, block i/o, pids
	cmd := exec.Command("docker", "stats", "--no-stream", "--format", "{{.ID}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// If no containers are running, docker stats might error
		// Return empty map instead of error
		return make(map[string]*ContainerStats), nil
	}

	statsMap := make(map[string]*ContainerStats)
	outputStr := strings.TrimSpace(string(output))

	if outputStr == "" {
		// No containers running
		return statsMap, nil
	}

	lines := strings.Split(outputStr, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			// Pad with empty strings if needed
			for len(fields) < 7 {
				fields = append(fields, "N/A")
			}
		}

		// Map container ID to its stats
		statsMap[fields[0]] = &ContainerStats{
			CPUPerc:  fields[1],
			MemUsage: fields[2],
			MemPerc:  fields[3],
			NetIO:    fields[4],
			BlockIO:  fields[5],
			PIDs:     fields[6],
		}
	}

	return statsMap, nil
}

// GetContainerProcesses retrieves all processes running in a container by name or ID
func GetContainerProcesses(containerName string) ([]ContainerProcess, error) {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return nil, fmt.Errorf("docker is not installed")
	}

	fmt.Printf("Fetching processes for container '%s'...\n", containerName)

	// Use docker top to get process information
	// Format: PID USER %CPU %MEM VSZ RSS TTY STAT START TIME COMMAND
	cmd := exec.Command("docker", "top", containerName, "aux")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get container processes: %w - %s", err, string(output))
	}

	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		return []ContainerProcess{}, nil
	}

	lines := strings.Split(outputStr, "\n")
	if len(lines) < 2 {
		// No processes or only header line
		return []ContainerProcess{}, nil
	}

	// Skip the header line (first line)
	processes := make([]ContainerProcess, 0, len(lines)-1)
	for i, line := range lines {
		if i == 0 {
			// Skip header
			continue
		}
		if line == "" {
			continue
		}

		// Split by whitespace, but command can have spaces so we need to be careful
		fields := strings.Fields(line)
		if len(fields) < 11 {
			// Not enough fields, skip this line
			continue
		}

		// The command is everything after the 10th field
		command := strings.Join(fields[10:], " ")

		process := ContainerProcess{
			User:    fields[0],
			PID:     fields[1],
			CPU:     fields[2],
			Memory:  fields[3],
			VSZ:     fields[4],
			RSS:     fields[5],
			TTY:     fields[6],
			Stat:    fields[7],
			Start:   fields[8],
			Time:    fields[9],
			Command: command,
		}

		processes = append(processes, process)
	}

	return processes, nil
}
