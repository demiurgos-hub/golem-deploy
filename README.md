# Golem Deploy

Golem Deploy is an open-source tool for self-hosting games built on [Golem Engine](https://golemengine.dev), the Go engine for server-authoritative multiplayer games. It runs on a Linux host next to Docker and is managed through an embedded web client.

**Status: early development.** Golem Deploy is not yet usable in production. APIs, storage formats and configuration will change without migration paths until a first release. The daemon core (accounts, sessions, API tokens, jobs, encrypted secrets), projects, the Docker runner, servers, images, static files and volumes have been rebuilt, and the API now covers the whole path from a Git repository to a running game: builds turn a project's checkout into a verified image, and servers run the images of successful builds with the Golem Engine deploy contract, health-gated starts and managed game certificates. The embedded web client covers that whole path.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## What works today

A single `golem-deploy` binary serves a versioned HTTP API under `/api/v1` and the embedded React web client.

- **Accounts:** admins created on the host with `golem-deploy admin create`, optional self-registration with admin approval (off by default), roles `admin` and `developer`, password change, logout.
- **Sessions and tokens:** short-lived session JWTs re-validated against the database on every request, revocable API tokens (`gdp_...`) that never exceed the developer role, login rate limiting.
- **Jobs:** background operations with persisted status, per-job logs that can be streamed, cancellation and per-subject serialisation.
- **Secrets at rest:** Git credentials and server environment values are stored AES-256-GCM encrypted.
- **Projects:** Git repositories cloned over HTTPS or SSH into the data directory, with pull, branch switching and remote branch listing as jobs, encrypted credentials and hardened, non-interactive git. See [docs/REPOSITORIES.md](docs/REPOSITORIES.md).
- **Builds:** a project's checkout is built into a hardened, labelled image by `docker build` from a generated multi-stage Dockerfile (Go and Node.js build stage, `golem-bake`, web client build, distroless non-root runtime), with an optional `golem-deploy.yaml` (game root, server package and arguments, web build, admin-enabled custom Dockerfile), static add-ons extracted from the static store, one build at a time, a time limit, retention of old builds and their images (never a build a server uses), and the repository treated as untrusted (the host only reads a few files from it, contained and size-capped). See [docs/BUILDS.md](docs/BUILDS.md).
- **Game servers:** hardened containers running the image of a successful build (verified by its build labels before every run), one host port per server published as UDP and TCP, the Golem Engine deploy contract injected as environment (listen addresses, TLS files, public and fallback URLs from the game host, allowed origins, a per-container admin token), starts that succeed only once the game's admin endpoint reports ready (`--start-timeout`), a runtime view (players, entities, tick statistics, schema fingerprint), streamed logs, stats, export/import by project and commit, and restart of servers that should be running when the daemon starts. Only objects labelled `dev.golemengine.deploy.managed=true` are ever touched; see [docs/SERVERS.md](docs/SERVERS.md) for the container hardening, the deploy contract, the port model and firewall notes.
- **Game certificate:** each server gets its own short-lived self-signed key and certificate, generated and rotated by the daemon. In custom mode all servers share the chain and key uploaded by an admin. Running games reload certificate files from their read-only mounts. See [docs/CERTIFICATES.md](docs/CERTIFICATES.md), including what a self-signed certificate cannot do for browser players on the internet.
- **Images, volumes, static files:** managed images and volumes can be listed and deleted (a build's image is removed by deleting the build); a static file store accepts uploads (streamed, never overwritten unless asked) that projects use as build add-ons.
- **Web client:** sign-in (with a registration link only when registration is on), account (password change with optional token revocation, API tokens created with the current password and shown once), account administration including other accounts' API tokens, settings (self-registration, the game host with its effective value, game and panel certificates with automatic issuance and live jobs, ACME settings and renewal), projects (create, live clone/pull/branch job logs, edit credentials, which must be re-entered when the URL moves to another host, repair a failed clone by pulling, builds with an optional pull and live build logs, a page per build with its image and schema fingerprint, static add-ons and the custom Dockerfile switch for admins), servers (create from a project's successful build with allowed origins, start/stop/restart/delete jobs with the delete outcome and failed starts shown, play addresses in server and project lists, a browser connection test for HTTPS, WebSocket and WebTransport, the game's runtime view, deploying another build of the same project with an optional restart, the server's certificate, live logs with follow, stats, environment editing with a restart-required notice, JSON export/import by project and commit), images (with the builds that record them), static files (store usage against the quota, the projects using each file, upload with confirmed replacement, download, delete) and volumes. A first-run guide shows the project, build, server order. Lists refresh on their own while the page is visible. Job logs and cancel buttons follow the daemon's rules for restricted jobs and job ownership.

The HTTP contract is documented in [docs/API.md](docs/API.md).

## Requirements

- Go 1.26 or newer
- Node.js 24 and npm (to build the web client)
- On the host that runs it: Linux, Docker CLI and daemon with BuildKit (the buildx plugin, standard in Docker 23 and newer), Git (and Git LFS if your repositories use it); builds need network access to Docker Hub, gcr.io, the Go module proxy and npm

Windows with Docker Desktop is a development platform only. The daemon does not check or restrict the data directory ACL on Windows. Certificate keys and temporary environment files inherit permissions from that directory. Use Linux for production.

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
shellcheck scripts/*.sh
go vet ./...
go run ./scripts/checkchars          # no bidi controls, U+2028/U+2029, NUL or other control characters in tracked files
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go test -race ./...
```

Tests against a real Docker daemon are opt-in: `GOLEM_DEPLOY_DOCKER_TESTS=1 go test -p 1 ./internal/build/... ./internal/docker/... ./internal/httpapi/... ./internal/servers/...` (they build small labelled `gdm2h-*` images from a pinned busybox and a `FROM scratch` image of a fake game compiled with the local Go toolchain, share the `golem-deploy` network, hence `-p 1`, and clean up after themselves; `internal/build` builds the fixture game in `internal/build/testdata/game` as `golem-deploy/gdm3b-*`, which needs network access for base images, Go modules and npm, and runs it briefly; `TestIntegrationBuildToServer` in `internal/httpapi` builds the same fixture through the API and runs it as a server until it reports ready, then checks the runtime view and the realtime config over HTTPS).

## Develop the web client

```sh
cd frontend
npm run dev                       # Vite on :5173, proxies /api to the daemon on 127.0.0.1:8000
VITE_API_MOCK=1 npm run dev       # simulates the git- and Docker-backed routes in the browser
npm test                          # Vitest + Testing Library
```

`GOLEM_DEPLOY_DEV_API=http://127.0.0.1:18080 npm run dev` points the proxy at a daemon on another address. The mock still needs a daemon for sign-in, accounts and settings; it simulates projects, builds, servers (with runtime and connectivity metadata), images, static files, volumes and certificate settings following the API v1 contract, for work without git or Docker. It is development-only: a production build refuses `VITE_API_MOCK` and fails if the mock code ends up in the bundle.

## Install

To run Golem Deploy as a systemd service on an Ubuntu or Debian host with Docker, build the binary and run `sudo scripts/install.sh /path/to/golem-deploy`. It creates the `golem-deploy` user, the data directory `/var/lib/golem-deploy`, the settings file `/etc/golem-deploy/golem-deploy.env` and the service; see [docs/INSTALL.md](docs/INSTALL.md) for requirements, upgrades, uninstalling and security notes.

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
| `--server-memory-mb` | `GOLEM_DEPLOY_SERVER_MEMORY_MB` | `2048` | Memory limit per server container in MiB, without extra swap (`0`: unlimited) |
| `--server-cpus` | `GOLEM_DEPLOY_SERVER_CPUS` | `2` | CPU limit per server container (`0`: unlimited) |
| `--max-repo-size-mb` | `GOLEM_DEPLOY_MAX_REPO_SIZE_MB` | `10240` | Maximum size of each project checkout in MiB, enforced during clone, pull and Git LFS downloads (`0` = unlimited) |
| `--static-quota-mb` | `GOLEM_DEPLOY_STATIC_QUOTA_MB` | `5120` | Total size limit of the static file store in MiB (`0`: unlimited) |
| `--start-timeout` | `GOLEM_DEPLOY_START_TIMEOUT` | `60s` | How long a server start waits for the game to report ready on its admin endpoint (1s–30m) |
| `--build-timeout` | `GOLEM_DEPLOY_BUILD_TIMEOUT` | `30m` | Time limit of one build (Go duration, 1m–24h) |
| `--build-retention` | `GOLEM_DEPLOY_BUILD_RETENTION` | `5` | Successful builds kept per project with their images (1–100); builds that servers use are always kept |

Without a TLS certificate the daemon only binds loopback addresses (`localhost` must resolve only to loopback addresses); to expose it on the network either configure TLS or put it behind a TLS-terminating reverse proxy and pass `--insecure-http`. It shuts down gracefully on SIGINT/SIGTERM. If no admin account exists it logs how to create one and keeps serving.

Requests are only served for IP addresses, `localhost` and the names given with `--public-host`; any other `Host` gets `421` (this stops DNS-rebinding attacks from web pages against a panel on loopback). If you reach the panel by a domain name, list it in `--public-host`. The first `--public-host` is also the default **game host** players use to reach game servers, unless an admin sets `game_host` in the settings; without either, servers cannot start.

Behind a reverse proxy, pass its address range with `--trusted-proxy` (for example `--trusted-proxy 127.0.0.1/32` for a proxy on the same host) so that login rate limits and the access log use the real client address from `X-Forwarded-For`. Without it, forwarding headers are ignored and all clients share the proxy's address for rate limiting. Never list networks that untrusted clients can connect from.

The data directory is dedicated to Golem Deploy, because the daemon removes leftover files below it at startup: it must not exist yet (it is then created with mode 0700), be empty, or have been initialised by Golem Deploy (it holds a `.golem-deploy-data` marker file); anything else is refused. On Unix an existing one must not be accessible by group or others, and the daemon refuses to start with the `chmod` command to run instead of changing it. Its path must not contain `"`, `$`, line breaks or (outside Windows) backslashes. Only one `serve` may use a data directory at a time (it holds a lock on `.lock`); `admin create` may initialise a new data directory, `admin reset-password` needs an initialised one. It contains `golem-deploy.db` (SQLite, mode 0600), `keys/jwt.key` and `keys/secrets.key` (0600; back up `secrets.key` with the database, without it stored credentials cannot be decrypted), `repositories/` (one checkout per project id), `known_hosts` (SSH host keys of Git remotes), `static/`, `certs/game/` (the game certificate and key, mounted read-only into server containers; its path must not contain `,`, `"`, `'` or `=`), `logs/jobs/`, `docker-build/` (dedicated Docker client config for builds), and `tmp/` (scratch space, including each build's private directory with its generated Dockerfile and extracted add-ons; leftovers are removed at startup).

## License

Licensed under the [Apache License 2.0](LICENSE).
