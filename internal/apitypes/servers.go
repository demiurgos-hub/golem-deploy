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

// Server states. "missing" means the server has no container (none was
// recorded for it, or the recorded one is gone); "unknown" means it has one
// but Docker could not be queried.
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

// Server is a game server: a container run from the image of a successful
// build, with a host port published as both <port>/udp and <port>/tcp.
//
// Environment values are treated as secrets: they are encrypted at rest and
// never returned. EnvKeys lists the variable names so the UI can show them;
// to change values use UpdateServerRequest.EnvSet / EnvUnset.
type Server struct {
	ID int64 `json:"id"`
	// Name: ^[a-z0-9][a-z0-9-]{0,62}$, unique, immutable.
	Name string `json:"name"`
	// BuildID is the build the server runs; null for servers created before
	// servers ran builds (they cannot start until a build is assigned).
	BuildID *int64 `json:"build_id" tstype:"number | null,required"`
	// ProjectID, Image, Commit and EngineVersion are derived from the build
	// (null/empty without one): its project, its immutable image id
	// ("sha256:..."), the commit that was built and the Golem Engine version
	// the game requires.
	ProjectID     *int64 `json:"project_id" tstype:"number | null,required"`
	Image         string `json:"image"`
	Commit        string `json:"commit"`
	EngineVersion string `json:"engine_version"`
	Port          int    `json:"port"`
	// PublicURL is the server's own page, https://<game host>:<port>/ (the
	// game's HTTPS sidecar); empty while no game host is configured.
	PublicURL string `json:"public_url"`
	// AllowedOrigins are extra browser origins allowed to open WebTransport
	// and WebSocket connections, besides the server's own origin.
	AllowedOrigins []string     `json:"allowed_origins"`
	EnvKeys        []string     `json:"env_keys"`
	DesiredState   DesiredState `json:"desired_state"`
	// Runtime fields, read from Docker on each request. ContainerID is the
	// container the daemon created for this server; other containers are
	// never reported, even when they carry golem-deploy labels.
	State       ServerState `json:"state"`
	ContainerID string      `json:"container_id"`
	StartedAt   *time.Time  `json:"started_at" tstype:"string | null,required"`
	ExitCode    *int        `json:"exit_code" tstype:"number | null,required"`
	// RestartRequired is true when the container is running with an older
	// definition (build, port, env, allowed origins, game host, certificate
	// mode, Inspector settings or daemon run settings changed since it was
	// started). Changes
	// never restart a server implicitly. A renewed certificate is picked up
	// without a restart.
	RestartRequired bool `json:"restart_required"`
	// Certificate is the TLS certificate the server serves: its own
	// self-signed one, or the shared custom one. Null when it has none yet
	// (a self-signed certificate is created on the first start).
	Certificate *CertificateInfo `json:"certificate" tstype:"CertificateInfo | null,required"`
	// Data describes the server's saved data and whether the assigned
	// build can load it.
	Data ServerData `json:"data"`
	// SnapshotIntervalSeconds is how often the running game saves its
	// entity snapshot on its own (0: only when it stops and for backups).
	// Default 300.
	SnapshotIntervalSeconds int `json:"snapshot_interval_seconds"`
	// BackupSchedule is the server's automatic backup setting (off by
	// default: interval_hours 0, keep 7).
	BackupSchedule BackupSchedule `json:"backup_schedule"`
	// Inspector is the server's Inspector setting: when it is on and the
	// installation's Inspector is turned on (GET /api/v1/inspector), the
	// game's Inspector agent is pointed at it from the next (re)start, and
	// the server appears in the Inspector under its name. Off by default.
	// InspectorActive is true when the server's current container was
	// started that way (false without a container, and while Docker cannot
	// be queried). The agent variables follow the desired configuration,
	// not whether the hub runs at that moment: a server started while the
	// hub is down still gets them, and its agent connects once the hub is
	// repaired.
	Inspector       bool `json:"inspector"`
	InspectorActive bool `json:"inspector_active"`
	// Profiling is the server's Profiling setting: when it is on, the game's
	// admin listener serves Go profiles from the next (re)start
	// (GOLEM_ADMIN_PROFILING=1), and GET /api/v1/servers/{id}/profile relays
	// them. On by default. ProfilingActive is true when the server's current
	// container was started that way (false without a container, and while
	// Docker cannot be queried).
	Profiling       bool      `json:"profiling"`
	ProfilingActive bool      `json:"profiling_active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ServerList is the body of GET /api/v1/servers.
type ServerList struct {
	Items []Server `json:"items"`
}

// CreateServerRequest is the body of POST /api/v1/servers. BuildID is
// required and must be a successful build. Port, when absent, is
// auto-assigned from the configured port range. AllowedOrigins are extra
// browser origins ("https://play.example.com", scheme http or https, a DNS
// name or IP address and an optional port; at most 16). Env keys must match
// ^[A-Za-z_][A-Za-z0-9_]{0,127}$, be unique ignoring case and not start with
// GOLEM_ (reserved for the daemon); values are single-line UTF-8 (no NUL, CR
// or LF); at most 32 KiB per variable, 128 variables and 64 KiB in total. A
// rejected value is reported with field "env.<KEY>". DesiredState defaults
// to "stopped"; "running" starts a server.start job right after creation.
// SnapshotIntervalSeconds is 0 or 10 to 86400 (default 300); BackupSchedule
// defaults to off (see BackupSchedule for its ranges). Inspector defaults to
// false, Profiling to true.
type CreateServerRequest struct {
	Name                    string            `json:"name"`
	BuildID                 int64             `json:"build_id"`
	Port                    *int              `json:"port,omitempty"`
	AllowedOrigins          []string          `json:"allowed_origins,omitempty"`
	Env                     map[string]string `json:"env,omitempty"`
	DesiredState            *DesiredState     `json:"desired_state,omitempty"`
	SnapshotIntervalSeconds *int              `json:"snapshot_interval_seconds,omitempty"`
	BackupSchedule          *BackupSchedule   `json:"backup_schedule,omitempty"`
	Inspector               *bool             `json:"inspector,omitempty"`
	Profiling               *bool             `json:"profiling,omitempty"`
}

// UpdateServerRequest is the body of PATCH /api/v1/servers/{id}. Absent fields
// are unchanged. BuildID switches the server to another successful build.
// AllowedOrigins replaces the list (send [] to clear it). EnvSet adds or
// overwrites variables, EnvUnset removes them (a key may not appear in
// both); unmentioned variables keep their stored values, so the UI never has
// to re-send secrets. Changes apply on the next (re)start. Changing Port
// while the server's container is running is refused with 409 conflict
// (stop the server first). SnapshotIntervalSeconds applies on the next
// (re)start; BackupSchedule applies at once and never needs a restart.
// Inspector and Profiling apply on the next (re)start.
type UpdateServerRequest struct {
	BuildID                 *int64            `json:"build_id,omitempty"`
	Port                    *int              `json:"port,omitempty"`
	AllowedOrigins          *[]string         `json:"allowed_origins,omitempty"`
	EnvSet                  map[string]string `json:"env_set,omitempty"`
	EnvUnset                []string          `json:"env_unset,omitempty"`
	SnapshotIntervalSeconds *int              `json:"snapshot_interval_seconds,omitempty"`
	BackupSchedule          *BackupSchedule   `json:"backup_schedule,omitempty"`
	Inspector               *bool             `json:"inspector,omitempty"`
	Profiling               *bool             `json:"profiling,omitempty"`
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

// ServerSpec is the portable definition of one server in an export. The
// build is referenced portably by project name and commit (ids differ
// between hosts): on import it resolves to the newest successful build of
// Project with that Commit, or to the newest successful build of Project
// when Commit is empty. DesiredState is validated but imported servers are
// always created stopped.
type ServerSpec struct {
	Name           string            `json:"name"`
	Project        string            `json:"project"`
	Commit         string            `json:"commit,omitempty"`
	Port           *int              `json:"port,omitempty"`
	AllowedOrigins []string          `json:"allowed_origins,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	DesiredState   DesiredState      `json:"desired_state"`
	// SnapshotIntervalSeconds and BackupSchedule are optional; an import
	// without them uses the defaults. Saved data and backups are not part
	// of an export.
	SnapshotIntervalSeconds *int            `json:"snapshot_interval_seconds,omitempty"`
	BackupSchedule          *BackupSchedule `json:"backup_schedule,omitempty"`
	// Inspector is the server's Inspector setting (false when absent).
	Inspector bool `json:"inspector,omitempty"`
	// Profiling is the server's Profiling setting (true when absent).
	Profiling *bool `json:"profiling,omitempty"`
}

// ServerExport is the body of GET /api/v1/servers/export and of
// POST /api/v1/servers/import (both admin; the export contains env values in
// plain text). One import accepts at most 128 servers and 64 MiB.
type ServerExport struct {
	// FormatVersion is 2.
	FormatVersion int          `json:"format_version"`
	Servers       []ServerSpec `json:"servers"`
}

// ServerImportResult is the response of POST /api/v1/servers/import. Import is
// all-or-nothing: on any validation error or name/port conflict nothing is
// created and an ErrorResponse is returned instead.
type ServerImportResult struct {
	Created []Server `json:"created"`
}

// ServerRuntime is the body of GET /api/v1/servers/{id}/runtime: what the
// running game reports through its admin endpoint. Every value comes from
// the game and is bounded and sanitised by the daemon.
type ServerRuntime struct {
	// Ready reports whether the game accepts players; NotReadyReason is the
	// game's reason otherwise (e.g. "starting", "stopping").
	Ready          bool   `json:"ready"`
	NotReadyReason string `json:"not_ready_reason"`
	// Sessions is the number of connected players.
	Sessions      int             `json:"sessions"`
	Entities      int             `json:"entities"`
	UptimeSeconds float64         `json:"uptime_seconds"`
	Tick          ServerTickStats `json:"tick"`
	GameName      string          `json:"game_name"`
	EngineVersion string          `json:"engine_version"`
	// Transport is "webtransport", "websocket" or "unknown".
	Transport         string `json:"transport"`
	SchemaFingerprint string `json:"schema_fingerprint"`
	// CertificateHashesPublished reports whether the game publishes its
	// certificate hashes for browser pinning (self-signed certificates).
	CertificateHashesPublished bool      `json:"certificate_hashes_published"`
	SampledAt                  time.Time `json:"sampled_at"`
}

// ServerTickStats are the game loop's tick statistics.
type ServerTickStats struct {
	// Tick is the current tick number.
	Tick int64 `json:"tick"`
	// Rate is the configured tick rate in ticks per second.
	Rate     int     `json:"rate"`
	BudgetMs float64 `json:"budget_ms"`
	LastMs   float64 `json:"last_ms"`
	MaxMs    float64 `json:"max_ms"`
	// Overruns counts ticks that took longer than the budget.
	Overruns int64 `json:"overruns"`
}

// ServerConnectivity is the body of GET /api/v1/servers/{id}/connectivity:
// what a browser needs to test the path players take to the server (the
// panel's connection test opens these URLs from the admin's browser and
// reports which step fails).
type ServerConnectivity struct {
	// Running reports whether the server's container is running.
	Running bool `json:"running"`
	// PageURL is the game's own page (https://<game host>:<port>/),
	// WebTransportURL the WebTransport endpoint (UDP) and WebSocketURL the
	// WebSocket fallback (TCP). All are "" without a game host or port.
	PageURL         string `json:"page_url"`
	WebTransportURL string `json:"webtransport_url"`
	WebSocketURL    string `json:"websocket_url"`
	// Pinned reports whether the server uses a self-signed certificate that
	// browsers accept only through WebTransport serverCertificateHashes
	// (Certificate.FingerprintSHA256); HTTPS and WebSocket to it cannot be
	// tested from a browser then.
	Pinned      bool             `json:"pinned"`
	Certificate *CertificateInfo `json:"certificate" tstype:"CertificateInfo | null,required"`
	// TestOrigins are the panel origins the daemon adds to the allowed
	// origins of every server so that this test can connect; the test only
	// works from a page at one of them. TestOriginsActive is false while the
	// running container was created without them (restart the server).
	TestOrigins       []string `json:"test_origins"`
	TestOriginsActive bool     `json:"test_origins_active"`
}

// ProfileKind is the kind query parameter of GET
// /api/v1/servers/{id}/profile: a CPU profile (?seconds 1 to 60, default
// 30) or an execution trace (?seconds 1 to 15, default 5), or the current
// heap, allocation or goroutine profile (no ?seconds).
type ProfileKind string

// Profile kinds.
const (
	ProfileKindCPU       ProfileKind = "cpu"
	ProfileKindTrace     ProfileKind = "trace"
	ProfileKindHeap      ProfileKind = "heap"
	ProfileKindAllocs    ProfileKind = "allocs"
	ProfileKindGoroutine ProfileKind = "goroutine"
)
