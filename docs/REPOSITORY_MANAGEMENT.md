# Repository management

Golem Deploy clones Git repositories onto the host and keeps them up to date. It does not build them yet. The HTTP contract is in [API.md](API.md). Handlers live under `internal/handlers/repo`; checkouts live under `repositories/`.

Host Git (and Git LFS when the repo uses it) must be installed. Stored HTTPS/SSH credentials are never returned on list responses.

## Routes

All of these require a bearer token (session JWT or `gdp_` API token). See [API.md](API.md) for auth kinds.

| Method | Path | Role |
|--------|------|------|
| POST | `/repo/add` | Create the record and start a **background clone** (no `job_id`) |
| GET | `/repo/list` | List repos plus checkout size; `password` and `ssh_key` stripped |
| POST | `/repo/pull` | Pull or clone if missing; returns `job_id` |
| POST | `/repo/branch` | Switch branch; returns `job_id` |
| GET | `/repo/job` | Poll a pull/branch job (`?job_id=`) |
| DELETE | `/repo/delete` | Remove the record and local checkout (`?repo_id=`) |

## Jobs

`POST /repo/pull` and `POST /repo/branch` insert a `repo_jobs` row (`running`) and return `{job_id}` immediately.

Poll:

```http
GET /repo/job?job_id=<id>
Authorization: Bearer <token>
```

`job.status` is `running`, `success`, or `error`. On `error`, `job.detail` is the terminal error. Unknown ids are HTTP 404. `job.kind` is `pull` or `branch`.

`repositories.status` is also updated so UIs can show `idle`, `cloning`, `pulling`, or `error`. Prefer `/repo/job` for agents; list status is a coarse label.

`POST /repo/add` clones in the background **without** a job row. Poll `GET /repo/list` until that repo's `status` is `idle` or `error`.

## Add

```json
POST /repo/add
{
  "name": "my-game",
  "url": "https://github.com/user/repo.git",
  "branch": "main",
  "auth_type": "https",
  "username": "token",
  "password": "<personal access token>",
  "ssh_key": ""
}
```

`auth_type` is `none`, `https`, or `ssh` (default `none`). `branch` defaults to `main`. HTTPS requires `username` and `password`. SSH requires `ssh_key`. Response includes `id`. Clone continues in the background.

## Pull and branch

```json
POST /repo/pull    { "repo_id": 1 }
POST /repo/branch  { "repo_id": 1, "branch": "release" }
```

Each start response includes `job_id`. Poll `GET /repo/job?job_id=` until `success` or `error`. Concurrent pull/clone on the same repo is HTTP 409.

## Authentication methods

- `none` — public clone
- `https` — username plus password or personal access token
- `ssh` — private key in `ssh_key`

Use least-privilege tokens. Credentials are stored with the repository record; they are omitted from list JSON.

## Git LFS

Native `git` is used for clone, pull, and branch. Install `git-lfs` on the host if the repository tracks LFS files; otherwise you get pointer files.

## Troubleshooting

- **401/403** — missing or wrong bearer; API tokens cannot call admin routes (repo routes are any-bearer).
- **409** — repo already `pulling` or `cloning`.
- **404 on `/repo/job`** — unknown `job_id`.
- **Auth failure on clone/pull** — check `auth_type` and credentials; GitHub needs a PAT with repo access.

## Host requirements

Git on PATH. Optional: Git LFS, SSH. Disk under `repositories/` must be large enough for checkouts and LFS objects.
