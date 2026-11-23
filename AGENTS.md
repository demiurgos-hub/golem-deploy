# Agent guidance

Golem Deploy is an open-source tool for self-hosting games built on Golem Engine. It is a single Go binary (`cmd/golem-deploy`, subcommands `serve`, `admin`, `version`) that serves a versioned HTTP API (`/api/v1`) plus an embedded React web client (`frontend/`) and manages the host by shelling out to `git` and `docker`. The project is in early development; see `README.md` for what exists today.

## Source of truth

- Prefer current code, manifests, and build scripts. `docs/API.md` is the human/agent HTTP contract; the route table in `internal/httpapi/routes.go` is authoritative for routes and their access levels, and `internal/apitypes` for wire types.
- Runtime configuration is the `serve` flags (with `GOLEM_DEPLOY_*` environment fallbacks) in `cmd/golem-deploy/main.go`. There is no config file. Runtime settings that admins change live in the database (`settings` table, `GET|PATCH /api/v1/settings`).
- Read the relevant `.cursor/rules/subsystem-*.mdc` files before changing a subsystem or its contracts.

## Change discipline

- Keep HTTP contract changes synchronized across handlers, `internal/apitypes`, `docs/API.md`, and the generated `frontend/src/api/types.gen.ts` (run `scripts/gen-types.sh`; never edit the generated file by hand).
- Register new routes only in the route table in `internal/httpapi/routes.go`, each with an explicit access level (`public`, `developer`, `session`, `admin`), and add them to `expectedRoutes` in `internal/httpapi/routes_test.go`. Handlers never re-check roles.
- Return errors from handlers as API errors (`internal/httpapi/respond.go`); never put internal error text, command output, paths or secrets in responses.
- No package-level mutable state: dependencies flow from `internal/daemon` through `httpapi.Deps`.
- Run `docker` only through `internal/docker`, and only against objects labelled `dev.golemengine.deploy.managed=true` (containers are found by server id, never by a client-supplied name or id). Container env values never go into argv, logs, errors or responses.
- Treat `frontend/dist` (except its committed `.gitkeep`), `frontend/node_modules`, `bin`, `data` (the default data directory, including its keys), and other secrets as generated or runtime state.
- Build the web client before the binary because `frontend/embed.go` embeds `frontend/dist`. Without a build, the binary serves a plain-text "web client not built" notice instead of the SPA.
- Before finishing, run what CI runs: `npm run lint` and `npm run build` in `frontend/`, then `scripts/gen-types.sh` (the generated file must not change), `go vet ./...`, `staticcheck ./...`, `govulncheck ./...`, and `go test -race ./...` from the root (see `.github/workflows/ci.yml`).
- With Docker available, changes to Docker or server code also need `GOLEM_DEPLOY_DOCKER_TESTS=1 go test ./internal/docker/... ./internal/httpapi/...` (CI does not run it).

## Required rule maintenance

- Any change to a subsystem MUST update its matching `.cursor/rules/subsystem-*.mdc` file when boundaries, contracts, entry points, invariants, or verification steps change.
- Any new subsystem MUST add a concise `subsystem-<name>.mdc` rule with precise globs.
- Before finishing, review affected subsystem rules and remove stale guidance; do not copy details that are quicker to discover from the code.
