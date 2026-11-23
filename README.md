# Golem Deploy

Golem Deploy is an open-source tool for self-hosting games built on [Golem Engine](https://golemengine.dev), the Go engine for server-authoritative multiplayer games. It runs on a Linux host next to Docker and is managed through an embedded web client.

**Status: early development.** Golem Deploy is not yet usable in production. APIs, storage formats and configuration will change without migration paths until a first release. The Golem-aware build pipeline, per-server data volumes and a hardened daemon are planned but do not exist yet. The current daemon has known security gaps (for example, any approved account can control every container on the host), so do not expose it to untrusted users or networks.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## What works today

A single `golem-deploy` binary serves an HTTP API and the embedded React web client on `:8000`. It manages the host by shelling out to `git` and `docker`.

- **Accounts:** first admin created on the host console at first start, self-registration with admin approval, password change, session JWTs and revocable API tokens (`gdp_...`).
- **Repositories:** clone, pull, switch branch and delete Git repositories under `repositories/`, with pollable jobs for pull and branch switch.
- **Docker:** list and delete images that already exist on the host; run, start, stop, delete, list containers and read their logs and processes.
- **Mass run:** save named container-run presets and start all of them for one image.
- **Static files:** upload, list, download and delete files in a static store (`uploads/static`).
- **Volumes:** list, inspect and delete Docker volumes labelled by Golem Deploy.

There is no HTTP route that builds Docker images yet; images must be built outside Golem Deploy for now.

The HTTP contract is documented in [docs/API.md](docs/API.md); repository handling in [docs/REPOSITORY_MANAGEMENT.md](docs/REPOSITORY_MANAGEMENT.md).

## Requirements

- Go 1.26 or newer
- Node.js 24 and npm (to build the web client)
- On the host that runs it: Linux, Docker CLI and daemon, Git (and Git LFS if your repositories use it)

## Build

The web client must be built first because the Go binary embeds `frontend/dist`:

```sh
cd frontend
npm ci
npm run build
cd ..
go build -o bin/golem-deploy ./cmd/golem-deploy
```

`scripts/build.sh` runs the same steps. If you compile without building the web client, the binary still works but every web page returns a short plain-text notice explaining how to build it.

Run the checks used in CI:

```sh
(cd frontend && npm run lint)
go vet ./...
go test ./...
```

## Run

Start the binary from the directory that should hold its runtime state:

```sh
./bin/golem-deploy
```

On first start it creates a `jwt_secret` file, SQLite databases under `data/`, and `uploads/`, and prompts on the console for the first admin account. It listens on `:8000` over plain HTTP unless both `TLS_CERT_FILE` and `TLS_KEY_FILE` are set. Optional environment variables: `CONFIG_PATH` (location of the optional config file; `jwt_secret` is stored next to it), `DB_PATH`, `MASSRUN_DB_PATH`.

## License

Licensed under the [Apache License 2.0](LICENSE).
