# Agent guidance

Golem Deploy is an open-source tool for self-hosting games built on Golem Engine. It is a single Go binary (`cmd/golem-deploy`) that serves an HTTP API plus an embedded React web client (`frontend/`) and manages the host by shelling out to `git` and `docker`. The project is in early development; see `README.md` for what exists today.

## Source of truth

- Prefer current code, manifests, and build scripts. `docs/API.md` is the human/agent HTTP contract; `cmd/golem-deploy/main.go` remains authoritative for routes.
- Runtime configuration is defined by `internal/config/config.go` and the environment variables read in `cmd/golem-deploy/main.go`. The YAML config file is optional and currently defines no keys.
- Read the relevant `.cursor/rules/subsystem-*.mdc` files before changing a subsystem or its contracts.

## Change discipline

- Keep HTTP contract changes synchronized across handlers, `internal/api`, `frontend/src/api`, and `docs/API.md`.
- Register new server routes in `cmd/golem-deploy/main.go`; protect non-public routes with the auth middleware.
- Treat `frontend/dist` (except its committed `.gitkeep`), `frontend/node_modules`, `bin`, `data`, `uploads`, `repositories`, `jwt_secret`, and other secrets as generated or runtime state.
- Build the web client before the binary because `frontend/embed.go` embeds `frontend/dist`. Without a build, the binary serves a plain-text "web client not built" notice instead of the SPA.
- Before finishing, run what CI runs: `npm run lint` and `npm run build` in `frontend/`, then `go vet ./...`, `staticcheck ./...`, `govulncheck ./...`, and `go test ./...` from the root (see `.github/workflows/ci.yml`).

## Required rule maintenance

- Any change to a subsystem MUST update its matching `.cursor/rules/subsystem-*.mdc` file when boundaries, contracts, entry points, invariants, or verification steps change.
- Any new subsystem MUST add a concise `subsystem-<name>.mdc` rule with precise globs.
- Before finishing, review affected subsystem rules and remove stale guidance; do not copy details that are quicker to discover from the code.
