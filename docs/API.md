# HTTP API (v1)

This is the human and agent guide to the Golem Deploy HTTP API. The route table with access levels is `internal/httpapi/routes.go`; wire types are `internal/apitypes` (the web client's `frontend/src/api/types.gen.ts` is generated from them). When this file and the code disagree, the code wins and this file is a bug.

Every route listed here is implemented.

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
| `name_taken` | 409 | Project/server name exists |
| `port_in_use` | 409 | Host port already assigned to another server, or the port range is full |
| `last_admin` | 409 | Change would leave no approved admin |
| `cannot_modify_self` | 409 | Admin tried to delete, demote or unapprove themselves |
| `job_in_progress` | 409 | Another job is queued/running for the same subject |
| `job_not_active` | 409 | Job already finished |
| `conflict` | 409 | Other state conflict (e.g. resource in use, file exists, token limit) |
| `request_too_large` | 413 | Body over the route limit |
| `unsupported_media_type` | 415 | Body is not `application/json` |
| `rate_limited` | 429 | Too many attempts, or too many concurrent logins / log streams / static uploads; wait `Retry-After` seconds |
| `internal_error` | 500 | Unexpected failure; see the daemon log for the request id |
| `not_implemented` | 501 | Reserved for routes that are documented but not built yet (no v1 route returns it today) |
| `insufficient_storage` | 507 | A static upload would exceed the store's size quota or file count cap, or leave too little free disk space |
| `service_unavailable` | 503 | Docker or git unavailable, a Git remote failed (`/projects/{id}/branches`), daemon shutting down, or too busy checking passwords (then with `Retry-After`) |

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
| GET | `/auth/options` | public | — | 200 `AuthOptions` `{registration_enabled}` | implemented | |
| POST | `/auth/login` | public | `LoginRequest` | 200 `SessionResponse` | implemented | `invalid_credentials`, `rate_limited`, `validation_failed` |
| POST | `/auth/register` | public | `RegisterRequest` | 201 `Account` (developer, unapproved) | implemented | `registration_disabled`, `validation_failed`, `username_taken`, `rate_limited` |
| POST | `/auth/logout` | session | — | 204 | implemented | Ends **every** session of the account |
| POST | `/auth/refresh` | session | — | 200 `SessionResponse` | implemented | `unauthenticated` after the absolute lifetime |
| GET | `/account` | developer | — | 200 `CurrentAccount` | implemented | |
| POST | `/account/password` | session | `ChangePasswordRequest` `{current_password, new_password, revoke_tokens?}` | 200 `ChangePasswordResponse` (old sessions end) | implemented | `invalid_current_password`, `validation_failed`, `rate_limited`, `service_unavailable` |
| GET | `/account/tokens` | session | — | 200 `APITokenList` | implemented | |
| POST | `/account/tokens` | session | `CreateAPITokenRequest` `{name, expires_at?, current_password}` | 201 `CreateAPITokenResponse` (secret shown once) | implemented | `validation_failed`, `invalid_current_password`, `rate_limited`, `service_unavailable`, `conflict` (50 tokens max) |
| DELETE | `/account/tokens/{id}` | session | — | 204 | implemented | `not_found` |

`GET /auth/options` lets a login page decide whether to offer registration before anyone has signed in; it reveals nothing else.

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
| PATCH | `/settings` | admin | `UpdateSettingsRequest` `{registration_enabled?, game_host?}` | 200 `Settings` | implemented | `validation_failed` (`field: game_host`) |
| GET | `/status` | developer | — | 200 `Status` `{version, docker_reachable, registration_enabled, counts{projects,servers,active_jobs,pending_accounts}}` | implemented | |

`Settings` is `{registration_enabled, game_host, effective_game_host}`. `game_host` is the DNS name or IP address players use to reach game servers (no scheme, port or path; lower-cased, a trailing dot dropped; `""` clears it). `effective_game_host` is what servers use: `game_host`, or the first `--public-host` when it is empty, or `""` when neither exists (servers then cannot start). Changing it regenerates installed per-server self-signed certificates for the new host and marks running servers `restart_required`.

There is always at least one approved admin: the last one cannot be deleted, demoted or unapproved, and admins cannot delete, demote or unapprove themselves (another admin, or `golem-deploy admin` on the host, must). Role changes and un-approval end the account's sessions; un-approval also deletes its API tokens. Approve a registered account with `PATCH /accounts/{id}` `{"approved": true}`.

**Registration and username enumeration.** While `registration_enabled` is on, `POST /auth/register` answers `409 username_taken` for an existing username, so it reveals whether a username exists to anyone who can reach it (rate limited to 5 attempts per hour per address). This is inherent to self-registration; login and its rate limits do not reveal it. Leave registration off unless you need it.

### Jobs

A `Job` is `{id, kind, subject_type, subject_id, status, detail, restricted, created_by, created_by_username, created_at, started_at, finished_at}`. `created_by` is the account that started the job and `created_by_username` its username at read time; both are `null` for jobs the daemon started itself and once that account is deleted. `status` is `queued` → `running` → `success` | `error` | `cancelled`. At most one job is active per `(subject_type, subject_id)`; starting another returns `409 job_in_progress`. `detail` is a short user-safe message (empty on success); the full output is the job log. Jobs that were active when the daemon stopped abruptly become `error` with detail `interrupted by daemon restart`; jobs cancelled by a graceful shutdown become `cancelled`.

Kinds: `project.clone`, `project.pull`, `project.branch`, `project.build`, `server.start`, `server.stop`, `server.restart`, `server.delete` (more may be added; display unknown kinds verbatim). Subject types: `project`, `server`; `subject_id` is the subject's id as a string.

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

### Projects

A project is a Git repository the daemon clones and builds. `Project` is `{id, name, repo_url, branch, auth_type, auth_username, has_secret, last_commit, last_pulled_at, status, static_addons, created_at, updated_at}`; secrets are never returned. `status`: `idle`, `cloning`, `pulling`, `switching_branch` (a job is active), `error` (the last clone failed, an operation failed after modifying the checkout, or the daemon restarted during an operation; a pull repairs it). Operators: see [REPOSITORIES.md](REPOSITORIES.md) for URL forms, credential handling and SSH host keys.

| Method | Path | Access | Request | Response | Status | Errors |
|--------|------|--------|---------|----------|--------|--------|
| GET | `/projects` | developer | — | 200 `ProjectList` (by name) | implemented | |
| POST | `/projects` | admin | `CreateProjectRequest` `{name, repo_url, branch?, auth}` | 201 `CreateProjectResponse` `{project, job}` (`project.clone` job) | implemented | `validation_failed`, `name_taken`, `service_unavailable` |
| GET | `/projects/{id}` | developer | — | 200 `Project` | implemented | `not_found` |
| PATCH | `/projects/{id}` | admin | `UpdateProjectRequest` `{repo_url?, auth?, static_addons?}` | 200 `Project` | implemented | `validation_failed`, `not_found`, `service_unavailable` (static store missing) |
| DELETE | `/projects/{id}` | admin | — | 204 | implemented | `not_found`, `job_in_progress`, `conflict` (servers use its builds, or a container uses one of their images), `service_unavailable` (Docker unreachable) |
| POST | `/projects/{id}/pull` | developer | — | 202 `Job` (`project.pull`, or `project.clone` without a checkout) | implemented | `not_found`, `job_in_progress`, `service_unavailable` |
| POST | `/projects/{id}/branch` | developer | `SwitchBranchRequest` `{branch}` | 202 `Job` (`project.branch`, or `project.clone` of that branch without a checkout) | implemented | `validation_failed`, `not_found`, `job_in_progress`, `service_unavailable` |
| GET | `/projects/{id}/branches` | developer | — | 200 `BranchList` `{current, items}` | implemented | `not_found`, `service_unavailable` |
| POST | `/projects/{id}/builds` | developer | optional `CreateBuildRequest` `{pull?}` (an empty body builds the current checkout) | 202 `CreateBuildResponse` `{build, job}` (`project.build` job) | implemented | see [Builds](#builds) |
| GET | `/projects/{id}/builds` | developer | query `status`, `limit` (1–500, default 50) | 200 `BuildList`, newest first | implemented | `not_found`, `bad_request` |

- `name`: `^[a-z0-9][a-z0-9-]{0,62}$`, unique, immutable. The checkout lives at `<data-dir>/repositories/<id>`; the name is never used in paths.
- `repo_url`: `https://host[:port]/path`, `ssh://[user@]host[:port]/path` or scp-style `user@host:path`. Rejected: other schemes (including `http://`, `git://`, `file://`, `ext::`), local paths, user name or password inside https URLs (use `auth`), query strings, fragments, whitespace, control characters, backslashes and a leading `-`.
- `branch`: optional on create (empty: the remote's default branch, recorded after the clone); rules of `git check-ref-format --branch`, at most 255 bytes, no leading `-`.
- `auth` (`ProjectAuth`) is a union on `type`: `{"type":"none"}` (https URLs only), `{"type":"https","username":"…","password":"…"}` (https URLs only; both fields are required — for token-only hosts use any non-empty user name your provider accepts, e.g. `x-access-token` or `oauth2`; password or access token, one line), `{"type":"ssh","private_key":"…"}` (SSH URLs only; unencrypted OpenSSH or PEM key — passphrase-protected keys are rejected; put the SSH user in the URL). Fields of other types are rejected. Validation errors name the field as `auth.type`, `auth.username`, `auth.password` or `auth.private_key` (or `auth` when a PATCH must include it).
- PATCH: omit `auth` to keep the stored credentials, send it to replace them entirely, `{"type":"none"}` clears them (https only). Changing `repo_url` to a different scheme, host or port (default ports count as equal) requires `auth` in the same request, otherwise `validation_failed` with `field: auth`; a path-only change keeps the stored credentials. A new URL does not re-clone: the next pull or branch switch points the checkout's `origin` at it and hard-resets to what it serves. PATCH never runs git and is allowed while a job runs: a job reads the project's URL, branch and credentials when it starts running, so a job still queued uses the patched settings and a job already running finishes with the old ones.
- Pull = fetch the project's branch, hard-reset the tree to it (`checkout -B` + `clean -ffdx`; local changes are discarded, there is no merge) and fetch Git LFS content; the job sets `last_commit`, `last_pulled_at` and `branch`. A failure before the tree is touched (network, authentication, missing branch) leaves `status` as it was; a later failure sets `error`.
- `static_addons` (`StaticAddon` `{name, target}`, never null) are ZIP files of the static store that every build of the project extracts at `target`, a relative path below the game root (`.` or empty for the root itself; segments of `[A-Za-z0-9._-]` separated by `/`, no `..`, no `.git`, not starting with `-`; stored normalised, e.g. `./assets/` becomes `assets`). PATCH `static_addons` replaces the whole list (at most 16, `[]` clears it); each `name` must be an existing static file that is a ZIP archive (`is_zip`), otherwise `validation_failed` with `field` such as `static_addons[0].name` or `static_addons[1].target`. A static file a project references cannot be deleted (`409 conflict` naming the project); replacing it with `POST /static?replace=1` is allowed and takes effect at the next build. See [BUILDS.md](BUILDS.md#static-add-ons).
- DELETE refuses while a job (including a build) is queued or running for the project, and with `409 conflict` while servers are configured with one of its builds (the message lists their ids) or while a container still uses the image of one of its builds (the same rule as `DELETE /builds/{id}`; the message names the servers whose container it is); `503 service_unavailable` when Docker is unreachable for that check. It removes the checkout first and the row (with its builds) second, so a failed delete can be retried, then removes the builds' images (best effort; an image that cannot be removed stays visible under `GET /images` and can be deleted there).
- `/branches` asks the remote synchronously (30 s timeout); results are cached per project for 30 s (failures for 10 s) and at most 4 listings run at once. Remote failures answer `503 service_unavailable` with a safe message (e.g. authentication failed, remote unreachable). `current` is the project's checked-out branch.
- Job `detail` on failure is a short safe message (e.g. `authentication with the Git remote failed; check the project's credentials`, or that the checkout exceeds the host's size limit, that disk space is low, or that the repository's `.lfsconfig` sets the LFS server); the scrubbed git output is in the job log (capped at 16 MiB per job). Credentials never appear in responses, job details or logs.
- When git is not installed on the host, routes that need it answer `503 service_unavailable`.

### Builds

A build turns a project's checkout into a managed Docker image; servers run the image of a successful build. How a game repository must look, `golem-deploy.yaml`, the generated Dockerfile and the security model: [BUILDS.md](BUILDS.md).

| Method | Path | Access | Request | Response | Status |
|--------|------|--------|---------|----------|--------|
| POST | `/projects/{id}/builds` | developer | optional `CreateBuildRequest` `{pull?}` | 202 `CreateBuildResponse` `{build, job}` | implemented |
| GET | `/projects/{id}/builds` | developer | query `status`, `limit` | 200 `BuildList` | implemented |
| GET | `/builds` | developer | query `project_id`, `status` (`queued`, `running`, `success`, `error`, `cancelled`), `limit` (1–500, default 50) | 200 `BuildList`, newest first | implemented |
| GET | `/builds/{id}` | developer | — | 200 `Build` | implemented |
| DELETE | `/builds/{id}` | admin | — | 204 | implemented |

- `Build` is `{id, project_id, branch, commit, status, detail, job_id, image_id, image_tag, engine_version, schema_fingerprint, webtransport_path, websocket_path, server_ids, created_by, created_at, started_at, finished_at}`. `status`: `queued` (waiting for the project's job or for the daemon's build slot), `running`, `success`, `error`, `cancelled`; a daemon restart turns interrupted builds into `error` (`interrupted by daemon restart`). `branch` and `commit` are the checkout that was built (`commit` is empty while queued). `image_id` (`sha256:…`) and `image_tag` (`golem-deploy/<project>:<first 12 hex of the commit>-b<build id>`, unique per build) are set on success. `engine_version` is the Golem Engine version from the game's `go.mod`. `schema_fingerprint` is empty until a server running the build reports it. It is a game-reported value and does not verify build contents. `webtransport_path` and `websocket_path` (set on success) are the game's transport endpoint paths from `golem-deploy.yaml` (`transport`; defaults `/wt` and `/api/ws`); servers running the build publish their WebTransport and WebSocket fallback URLs with them. `server_ids` lists the servers configured with the build (never null). `detail` is a short safe message on failure (e.g. `golem.yaml was not found in the game root "."`, `go.mod requires github.com/demiurgos-hub/golem-engine v0.3.2; Golem Deploy needs v0.4.0-rc.1 or newer`, `docker build failed; see the build log`, `the build exceeded the time limit of 30m0s`); the full, sanitised build output is the log of job `job_id`.
- POST starts a `project.build` job whose subject is the project, so it is serialised with clone, pull and branch jobs of that project (`409 job_in_progress` while one is active, and other project jobs answer the same while the build runs). With `"pull": true` the job first pulls (or clones, when there is no checkout yet) exactly like `POST /projects/{id}/pull`; a failed pull fails the build with `updating the checkout failed: …`. Without `pull`, a project that has no checkout yet is `409 conflict`. One build runs at a time across the daemon; others wait in `queued`. At most 4 builds may be queued or running at once (`409 conflict`, "too many builds are queued"). Builds are bounded by `--build-timeout` (default 30 minutes). Cancelling the job (`POST /jobs/{id}/cancel`) stops the build; the build ends `cancelled`.
- Retention: after a successful build, the project's successful builds beyond the newest `--build-retention` (default 5) are deleted with their images, except builds that servers are configured with or whose image a container (also a stopped server's) uses; failed and cancelled builds beyond the newest 20 per project are deleted.
- DELETE removes a finished build and its image (unless another successful build recorded the same image id). Errors: `not_found`; `409 conflict` while the build is queued or running ("cancel its job first"), while servers are configured with it, or while a container uses its image (the message names the servers whose container it is: a server keeps its container, also when stopped, until it is started again or deleted, so a server switched to another build holds its old build's image until its next start); `503 service_unavailable` when Docker is unreachable (nothing deleted).
- When the build service is not available (no Docker client), POST and DELETE answer `503 service_unavailable`; listing and reading builds still work.
- Expected errors of POST: `not_found`, `bad_request` (body), `conflict`, `job_in_progress`, `service_unavailable` (git missing for `pull`, or builds unavailable).

### Servers

A server is a container run from the image of a successful build, its host `port` published as both `<port>/udp` and `<port>/tcp` to container port 7777. `Server` is `{id, name, build_id, project_id, image, commit, engine_version, port, public_url, allowed_origins, env_keys, desired_state, state, container_id, started_at, exit_code, restart_required, certificate, created_at, updated_at}`. `certificate` describes the server’s TLS leaf (see below) and is `null` when there is no certificate. `project_id`, `image` (the build's immutable image id), `commit` and `engine_version` are derived from the build (`null`/empty when the server has none). `public_url` is `https://<game host>:<port>/` (empty while no game host is configured). Environment values are secrets: encrypted at rest and never returned — only `env_keys`. `state`: `created`, `running`, `restarting`, `exited`, `paused`, `dead`, `missing` (the server has no container), `unknown` (it has one but Docker is unreachable; the stored fields are still returned). A server's container is the one the daemon created for it and recorded (`container_id`); other containers, are omitted from the API. Recovery can remove the result of an interrupted create only when the container matches the unique launch identity recorded before that create. Operator background (image trust, engine deploy contract, health-gated start, container hardening, environment, logs, ports, network isolation, firewall): [SERVERS.md](SERVERS.md); the game certificate: [CERTIFICATES.md](CERTIFICATES.md).

| Method | Path | Access | Request | Response | Status |
|--------|------|--------|---------|----------|--------|
| GET | `/servers` | developer | — | 200 `ServerList` | implemented |
| POST | `/servers` | developer | `CreateServerRequest` `{name, build_id, port?, allowed_origins?, env?, desired_state?}` | 201 `Server` | implemented |
| GET | `/servers/export` | admin | — | 200 `ServerExport` (contains env values in plain text) | implemented |
| POST | `/servers/import` | admin | `ServerExport` (at most 128 servers, 64 MiB) | 201 `ServerImportResult` (all-or-nothing) | implemented |
| GET | `/servers/{id}` | developer | — | 200 `Server` | implemented |
| PATCH | `/servers/{id}` | developer | `UpdateServerRequest` `{build_id?, port?, allowed_origins?, env_set?, env_unset?}` | 200 `Server` | implemented |
| DELETE | `/servers/{id}` | developer | — | 204 | implemented |
| POST | `/servers/{id}/start` | developer | — | 202 `Job` (`server.start`) | implemented |
| POST | `/servers/{id}/stop` | developer | — | 202 `Job` (`server.stop`) | implemented |
| POST | `/servers/{id}/restart` | developer | — | 202 `Job` (`server.restart`) | implemented |
| GET | `/servers/{id}/logs` | developer | query `tail=N` (1–5000, default 200), `follow=1` | 200 `text/plain` | implemented |
| GET | `/servers/{id}/stats` | developer | — | 200 `ServerStats` | implemented |
| GET | `/servers/{id}/runtime` | developer | — | 200 `ServerRuntime` | implemented |

- `name`: `^[a-z0-9][a-z0-9-]{0,62}$`, unique, immutable; the container is named `golem-deploy-<name>`.
- `build_id`: required on create; the id of a build with status `success` (otherwise `validation_failed`, `field: build_id`). PATCH with `build_id` switches the server to another successful build of the same project (`validation_failed`, `field: build_id` for a different project). Docker is not asked here: the build's image is verified (it must exist and carry the managed label, this installation's instance label and a `build-id` label equal to the build's id) when the server starts. Servers created before servers ran builds have `build_id: null` and cannot start until one is assigned. That first assignment fixes the project for those legacy servers.
- `port`: omit to auto-assign the lowest free port of the daemon's range (`--port-range`, default `7000-7099`; browser-unsafe ports such as 6000 or 6665–6669 are skipped). An explicit port outside the range (or unsafe) is `validation_failed`; one assigned to another server is `port_in_use`, as is a full range.
- `allowed_origins`: extra browser origins allowed to open WebTransport and WebSocket connections, besides the server's own `https://<game host>:<port>` (at most 16). Each is `http(s)://host[:port]` with a DNS name or IP address, without credentials, path, query or fragment; they are normalised (lower-cased, default ports dropped, duplicates removed). A bad entry is `validation_failed` with `field` such as `allowed_origins[1]`. PATCH replaces the whole list (`[]` clears it).
- Env keys match `^[A-Za-z_][A-Za-z0-9_]{0,127}$`, are unique ignoring case and may not start with `GOLEM_` (reserved for variables the daemon injects: the engine deploy contract); names such as `PATH` or `LD_PRELOAD` are ordinary container variables. Values are single-line UTF-8: no NUL, CR or LF (`validation_failed` with `field` naming the key, e.g. `env.MOTD`, `env_set.MOTD`, `servers[0].env.MOTD`; the value is never echoed); every other character arrives in the container unchanged. At most 32 KiB per variable, 128 variables and 64 KiB in total. Values reach Docker only through an env file, never the command line or the docker CLI's environment. To edit env without re-sending secrets, PATCH with `env_set` (add/overwrite) and `env_unset` (remove); unmentioned keys keep their values; a key may not be in both. A PATCH never changes `desired_state`, and concurrent env changes are not lost (a change that keeps colliding answers `409 conflict`: retry).
- Env values are protected from other servers and from the API surface, not from the developers of the game: anyone who can change a server's build or the game's code can read that server's environment. The redaction of job-log excerpts (below) is best effort.
- `desired_state` defaults to `stopped`; `running` on create also starts a `server.start` job (find it with `GET /jobs?subject_type=server&subject_id=<id>`). Start and restart set it to `running`, stop to `stopped`. On startup, recovery jobs re-verify each desired-running server. A completed start with the correct running image and a ready game is adopted. Other desired-running servers go through a fresh start.
- PATCH never restarts a server. When the running container was created from an older definition (build, port, env, allowed origins, the game host, the certificate mode or the daemon's run settings), reads report `restart_required: true`; `POST /restart` applies the change. A renewed or replaced certificate of the same mode is picked up by running servers without a restart. Changing `port` while the server's container is running is `409 conflict` ("stop the server before changing its port"); changing either `port` or `build_id` while a server job is queued or running is `409 job_in_progress`.
- Start (and restart) fails the job with a short message when the server has no build or its build did not succeed, when the port is no longer allowed (outside the current range, browser-unsafe or the panel's port: assign a new one), when no game host is configured, when a custom game certificate is missing or expired, or when the build's image is gone or does not carry this installation's labels for that build. Otherwise it removes the server's previous container, ensures the `golem-deploy` network, checks that no other process holds the port (TCP and UDP), creates a new hardened container with the engine deploy contract (see [SERVERS.md](SERVERS.md#engine-deploy-contract)), records its id and sealed admin token, starts it and **waits for the game's admin endpoint to report ready**, at most `--start-timeout` (default 60 s), without a restart policy. On readiness, the schema fingerprint reported by the game is recorded on the build (`Build.schema_fingerprint`) and restart policy `on-failure:5` is applied. The container exits first or the timeout passes: the job fails (`detail` gives the exit code, or says the game did not report ready and, if its admin endpoint never answered, that the game must use the Golem Engine deploy contract, `golem.ServerConfigFromEnv` and the generated `Runtime.Host`; it points at `GET /servers/{id}/logs`), the container is stopped and kept so its logs stay readable, and its last output is in the job log (sanitised, each line prefixed `container | `, with every line of every env value of at least 4 bytes and the admin token replaced by `[redacted]`). A container named `golem-deploy-<name>` that this installation did not create, or a `golem-deploy` network of someone else, fails the job instead of being touched. Docker failures appear in jobs as short fixed messages (image missing, name in use, port busy, Docker unreachable, timeout, other); Docker's own output goes to the daemon log with the job id.
- Stop is graceful (`SIGTERM`, then `SIGKILL` after 30 s); the container is kept.
- `DELETE` runs a `server.delete` job (serialised with the server's other jobs, so it answers `409 job_in_progress` while one is active) and waits for it: the container is stopped gracefully and removed, then the row and per-server certificate directory are removed and the port is released. It answers `204`, or `503 service_unavailable` (nothing deleted) when Docker is unreachable.
- Logs are the container's stdout and stderr interleaved, each line prefixed with an RFC 3339 timestamp. `tail` selects the last N lines (default 200, 1–5000; anything else, like a non-number or `0`, is `400 bad_request`). A stopped container still has its logs: the response is its last output, and with `follow=1` it ends right after that output. `follow=1` on a running container streams like job logs and ends when the container stops or is removed (also on restart and delete), the client disconnects or the daemon shuts down; at most 4 follow streams per account, job and server logs together (`429 rate_limited` with `Retry-After`). Container output is the game's raw output, never redacted, but sanitised: invalid UTF-8 becomes U+FFFD, control characters (including ESC and bidirectional overrides) are shown escaped (`\x1b[31m`, `\u202e`), so colour codes arrive as text, not as escape sequences and lines are cut at 16 KiB. Without a container (never started, or removed): `409 conflict`.
- Stats are one sample of the running container. Without a container, or when it is not running: `409 conflict` (the message says which).
- Runtime: `ServerRuntime` is `{ready, not_ready_reason, sessions, entities, uptime_seconds, tick{tick, rate, budget_ms, last_ms, max_ms, overruns}, game_name, engine_version, transport, schema_fingerprint, certificate_hashes_published, sampled_at}`, read from the running game's admin endpoint (`/ready`, `/stats`, `/info`) over a single connection to the current loopback binding. `sessions` is the number of connected players; `transport` is `webtransport`, `websocket` or `unknown`. Everything is reported by the game: strings are bounded and stripped of control and formatting characters, strings containing the admin token are dropped, and numbers are clamped (never negative). Without a container, or when it is not running: `409 conflict`. When the admin endpoint does not answer within a few seconds, or answers outside the contract (redirect, oversized or malformed body): `503 service_unavailable` ("the game's admin endpoint did not answer; see the server log"); `503` also when Docker is unreachable.
- Export/import: `ServerExport` is `{format_version: 2, servers: [ServerSpec]}`, `ServerSpec` `{name, project, commit?, port?, allowed_origins?, env?, desired_state}`. The build is referenced portably by project name and commit: import resolves the newest successful build of `project` with that `commit` (a full lower-case hash), or the newest successful build of `project` when `commit` is empty. A server without a build is exported with an empty `project` and cannot be imported until one is filled in. Import validates the whole document (names, duplicate names and ports, env, allowed origins, that every project exists and has a matching successful build, ports inside the range) and then creates every server in one transaction or none. Imported servers are created `stopped`, with their project fixed to the resolved project. Validation stops at the first problem and answers with one ordinary error envelope, not a list: `validation_failed` with `field` naming the entry and attribute, e.g. `servers[1].env`, `servers[0].project` (unknown project or no successful build), `servers[0].commit` (no successful build of that commit) (`format_version` and `servers` for document-level problems; malformed JSON or unknown fields, such as the `image` of a version 1 document, are `400 bad_request`), or `409 name_taken` / `port_in_use` (without `field`) when a name or port is already used by an existing server. One document holds at most 128 servers and 64 MiB (an export of 128 servers with the largest possible env and allowed origins fits); split larger exports.
- Expected errors: `validation_failed`, `name_taken`, `port_in_use`, `not_found`, `job_in_progress`, `conflict`, `service_unavailable` (Docker unreachable; game admin endpoint not answering).

### Game certificate

| Method | Path | Access | Request | Response | Status |
|--------|------|--------|---------|----------|--------|
| GET | `/certificates/game` | admin | — | 200 `GameCertificate` | implemented |
| PUT | `/certificates/game` | admin | `UpdateGameCertificateRequest` `{mode, certificate_pem?, private_key_pem?}` (at most 160 KiB) | 200 `GameCertificate` | implemented |

Game servers use TLS for WebTransport and HTTPS. See [CERTIFICATES.md](CERTIFICATES.md) for storage and rotation.

- `GameCertificate` is `{mode, game_host, certificate}`. In `self_signed` mode `certificate` is always `null`. In `custom` mode it describes the uploaded pair, or is `null` when a pair is missing. Expired custom metadata is still returned.
- `CertificateInfo` is `{subject, issuer, dns_names, ip_addresses, not_before, not_after, fingerprint_sha256, self_signed, chain_length, covers_game_host}`. The fingerprint is lowercase hex SHA-256 of the leaf. `covers_game_host` reports whether the leaf covers the effective game host. The key is never returned.
- `Server.certificate` describes the server's individual self-signed certificate after it is generated, or the shared custom certificate in custom mode. It is `null` if the applicable certificate is missing.
- PUT `{"mode":"self_signed"}` generates new pairs for the existing servers. PEM fields in this mode produce `validation_failed` on `certificate_pem`. In `custom` mode upload a PEM chain (leaf first, at most 8 certificates and 64 KiB) and an unencrypted PEM key (PKCS#8, PKCS#1 or SEC 1, at most 16 KiB, RSA of at least 2048 bits or ECDSA P-256/P-384/P-521). The key must match the leaf. The leaf must be valid now for TLS servers and each certificate must be signed by the next one in the chain. Invalid input produces `validation_failed` with `field` set to `mode`, `certificate_pem` or `private_key_pem`.
- Running servers pick up certificate replacements through the engine's file poll. Switching modes reports `restart_required` because the running game's pinning setting changes only on restart.
- Expected errors: `validation_failed`, `service_unavailable`.

### Images, static files, volumes

| Method | Path | Access | Request | Response | Status |
|--------|------|--------|---------|----------|--------|
| GET | `/images` | developer | — | 200 `ImageList` (this installation's managed images only) | implemented |
| DELETE | `/images/{id}` | admin | — | 204 (`conflict` if in use) | implemented |
| GET | `/static` | developer | — | 200 `StaticFileList` | implemented |
| POST | `/static` | developer | `multipart/form-data`, repeatable field `files`, query `replace=1`; at most 1 GiB per request and per file, 32 files; 2 uploads at a time per account | 201 `StaticFileList` (stored files) | implemented |
| GET | `/static/{name}` | developer | — | 200 file download | implemented |
| DELETE | `/static/{name}` | developer | — | 204 (`conflict` while a project uses it as a static add-on) | implemented |
| GET | `/volumes` | developer | — | 200 `VolumeList` (managed volumes only) | implemented |
| DELETE | `/volumes/{name}` | admin | — | 204 (`conflict` if in use) | implemented |

- `Image` is `{id, tags, project_id, commit, size_bytes, created_at, server_ids, build_ids, in_use}` for images carrying the managed label and this installation's instance label; `project_id` and `commit` come from the image's build labels. `build_ids` are the successful builds that recorded it, `server_ids` the servers whose build has it. Images of builds are managed through `/builds`: deleting a build removes its image. `DELETE /images/{id}` only removes images no build records (for example one left behind when a build's image could not be removed): deleting an image that a build records (the message names the build), that a server's build uses or that any container was created from is `409 conflict`; an image that is not managed by this installation is `404 not_found`, exactly like a missing one.
- `Volume` is `{name, server_id, server_name, created_at, in_use}`; `server_id` is null when the labelled server no longer exists. A volume mounted by any container is `409 conflict`; unmanaged volumes are `404 not_found`. Sizes are not reported (Docker computes them only with an expensive scan).
- Static file names are a single segment of 1–128 characters from `[A-Za-z0-9._-]`, starting with a letter or digit, not ending with a dot and not a Windows device name (`CON`, `nul.txt`, `COM1`, …); invalid names are rejected (`validation_failed`, `field: files`), never rewritten. `replace` accepts `1`/`true` and `0`/`false`; anything else is `400 bad_request`. `StaticFile` is `{name, size_bytes, modified_at, is_zip}` (`is_zip`: `.zip` name and ZIP signature). `StaticFileList` is `{items, used_bytes, quota_bytes, file_count, max_files}`; the usage fields describe the whole store (`quota_bytes` 0: unlimited).
- Limits: the store's total size is capped by `--static-quota-mb` (default 5120 MiB) and its file count at 2000; an upload also must leave 1 GiB free on the data directory's file system. They are checked against the bytes actually received (streaming stops once the quota cannot be met), and a violation is `507 insufficient_storage`. An account may run 2 uploads at a time (`429 rate_limited`).
- Atomicity: every file of a request is first streamed to a temporary file; then names, sizes, the quota, the file count and conflicts are checked together (an existing name is `409 conflict` unless `replace=1`); only then are the files moved into place. If any check fails, nothing becomes visible and nothing is replaced. If moving a file fails midway (a file system error), the files already placed are removed and the replaced ones restored from links kept until the end, so the store is as before; only if that restoration itself fails can part of the upload remain visible, and the request then fails with `500`. Commits are serialised, so a rollback never touches files of another request. Downloads are `application/octet-stream` with `Content-Disposition: attachment` and support `Range`.

## Agent cookbook

1. **Log in:** `POST /api/v1/auth/login` `{"username":"…","password":"…"}` → keep `token`. For automation, create an API token once with a session (`POST /api/v1/account/tokens` `{"name":"ci","current_password":"…"}`) and store `secret`; it works on every **developer** route, and `GET /api/v1/account` tells it who it is.
2. **Check health:** `GET /api/v1/status`.
3. **Long operations:** call the start route, take the `Job` from the `202`, then poll `GET /api/v1/jobs/{id}` until `status` is terminal, or stream `GET /api/v1/jobs/{id}/log?follow=1`.
   **Build a project:** `POST /api/v1/projects/{id}/builds` `{"pull":true}` → follow `job.id` as above, then `GET /api/v1/builds/{build.id}` for `status`, `detail` and `image_id`.
4. **On `401`:** re-authenticate. **On `429` or `503` with `Retry-After`:** wait that many seconds. **On `409 job_in_progress`:** wait for the active job (`GET /api/v1/jobs?subject_type=…&subject_id=…&limit=1`). **On `421 bad_host`:** the daemon needs `--public-host <name>` for the host name you used.
