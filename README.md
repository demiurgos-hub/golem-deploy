# Golem Deploy

Golem Deploy is an open-source tool for self-hosting games built on [Golem Engine](https://golemengine.dev), the Go engine for server-authoritative multiplayer games. It runs on a Linux host next to Docker and is managed through an embedded web client.

**Status: early development.** Golem Deploy is not yet usable in production. APIs, storage formats and configuration will change without migration paths until a first release. The daemon core (accounts, sessions, API tokens, jobs, encrypted secrets), the Docker runner, servers, images, static files and volumes have been rebuilt; projects are being rebuilt on top of it and their API routes currently answer `501 not_implemented`. The web client still targets the previous API and does not work against this daemon yet.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## What works today

A single `golem-deploy` binary serves a versioned HTTP API under `/api/v1` and the embedded React web client.

- **Accounts:** admins created on the host with `golem-deploy admin create`, optional self-registration with admin approval (off by default), roles `admin` and `developer`, password change, logout.
- **Sessions and tokens:** short-lived session JWTs re-validated against the database on every request, revocable API tokens (`gdp_...`) that never exceed the developer role, login rate limiting.
- **Jobs:** background operations with persisted status, per-job logs that can be streamed, cancellation and per-subject serialisation.
- **Secrets at rest:** Git credentials and server environment values are stored AES-256-GCM encrypted.
- **Game servers:** hardened containers from managed images, one host port per server published as UDP and TCP, start/stop/restart jobs, streamed logs, stats, export/import, and restart of servers that should be running when the daemon starts. Only objects labelled `dev.golemengine.deploy.managed=true` are ever touched; see [docs/SERVERS.md](docs/SERVERS.md) for the container hardening, the port model and firewall notes.
- **Images, volumes, static files:** managed images and volumes can be listed and deleted; a static file store accepts uploads (streamed, never overwritten unless asked) for later builds.

The HTTP contract, including the planned routes, is documented in [docs/API.md](docs/API.md).

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

`scripts/build.sh` runs the same steps (pass `-ldflags "-X main.version=v0.1.0"` to stamp a version). If you compile without building the web client, the binary still works but every web page returns a short plain-text notice explaining how to build it.

After changing `internal/apitypes`, regenerate the web client's types with `scripts/gen-types.sh`.

Run the checks used in CI:

```sh
(cd frontend && npm run lint && npm run build)
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go test -race ./...
```

Tests against a real Docker daemon are opt-in: `GOLEM_DEPLOY_DOCKER_TESTS=1 go test ./internal/docker/... ./internal/httpapi/...` (they build small labelled images from a pinned busybox and clean up after themselves).

## Run

Create the first admin account, then start the daemon:

```sh
./bin/golem-deploy admin create --username admin        # prompts for the password twice
./bin/golem-deploy serve
```

`admin create --password-stdin` reads the password from the first line of standard input instead (for scripts). `admin reset-password --username <name>` sets a new password and ends that account's sessions. Both work while the daemon is running.

`serve` options (flags, with environment fallbacks):

| Flag | Environment | Default | Meaning |
|------|-------------|---------|---------|
| `--data-dir` | `GOLEM_DEPLOY_DATA_DIR` | `./data` | All runtime state (also used by `admin`) |
| `--listen` | `GOLEM_DEPLOY_LISTEN` | `127.0.0.1:8000` | Listen address |
| `--tls-cert` | `GOLEM_DEPLOY_TLS_CERT` | | TLS certificate (PEM) |
| `--tls-key` | `GOLEM_DEPLOY_TLS_KEY` | | TLS private key (PEM) |
| `--insecure-http` | `GOLEM_DEPLOY_INSECURE_HTTP` | `false` | Allow plaintext HTTP on a non-loopback address |
| `--port-range` | `GOLEM_DEPLOY_PORT_RANGE` | `7000-7099` | Host ports for game servers (within 1024–32767, not the listen port) |
| `--port-bind-address` | `GOLEM_DEPLOY_PORT_BIND_ADDRESS` | all interfaces | Host IP game ports are published on |
| `--server-memory-mb` | `GOLEM_DEPLOY_SERVER_MEMORY_MB` | `0` (unlimited) | Memory limit per server container |
| `--server-cpus` | `GOLEM_DEPLOY_SERVER_CPUS` | `0` (unlimited) | CPU limit per server container |

Without a TLS certificate the daemon only binds loopback addresses; to expose it on the network either configure TLS or put it behind a TLS-terminating reverse proxy and pass `--insecure-http`. It shuts down gracefully on SIGINT/SIGTERM. If no admin account exists it logs how to create one and keeps serving.

The data directory (created with mode 0700) contains `golem-deploy.db` (SQLite), `keys/jwt.key` and `keys/secrets.key` (0600; back up `secrets.key` with the database, without it stored credentials cannot be decrypted), `repositories/`, `static/`, `logs/jobs/` and `tmp/`.

## License

Licensed under the [Apache License 2.0](LICENSE).
