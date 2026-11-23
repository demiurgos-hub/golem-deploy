package volumes

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Docker labels set on managed volumes. Listing is scoped to ContainerLabel.
const (
	ContainerLabel = "dev.golemengine.deploy.container"
	CreatedLabel   = "dev.golemengine.deploy.created"
)

// CreateVolume creates a new Docker data volume for a container.
// Volume naming: golem-deploy-{containerName}-data
func CreateVolume(containerName string) (string, error) {
	volumeName := GetVolumeForContainer(containerName)

	fmt.Printf("Creating volume '%s' for container '%s'...\n", volumeName, containerName)

	// Create the volume with a label to associate it with the container
	cmd := exec.Command("docker", "volume", "create",
		"--label", fmt.Sprintf("%s=%s", ContainerLabel, containerName),
		"--label", fmt.Sprintf("%s=%s", CreatedLabel, time.Now().Format(time.RFC3339)),
		volumeName)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to create volume: %w - %s", err, string(output))
	}

	fmt.Printf("Successfully created volume: %s\n", volumeName)
	return volumeName, nil
}

// DeleteVolume removes a Docker volume
func DeleteVolume(volumeName string) error {
	fmt.Printf("Deleting volume '%s'...\n", volumeName)

	cmd := exec.Command("docker", "volume", "rm", volumeName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete volume: %w - %s", err, string(output))
	}

	fmt.Printf("Successfully deleted volume: %s\n", volumeName)
	return nil
}

// GetVolumeForContainer returns the volume name for a specific container
func GetVolumeForContainer(containerName string) string {
	return fmt.Sprintf("golem-deploy-%s-data", containerName)
}

// VolumeExists checks if a volume exists
func VolumeExists(volumeName string) bool {
	cmd := exec.Command("docker", "volume", "inspect", volumeName)
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		return false
	}
	return true
}

// ListAllVolumes lists all Golem Deploy managed volumes
func ListAllVolumes() ([]VolumeInfo, error) {
	// Get all volumes carrying the managed container label
	cmd := exec.Command("docker", "volume", "ls",
		"--filter", "label="+ContainerLabel,
		"--format", "{{json .}}")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list volumes: %w - %s", err, string(output))
	}

	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		return []VolumeInfo{}, nil
	}

	lines := strings.Split(outputStr, "\n")
	volumes := make([]VolumeInfo, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Parse JSON output
		var rawVolume struct {
			Name       string `json:"Name"`
			Driver     string `json:"Driver"`
			Mountpoint string `json:"Mountpoint"`
		}

		if err := json.Unmarshal([]byte(line), &rawVolume); err != nil {
			continue // Skip malformed entries
		}

		// Get detailed volume info including labels
		info, err := getVolumeDetails(rawVolume.Name)
		if err != nil {
			continue
		}

		volumes = append(volumes, info)
	}

	return volumes, nil
}

// getVolumeDetails retrieves detailed information about a volume
func getVolumeDetails(volumeName string) (VolumeInfo, error) {
	cmd := exec.Command("docker", "volume", "inspect", volumeName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return VolumeInfo{}, fmt.Errorf("failed to inspect volume: %w", err)
	}

	// Parse JSON output (docker volume inspect returns an array)
	var inspectData []struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		Mountpoint string            `json:"Mountpoint"`
		Labels     map[string]string `json:"Labels"`
		CreatedAt  string            `json:"CreatedAt"`
	}

	if err := json.Unmarshal(output, &inspectData); err != nil {
		return VolumeInfo{}, fmt.Errorf("failed to parse volume inspect output: %w", err)
	}

	if len(inspectData) == 0 {
		return VolumeInfo{}, fmt.Errorf("volume not found: %s", volumeName)
	}

	vol := inspectData[0]

	// Parse creation time
	var createdAt time.Time
	if vol.CreatedAt != "" {
		createdAt, _ = time.Parse(time.RFC3339, vol.CreatedAt)
	}
	// Fallback to label if CreatedAt field is empty
	if createdAt.IsZero() && vol.Labels[CreatedLabel] != "" {
		createdAt, _ = time.Parse(time.RFC3339, vol.Labels[CreatedLabel])
	}

	return VolumeInfo{
		Name:          vol.Name,
		Driver:        vol.Driver,
		ContainerName: vol.Labels[ContainerLabel],
		CreatedAt:     createdAt,
		Mountpoint:    vol.Mountpoint,
	}, nil
}

// ListOrphanedVolumes lists volumes that don't have an associated running or stopped container
func ListOrphanedVolumes() ([]VolumeInfo, error) {
	allVolumes, err := ListAllVolumes()
	if err != nil {
		return nil, err
	}

	// Get list of all containers (running and stopped)
	cmd := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	// Build a map of existing container names
	containerNames := make(map[string]bool)
	outputStr := strings.TrimSpace(string(output))
	if outputStr != "" {
		for _, name := range strings.Split(outputStr, "\n") {
			containerNames[strings.TrimSpace(name)] = true
		}
	}

	// Filter volumes without associated containers
	orphanedVolumes := make([]VolumeInfo, 0)
	for _, vol := range allVolumes {
		if !containerNames[vol.ContainerName] {
			orphanedVolumes = append(orphanedVolumes, vol)
		}
	}

	return orphanedVolumes, nil
}

// GetVolumeInfo returns detailed information about a specific volume
func GetVolumeInfo(volumeName string) (VolumeInfo, error) {
	info, err := getVolumeDetails(volumeName)
	if err != nil {
		return info, err
	}

	// List files in the volume
	files, err := listVolumeFiles(volumeName)
	if err != nil {
		// Don't fail the entire request if file listing fails
		fmt.Printf("Warning: Failed to list files in volume %s: %v\n", volumeName, err)
		info.Files = []string{fmt.Sprintf("Error listing files: %v", err)}
	} else {
		info.Files = files
	}

	return info, nil
}

// listVolumeFiles lists all files in a Docker volume using a temporary container
func listVolumeFiles(volumeName string) ([]string, error) {
	// Use a temporary alpine container to list files in the volume
	// We mount the volume to /volume in the container and run 'find /volume -type f'
	cmd := exec.Command("docker", "run", "--rm",
		"-v", fmt.Sprintf("%s:/volume", volumeName),
		"alpine:latest",
		"find", "/volume", "-type", "f", "-o", "-type", "d")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list volume files: %w - %s", err, string(output))
	}

	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		return []string{"(empty - no files)"}, nil
	}

	lines := strings.Split(outputStr, "\n")
	files := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "/volume" {
			continue
		}
		// Remove the /volume prefix to show relative paths
		relativePath := strings.TrimPrefix(line, "/volume/")
		if relativePath == "" {
			continue
		}
		files = append(files, relativePath)
	}

	if len(files) == 0 {
		return []string{"(empty - no files)"}, nil
	}

	return files, nil
}
