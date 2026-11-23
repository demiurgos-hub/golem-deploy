# Golem Deploy

Golem Deploy is an open-source tool for self-hosting games built on [Golem Engine](https://golemengine.dev), the Go engine for server-authoritative multiplayer games. It runs on a Linux host next to Docker and is managed through an embedded web client.

**Status: early development.** Golem Deploy is not yet usable in production. APIs, storage formats and configuration will change without migration paths until a first release. The daemon core (accounts, sessions, API tokens, jobs, encrypted secrets), projects, the Docker runner, servers, images, static files and volumes have been rebuilt. There is no build pipeline yet, so no managed images exist on a host until one is labelled by hand. The web client covers every API v1 route and has been exercised end to end against the daemon with real git and Docker.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## What works today

A single `golem-deploy` binary serves a versioned HTTP API under `/api/v1` and the embedded React web client.

- **Accounts:** admins created on the host with `golem-deploy admin create`, optional self-registration with admin approval (off by default), roles `admin` and `developer`, password change, logout.
- **Sessions and tokens:** short-lived session JWTs re-validated against the database on every request, revocable API tokens (`gdp_...`) that never exceed the developer role, login rate limiting.
- **Jobs:** background operations with persisted status, per-job logs that can be streamed, cancellation and per-subject serialisation.
- **Secrets at rest:** Git credentials and server environment values are stored AES-256-GCM encrypted.
- **Projects:** Git repositories cloned over HTTPS or SSH into the data directory, with pull, branch switching and remote branch listing as jobs, encrypted credentials and hardened, non-interactive git. See [docs/REPOSITORIES.md](docs/REPOSITORIES.md).
- **Game servers:** hardened containers from managed images, one host port per server published as UDP and TCP, start/stop/restart jobs, streamed logs, stats, export/import, and restart of servers that should be running when the daemon starts. Only objects labelled `dev.golemengine.deploy.managed=true` are ever touched; see [docs/SERVERS.md](docs/SERVERS.md) for the container hardening, the port model and firewall notes.
- **Images, volumes, static files:** managed images and volumes can be listed and deleted; a static file store accepts uploads (streamed, never overwritten unless asked) for later builds.
- **Web client:** sign-in (with a registration link only when registration is on), account (password change with optional token revocation, API tokens created with the current password and shown once), account administration including other accounts' API tokens, settings, projects (create, live clone/pull/branch job logs, edit credentials, repair a failed clone by pulling), servers (create from managed images, start/stop/restart/delete jobs, live logs with follow, stats, environment editing with a restart-required notice, JSON export/import), images, static files (upload with confirmed replacement, download, delete) and volumes. Job logs and cancel buttons follow the daemon's rules for restricted jobs and job ownership.

The HTTP contract is documented in [docs/API.md](docs/API.md).

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
(cd frontend && npm run lint && npm test && npm run build)
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go test -race ./...
```

Tests against a real Docker daemon are opt-in: `GOLEM_DEPLOY_DOCKER_TESTS=1 go test ./internal/docker/... ./internal/httpapi/...` (they build small labelled images from a pinned busybox and clean up after themselves).

## Develop the web client

```sh
cd frontend
npm run dev                       # Vite on :5173, proxies /api to the daemon on 127.0.0.1:8000
VITE_API_MOCK=1 npm run dev       # simulates the git- and Docker-backed routes in the browser
npm test                          # Vitest + Testing Library
```

`GOLEM_DEPLOY_DEV_API=http://127.0.0.1:18080 npm run dev` points the proxy at a daemon on another address. The mock still needs a daemon for sign-in, accounts and settings; it simulates projects, servers, images, static files and volumes following the API v1 contract, for work without git or Docker. It is development-only: a production build refuses `VITE_API_MOCK` and fails if the mock code ends up in the bundle.

## Run

Create the first admin account, then start the daemon:

```sh
./bin/golem-deploy admin create --username admin        # prompts for the password twice
./bin/golem-deploy serve
```

`admin create --password-stdin` reads the password from the first line of standard input instead (for scripts). `admin reset-password --username <name>` sets a new password, ends all of that account's sessions and deletes all of its API tokens (use it to recover a compromised account). Both work while the daemon is running.

`serve` options (flags, with environment fallbacks):

| Flag | Environment | Default | Meaning |
|------|-------------|---------|---------|
| `--data-dir` | `GOLEM_DEPLOY_DATA_DIR` | `./data` | All runtime state (also used by `admin`) |
| `--listen` | `GOLEM_DEPLOY_LISTEN` | `127.0.0.1:8000` | Listen address |
| `--tls-cert` | `GOLEM_DEPLOY_TLS_CERT` | | TLS certificate (PEM) |
| `--tls-key` | `GOLEM_DEPLOY_TLS_KEY` | | TLS private key (PEM) |
| `--insecure-http` | `GOLEM_DEPLOY_INSECURE_HTTP` | `false` | Allow plaintext HTTP on a non-loopback address |
| `--public-host` | `GOLEM_DEPLOY_PUBLIC_HOST` | | Comma-separated host names the panel is reached by (e.g. `deploy.example.com`) |
| `--trusted-proxy` | `GOLEM_DEPLOY_TRUSTED_PROXY` | | Comma-separated CIDRs of reverse proxies whose `X-Forwarded-For` is trusted |
| `--port-range` | `GOLEM_DEPLOY_PORT_RANGE` | `7000-7099` | Host ports for game servers (within 1024–32767, not the listen port) |
| `--port-bind-address` | `GOLEM_DEPLOY_PORT_BIND_ADDRESS` | all interfaces | Host IP game ports are published on |
| `--server-memory-mb` | `GOLEM_DEPLOY_SERVER_MEMORY_MB` | `0` (unlimited) | Memory limit per server container |
| `--server-cpus` | `GOLEM_DEPLOY_SERVER_CPUS` | `0` (unlimited) | CPU limit per server container |

Without a TLS certificate the daemon only binds loopback addresses (`localhost` must resolve only to loopback addresses); to expose it on the network either configure TLS or put it behind a TLS-terminating reverse proxy and pass `--insecure-http`. It shuts down gracefully on SIGINT/SIGTERM. If no admin account exists it logs how to create one and keeps serving.

Requests are only served for IP addresses, `localhost` and the names given with `--public-host`; any other `Host` gets `421` (this stops DNS-rebinding attacks from web pages against a panel on loopback). If you reach the panel by a domain name, list it in `--public-host`.

Behind a reverse proxy, pass its address range with `--trusted-proxy` (for example `--trusted-proxy 127.0.0.1/32` for a proxy on the same host) so that login rate limits and the access log use the real client address from `X-Forwarded-For`. Without it, forwarding headers are ignored and all clients share the proxy's address for rate limiting. Never list networks that untrusted clients can connect from.

The data directory (created with mode 0700 if it does not exist; on Unix an existing one must not be accessible by group or others, and the daemon refuses to start with the `chmod` command to run instead of changing it) contains `golem-deploy.db` (SQLite, mode 0600), `keys/jwt.key` and `keys/secrets.key` (0600; back up `secrets.key` with the database, without it stored credentials cannot be decrypted), `repositories/` (one checkout per project id), `known_hosts` (SSH host keys of Git remotes), `static/`, `logs/jobs/` and `tmp/`.

## License

Licensed under the [Apache License 2.0](LICENSE).
