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
- The `Host` header must be an IP address, `localhost` (any port) or a name configured with `--public-host`; anything else, including a missing `Host`, gets `421 bad_host` on every path (API and web client). This blocks DNS-rebinding attacks against a panel bound to loopback.

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
| `invalid_current_password` | 400 | Wrong current password on password change or API token creation |
| `unauthenticated` | 401 | Missing, invalid, expired or revoked credentials, or the account is gone/unapproved. Drop the token and show the login page |
| `invalid_credentials` | 401 | Login failed (same body for unknown user, wrong password and unapproved account) |
| `forbidden` | 403 | Role too low (API tokens are never admin), or a per-resource rule (e.g. restricted job logs, cancelling another account's job) |
| `session_required` | 403 | Route needs a session JWT, called with an API token |
| `registration_disabled` | 403 | Self-registration is off |
| `not_found` | 404 | Unknown route or resource |
| `method_not_allowed` | 405 | See `Allow` header |
| `bad_host` | 421 | `Host` header is not an IP address, `localhost` or a `--public-host` name |
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
| `rate_limited` | 429 | Too many attempts, or too many concurrent logins / log streams; wait `Retry-After` seconds |
| `internal_error` | 500 | Unexpected failure; see the daemon log for the request id |
| `not_implemented` | 501 | Route planned but not built yet |
| `service_unavailable` | 503 | Docker unreachable, daemon shutting down, or too busy checking passwords (then with `Retry-After`) |

## Authentication

Send credentials only as `Authorization: Bearer <token>`. Cookies are never used (game servers share the hostname on other ports, and cookies are not port-scoped).

| Kind | Obtained from | Lifetime | Acts as |
|------|---------------|----------|---------|
| Session JWT | `POST /auth/login`, `/auth/refresh`, `/account/password` | 12 h per token; 30 days per login (absolute) | The account's role |
| API token `gdp_…` | `POST /account/tokens` (session **and** the current password) | Until revoked, its optional `expires_at`, or a recovery action below | Developer at most, even for admin owners |

Every authenticated request reloads the account: a deleted or unapproved account is rejected immediately, and sessions die when the account's token version changes (password change, logout, role change, un-approval). Each account row starts at a random token version, so a session can never authenticate as a different account, even one created later.

API tokens are **deleted** (not just disabled) when the account is deleted, when it is un-approved (re-approval does not bring them back), by `golem-deploy admin reset-password`, and by `POST /account/password` unless it is sent with `"revoke_tokens": false`. They survive logout and role changes. The owner revokes one with `DELETE /account/tokens/{id}`; an admin lists and revokes anyone's with `/accounts/{id}/tokens`.

Access levels (the **Access** column):

- **public** — no credentials.
- **developer** — any authenticated identity: session of any role, or API token.
- **session** — session JWT of any role; API tokens get `403 session_required`.
- **admin** — session JWT of an admin; developers and all API tokens get `403 forbidden`.

Password checks (login, password change, API token creation) are rate limited in memory. Every attempt is counted before the password is checked and stays counted if it fails:

- **Per client address:** 5 failures per 15 minutes (IPv4 address, or IPv6 /64).
- **Per submitted username:** 20 failures per 15 minutes, counted the same way whether or not the account exists, so the limits reveal nothing about which usernames exist. An address that has logged in successfully as that user since the daemon started is exempt from this limit (not from the per-address one), so someone hammering your username does not lock you out of your usual address. A successful check clears the username's counter.
- **Concurrency:** one login per username at a time and a few per address; extra concurrent attempts get `429` immediately, without being counted.
- Passwords are hashed with bcrypt on a bounded number of workers (one per CPU); when none frees up within about 2 seconds the request gets `503 service_unavailable` with `Retry-After` and is not counted.

Over a limit the answer is `429 rate_limited` with `Retry-After`, even for the correct password. Registration is limited to 5 attempts per hour per address.

The client address is the TCP peer. Behind a reverse proxy, start the daemon with `--trusted-proxy <CIDR,...>`: only when the peer is inside one of those networks is `X-Forwarded-For` used, taking its right-most entry that is not itself a trusted proxy (entries further left are client-controlled and ignored). Without that flag forwarding headers are ignored, and every client behind a proxy shares the proxy's limits.

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
| GET | `/account` | developer | — | 200 `CurrentAccount` | implemented | |
| POST | `/account/password` | session | `ChangePasswordRequest` `{current_password, new_password, revoke_tokens?}` | 200 `ChangePasswordResponse` (old sessions end) | implemented | `invalid_current_password`, `validation_failed`, `rate_limited`, `service_unavailable` |
| GET | `/account/tokens` | session | — | 200 `APITokenList` | implemented | |
| POST | `/account/tokens` | session | `CreateAPITokenRequest` `{name, expires_at?, current_password}` | 201 `CreateAPITokenResponse` (secret shown once) | implemented | `validation_failed`, `invalid_current_password`, `rate_limited`, `service_unavailable`, `conflict` (50 tokens max) |
| DELETE | `/account/tokens/{id}` | session | — | 204 | implemented | `not_found` |

`SessionResponse` is `{token, expires_at, session_expires_at, account}`. Refresh before `expires_at`; after `session_expires_at` log in again. Logging in again does not end other sessions; logout does.

`GET /account` is the only `/account` route an API token can call, so a token can identify itself: `CurrentAccount` is the `Account` fields plus `auth` (`"session"` or `"api_token"`) and `api_token` (the calling token's `APIToken` metadata, or `null` for a session).

`POST /account/password` validates `new_password` first, then checks `current_password` under the limits above, ends every session, deletes the account's API tokens unless `revoke_tokens` is `false` (default `true`), and returns `ChangePasswordResponse`: the `SessionResponse` fields plus `tokens_revoked` (how many tokens were deleted). A missing `current_password` (here and on token creation) is `validation_failed` and is not counted.

### Account administration and settings

| Method | Path | Access | Request | Response | Status | Errors |
|--------|------|--------|---------|----------|--------|--------|
| GET | `/accounts` | admin | — | 200 `AccountList` | implemented | |
| POST | `/accounts` | admin | `CreateAccountRequest` `{username,password,role,approved}` | 201 `Account` | implemented | `validation_failed`, `username_taken` |
| PATCH | `/accounts/{id}` | admin | `UpdateAccountRequest` `{role?,approved?}` | 200 `Account` | implemented | `not_found`, `last_admin`, `cannot_modify_self` |
| DELETE | `/accounts/{id}` | admin | — | 204 | implemented | `not_found`, `last_admin`, `cannot_modify_self` |
| GET | `/accounts/{id}/tokens` | admin | — | 200 `APITokenList` (metadata only) | implemented | `not_found` |
| DELETE | `/accounts/{id}/tokens/{token_id}` | admin | — | 204 | implemented | `not_found` (also when the token belongs to another account) |
| GET | `/settings` | admin | — | 200 `Settings` | implemented | |
| PATCH | `/settings` | admin | `UpdateSettingsRequest` `{registration_enabled?}` | 200 `Settings` | implemented | |
| GET | `/status` | developer | — | 200 `Status` `{version, docker_reachable, registration_enabled, counts{projects,servers,active_jobs,pending_accounts}}` | implemented | |

There is always at least one approved admin: the last one cannot be deleted, demoted or unapproved, and admins cannot delete, demote or unapprove themselves (another admin, or `golem-deploy admin` on the host, must). Role changes and un-approval end the account's sessions; un-approval also deletes its API tokens. Approve a registered account with `PATCH /accounts/{id}` `{"approved": true}`.

**Registration and username enumeration.** While `registration_enabled` is on, `POST /auth/register` answers `409 username_taken` for an existing username, so it reveals whether a username exists to anyone who can reach it (rate limited to 5 attempts per hour per address). This is inherent to self-registration; login and its rate limits do not reveal it. Leave registration off unless you need it.

### Jobs

A `Job` is `{id, kind, subject_type, subject_id, status, detail, restricted, created_by, created_at, started_at, finished_at}`. `status` is `queued` → `running` → `success` | `error` | `cancelled`. At most one job is active per `(subject_type, subject_id)`; starting another returns `409 job_in_progress`. `detail` is a short user-safe message (empty on success); the full output is the job log. Jobs that were active when the daemon stopped abruptly become `error` with detail `interrupted by daemon restart`; jobs cancelled by a graceful shutdown become `cancelled`.

Kinds: `project.clone`, `project.pull`, `project.branch`, `server.start`, `server.stop`, `server.restart` (more may be added; display unknown kinds verbatim). Subject types: `project`, `server`; `subject_id` is the subject's id as a string.

| Method | Path | Access | Request | Response | Status | Errors |
|--------|------|--------|---------|----------|--------|--------|
| GET | `/jobs` | developer | query `subject_type`, `subject_id`, `limit` (1–500, default 50) | 200 `JobList`, newest first | implemented | `bad_request` |
| GET | `/jobs/{id}` | developer | — | 200 `Job` | implemented | `not_found` |
| POST | `/jobs/{id}/cancel` | developer | — | 202 `Job` (cancellation requested) | implemented | `not_found`, `forbidden`, `job_not_active` |
| GET | `/jobs/{id}/log` | developer | query `follow=1` | 200 `text/plain` | implemented | `not_found`, `forbidden`, `bad_request`, `rate_limited` |

`/jobs/{id}/log` returns the log so far. With `follow=1` the response stays open and streams appended output (flushed per chunk) until the job ends, the client disconnects or the daemon shuts down; each account may hold 4 following streams at once (more → `429 rate_limited`). Browsers should read it with `fetch` and a `ReadableStream` (it needs the bearer header, so not `EventSource`).

Who may do what with a job (everyone with developer access sees every job, including `status` and `detail`, in `GET /jobs` and `GET /jobs/{id}`):

| Job | Read log | Cancel |
|-----|----------|--------|
| `restricted: true` — started from an admin route | admin sessions only | admin sessions only |
| `restricted: false` | any developer identity (session or API token) | the account that started it (session or its API tokens), or an admin session |

Other callers get `403 forbidden`.

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

1. **Log in:** `POST /api/v1/auth/login` `{"username":"…","password":"…"}` → keep `token`. For automation, create an API token once with a session (`POST /api/v1/account/tokens` `{"name":"ci","current_password":"…"}`) and store `secret`; it works on every **developer** route, and `GET /api/v1/account` tells it who it is.
2. **Check health:** `GET /api/v1/status`.
3. **Long operations:** call the start route, take the `Job` from the `202`, then poll `GET /api/v1/jobs/{id}` until `status` is terminal, or stream `GET /api/v1/jobs/{id}/log?follow=1`.
4. **On `401`:** re-authenticate. **On `429` or `503` with `Retry-After`:** wait that many seconds. **On `409 job_in_progress`:** wait for the active job (`GET /api/v1/jobs?subject_type=…&subject_id=…&limit=1`). **On `421 bad_host`:** the daemon needs `--public-host <name>` for the host name you used.
