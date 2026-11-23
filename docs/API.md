# HTTP API (v1)

This is the human and agent guide to the Golem Deploy HTTP API. The route table with access levels is `internal/httpapi/routes.go`; wire types are `internal/apitypes` (the web client's `frontend/src/api/types.gen.ts` is generated from them). When this file and the code disagree, the code wins and this file is a bug.

Status column: **implemented**, or **planned (task B/C)** — planned routes are registered with their final access level and currently answer `501 not_implemented`; their request and response types already exist in `internal/apitypes`.

## Conventions

- Base path: `/api/v1`. Anything else under `/api/` is a JSON `404 not_found`; a known path with the wrong method is a JSON `405 method_not_allowed` with an `Allow` header. Paths outside `/api/` serve the embedded web client.
- Request bodies are JSON (`Content-Type: application/json`, otherwise `415`), at most 1 MiB unless a route says otherwise (`413 request_too_large`). Unknown fields and trailing data are rejected (`400 bad_request`, with `field` naming an unknown or mistyped field).
- Responses are the resource itself, no wrapper. Collections are `{"items": [...]}`. Arrays are never `null`. Timestamps are RFC 3339 in UTC. Nullable fields are present with `null`, never omitted.
- PATCH bodies only contain the fields to change; absent fields stay as they are.
- `DELETE` and other bodiless successes return `204`.
- Routes that start long work respond `202` with a [`Job`](#jobs).
- Every response carries `X-Request-Id`. API responses send `Cache-Control: no-store`.

### Errors

Every non-2xx response has this body:

```json
{"error": {"code": "validation_failed", "message": "password must be at least 12 bytes", "request_id": "3f1c…", "field": "password"}}
```

`message` is safe to display; `field` is only set when one request field is at fault. Internal details (SQL, command output, paths) are never sent; they are logged under `request_id`. Switch on `code`:

| Code | Status | Meaning |
|------|--------|---------|
| `bad_request` | 400 | Malformed JSON, unknown/mistyped field, trailing data, bad query or path parameter |
| `validation_failed` | 400 | A field violates a rule (`field` names it) |
| `invalid_current_password` | 400 | Wrong current password on password change |
| `unauthenticated` | 401 | Missing, invalid, expired or revoked credentials, or the account is gone/unapproved. Drop the token and show the login page |
| `invalid_credentials` | 401 | Login failed (same body for unknown user, wrong password and unapproved account) |
| `forbidden` | 403 | Role too low (API tokens are never admin) |
| `session_required` | 403 | Route needs a session JWT, called with an API token |
| `registration_disabled` | 403 | Self-registration is off |
| `not_found` | 404 | Unknown route or resource |
| `method_not_allowed` | 405 | See `Allow` header |
| `username_taken` | 409 | Account username exists |
| `name_taken` | 409 | Project/server name exists (task B/C) |
| `port_in_use` | 409 | Host port already assigned (task C) |
| `last_admin` | 409 | Change would leave no approved admin |
| `cannot_modify_self` | 409 | Admin tried to delete, demote or unapprove themselves |
| `job_in_progress` | 409 | Another job is queued/running for the same subject |
| `job_not_active` | 409 | Job already finished |
| `conflict` | 409 | Other state conflict (e.g. resource in use, token limit) |
| `request_too_large` | 413 | Body over the route limit |
| `unsupported_media_type` | 415 | Body is not `application/json` |
| `rate_limited` | 429 | Too many attempts; wait `Retry-After` seconds |
| `internal_error` | 500 | Unexpected failure; see the daemon log for the request id |
| `not_implemented` | 501 | Route planned but not built yet |
| `service_unavailable` | 503 | Docker unreachable or daemon shutting down |

## Authentication

Send credentials only as `Authorization: Bearer <token>`. Cookies are never used (game servers share the hostname on other ports, and cookies are not port-scoped).

| Kind | Obtained from | Lifetime | Acts as |
|------|---------------|----------|---------|
| Session JWT | `POST /auth/login`, `/auth/refresh`, `/account/password` | 12 h per token; 30 days per login (absolute) | The account's role |
| API token `gdp_…` | `POST /account/tokens` (session) | Until revoked or its optional `expires_at` | Developer at most, even for admin owners |

Every authenticated request reloads the account: a deleted or unapproved account is rejected immediately, and sessions die when the account's token version changes (password change, logout, role change, un-approval). API tokens die with their owner's deletion or un-approval; they survive password changes (revoke them explicitly).

Access levels (the **Access** column):

- **public** — no credentials.
- **developer** — any authenticated identity: session of any role, or API token.
- **session** — session JWT of any role; API tokens get `403 session_required`.
- **admin** — session JWT of an admin; developers and all API tokens get `403 forbidden`.

Login and password-change failures are rate limited in memory: 5 failures per 15 minutes per client IP (TCP peer address; IPv6 grouped by /64) and per username → `429 rate_limited` with `Retry-After`. A successful login clears the username's counter. Registration is limited to 5 attempts per hour per IP.

Account rules: usernames match `^[a-z0-9][a-z0-9._-]{2,31}$` after lower-casing and are unique case-insensitively; passwords are 12–72 bytes (longer is rejected, never truncated).

The first admin is created on the host: `golem-deploy admin create --username <name>` (see README).

## Endpoints

### Auth and account

| Method | Path | Access | Request | Response | Status | Errors |
|--------|------|--------|---------|----------|--------|--------|
| POST | `/auth/login` | public | `LoginRequest` | 200 `SessionResponse` | implemented | `invalid_credentials`, `rate_limited`, `validation_failed` |
| POST | `/auth/register` | public | `RegisterRequest` | 201 `Account` (developer, unapproved) | implemented | `registration_disabled`, `validation_failed`, `username_taken`, `rate_limited` |
| POST | `/auth/logout` | session | — | 204 | implemented | Ends **every** session of the account |
| POST | `/auth/refresh` | session | — | 200 `SessionResponse` | implemented | `unauthenticated` after the absolute lifetime |
| GET | `/account` | session | — | 200 `Account` | implemented | |
| POST | `/account/password` | session | `ChangePasswordRequest` | 200 `SessionResponse` (old sessions end) | implemented | `invalid_current_password`, `validation_failed`, `rate_limited` |
| GET | `/account/tokens` | session | — | 200 `APITokenList` | implemented | |
| POST | `/account/tokens` | session | `CreateAPITokenRequest` | 201 `CreateAPITokenResponse` (secret shown once) | implemented | `validation_failed`, `conflict` (50 tokens max) |
| DELETE | `/account/tokens/{id}` | session | — | 204 | implemented | `not_found` |

`SessionResponse` is `{token, expires_at, session_expires_at, account}`. Refresh before `expires_at`; after `session_expires_at` log in again. Logging in again does not end other sessions; logout does.

### Account administration and settings

| Method | Path | Access | Request | Response | Status | Errors |
|--------|------|--------|---------|----------|--------|--------|
| GET | `/accounts` | admin | — | 200 `AccountList` | implemented | |
| POST | `/accounts` | admin | `CreateAccountRequest` `{username,password,role,approved}` | 201 `Account` | implemented | `validation_failed`, `username_taken` |
| PATCH | `/accounts/{id}` | admin | `UpdateAccountRequest` `{role?,approved?}` | 200 `Account` | implemented | `not_found`, `last_admin`, `cannot_modify_self` |
| DELETE | `/accounts/{id}` | admin | — | 204 | implemented | `not_found`, `last_admin`, `cannot_modify_self` |
| GET | `/settings` | admin | — | 200 `Settings` | implemented | |
| PATCH | `/settings` | admin | `UpdateSettingsRequest` `{registration_enabled?}` | 200 `Settings` | implemented | |
| GET | `/status` | developer | — | 200 `Status` `{version, docker_reachable, registration_enabled, counts{projects,servers,active_jobs,pending_accounts}}` | implemented | |

There is always at least one approved admin: the last one cannot be deleted, demoted or unapproved, and admins cannot delete, demote or unapprove themselves (another admin, or `golem-deploy admin` on the host, must). Role changes and un-approval end the account's sessions. Approve a registered account with `PATCH /accounts/{id}` `{"approved": true}`.

### Jobs

A `Job` is `{id, kind, subject_type, subject_id, status, detail, created_by, created_at, started_at, finished_at}`. `status` is `queued` → `running` → `success` | `error` | `cancelled`. At most one job is active per `(subject_type, subject_id)`; starting another returns `409 job_in_progress`. `detail` is a short user-safe message (empty on success); the full output is the job log. Jobs that were active when the daemon stopped abruptly become `error` with detail `interrupted by daemon restart`; jobs cancelled by a graceful shutdown become `cancelled`.

Kinds: `project.clone`, `project.pull`, `project.branch`, `server.start`, `server.stop`, `server.restart` (more may be added; display unknown kinds verbatim). Subject types: `project`, `server`; `subject_id` is the subject's id as a string.

| Method | Path | Access | Request | Response | Status | Errors |
|--------|------|--------|---------|----------|--------|--------|
| GET | `/jobs` | developer | query `subject_type`, `subject_id`, `limit` (1–500, default 50) | 200 `JobList`, newest first | implemented | `bad_request` |
| GET | `/jobs/{id}` | developer | — | 200 `Job` | implemented | `not_found` |
| POST | `/jobs/{id}/cancel` | developer | — | 202 `Job` (cancellation requested) | implemented | `not_found`, `job_not_active` |
| GET | `/jobs/{id}/log` | developer | query `follow=1` | 200 `text/plain` | implemented | `not_found`, `bad_request` |

`/jobs/{id}/log` returns the log so far. With `follow=1` the response stays open and streams appended output (flushed per chunk) until the job ends, the client disconnects or the daemon shuts down. Browsers should read it with `fetch` and a `ReadableStream` (it needs the bearer header, so not `EventSource`).

### Projects (task B)

A project is a Git repository the daemon clones (and later builds). `Project` is `{id, name, repo_url, branch, auth_type, auth_username, has_secret, last_commit, last_pulled_at, status, created_at, updated_at}`; secrets are never returned. `status`: `idle`, `cloning`, `pulling`, `switching_branch`, `error`.

| Method | Path | Access | Request | Response | Status |
|--------|------|--------|---------|----------|--------|
| GET | `/projects` | developer | — | 200 `ProjectList` | planned (task B) |
| POST | `/projects` | admin | `CreateProjectRequest` `{name, repo_url, branch?, auth}` | 201 `CreateProjectResponse` `{project, job}` (clone job) | planned (task B) |
| GET | `/projects/{id}` | developer | — | 200 `Project` | planned (task B) |
| PATCH | `/projects/{id}` | admin | `UpdateProjectRequest` `{repo_url?, auth?}` | 200 `Project` | planned (task B) |
| DELETE | `/projects/{id}` | admin | — | 204 | planned (task B) |
| POST | `/projects/{id}/pull` | developer | — | 202 `Job` | planned (task B) |
| POST | `/projects/{id}/branch` | developer | `SwitchBranchRequest` `{branch}` | 202 `Job` | planned (task B) |
| GET | `/projects/{id}/branches` | developer | — | 200 `BranchList` `{current, items}` | planned (task B) |

- `name`: `^[a-z0-9][a-z0-9-]{0,62}$`, unique, immutable. `repo_url`: `https://…`, `ssh://…` or scp-style `user@host:path`; local paths and other schemes are rejected.
- `auth` (`ProjectAuth`) is a union on `type`: `{"type":"none"}`, `{"type":"https","username":"…","password":"…"}`, `{"type":"ssh","private_key":"…"}`. Fields of other types are rejected. In PATCH, omit `auth` to keep the stored credentials; sending it replaces them entirely.
- Expected errors: `validation_failed`, `name_taken`, `not_found`, `job_in_progress`.

### Servers (task C)

A server is a container run from an image, its host `port` published as both `<port>/udp` and `<port>/tcp`. `Server` is `{id, name, project_id, image, port, env_keys, desired_state, state, container_id, started_at, exit_code, created_at, updated_at}`. Environment values are secrets: encrypted at rest and never returned — only `env_keys`. `state`: `created`, `running`, `restarting`, `exited`, `paused`, `dead`, `missing` (no container), `unknown` (Docker unreachable).

| Method | Path | Access | Request | Response | Status |
|--------|------|--------|---------|----------|--------|
| GET | `/servers` | developer | — | 200 `ServerList` | planned (task C) |
| POST | `/servers` | developer | `CreateServerRequest` `{name, project_id?, image, port?, env?, desired_state?}` | 201 `Server` | planned (task C) |
| GET | `/servers/export` | admin | — | 200 `ServerExport` (contains env values in plain text) | planned (task C) |
| POST | `/servers/import` | admin | `ServerExport` | 201 `ServerImportResult` (all-or-nothing) | planned (task C) |
| GET | `/servers/{id}` | developer | — | 200 `Server` | planned (task C) |
| PATCH | `/servers/{id}` | developer | `UpdateServerRequest` `{image?, port?, env_set?, env_unset?}` | 200 `Server` | planned (task C) |
| DELETE | `/servers/{id}` | developer | — | 204 | planned (task C) |
| POST | `/servers/{id}/start` | developer | — | 202 `Job` | planned (task C) |
| POST | `/servers/{id}/stop` | developer | — | 202 `Job` | planned (task C) |
| POST | `/servers/{id}/restart` | developer | — | 202 `Job` | planned (task C) |
| GET | `/servers/{id}/logs` | developer | query `tail=N`, `follow=1` | 200 `text/plain` (streams like job logs) | planned (task C) |
| GET | `/servers/{id}/stats` | developer | — | 200 `ServerStats` | planned (task C) |

- `name`: `^[a-z0-9][a-z0-9-]{0,62}$`, unique, immutable; `project_id` is immutable. Omitting `port` on create auto-assigns one from the configured range. `desired_state` defaults to `stopped`.
- Env keys match `^[A-Za-z_][A-Za-z0-9_]*$`. To edit env without re-sending secrets, PATCH with `env_set` (add/overwrite) and `env_unset` (remove); unmentioned keys keep their values; a key may not be in both. Changes apply on the next (re)start.
- Expected errors: `validation_failed`, `name_taken`, `port_in_use`, `not_found`, `job_in_progress`, `service_unavailable`.

### Images, static files, volumes (task C)

| Method | Path | Access | Request | Response | Status |
|--------|------|--------|---------|----------|--------|
| GET | `/images` | developer | — | 200 `ImageList` (managed images only) | planned (task C) |
| DELETE | `/images/{id}` | admin | — | 204 (`conflict` if in use) | planned (task C) |
| GET | `/static` | developer | — | 200 `StaticFileList` | planned (task C) |
| POST | `/static` | developer | `multipart/form-data`, repeatable field `files`, at most 1 GiB per request | 201 `StaticFileList` (stored files) | planned (task C) |
| GET | `/static/{name}` | developer | — | 200 file download | planned (task C) |
| DELETE | `/static/{name}` | developer | — | 204 | planned (task C) |
| GET | `/volumes` | developer | — | 200 `VolumeList` (managed volumes only) | planned (task C) |
| DELETE | `/volumes/{name}` | admin | — | 204 (`conflict` if in use) | planned (task C) |

Static file names are a single segment of 1–128 characters from `[A-Za-z0-9._-]`, not starting with a dot.

## Agent cookbook

1. **Log in:** `POST /api/v1/auth/login` `{"username":"…","password":"…"}` → keep `token`. For automation, create an API token once with a session (`POST /api/v1/account/tokens` `{"name":"ci"}`) and store `secret`; it works on every **developer** route.
2. **Check health:** `GET /api/v1/status`.
3. **Long operations:** call the start route, take the `Job` from the `202`, then poll `GET /api/v1/jobs/{id}` until `status` is terminal, or stream `GET /api/v1/jobs/{id}/log?follow=1`.
4. **On `401`:** re-authenticate. **On `429`:** wait `Retry-After` seconds. **On `409 job_in_progress`:** wait for the active job (`GET /api/v1/jobs?subject_type=…&subject_id=…&limit=1`).
