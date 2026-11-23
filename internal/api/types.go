package api

import "time"

// LoginRequest represents login credentials
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResponse represents the login response
type LoginResponse struct {
	Status  string `json:"status"`
	Token   string `json:"token,omitempty"`
	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// RefreshResponse represents the token refresh response
type RefreshResponse struct {
	Status string `json:"status"`
	Token  string `json:"token,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// APIToken is a stored API token without the plaintext secret.
type APIToken struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"created_at"`
}

// CreateAPITokenRequest is the body for POST /account/tokens.
type CreateAPITokenRequest struct {
	Name string `json:"name"`
}

// CreateAPITokenResponse returns the plaintext secret once on create.
type CreateAPITokenResponse struct {
	Status    string `json:"status"`
	Token     string `json:"token,omitempty"`
	ID        int    `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// ListAPITokensResponse is the body for GET /account/tokens.
type ListAPITokensResponse struct {
	Status string     `json:"status"`
	Tokens []APIToken `json:"tokens"`
	Detail string     `json:"detail,omitempty"`
}

// RevokeAPITokenRequest is the body for POST /account/tokens/revoke.
type RevokeAPITokenRequest struct {
	ID int `json:"id"`
}

// RepoJob is a pollable pull or branch job.
type RepoJob struct {
	ID         int    `json:"id"`
	RepoID     int    `json:"repo_id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// RepoJobResponse is the body for GET /repo/job.
type RepoJobResponse struct {
	Status string  `json:"status"`
	Job    RepoJob `json:"job,omitempty"`
	Detail string  `json:"detail,omitempty"`
}

// StaticFile represents a static file
type StaticFile struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Extension string `json:"extension"`
}

// StaticAddon describes a static ZIP archive from the static store that is
// extracted into a Docker build context at a relative destination.
type StaticAddon struct {
	Filename  string `json:"filename"`
	ExtractTo string `json:"extract_to"`
}

// VolumeInfo represents information about a Docker volume
type VolumeInfo struct {
	Name          string    `json:"name"`
	Driver        string    `json:"driver"`
	ContainerName string    `json:"container_name"`
	CreatedAt     time.Time `json:"created_at"`
	Mountpoint    string    `json:"mountpoint"`
	Files         []string  `json:"files"`
}

// DockerImage represents a Docker image
type DockerImage struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
	Created    string `json:"created"`
}

// RunContainerRequest represents a request to run a container
type RunContainerRequest struct {
	ImageName     string            `json:"image_name,omitempty"`
	ContainerName string            `json:"container_name,omitempty"`
	Port          string            `json:"port,omitempty"`
	EnvVars       map[string]string `json:"env_vars,omitempty"` // Additional environment variables
	Label         string            `json:"label,omitempty"`    // Container label for organization
}

// MassRunConfig represents a saved container run configuration
type MassRunConfig struct {
	ID            int               `json:"id"`
	Name          string            `json:"name"`
	ContainerName string            `json:"container_name"`
	Port          string            `json:"port"`
	Label         string            `json:"label"`
	EnvVars       map[string]string `json:"env_vars"`
	CreatedAt     string            `json:"created_at"`
}

// AddMassRunRequest represents a request to add a mass run configuration
type AddMassRunRequest struct {
	Name          string            `json:"name"`
	ContainerName string            `json:"container_name"`
	Port          string            `json:"port"`
	Label         string            `json:"label"`
	EnvVars       map[string]string `json:"env_vars"`
}

// EditMassRunRequest represents a request to edit a mass run configuration
type EditMassRunRequest struct {
	Name          string            `json:"name"` // Original name to identify which config to edit
	ContainerName string            `json:"container_name"`
	Port          string            `json:"port"`
	Label         string            `json:"label"`
	EnvVars       map[string]string `json:"env_vars"`
}

// DeleteMassRunRequest represents a request to delete a mass run configuration
type DeleteMassRunRequest struct {
	Name  string   `json:"name,omitempty"`
	Names []string `json:"names,omitempty"`
}

// ReplaceMassRunRequest represents a request to replace all mass run configurations
type ReplaceMassRunRequest struct {
	Configs []AddMassRunRequest `json:"configs"`
}

// ReplaceMassRunResponse represents the response from replacing mass run configurations
type ReplaceMassRunResponse struct {
	Status        string `json:"status"`
	Message       string `json:"message,omitempty"`
	ReplacedCount int    `json:"replaced_count"`
}

// ExecuteMassRunRequest starts every saved preset for an image.
type ExecuteMassRunRequest struct {
	ImageName string `json:"image_name"`
}

// MassRunExecuteFailure is one preset that failed during execute.
type MassRunExecuteFailure struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// ExecuteMassRunResponse is the result of POST /massrun/execute.
type ExecuteMassRunResponse struct {
	Status  string                  `json:"status"`
	Started int                     `json:"started"`
	Failed  []MassRunExecuteFailure `json:"failed"`
}
