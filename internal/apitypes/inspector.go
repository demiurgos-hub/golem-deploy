package apitypes

import "time"

// InspectorState is the observed state of the Inspector hub.
type InspectorState string

// Inspector states.
const (
	// InspectorStateOff: the Inspector is not turned on.
	InspectorStateOff InspectorState = "off"
	// InspectorStateRunning: the hub container runs and the Inspector
	// listener is open.
	InspectorStateRunning InspectorState = "running"
	// InspectorStateStopped: the Inspector is turned on but not serving (its
	// image could not be downloaded, the container is not running or the
	// listener could not be opened). Detail says why.
	InspectorStateStopped InspectorState = "stopped"
	// InspectorStateUnknown: Docker could not be queried.
	InspectorStateUnknown InspectorState = "unknown"
)

// Inspector is the body of GET /api/v1/inspector: the Golem Inspector of
// this installation. The Inspector shows the live entities of game servers.
// Its hub runs as a container the daemon manages; the daemon alone talks to
// it. Browsers reach it through the Inspector listener of the daemon, and
// the games' Inspector agents connect to that same listener from their
// containers.
//
// The hub's own secrets are never returned.
type Inspector struct {
	// Available is false when the daemon runs with "--inspector-listen off";
	// the Inspector cannot be turned on then.
	Available bool `json:"available"`
	// Enabled is the setting (PATCH /api/v1/inspector).
	Enabled bool           `json:"enabled"`
	State   InspectorState `json:"state"`
	// Detail is a short, user-safe reason while State is "stopped" (empty
	// otherwise).
	Detail string `json:"detail"`
	// Image is the configured image reference (--inspector-image) and
	// ImageID the id of the image the hub container was created from ("" when
	// there is no container).
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	// StartedAt is when the hub container started; null unless it runs.
	StartedAt *time.Time `json:"started_at" tstype:"string | null,required"`
	// Port is the TCP port of the Inspector listener (0 when not Available).
	// Browsers use it over HTTPS (plain HTTP when the panel itself is served
	// without TLS); the games' agents use it without TLS from the Docker
	// network. A host firewall has to allow this port.
	Port int `json:"port"`
	// PublicURL is --inspector-public-url ("https://inspector.example.com",
	// no trailing slash) when a reverse proxy fronts the listener; otherwise
	// "" and the Inspector is at the panel's own scheme and host name with
	// Port.
	PublicURL string `json:"public_url"`
	// AgentAddress is the host:port the games' agents connect to from their
	// containers; "" while it is not known (the Inspector is off or Docker's
	// network does not exist yet) or when containers cannot reach the
	// listener (AgentsReachable false).
	AgentAddress string `json:"agent_address"`
	// AgentsReachable is false when the listener is bound to an address
	// containers cannot connect to (a loopback address on Linux): the
	// Inspector opens, but no server appears in it.
	AgentsReachable bool `json:"agents_reachable"`
	// Targets are the servers whose agents the hub knows, by server name.
	// Null while the hub is not running or did not answer.
	Targets []InspectorTarget `json:"targets" tstype:"InspectorTarget[] | null,required"`
	// OtherTargets counts targets the hub knows that belong to no server of
	// this installation (0 when Targets is null).
	OtherTargets int `json:"other_targets"`
}

// InspectorTarget is a server as the Inspector hub sees it. Only values the
// daemon can bound are relayed; nothing here is text written by a game.
type InspectorTarget struct {
	ServerID int64 `json:"server_id"`
	// Connected reports whether the server's agent is connected to the hub
	// now. A target stays listed for a short while after its agent left.
	Connected bool `json:"connected"`
	// Entities is the number of entities the hub holds for the server; it is
	// 0 unless somebody is looking at the server in the Inspector.
	Entities int `json:"entities"`
	// ConnectedAt is when the agent connected; null when not Connected.
	ConnectedAt *time.Time `json:"connected_at" tstype:"string | null,required"`
}

// UpdateInspectorRequest is the body of PATCH /api/v1/inspector (admin). It
// answers 202 with a restricted "inspector.apply" job that brings the hub in
// line with the request: with the Inspector on, it downloads the image when
// the host does not have it, creates or replaces the hub container, waits
// until the hub is ready and opens the listener; with it off, it closes the
// listener and removes the container.
//
// Enabled absent leaves the setting as it is (the job then repairs or
// refreshes a hub that is on). PullImage downloads the image again even
// though the host has it (for a tag that moved). RotateSecrets replaces the
// hub's secrets and the tokens of every server: open Inspector sessions end
// and servers with the Inspector on report restart_required until they are
// restarted. PullImage and RotateSecrets are refused with 400 when the
// Inspector would be off.
type UpdateInspectorRequest struct {
	Enabled       *bool `json:"enabled,omitempty"`
	PullImage     bool  `json:"pull_image,omitempty"`
	RotateSecrets bool  `json:"rotate_secrets,omitempty"`
}

// CreateInspectorSessionRequest is the body of POST
// /api/v1/inspector/sessions (the body may be empty). ServerID opens the
// Inspector on that server; it must be a server with the Inspector on.
type CreateInspectorSessionRequest struct {
	ServerID *int64 `json:"server_id,omitempty"`
}

// InspectorSession is the response of POST /api/v1/inspector/sessions: a
// one-time entry link into the Inspector for the calling account. The route
// needs a login session (API tokens get 403 session_required) and answers
// 409 conflict while the Inspector is not running.
//
// Opening Path on the Inspector's address signs the browser in there and
// redirects to the Inspector itself. The Inspector's address is PublicURL
// when it is set, otherwise the panel's scheme and host name with Port. The
// link works once and until ExpiresAt (60 seconds). The Inspector session it
// starts ends after 8 hours, at sign-out there, when the account is deleted,
// loses its approval or changes its password, and when the daemon restarts.
type InspectorSession struct {
	// Path is an absolute path with a query ("/_golem-deploy/enter?...").
	Path      string    `json:"path"`
	Port      int       `json:"port"`
	PublicURL string    `json:"public_url"`
	ExpiresAt time.Time `json:"expires_at"`
}
