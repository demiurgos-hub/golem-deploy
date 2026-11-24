package apitypes

// Status is the body of GET /api/v1/status.
type Status struct {
	Version string `json:"version"`
	// DockerReachable reports whether the daemon could talk to Docker recently
	// (cached for a few seconds).
	DockerReachable     bool         `json:"docker_reachable"`
	RegistrationEnabled bool         `json:"registration_enabled"`
	Counts              StatusCounts `json:"counts"`
}

// StatusCounts are summary counters for the dashboard.
type StatusCounts struct {
	Projects int `json:"projects"`
	Servers  int `json:"servers"`
	// ActiveJobs counts queued and running jobs.
	ActiveJobs int `json:"active_jobs"`
	// PendingAccounts counts accounts awaiting approval.
	PendingAccounts int `json:"pending_accounts"`
}

// Settings is the body of GET and PATCH /api/v1/settings (admin).
type Settings struct {
	// RegistrationEnabled allows POST /api/v1/auth/register. Default false.
	// While it is on, anyone can learn whether a username exists (register
	// answers username_taken); login and its rate limits do not reveal it.
	RegistrationEnabled bool `json:"registration_enabled"`
	// GameHost is the DNS name or IP address players use to reach game
	// servers ("" when not set). EffectiveGameHost is what servers use: the
	// game host, or the first --public-host when it is empty, or "" when
	// neither exists (servers cannot start then).
	GameHost          string `json:"game_host"`
	EffectiveGameHost string `json:"effective_game_host"`
}

// UpdateSettingsRequest is the body of PATCH /api/v1/settings. Absent fields
// are left unchanged. GameHost must be a DNS name or an IP address (no
// scheme, port or path); "" clears it. Changing it regenerates a
// self-signed game certificate and marks running servers restart_required.
type UpdateSettingsRequest struct {
	RegistrationEnabled *bool   `json:"registration_enabled,omitempty"`
	GameHost            *string `json:"game_host,omitempty"`
}
