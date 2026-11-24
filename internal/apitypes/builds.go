package apitypes

import "time"

// BuildStatus is the lifecycle state of a build. queued and running are
// active; the others are terminal.
type BuildStatus string

// Build statuses.
const (
	BuildStatusQueued    BuildStatus = "queued"
	BuildStatusRunning   BuildStatus = "running"
	BuildStatusSuccess   BuildStatus = "success"
	BuildStatusError     BuildStatus = "error"
	BuildStatusCancelled BuildStatus = "cancelled"
)

// Build is one build of a project's checkout into a managed image. Servers
// run the image of a successful build. The build's output is the log of the
// job JobID.
type Build struct {
	ID        int64 `json:"id"`
	ProjectID int64 `json:"project_id"`
	// Branch and Commit (full hash) of the checkout that was built; Commit
	// is empty while the build is queued.
	Branch string      `json:"branch"`
	Commit string      `json:"commit"`
	Status BuildStatus `json:"status"`
	// Detail is a short, user-safe outcome message for terminal builds
	// (empty on success).
	Detail string `json:"detail"`
	// JobID is the project.build job; null when that job record is gone.
	JobID *int64 `json:"job_id" tstype:"number | null,required"`
	// ImageID ("sha256:...") and ImageTag are set on success.
	ImageID  string `json:"image_id"`
	ImageTag string `json:"image_tag"`
	// EngineVersion is the Golem Engine version the game requires.
	EngineVersion string `json:"engine_version"`
	// SchemaFingerprint is the game's schema fingerprint: read from the
	// repository's committed generated code when the build has it;
	// otherwise the first value a game running this build reports once its
	// server has become ready is filled in. A recorded value is never
	// replaced. Empty while neither is known.
	SchemaFingerprint string `json:"schema_fingerprint"`
	// WebTransportPath and WebSocketPath are the endpoint paths the game
	// serves its transports on (transport in golem-deploy.yaml, defaults
	// "/wt" and "/api/ws"); servers running the build publish
	// https://<game host>:<port><webtransport_path> and
	// wss://<game host>:<port><websocket_path>. Empty unless the build
	// succeeded.
	WebTransportPath string `json:"webtransport_path"`
	WebSocketPath    string `json:"websocket_path"`
	// ServerIDs lists the servers configured with this build; a build that
	// is in use cannot be deleted (409 conflict).
	ServerIDs  []int64    `json:"server_ids"`
	CreatedBy  *int64     `json:"created_by" tstype:"number | null,required"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at" tstype:"string | null,required"`
	FinishedAt *time.Time `json:"finished_at" tstype:"string | null,required"`
}

// BuildList is the body of GET /api/v1/projects/{id}/builds and of
// GET /api/v1/builds, newest first.
type BuildList struct {
	Items []Build `json:"items"`
}

// CreateBuildRequest is the body of POST /api/v1/projects/{id}/builds. Pull
// first updates the checkout from the remote (like POST .../pull); without
// it the current checkout is built.
type CreateBuildRequest struct {
	Pull bool `json:"pull,omitempty"`
}

// CreateBuildResponse is returned (202) by POST /api/v1/projects/{id}/builds:
// the queued build and the job that runs it.
type CreateBuildResponse struct {
	Build Build `json:"build"`
	Job   Job   `json:"job"`
}
