# HTTP API

This is the human and agent HTTP guide for Golem Deploy. Routes are registered in `cmd/golem-deploy/main.go`. Shared request and response types live in `internal/api` and `frontend/src/api`. When this file and the code disagree, the code wins.

The server listens on `:8000` (HTTP, or HTTPS when `TLS_CERT_FILE` and `TLS_KEY_FILE` are both set). Send JSON unless a row says multipart or a file download. Unknown paths that are not registered API routes serve the embedded SPA HTML (`index.html`), not a JSON 404. If the web client was not built before compiling, those paths return a plain-text 503 explaining how to build it.

## Authentication

Send tokens as `Authorization: Bearer <token>`. There are two bearer kinds:

| Kind | How you get it | Lifetime | Privileges |
|------|----------------|----------|------------|
| Session JWT | `POST /login` or `POST /account/refresh` | 24 hours | Keeps the account `access_level` (`0` admin, `1` developer) |
| API token | `POST /account/tokens` (session only) | Until revoked | Always `access_level` 1 (developer). Never admin. Prefix `gdp_` |

- Public registration (`POST /account/create`) always stores `access_level=1` and `approved=false`. Client `access_level` and `approved` are ignored.
- Token CRUD, password change, and refresh require a **session** JWT. A `gdp_` token cannot mint tokens or refresh into a session.
- Admin routes require a **session** JWT with `access_level == 0`. API tokens never satisfy admin checks.
- API-token auth and token create require a live approved account.

Auth column in the table:

- **public** — no `Authorization` header
- **session** — session JWT only (`RequireSession`)
- **any-bearer** — session JWT or `gdp_` API token (`AuthMiddleware`)
- **admin-session** — session JWT and `access_level == 0` (`RequireAdmin`)

## Async vs sync

Most routes finish the work before they return JSON.

**Pollable jobs** (`job_id` in the start response): `POST /repo/pull` and `POST /repo/branch`. Poll `GET /repo/job?job_id=` until `job.status` is `success` or `error`. Unknown `job_id` is JSON 404. Failure `job.detail` is the terminal error text. Those operations also update `repositories.status` (`pulling` / `cloning` / `idle` / `error`).

**Background without `job_id`**: `POST /repo/add` starts clone in the background and returns `{id}`. Watch `GET /repo/list` until `status` is `idle` or `error`.

**Synchronous**: Docker image/container/volume routes, mass-run CRUD and `POST /massrun/execute`, static-file upload/list/delete/download.

## Agent cookbook

1. **Authenticate.** `POST /login` with `{username,password}` and keep the JWT, **or** use an existing `gdp_` token from a session user. Refresh a session with `POST /account/refresh` before the 24h expiry. Mint tokens only with a session: `POST /account/tokens` `{name}` (plaintext secret is returned once).
2. **Repositories.** `GET /repo/list`. `POST /repo/pull` `{repo_id}` and optionally `POST /repo/branch` `{repo_id,branch}`; poll `GET /repo/job?job_id=` until `success` or `error`.
3. **Run.** Images must already exist on the Docker host. Single container: `POST /docker/container/run`. Saved presets: `POST /massrun/execute` `{image_name}` (do not loop `/docker/container/run`).
4. **Volumes.** `GET /docker/volume/list`, `GET /docker/volume/orphaned`, `GET /docker/volume/info?container_name=`, `POST /docker/volume/delete` `{volume_names}`.

## Endpoints

| Method | Path | Body / query | Auth |
|--------|------|--------------|------|
| POST | `/login` | `{username,password}` | public |
| GET | `/status` | none → `{docker_installed}` | any-bearer |
| POST | `/account/create` | `{username,password}` (`access_level`,`approved` ignored) | public |
| POST | `/account/refresh` | none | session |
| POST | `/account/password` | `{old_password,new_password}` | session |
| POST | `/account/delete` | `{username}` | admin-session |
| POST | `/account/approve` | `{username,approve}` | admin-session |
| GET | `/account/unapproved` | none | admin-session |
| GET | `/account/list` | none | admin-session |
| GET | `/account/tokens` | none | session |
| POST | `/account/tokens` | `{name}` | session |
| POST | `/account/tokens/revoke` | `{id}` | session |
| POST | `/build/static/upload` | multipart: `files` (repeatable) | any-bearer |
| GET | `/build/static/list` | none | any-bearer |
| POST | `/build/static/delete` | `{file_names}` | any-bearer |
| GET | `/build/static/download` | query `file_name` (file) | any-bearer |
| GET | `/docker/image/list` | none | any-bearer |
| POST | `/docker/image/delete` | `{image_ids}` | any-bearer |
| POST | `/docker/container/run` | `{image_name,container_name?,port?,env_vars?,label?}` | any-bearer |
| POST | `/docker/container/start` | `{container_name}` | any-bearer |
| POST | `/docker/container/stop` | `{container_name}` | any-bearer |
| POST | `/docker/container/delete` | `{container_names,delete_saves?}` | any-bearer |
| GET | `/docker/container/list` | none | any-bearer |
| POST | `/docker/container/logs` | `{container_name}` (text file) | any-bearer |
| POST | `/docker/container/processes` | `{container_name}` | any-bearer |
| GET | `/docker/volume/list` | none | any-bearer |
| GET | `/docker/volume/orphaned` | none | any-bearer |
| POST | `/docker/volume/delete` | `{volume_names}` | any-bearer |
| GET | `/docker/volume/info` | query `container_name` | any-bearer |
| POST | `/massrun/add` | `{name,container_name,port,label,env_vars}` | any-bearer |
| POST | `/massrun/edit` | `{name,container_name,port,label,env_vars}` | any-bearer |
| GET | `/massrun/list` | none | any-bearer |
| POST | `/massrun/delete` | `{names}` or `{name}` | any-bearer |
| POST | `/massrun/replace` | `{configs:[...]}` | any-bearer |
| POST | `/massrun/execute` | `{image_name}` | any-bearer |
| POST | `/repo/add` | `{name,url,branch?,auth_type?,username?,password?,ssh_key?}` | any-bearer |
| GET | `/repo/list` | none | any-bearer |
| POST | `/repo/pull` | `{repo_id}` → `{job_id}` | any-bearer |
| DELETE | `/repo/delete` | query `repo_id` | any-bearer |
| POST | `/repo/branch` | `{repo_id,branch}` → `{job_id}` | any-bearer |
| GET | `/repo/job` | query `job_id` | any-bearer |
| * | `/` and unmatched paths | SPA HTML | public |

There is no `/health` route and no HTTP route that builds images yet.

## Notes

- JSON errors typically use `{status,detail}`. Some success envelopes still include `message`.
- `POST /account/tokens` returns the plaintext `gdp_...` secret once. List and revoke never include it.
- `GET /repo/list` omits stored `password` and `ssh_key`.
- Containers started through `/docker/container/run` or `/massrun/execute` run without a data volume and carry the Docker label `dev.golemengine.deploy.label` (default `default`).
- Volume routes only see volumes labelled `dev.golemengine.deploy.container`. `delete_saves` on container delete removes the matching `golem-deploy-<container>-data` volume if it exists.
- `POST /massrun/execute` is HTTP 400 when `image_name` is missing or no presets exist. Mixed start failures stay HTTP 200 with `status` `partial`, `started`, and `failed[{name,detail}]`.
- First process start with an empty accounts database prompts on the host console for an admin user. That is not an HTTP route.
