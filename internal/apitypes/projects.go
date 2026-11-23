package apitypes

import "time"

// ProjectAuthType selects how the daemon authenticates to a project's Git
// remote.
type ProjectAuthType string

// Project auth types.
const (
	ProjectAuthTypeNone  ProjectAuthType = "none"
	ProjectAuthTypeHTTPS ProjectAuthType = "https"
	ProjectAuthTypeSSH   ProjectAuthType = "ssh"
)

// ProjectStatus is the checkout state of a project. cloning, pulling and
// switching_branch are transient (a job is active); the daemon resets them to
// idle at startup.
type ProjectStatus string

// Project statuses.
const (
	ProjectStatusIdle      ProjectStatus = "idle"
	ProjectStatusCloning   ProjectStatus = "cloning"
	ProjectStatusPulling   ProjectStatus = "pulling"
	ProjectStatusSwitching ProjectStatus = "switching_branch"
	ProjectStatusError     ProjectStatus = "error"
)

// ProjectAuth is write-only Git credential input, a union selected by Type:
//   - "none": no other field.
//   - "https": Username and Password (a password or access token).
//   - "ssh": PrivateKey (an unencrypted OpenSSH/PEM private key).
//
// Fields that do not belong to the selected type are rejected. Secrets are
// encrypted at rest and never returned.
type ProjectAuth struct {
	Type       ProjectAuthType `json:"type"`
	Username   string          `json:"username,omitempty"`
	Password   string          `json:"password,omitempty"`
	PrivateKey string          `json:"private_key,omitempty"`
}

// Project is a Git repository the daemon clones and builds. It never contains
// credentials: only AuthType, AuthUsername and whether a secret is stored.
type Project struct {
	ID int64 `json:"id"`
	// Name: ^[a-z0-9][a-z0-9-]{0,62}$, unique, immutable.
	Name         string          `json:"name"`
	RepoURL      string          `json:"repo_url"`
	Branch       string          `json:"branch"`
	AuthType     ProjectAuthType `json:"auth_type"`
	AuthUsername string          `json:"auth_username"`
	HasSecret    bool            `json:"has_secret"`
	// LastCommit is the full commit hash of the checkout; empty before the
	// first successful clone.
	LastCommit   string        `json:"last_commit"`
	LastPulledAt *time.Time    `json:"last_pulled_at" tstype:"string | null,required"`
	Status       ProjectStatus `json:"status"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// ProjectList is the body of GET /api/v1/projects.
type ProjectList struct {
	Items []Project `json:"items"`
}

// CreateProjectRequest is the body of POST /api/v1/projects (admin).
// RepoURL must be https:// or ssh (ssh:// or scp-style user@host:path); local
// paths and other schemes are rejected. Branch defaults to the remote's
// default branch when empty.
type CreateProjectRequest struct {
	Name    string      `json:"name"`
	RepoURL string      `json:"repo_url"`
	Branch  string      `json:"branch,omitempty"`
	Auth    ProjectAuth `json:"auth"`
}

// CreateProjectResponse is returned (201) by POST /api/v1/projects: the
// stored project and the clone job it started.
type CreateProjectResponse struct {
	Project Project `json:"project"`
	Job     Job     `json:"job"`
}

// UpdateProjectRequest is the body of PATCH /api/v1/projects/{id} (admin).
// Absent fields are unchanged. Auth replaces the stored credentials entirely;
// to keep the stored secret, omit Auth. Use POST .../branch to change branch.
type UpdateProjectRequest struct {
	RepoURL *string      `json:"repo_url,omitempty"`
	Auth    *ProjectAuth `json:"auth,omitempty"`
}

// SwitchBranchRequest is the body of POST /api/v1/projects/{id}/branch.
type SwitchBranchRequest struct {
	Branch string `json:"branch"`
}

// BranchList is the body of GET /api/v1/projects/{id}/branches: remote
// branches of the checkout, sorted, and the current one.
type BranchList struct {
	Current string   `json:"current"`
	Items   []string `json:"items"`
}
