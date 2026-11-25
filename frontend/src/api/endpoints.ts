import type { ApiClient } from "@/api/client";
import type {
  ACME,
  APITokenList,
  Account,
  AccountList,
  AuthOptions,
  Backup,
  BackupList,
  BranchList,
  Build,
  BuildList,
  BuildStatus,
  ChangePasswordRequest,
  ChangePasswordResponse,
  CreateAPITokenRequest,
  CreateAPITokenResponse,
  CreateAccountRequest,
  CreateBackupRequest,
  CreateBuildRequest,
  CreateBuildResponse,
  CreateInspectorSessionRequest,
  CreateProjectRequest,
  CreateProjectResponse,
  CreateServerRequest,
  CurrentAccount,
  GameCertificate,
  ImageList,
  Inspector,
  InspectorSession,
  Job,
  JobList,
  LoginRequest,
  PanelCertificate,
  Project,
  ProjectList,
  RegisterRequest,
  ResetServerDataRequest,
  RestoreServerRequest,
  Server,
  ServerConnectivity,
  ServerExport,
  ServerImportResult,
  ServerList,
  ServerRuntime,
  ServerStats,
  SessionResponse,
  SetupRequest,
  Settings,
  StaticFileList,
  Status,
  SubjectType,
  SwitchBranchRequest,
  UpdateACMERequest,
  UpdateAccountRequest,
  UpdateGameCertificateRequest,
  UpdateGameCertificateResponse,
  UpdateInspectorRequest,
  UpdatePanelCertificateRequest,
  UpdatePanelCertificateResponse,
  UpdateProjectRequest,
  UpdateServerRequest,
  UpdateSettingsRequest,
  VolumeList,
} from "@/api/types.gen";

const seg = (v: string | number) => encodeURIComponent(String(v));

/** Typed functions for every route in docs/API.md. */
export function createEndpoints(c: ApiClient) {
  return {
    auth: {
      options: (signal?: AbortSignal) =>
        c.request<AuthOptions>("GET", "/auth/options", { signal, auth: false }),
      login: (body: LoginRequest) =>
        c.request<SessionResponse>("POST", "/auth/login", { body, auth: false }),
      register: (body: RegisterRequest) =>
        c.request<Account>("POST", "/auth/register", { body, auth: false }),
      logout: () => c.request<void>("POST", "/auth/logout"),
      refresh: () => c.request<SessionResponse>("POST", "/auth/refresh"),
      setup: (body: SetupRequest) =>
        c.request<SessionResponse>("POST", "/setup", { body, auth: false }),
    },
    account: {
      get: (signal?: AbortSignal) => c.request<CurrentAccount>("GET", "/account", { signal }),
      changePassword: (body: ChangePasswordRequest) =>
        c.request<ChangePasswordResponse>("POST", "/account/password", { body }),
      listTokens: (signal?: AbortSignal) =>
        c.request<APITokenList>("GET", "/account/tokens", { signal }),
      createToken: (body: CreateAPITokenRequest) =>
        c.request<CreateAPITokenResponse>("POST", "/account/tokens", { body }),
      deleteToken: (id: number) => c.request<void>("DELETE", `/account/tokens/${seg(id)}`),
    },
    accounts: {
      list: (signal?: AbortSignal) => c.request<AccountList>("GET", "/accounts", { signal }),
      create: (body: CreateAccountRequest) => c.request<Account>("POST", "/accounts", { body }),
      update: (id: number, body: UpdateAccountRequest) =>
        c.request<Account>("PATCH", `/accounts/${seg(id)}`, { body }),
      remove: (id: number) => c.request<void>("DELETE", `/accounts/${seg(id)}`),
      listTokens: (id: number, signal?: AbortSignal) =>
        c.request<APITokenList>("GET", `/accounts/${seg(id)}/tokens`, { signal }),
      deleteToken: (id: number, tokenId: number) =>
        c.request<void>("DELETE", `/accounts/${seg(id)}/tokens/${seg(tokenId)}`),
    },
    system: {
      status: (signal?: AbortSignal) => c.request<Status>("GET", "/status", { signal }),
      settings: (signal?: AbortSignal) => c.request<Settings>("GET", "/settings", { signal }),
      updateSettings: (body: UpdateSettingsRequest) =>
        c.request<Settings>("PATCH", "/settings", { body }),
    },
    jobs: {
      list: (
        filter: { subject_type?: SubjectType; subject_id?: string; limit?: number },
        signal?: AbortSignal,
      ) => c.request<JobList>("GET", "/jobs", { query: filter, signal }),
      get: (id: number, signal?: AbortSignal) => c.request<Job>("GET", `/jobs/${seg(id)}`, { signal }),
      cancel: (id: number) => c.request<Job>("POST", `/jobs/${seg(id)}/cancel`),
      logPath: (id: number) => `/jobs/${seg(id)}/log`,
    },
    projects: {
      list: (signal?: AbortSignal) => c.request<ProjectList>("GET", "/projects", { signal }),
      get: (id: number, signal?: AbortSignal) =>
        c.request<Project>("GET", `/projects/${seg(id)}`, { signal }),
      create: (body: CreateProjectRequest) =>
        c.request<CreateProjectResponse>("POST", "/projects", { body }),
      update: (id: number, body: UpdateProjectRequest) =>
        c.request<Project>("PATCH", `/projects/${seg(id)}`, { body }),
      remove: (id: number) => c.request<void>("DELETE", `/projects/${seg(id)}`),
      pull: (id: number) => c.request<Job>("POST", `/projects/${seg(id)}/pull`),
      switchBranch: (id: number, body: SwitchBranchRequest) =>
        c.request<Job>("POST", `/projects/${seg(id)}/branch`, { body }),
      branches: (id: number, signal?: AbortSignal) =>
        c.request<BranchList>("GET", `/projects/${seg(id)}/branches`, { signal }),
      /** An empty body builds the current checkout; `pull` updates it first in the same job. */
      build: (id: number, body: CreateBuildRequest) =>
        c.request<CreateBuildResponse>("POST", `/projects/${seg(id)}/builds`, { body }),
      builds: (id: number, filter: { status?: BuildStatus; limit?: number }, signal?: AbortSignal) =>
        c.request<BuildList>("GET", `/projects/${seg(id)}/builds`, { query: filter, signal }),
    },
    builds: {
      list: (filter: { project_id?: number; status?: BuildStatus; limit?: number }, signal?: AbortSignal) =>
        c.request<BuildList>("GET", "/builds", { query: filter, signal }),
      get: (id: number, signal?: AbortSignal) => c.request<Build>("GET", `/builds/${seg(id)}`, { signal }),
      remove: (id: number) => c.request<void>("DELETE", `/builds/${seg(id)}`),
    },
    certificates: {
      game: (signal?: AbortSignal) => c.request<GameCertificate>("GET", "/certificates/game", { signal }),
      updateGame: (body: UpdateGameCertificateRequest) =>
        c.request<UpdateGameCertificateResponse>("PUT", "/certificates/game", { body }),
      panel: (signal?: AbortSignal) => c.request<PanelCertificate>("GET", "/certificates/panel", { signal }),
      updatePanel: (body: UpdatePanelCertificateRequest) =>
        c.request<UpdatePanelCertificateResponse>("PUT", "/certificates/panel", { body }),
      acme: (signal?: AbortSignal) => c.request<ACME>("GET", "/certificates/acme", { signal }),
      updateACME: (body: UpdateACMERequest) => c.request<ACME>("PUT", "/certificates/acme", { body }),
      renew: () => c.request<Job>("POST", "/certificates/acme/renew"),
    },
    inspector: {
      get: (signal?: AbortSignal) => c.request<Inspector>("GET", "/inspector", { signal }),
      /** Admin. 202 restricted inspector.apply job; 409 job_in_progress while one runs, 409 conflict when not available. */
      update: (body: UpdateInspectorRequest) => c.request<Job>("PATCH", "/inspector", { body }),
      /** Login sessions only (API tokens get 403 session_required). A one-time entry link, valid for 60 seconds. */
      createSession: (body: CreateInspectorSessionRequest) =>
        c.request<InspectorSession>("POST", "/inspector/sessions", { body }),
    },
    servers: {
      list: (signal?: AbortSignal) => c.request<ServerList>("GET", "/servers", { signal }),
      get: (id: number, signal?: AbortSignal) =>
        c.request<Server>("GET", `/servers/${seg(id)}`, { signal }),
      create: (body: CreateServerRequest) => c.request<Server>("POST", "/servers", { body }),
      update: (id: number, body: UpdateServerRequest) =>
        c.request<Server>("PATCH", `/servers/${seg(id)}`, { body }),
      /**
       * Runs a server.delete job and waits for it. A last backup of the saved data comes first; 409 conflict when it
       * cannot be taken or would be incomplete. `skipLastBackup` (admin) sends skip_safety_backup=1.
       */
      remove: (id: number, opts: { skipLastBackup?: boolean } = {}) =>
        c.request<void>("DELETE", `/servers/${seg(id)}`, opts.skipLastBackup ? { query: { skip_safety_backup: "1" } } : {}),
      start: (id: number) => c.request<Job>("POST", `/servers/${seg(id)}/start`),
      stop: (id: number) => c.request<Job>("POST", `/servers/${seg(id)}/stop`),
      restart: (id: number) => c.request<Job>("POST", `/servers/${seg(id)}/restart`),
      stats: (id: number, signal?: AbortSignal) =>
        c.request<ServerStats>("GET", `/servers/${seg(id)}/stats`, { signal }),
      runtime: (id: number, signal?: AbortSignal) =>
        c.request<ServerRuntime>("GET", `/servers/${seg(id)}/runtime`, { signal }),
      connectivity: (id: number, signal?: AbortSignal) =>
        c.request<ServerConnectivity>("GET", `/servers/${seg(id)}/connectivity`, { signal }),
      logsPath: (id: number) => `/servers/${seg(id)}/logs`,
      exportAll: () => c.request<ServerExport>("GET", "/servers/export"),
      importAll: (body: ServerExport) =>
        c.request<ServerImportResult>("POST", "/servers/import", { body }),
      /** 202 server.backup job; the body (a note) is optional. */
      backup: (id: number, body: CreateBackupRequest) => c.request<Job>("POST", `/servers/${seg(id)}/backups`, { body }),
      /** Admin. 202 server.restore job; 409 schema_mismatch unless `force` when the fingerprints are known and differ. */
      restore: (id: number, body: RestoreServerRequest) => c.request<Job>("POST", `/servers/${seg(id)}/restore`, { body }),
      /** Admin. 202 server.data_reset job. */
      resetData: (id: number, body: ResetServerDataRequest) =>
        c.request<Job>("POST", `/servers/${seg(id)}/data/reset`, { body }),
    },
    backups: {
      /** Newest first; `server_id` filters by the server a backup was taken from. */
      list: (filter: { server_id?: number }, signal?: AbortSignal) =>
        c.request<BackupList>("GET", "/backups", { query: filter, signal }),
      get: (id: number, signal?: AbortSignal) => c.request<Backup>("GET", `/backups/${seg(id)}`, { signal }),
      /** The gzip tar archive, exactly the bytes `sha256` describes. */
      download: (id: number) => c.download(`/backups/${seg(id)}/download`),
      remove: (id: number) => c.request<void>("DELETE", `/backups/${seg(id)}`),
    },
    images: {
      list: (signal?: AbortSignal) => c.request<ImageList>("GET", "/images", { signal }),
      remove: (id: string) => c.request<void>("DELETE", `/images/${seg(id)}`),
    },
    staticFiles: {
      list: (signal?: AbortSignal) => c.request<StaticFileList>("GET", "/static", { signal }),
      /** Without `replace`, an existing name fails the whole request with 409 conflict. */
      upload: (
        files: File[],
        opts: { replace?: boolean; onProgress?: (loaded: number, total: number) => void; signal?: AbortSignal },
      ) => {
        const form = new FormData();
        for (const f of files) form.append("files", f, f.name);
        const { replace, ...rest } = opts;
        return c.upload<StaticFileList>(replace ? "/static?replace=1" : "/static", form, rest);
      },
      download: (name: string) => c.download(`/static/${seg(name)}`),
      remove: (name: string) => c.request<void>("DELETE", `/static/${seg(name)}`),
    },
    volumes: {
      list: (signal?: AbortSignal) => c.request<VolumeList>("GET", "/volumes", { signal }),
      remove: (name: string) => c.request<void>("DELETE", `/volumes/${seg(name)}`),
    },
  };
}

export type Endpoints = ReturnType<typeof createEndpoints>;
