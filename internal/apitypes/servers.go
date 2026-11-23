package apitypes

import "time"

// DesiredState is what the operator asked a server to be.
type DesiredState string

// Desired states.
const (
	DesiredStateRunning DesiredState = "running"
	DesiredStateStopped DesiredState = "stopped"
)

// ServerState is the observed state of a server's container.
type ServerState string

// Server states. "missing" means no container exists for the server;
// "unknown" means Docker could not be queried.
const (
	ServerStateCreated    ServerState = "created"
	ServerStateRunning    ServerState = "running"
	ServerStateRestarting ServerState = "restarting"
	ServerStateExited     ServerState = "exited"
	ServerStatePaused     ServerState = "paused"
	ServerStateDead       ServerState = "dead"
	ServerStateMissing    ServerState = "missing"
	ServerStateUnknown    ServerState = "unknown"
)

// Server is a game server: a container run from an image with a host port
// published as both <port>/udp and <port>/tcp.
//
// Environment values are treated as secrets: they are encrypted at rest and
// never returned. EnvKeys lists the variable names so the UI can show them;
// to change values use UpdateServerRequest.EnvSet / EnvUnset.
type Server struct {
	ID int64 `json:"id"`
	// Name: ^[a-z0-9][a-z0-9-]{0,62}$, unique, immutable.
	Name string `json:"name"`
	// ProjectID is the project the image was built from; null for servers
	// using an image not built by Golem Deploy, or after that project was
	// deleted. Immutable.
	ProjectID    *int64       `json:"project_id" tstype:"number | null,required"`
	Image        string       `json:"image"`
	Port         int          `json:"port"`
	EnvKeys      []string     `json:"env_keys"`
	DesiredState DesiredState `json:"desired_state"`
	// Runtime fields, read from Docker on each request.
	State       ServerState `json:"state"`
	ContainerID string      `json:"container_id"`
	StartedAt   *time.Time  `json:"started_at" tstype:"string | null,required"`
	ExitCode    *int        `json:"exit_code" tstype:"number | null,required"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// ServerList is the body of GET /api/v1/servers.
type ServerList struct {
	Items []Server `json:"items"`
}

// CreateServerRequest is the body of POST /api/v1/servers. Port, when absent,
// is auto-assigned from the configured port range. Env keys must match
// ^[A-Za-z_][A-Za-z0-9_]*$. DesiredState defaults to "stopped"; "running"
// starts the server right after creation.
type CreateServerRequest struct {
	Name         string            `json:"name"`
	ProjectID    *int64            `json:"project_id,omitempty"`
	Image        string            `json:"image"`
	Port         *int              `json:"port,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	DesiredState *DesiredState     `json:"desired_state,omitempty"`
}

// UpdateServerRequest is the body of PATCH /api/v1/servers/{id}. Absent fields
// are unchanged. EnvSet adds or overwrites variables, EnvUnset removes them
// (a key may not appear in both); unmentioned variables keep their stored
// values, so the UI never has to re-send secrets. Changes apply on the next
// (re)start.
type UpdateServerRequest struct {
	Image    *string           `json:"image,omitempty"`
	Port     *int              `json:"port,omitempty"`
	EnvSet   map[string]string `json:"env_set,omitempty"`
	EnvUnset []string          `json:"env_unset,omitempty"`
}

// ServerStats is the body of GET /api/v1/servers/{id}/stats: a single sample
// of the running container's resource usage.
type ServerStats struct {
	CPUPercent       float64   `json:"cpu_percent"`
	MemoryBytes      int64     `json:"memory_bytes"`
	MemoryLimitBytes int64     `json:"memory_limit_bytes"`
	NetworkRxBytes   int64     `json:"network_rx_bytes"`
	NetworkTxBytes   int64     `json:"network_tx_bytes"`
	PIDs             int       `json:"pids"`
	SampledAt        time.Time `json:"sampled_at"`
}

// ServerSpec is the portable definition of one server in an export.
// Project is referenced by name because ids differ between hosts.
type ServerSpec struct {
	Name         string            `json:"name"`
	Project      string            `json:"project,omitempty"`
	Image        string            `json:"image"`
	Port         *int              `json:"port,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	DesiredState DesiredState      `json:"desired_state"`
}

// ServerExport is the body of GET /api/v1/servers/export and of
// POST /api/v1/servers/import (both admin; the export contains env values in
// plain text).
type ServerExport struct {
	// FormatVersion is 1.
	FormatVersion int          `json:"format_version"`
	Servers       []ServerSpec `json:"servers"`
}

// ServerImportResult is the response of POST /api/v1/servers/import. Import is
// all-or-nothing: on any validation error or name/port conflict nothing is
// created and an ErrorResponse is returned instead.
type ServerImportResult struct {
	Created []Server `json:"created"`
}
