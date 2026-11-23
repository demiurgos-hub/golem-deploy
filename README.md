# Golem Deploy

Golem Deploy is an open-source tool for self-hosting games built on [Golem Engine](https://golemengine.dev), the Go engine for server-authoritative multiplayer games. It runs on a Linux host next to Docker and is managed through an embedded web client.

**Status: early development.** Golem Deploy is not yet usable in production. APIs, storage formats and configuration will change without migration paths until a first release. The daemon core (accounts, sessions, API tokens, jobs, encrypted secrets) has been rebuilt; projects, the Docker runner, servers, images, static files and volumes are being rebuilt on top of it and their API routes currently answer `501 not_implemented`. The web client still targets the previous API and does not work against this daemon yet.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## What works today

A single `golem-deploy` binary serves a versioned HTTP API under `/api/v1` and the embedded React web client.

- **Accounts:** admins created on the host with `golem-deploy admin create`, optional self-registration with admin approval (off by default), roles `admin` and `developer`, password change, logout.
- **Sessions and tokens:** short-lived session JWTs re-validated against the database on every request, revocable API tokens (`gdp_...`) that never exceed the developer role, login rate limiting.
- **Jobs:** background operations with persisted status, per-job logs that can be streamed, cancellation and per-subject serialisation.
- **Secrets at rest:** Git credentials and server environment values are stored AES-256-GCM encrypted.

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

Without a TLS certificate the daemon only binds loopback addresses (`localhost` must resolve only to loopback addresses); to expose it on the network either configure TLS or put it behind a TLS-terminating reverse proxy and pass `--insecure-http`. It shuts down gracefully on SIGINT/SIGTERM. If no admin account exists it logs how to create one and keeps serving.

Requests are only served for IP addresses, `localhost` and the names given with `--public-host`; any other `Host` gets `421` (this stops DNS-rebinding attacks from web pages against a panel on loopback). If you reach the panel by a domain name, list it in `--public-host`.

Behind a reverse proxy, pass its address range with `--trusted-proxy` (for example `--trusted-proxy 127.0.0.1/32` for a proxy on the same host) so that login rate limits and the access log use the real client address from `X-Forwarded-For`. Without it, forwarding headers are ignored and all clients share the proxy's address for rate limiting. Never list networks that untrusted clients can connect from.

The data directory (created with mode 0700 if it does not exist; on Unix an existing one must not be accessible by group or others, and the daemon refuses to start with the `chmod` command to run instead of changing it) contains `golem-deploy.db` (SQLite, mode 0600), `keys/jwt.key` and `keys/secrets.key` (0600; back up `secrets.key` with the database, without it stored credentials cannot be decrypted), `repositories/`, `static/`, `logs/jobs/` and `tmp/`.

## License

Licensed under the [Apache License 2.0](LICENSE).
