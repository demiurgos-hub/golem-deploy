package apitypes

import "time"

// Image is a Docker image managed (built and labelled) by Golem Deploy.
type Image struct {
	// ID is the image id ("sha256:..."); use it with DELETE /api/v1/images/{id}.
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	// ProjectID and Commit come from the image's build labels; null/empty for
	// images not built by the build pipeline.
	ProjectID *int64    `json:"project_id" tstype:"number | null,required"`
	Commit    string    `json:"commit"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	// ServerIDs lists the servers configured with this image.
	ServerIDs []int64 `json:"server_ids"`
	// InUse reports whether a server references the image or any container
	// was created from it; such images cannot be deleted (409 conflict).
	InUse bool `json:"in_use"`
}

// ImageList is the body of GET /api/v1/images.
type ImageList struct {
	Items []Image `json:"items"`
}

// StaticFile is a file in the static store, made available to builds.
// Name: a single path segment of 1 to 128 characters from [A-Za-z0-9._-],
// starting with a letter or digit, not ending with a dot and not a Windows
// reserved device name.
type StaticFile struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	// IsZip reports whether the file has a .zip name and ZIP signature (only
	// ZIP archives can be extracted as static add-ons).
	IsZip bool `json:"is_zip"`
}

// StaticFileList is the body of GET /api/v1/static and the 201 response of
// POST /api/v1/static (Items: the files that were stored). The usage fields
// describe the whole store after the request.
type StaticFileList struct {
	Items []StaticFile `json:"items"`
	// UsedBytes is the total size of all stored files.
	UsedBytes int64 `json:"used_bytes"`
	// QuotaBytes is the store's size quota; 0 means unlimited.
	QuotaBytes int64 `json:"quota_bytes"`
	// FileCount is the number of stored files; MaxFiles caps it.
	FileCount int `json:"file_count"`
	MaxFiles  int `json:"max_files"`
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
