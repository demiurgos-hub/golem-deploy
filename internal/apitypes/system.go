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
	RegistrationEnabled bool `json:"registration_enabled"`
}

// UpdateSettingsRequest is the body of PATCH /api/v1/settings. Absent fields
// are left unchanged.
type UpdateSettingsRequest struct {
	RegistrationEnabled *bool `json:"registration_enabled,omitempty"`
}
