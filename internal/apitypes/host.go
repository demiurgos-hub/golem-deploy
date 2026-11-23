package apitypes

import "time"

// Image is a Docker image managed (built and labelled) by Golem Deploy.
type Image struct {
	// ID is the image id ("sha256:..."); use it with DELETE /api/v1/images/{id}.
	ID        string    `json:"id"`
	Tags      []string  `json:"tags"`
	ProjectID *int64    `json:"project_id" tstype:"number | null,required"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	// InUse reports whether any server's container uses the image; such images
	// cannot be deleted (409 conflict).
	InUse bool `json:"in_use"`
}

// ImageList is the body of GET /api/v1/images.
type ImageList struct {
	Items []Image `json:"items"`
}

// StaticFile is a file in the static store, made available to builds.
// Name: a single path segment of 1 to 128 characters from [A-Za-z0-9._-],
// not starting with a dot.
type StaticFile struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	ModifiedAt time.Time `json:"modified_at"`
}

// StaticFileList is the body of GET /api/v1/static and the 201 response of
// POST /api/v1/static (the files that were stored).
type StaticFileList struct {
	Items []StaticFile `json:"items"`
}

// Volume is a Docker volume managed by Golem Deploy (a server's data volume).
type Volume struct {
	Name string `json:"name"`
	// ServerID is the owning server; null when that server was deleted
	// (an orphaned volume).
	ServerID   *int64    `json:"server_id" tstype:"number | null,required"`
	ServerName string    `json:"server_name"`
	CreatedAt  time.Time `json:"created_at"`
	// InUse reports whether a container currently mounts the volume; such
	// volumes cannot be deleted (409 conflict).
	InUse bool `json:"in_use"`
}

// VolumeList is the body of GET /api/v1/volumes.
type VolumeList struct {
	Items []Volume `json:"items"`
}
