package volumes

import "time"

// VolumeInfo describes a managed Docker volume.
type VolumeInfo struct {
	Name          string    `json:"name"`
	Driver        string    `json:"driver"`
	ContainerName string    `json:"container_name"`
	CreatedAt     time.Time `json:"created_at"`
	Mountpoint    string    `json:"mountpoint"`
	Files         []string  `json:"files"`
}
