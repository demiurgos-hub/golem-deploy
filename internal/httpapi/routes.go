package httpapi

import "net/http"

// routes is the single place where API routes and their access levels are
// declared. Paths are relative to /api/v1. Handlers never re-check roles.
func (s *Server) routes() []route {
	ni := func(http.ResponseWriter, *http.Request) error { return errNotImplemented }
	return []route{
		// Auth.
		{method: "POST", path: "/auth/login", access: AccessPublic, handler: s.handleLogin},
		{method: "POST", path: "/auth/register", access: AccessPublic, handler: s.handleRegister},
		{method: "POST", path: "/auth/logout", access: AccessSession, handler: s.handleLogout},
		{method: "POST", path: "/auth/refresh", access: AccessSession, handler: s.handleRefresh},

		// System.
		{method: "GET", path: "/status", access: AccessDeveloper, handler: s.handleStatus},
		{method: "GET", path: "/settings", access: AccessAdmin, handler: s.handleGetSettings},
		{method: "PATCH", path: "/settings", access: AccessAdmin, handler: s.handleUpdateSettings},

		// Own account.
		{method: "GET", path: "/account", access: AccessSession, handler: s.handleMe},
		{method: "POST", path: "/account/password", access: AccessSession, handler: s.handleChangePassword},
		{method: "GET", path: "/account/tokens", access: AccessSession, handler: s.handleListTokens},
		{method: "POST", path: "/account/tokens", access: AccessSession, handler: s.handleCreateToken},
		{method: "DELETE", path: "/account/tokens/{id}", access: AccessSession, handler: s.handleDeleteToken},

		// Account administration.
		{method: "GET", path: "/accounts", access: AccessAdmin, handler: s.handleListAccounts},
		{method: "POST", path: "/accounts", access: AccessAdmin, handler: s.handleCreateAccount},
		{method: "PATCH", path: "/accounts/{id}", access: AccessAdmin, handler: s.handleUpdateAccount},
		{method: "DELETE", path: "/accounts/{id}", access: AccessAdmin, handler: s.handleDeleteAccount},

		// Jobs.
		{method: "GET", path: "/jobs", access: AccessDeveloper, handler: s.handleListJobs},
		{method: "GET", path: "/jobs/{id}", access: AccessDeveloper, handler: s.handleGetJob},
		{method: "POST", path: "/jobs/{id}/cancel", access: AccessDeveloper, handler: s.handleCancelJob},
		{method: "GET", path: "/jobs/{id}/log", access: AccessDeveloper, handler: s.handleJobLog},

		// Projects.
		{method: "GET", path: "/projects", access: AccessDeveloper, handler: s.handleListProjects},
		{method: "POST", path: "/projects", access: AccessAdmin, handler: s.handleCreateProject},
		{method: "GET", path: "/projects/{id}", access: AccessDeveloper, handler: s.handleGetProject},
		{method: "PATCH", path: "/projects/{id}", access: AccessAdmin, handler: s.handleUpdateProject},
		{method: "DELETE", path: "/projects/{id}", access: AccessAdmin, handler: s.handleDeleteProject},
		{method: "POST", path: "/projects/{id}/pull", access: AccessDeveloper, handler: s.handlePullProject},
		{method: "POST", path: "/projects/{id}/branch", access: AccessDeveloper, handler: s.handleSwitchBranch},
		{method: "GET", path: "/projects/{id}/branches", access: AccessDeveloper, handler: s.handleListBranches},

		// Servers (task C).
		{method: "GET", path: "/servers", access: AccessDeveloper, handler: ni},
		{method: "POST", path: "/servers", access: AccessDeveloper, handler: ni},
		{method: "GET", path: "/servers/export", access: AccessAdmin, handler: ni},
		{method: "POST", path: "/servers/import", access: AccessAdmin, handler: ni},
		{method: "GET", path: "/servers/{id}", access: AccessDeveloper, handler: ni},
		{method: "PATCH", path: "/servers/{id}", access: AccessDeveloper, handler: ni},
		{method: "DELETE", path: "/servers/{id}", access: AccessDeveloper, handler: ni},
		{method: "POST", path: "/servers/{id}/start", access: AccessDeveloper, handler: ni},
		{method: "POST", path: "/servers/{id}/stop", access: AccessDeveloper, handler: ni},
		{method: "POST", path: "/servers/{id}/restart", access: AccessDeveloper, handler: ni},
		{method: "GET", path: "/servers/{id}/logs", access: AccessDeveloper, handler: ni},
		{method: "GET", path: "/servers/{id}/stats", access: AccessDeveloper, handler: ni},

		// Images (task C).
		{method: "GET", path: "/images", access: AccessDeveloper, handler: ni},
		{method: "DELETE", path: "/images/{id}", access: AccessAdmin, handler: ni},

		// Static files (task C).
		{method: "GET", path: "/static", access: AccessDeveloper, handler: ni},
		{method: "POST", path: "/static", access: AccessDeveloper, handler: ni, maxBody: StaticUploadLimit},
		{method: "GET", path: "/static/{name}", access: AccessDeveloper, handler: ni},
		{method: "DELETE", path: "/static/{name}", access: AccessDeveloper, handler: ni},

		// Volumes (task C).
		{method: "GET", path: "/volumes", access: AccessDeveloper, handler: ni},
		{method: "DELETE", path: "/volumes/{name}", access: AccessAdmin, handler: ni},
	}
}
