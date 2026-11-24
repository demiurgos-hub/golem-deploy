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
// switching_branch are transient (a job is active). error means the last
// clone failed (no checkout) or an operation failed after modifying the
// checkout; a pull repairs it. A daemon restart turns interrupted transient
// statuses into error.
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
	// StaticAddons are extracted into the game root when the project is
	// built, in order (never null).
	StaticAddons []StaticAddon `json:"static_addons"`
	// AllowCustomDockerfile lets builds use the Dockerfile the repository
	// names in golem-deploy.yaml instead of the generated one. Default
	// false: a custom Dockerfile shares the build cache and builder with
	// other projects, so an admin must trust the repository.
	AllowCustomDockerfile bool      `json:"allow_custom_dockerfile"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// StaticAddon is a ZIP file of the static store that a build extracts into
// the game. Name is the static file's name (it must end in .zip; a static
// file referenced by a project cannot be deleted). Target is a relative
// path below the game root where the archive's contents appear, in the
// build stage before the web build and at the same path below /app in the
// image: segments of letters, digits, '.', '_' and '-' separated by '/', no
// '..' or .git; "." or empty means the game root (stored as ".").
type StaticAddon struct {
	Name   string `json:"name"`
	Target string `json:"target"`
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
// to keep the stored secret, omit Auth; {"type":"none"} clears it. A new
// RepoURL is applied to the checkout by the next pull (no re-clone). Use
// POST .../branch to change branch. StaticAddons replaces the project's
// add-ons entirely (at most 16; [] removes them); every named file must
// exist in the static store and be a ZIP archive.
type UpdateProjectRequest struct {
	RepoURL      *string        `json:"repo_url,omitempty"`
	Auth         *ProjectAuth   `json:"auth,omitempty"`
	StaticAddons *[]StaticAddon `json:"static_addons,omitempty"`
	// AllowCustomDockerfile: see Project.
	AllowCustomDockerfile *bool `json:"allow_custom_dockerfile,omitempty"`
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
