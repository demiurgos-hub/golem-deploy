package httpapi

// routes is the single place where API routes and their access levels are
// declared. Paths are relative to /api/v1. Handlers never re-check roles.
func (s *Server) routes() []route {
	return []route{
		// Auth.
		{method: "GET", path: "/auth/options", access: AccessPublic, handler: s.handleAuthOptions},
		{method: "POST", path: "/auth/login", access: AccessPublic, handler: s.handleLogin},
		{method: "POST", path: "/auth/register", access: AccessPublic, handler: s.handleRegister},
		{method: "POST", path: "/auth/logout", access: AccessSession, handler: s.handleLogout},
		{method: "POST", path: "/auth/refresh", access: AccessSession, handler: s.handleRefresh},

		// System.
		{method: "GET", path: "/status", access: AccessDeveloper, handler: s.handleStatus},
		{method: "GET", path: "/settings", access: AccessAdmin, handler: s.handleGetSettings},
		{method: "PATCH", path: "/settings", access: AccessAdmin, handler: s.handleUpdateSettings},

		// Own account.
		{method: "GET", path: "/account", access: AccessDeveloper, handler: s.handleMe},
		{method: "POST", path: "/account/password", access: AccessSession, handler: s.handleChangePassword},
		{method: "GET", path: "/account/tokens", access: AccessSession, handler: s.handleListTokens},
		{method: "POST", path: "/account/tokens", access: AccessSession, handler: s.handleCreateToken},
		{method: "DELETE", path: "/account/tokens/{id}", access: AccessSession, handler: s.handleDeleteToken},

		// Account administration.
		{method: "GET", path: "/accounts", access: AccessAdmin, handler: s.handleListAccounts},
		{method: "POST", path: "/accounts", access: AccessAdmin, handler: s.handleCreateAccount},
		{method: "PATCH", path: "/accounts/{id}", access: AccessAdmin, handler: s.handleUpdateAccount},
		{method: "DELETE", path: "/accounts/{id}", access: AccessAdmin, handler: s.handleDeleteAccount},
		{method: "GET", path: "/accounts/{id}/tokens", access: AccessAdmin, handler: s.handleListAccountTokens},
		{method: "DELETE", path: "/accounts/{id}/tokens/{token_id}", access: AccessAdmin, handler: s.handleDeleteAccountToken},

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

		// Servers.
		{method: "GET", path: "/servers", access: AccessDeveloper, handler: s.handleListServers},
		{method: "POST", path: "/servers", access: AccessDeveloper, handler: s.handleCreateServer},
		{method: "GET", path: "/servers/export", access: AccessAdmin, handler: s.handleExportServers},
		{method: "POST", path: "/servers/import", access: AccessAdmin, handler: s.handleImportServers},
		{method: "GET", path: "/servers/{id}", access: AccessDeveloper, handler: s.handleGetServer},
		{method: "PATCH", path: "/servers/{id}", access: AccessDeveloper, handler: s.handleUpdateServer},
		{method: "DELETE", path: "/servers/{id}", access: AccessDeveloper, handler: s.handleDeleteServer},
		{method: "POST", path: "/servers/{id}/start", access: AccessDeveloper, handler: s.handleStartServer()},
		{method: "POST", path: "/servers/{id}/stop", access: AccessDeveloper, handler: s.handleStopServer()},
		{method: "POST", path: "/servers/{id}/restart", access: AccessDeveloper, handler: s.handleRestartServer()},
		{method: "GET", path: "/servers/{id}/logs", access: AccessDeveloper, handler: s.handleServerLogs},
		{method: "GET", path: "/servers/{id}/stats", access: AccessDeveloper, handler: s.handleServerStats},

		// Images.
		{method: "GET", path: "/images", access: AccessDeveloper, handler: s.handleListImages},
		{method: "DELETE", path: "/images/{id}", access: AccessAdmin, handler: s.handleDeleteImage},

		// Static files.
		{method: "GET", path: "/static", access: AccessDeveloper, handler: s.handleListStatic},
		{method: "POST", path: "/static", access: AccessDeveloper, handler: s.handleUploadStatic, maxBody: StaticUploadLimit},
		{method: "GET", path: "/static/{name}", access: AccessDeveloper, handler: s.handleDownloadStatic},
		{method: "DELETE", path: "/static/{name}", access: AccessDeveloper, handler: s.handleDeleteStatic},

		// Volumes.
		{method: "GET", path: "/volumes", access: AccessDeveloper, handler: s.handleListVolumes},
		{method: "DELETE", path: "/volumes/{name}", access: AccessAdmin, handler: s.handleDeleteVolume},
	}
}
