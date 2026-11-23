package docker

import (
	"fmt"
	"os/exec"
	"strings"

	apiTypes "github.com/demiurgos-hub/golem-deploy/internal/api"
)

// ListDockerImages returns a list of all Docker images
func ListDockerImages() ([]apiTypes.DockerImage, error) {
	// Use --format to get parseable output
	cmd := exec.Command("docker", "images", "--format", "{{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list images: %w", err)
	}

	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		// No images found
		return []apiTypes.DockerImage{}, nil
	}

	lines := strings.Split(outputStr, "\n")
	images := make([]apiTypes.DockerImage, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		// Pad with empty strings if needed
		if len(fields) < 5 {
			for len(fields) < 5 {
				fields = append(fields, "")
			}
		}
		images = append(images, apiTypes.DockerImage{
			Repository: fields[0],
			Tag:        fields[1],
			ID:         fields[2],
			Created:    fields[3],
			Size:       fields[4],
		})
	}

	return images, nil
}

// DeleteImages removes docker images by ID
func DeleteImages(imageIDs []string) error {
	// Ensure Docker is installed
	if !CheckDockerInstalled() {
		return fmt.Errorf("docker is not installed")
	}

	if len(imageIDs) == 0 {
		return fmt.Errorf("no image IDs provided")
	}

	// First, verify all images exist
	existingImages, err := ListDockerImages()
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	// Create a map of existing image IDs for quick lookup
	existingIDs := make(map[string]bool)
	for _, img := range existingImages {
		existingIDs[img.ID] = true
	}

	// Check which images don't exist
	var missingIDs []string
	for _, id := range imageIDs {
		if !existingIDs[id] {
			missingIDs = append(missingIDs, id)
		}
	}

	if len(missingIDs) > 0 {
		return fmt.Errorf("image(s) not found: %s", strings.Join(missingIDs, ", "))
	}

	fmt.Printf("Deleting %d image(s)...\n", len(imageIDs))

	// Build docker rmi command with all image IDs
	args := append([]string{"rmi", "-f"}, imageIDs...)
	cmd := exec.Command("docker", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete images: %w - %s", err, string(output))
	}

	fmt.Printf("Successfully deleted %d image(s)\n", len(imageIDs))
	return nil
}
