# Making a game deployable

This guide is for a game developer who has a [Golem Engine](https://golemengine.dev) game and wants Golem Deploy to build and run it. It covers what the repository must contain, the two calls the game's server must make, what Golem Deploy does to build and run the game, and the failures you are most likely to meet.

The reference game, [Lantern Ring](https://github.com/demiurgos-hub/golem-reference-game), is a complete example: it depends only on released Golem Engine packages, commits its generated code and is configured only through the engine's environment variables. Copy its layout when in doubt.

For depth, see [BUILDS.md](BUILDS.md) (the build pipeline), [SERVERS.md](SERVERS.md) (how servers run), [BACKUPS.md](BACKUPS.md) (saved data), [CERTIFICATES.md](CERTIFICATES.md) (TLS) and [INSPECTOR.md](INSPECTOR.md).

## The path from a repository to a running game

1. An admin creates a **project** for the game's Git repository. Golem Deploy clones it into its data directory ([REPOSITORIES.md](REPOSITORIES.md)).
2. A developer starts a **build** of the project's current commit. The build turns the checkout into a Docker image.
3. A developer creates a **server** from a successful build and starts it. The server is one container running that image, reachable by players on one host port.

## 1. What the repository must contain

Golem Deploy looks for the game in the **game root**: the repository root, or the directory that `root` in `golem-deploy.yaml` names.

| Item | Requirement |
|------|-------------|
| `golem.yaml` | The Golem Engine project file, in the game root, at most 64 KiB. |
| `go.mod` | In the game root, or in the directory `server.module` names. It must require `github.com/demiurgos-hub/golem-engine` at **v0.4.0-rc.1 or newer**: a release, a prerelease or a pseudo-version. Versions with `+build` metadata are refused. Its `go` directive (or a newer `toolchain` directive) picks the Go image, `golang:<version>-bookworm`. |
| Server package | A Go `package main` that runs the game (see [the two calls](#2-the-two-calls-in-the-games-server)). |
| Web client | Optional: a `package.json` in the game root (or in `web.dir`). |
| Generated code | Committed, or generated during the build (see below). |
| `golem-deploy.yaml` | Optional, at the repository root (see [below](#golem-deployyaml)). |

### Released packages only

The build runs on a fresh copy of the repository, with nothing else next to it. So every dependency must come from a registry:

- **Go:** no `replace` directive may point the engine module, or any of its submodules, at a local path. The preflight refuses it with `go.mod replaces github.com/demiurgos-hub/golem-engine with a local path; remove the replace directive and require a released Golem Engine version`. Replacing the root engine module with another module is refused too. The engine's own examples use local replacements; a standalone game must not.
- **npm:** require `golem-engine` and `golem-threlte` at a published version (the reference game uses `0.4.0-rc.1`), not `file:` paths. Golem Deploy does not check this before the build, but a `file:` path outside the repository does not exist in the build, so `npm ci` fails.

### Generated code

`golem-bake` generates the engine's code from your schemas, into the outputs `golem.yaml` names (`proto.out` and each integration's `out`). The build decides what to do by looking at those outputs:

- **When any of them exists in the checkout**, the code is treated as committed. The build runs `golem-bake --check` and fails if the code is out of date. Run `golem-bake` and commit the result.
- **When none exists**, the build runs `golem-bake` to generate it.

Committing the generated code is recommended. It is also the only way the build learns the game's **schema fingerprint** before the game first runs: the build reads the `SchemaFingerprint` constant from `golem_helpers.go` in the `go-server` integration's output directory. Golem Deploy uses the fingerprint to protect saved data (see [Saved data](#7-saved-data)).

Baking a JavaScript integration runs `tsc` from `node_modules`, so `typescript` must be among the project's npm dependencies.

### Where the server package is found

In this order, relative to the directory of `go.mod` (the game root, or `server.module`):

1. `server.package` from `golem-deploy.yaml`;
2. `./cmd/server`;
3. the only directory under `./cmd` that holds Go files (not counting `_test.go` files);
4. the directory of `go.mod`, when it holds Go files.

The package must be `package main`.

### A monorepo with a Unity client

When `golem.yaml` sits at the repository root and covers both a Go server and a Unity client, the Go module usually lives in a subdirectory, and the Unity project brings many gigabytes of Git LFS files the server does not need. Two keys in `golem-deploy.yaml` handle this:

```yaml
server:
  module: workers/game-server      # directory of go.mod, relative to the game root
lfs:
  include:                         # only these Git LFS paths are downloaded
    - content/**
    - workers/unity/Assets/AssetsByType/Databases/*.bytes
```

The build runs `go build` in `workers/game-server`; `golem-bake` and the web build still run in the game root. Clones and pulls download only the listed Git LFS files, which keeps the checkout under the host's size limit. The other Git LFS files stay pointer files, so nothing in the build may copy or read them.

### The web client

When the game root (or `web.dir`) has a `package.json`, the build installs its dependencies, with `npm ci` when there is a `package-lock.json` or `npm-shrinkwrap.json` and with `npm install` otherwise. Commit a lockfile for reproducible builds. When `web.dir` is not the game root and the game root also has a `package.json`, both are installed.

When `package.json` has a non-empty `build` script, the build runs `npm run build`, and the result must end up in `web/dist` (or in `web.dist`). The image contains that directory at the same path below `/app`, the server's working directory, so the server finds it at the relative path `web/dist`.

### `golem-deploy.yaml`

An optional file at the **repository root**. Unknown keys are errors. It is at most 32 KiB and one YAML document of at most 1024 nodes, 32 levels and 128 keys per mapping, without aliases. Every field:

| Field | Default | Meaning |
|-------|---------|---------|
| `root` | `.` | Directory of `golem.yaml`, relative to the repository root. |
| `server.module` | `.` | Directory of `go.mod`, relative to the game root. The server is built there. |
| `server.package` | detected | Go package of the server, relative to the directory of `go.mod`. |
| `server.args` | none | Arguments passed to the server binary. |
| `web.dir` | the game root | Directory of `package.json`, relative to the game root. |
| `web.build` | `npm run build` when `package.json` has a `build` script, otherwise none | The web build command: a list of words, or a string split on spaces. |
| `web.dist` | `web/dist` when there is a web build | The built client, relative to the game root. Set it without a build to ship committed assets. |
| `dockerfile` | none | A custom Dockerfile, relative to the game root (see below). |
| `transport.webtransport_path` | `/wt` | The WebTransport path, when the game changed `golem.ServerConfig.Path`. |
| `transport.websocket_path` | `/api/ws` | The WebSocket fallback path, when the game changed `host.GameOptions.WebSocketPath`. |
| `lfs.include` | all | The Git LFS path patterns that clones and pulls download. |

Rules for the values:

- **Paths** are relative, use `/` and consist of letters, digits, `.`, `_` and `-`. A segment may not start with `-`, end with `.`, be `.` or `..`, or be `.git`. At most 16 segments and 255 bytes. A leading `./` and a trailing `/` are ignored.
- **Commands and arguments** run without a shell: no variable expansion, no `&&`. Each word is single-line text of at most 1024 bytes, at most 32 words. For shell features, write the shell out: `build: ["sh", "-c", "npm run lint && npm run build"]`.
- **`lfs.include` patterns** are relative paths of `/`-separated segments of letters, digits, `.`, `_`, `-` and `*`. A segment may not be empty, be `.` or `..`, or start with `-`. No comma, no white space. 1 to 32 patterns, each at most 255 bytes and 16 segments, and at most 4096 bytes in total once joined with commas. Quote a pattern that starts with `*`, because YAML reads a bare `*` as an alias.
- **Transport paths** are `/` followed by at most 8 segments of letters, digits, `.`, `_`, `~` and `-`, at most 128 bytes. Golem Deploy cannot read the paths from your code: if the game serves other paths than the file declares, the server still starts but clients cannot connect.

An example with every field:

```yaml
root: games/arena
server:
  module: .
  package: ./cmd/arena
  args: ["-assets", "web/dist"]
web:
  dir: .
  build: npm run build
  dist: web/dist
# dockerfile: deploy/Dockerfile
transport:
  webtransport_path: /wt
  websocket_path: /api/ws
lfs:
  include:
    - assets/server/**
    - "*.bytes"
```

### The custom Dockerfile

`dockerfile:` replaces the generated Dockerfile with your own. **Only an admin can allow it**, by setting `allow_custom_dockerfile` on the project (`PATCH /api/v1/projects/{id}`, or the switch on the project page). Without it the build fails with `the repository requests a custom Dockerfile; ask an admin to enable allow_custom_dockerfile for this project`. Admins should allow it only for repositories they trust with the host's shared BuildKit builder and cache; see [SECURITY.md](SECURITY.md#builds).

The preflight still requires `golem.yaml` and a valid `go.mod` (in `server.module` when it is set). The Dockerfile may not select a BuildKit frontend (no `# syntax=` line). The image it produces must listen on port 7777, run as a non-root user, have a `/data` directory writable by uid 65532, declare at most 8 `VOLUME`s and set no labels starting with `dev.golemengine.deploy.`. The details are in [BUILDS.md](BUILDS.md#custom-dockerfile).

## 2. The two calls in the game's server

Golem Deploy configures a game only through `GOLEM_*` environment variables, the engine's deploy contract. The game takes part in that contract with two calls.

**`golem.ServerConfigFromEnv(base)`** returns your base `golem.ServerConfig` with fields replaced from the environment: the WebTransport listen address (`GOLEM_ADDR`), the TLS files and their reload interval (`GOLEM_TLS_*`), the allowed browser origins (`GOLEM_WT_ALLOWED_ORIGINS`) and the tick rate (`GOLEM_TICK_RATE`). Unset variables keep your values; malformed ones are logged and ignored.

**The generated `Runtime.Host(ctx, opts)`** runs the server until it gets SIGINT or SIGTERM, and wires up everything else from the environment:

- the **sidecar**, an HTTP(S) server on `GOLEM_HTTP_ADDR` that serves the realtime config at `/api/realtime-config`, the WebSocket endpoint at `/api/ws`, your `API` handler and your web client;
- the **realtime config** (`GOLEM_PUBLIC_URL`, `GOLEM_WS_FALLBACK_URL`, `GOLEM_TLS_PINNED`), which tells clients where to connect;
- the **admin listener** (`GOLEM_ADMIN_ADDR`, `GOLEM_ADMIN_TOKEN`) that Golem Deploy uses to check readiness and take snapshots;
- **snapshots** (`GOLEM_SNAPSHOT_DIR`, `GOLEM_SNAPSHOT_INTERVAL`): the saved world is loaded before the server starts and saved after it stops, and on the autosave period;
- the **Inspector agent** (`GOLEM_INSPECTOR_*`), when those variables are set.

The WebTransport primary transport is required: with a WebSocket fallback URL set, a server whose primary transport is not WebTransport fails at startup.

From the reference game's [`cmd/server/main.go`](https://github.com/demiurgos-hub/golem-reference-game/blob/main/cmd/server/main.go), shortened:

```go
// run hosts the game until ctx is cancelled or SIGINT/SIGTERM arrives.
func run(ctx context.Context, addr, assets string) error {
	cfg := serverConfig(addr)
	g := newGame(cfg) // generated.NewServer(cfg), hooks and command handlers; no entities yet
	return g.runtime.Host(ctx, generated.HostOptions{
		Name:         gameName,
		HTTPAddr:     addr,   // sidecar address for local runs; GOLEM_HTTP_ADDR overrides it
		AssetsDir:    assets, // "web/dist", relative to the working directory
		Realtime:     developmentRealtime(cfg),
		AfterRestore: g.afterRestore,
	})
}

func serverConfig(addr string) golem.ServerConfig {
	cfg := golem.ServerConfigFromEnv(golem.ServerConfig{
		TickRate:  20,
		Addr:      addr, // local development address; GOLEM_ADDR overrides it
		Transport: golem.TransportWebTransport,
		Path:      webTransportPath, // "/wt"
	})
	cfg.DevSelfSignedCert = cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" && isLoopbackAddr(cfg.Addr)
	return cfg
}
```

`AfterRestore(restored bool)` runs once before the server starts, after the snapshot was loaded or not. Seed a fresh world there when `restored` is false, and adopt the restored entities when it is true. Do not create entities before `Host`, or a restored snapshot and your seed collide.

`developmentRealtime` and `DevSelfSignedCert` give local runs working URLs and the engine's development certificate, but only when the address is loopback. Under Golem Deploy the address is `:7777`, so the game uses the files and URLs Golem Deploy provides, and a deployment without a certificate fails to start instead of serving a throwaway one.

## 3. What Golem Deploy does to build it

A build runs as a background job. Its log is on the build's page and at `GET /api/v1/jobs/{id}/log`.

1. With "pull" requested, the checkout is updated first.
2. The build waits for the build slot: one build runs at a time, and at most 4 may be queued or running.
3. The **preflight** checks that the checkout is idle and at the recorded commit, then reads and validates `golem-deploy.yaml`, `.dockerignore`, `golem.yaml`, `go.mod`, the `package.json` files and a custom Dockerfile. Nothing in the repository runs on the host.
4. Golem Deploy writes the generated Dockerfile (printed at the top of the build log) and extracts the [static add-ons](#9-static-add-ons) into a private directory. Nothing is written into the checkout.
5. `docker build` runs with the repository root as its context. `.git` and `node_modules` are always excluded, after your own `.dockerignore`.
6. The image is verified by its id and labels and recorded on the build.

The generated Dockerfile has two stages:

- **Build stage**, `golang:<go version>-bookworm`, with Node.js 24 and npm copied from `node:24-bookworm-slim` when there is a `package.json`. It copies the repository and the static add-ons, then runs `npm ci` (or `npm install`), `go run github.com/demiurgos-hub/golem-engine/cmd/golem-bake@<engine version>` (with `--check` when the generated code is committed), the web build, a check that the web client directory exists, and `CGO_ENABLED=0 go build -trimpath` of the server package in the directory of `go.mod`. Go and npm caches are kept per project between builds.
- **Final stage**, `gcr.io/distroless/static-debian12:nonroot`: no shell and no package manager. It contains the server at `/usr/local/bin/golem-server`, the web client at `/app/<web.dist>`, the static add-ons at `/app/<target>`, and an empty `/data` owned by uid 65532. The working directory is `/app` (your game root), the user is `65532:65532`, and the entrypoint is the server with `server.args`.

Limits:

| Limit | Value |
|-------|-------|
| Time per build | `--build-timeout`, default 30 minutes. |
| Builds at once | 1 running, at most 4 queued or running. |
| Files read by the preflight | `golem-deploy.yaml` 32 KiB, `golem.yaml` 64 KiB, `.dockerignore` 64 KiB, `go.mod`, `package.json` and a custom Dockerfile 1 MiB each. Symbolic links are refused. |
| Checkout size | `--max-repo-size-mb`, default 10240 MiB, including Git LFS content. Use `lfs.include` to download only the Git LFS files the server needs. |
| Network | Builds download base images (Docker Hub, gcr.io), Go modules (the Go module proxy) and npm packages. |

Golem Deploy keeps the newest successful builds of each project with their images (5 by default), plus every build a server uses.

## 4. What Golem Deploy does to run it

A server is one container. Every container is created with:

- a **read-only root file system**. The game can write only to `/tmp` (a 64 MiB tmpfs), to `/data` and to tmpfs mounts on paths the image declares with `VOLUME`;
- user **65532:65532**, all Linux capabilities dropped, `no-new-privileges`;
- a memory limit (`--server-memory-mb`, default 2048 MiB, no extra swap), a CPU limit (`--server-cpus`, default 2) and at most 512 processes;
- the server's **data volume** at `/data`;
- its **certificate** mounted read-only at `/run/golem-deploy/tls`;
- one host port `N` from the port range (default 7000-7099) published as `N/udp` and `N/tcp` to the **game port 7777** in the container;
- the **admin port 7778** published on `127.0.0.1` only, for the daemon;
- the `golem-deploy` network, on which containers cannot reach each other.

A start succeeds only once the game's admin endpoint reports ready, within `--start-timeout` (default 60 seconds). A stop sends SIGTERM and gives the game 30 seconds to save and exit. After a successful start, Docker restarts a crashed game up to 5 times. See [SERVERS.md](SERVERS.md#container-hardening) for the full list.

## 5. The environment Golem Deploy injects

These variables come from `internal/servers/engineenv.go`. `<host>` is the **game host** (the admin's `game_host` setting, or the first `--public-host`), and `<port>` is the server's host port.

| Variable | Value | What it means for the game |
|----------|-------|----------------------------|
| `GOLEM_ADDR` | `:7777` | WebTransport listens on UDP 7777 in the container. |
| `GOLEM_HTTP_ADDR` | `:7777` | The sidecar listens on TCP 7777: the page, the realtime config, the WebSocket fallback and your `API`. |
| `GOLEM_TLS_CERT_FILE` | `/run/golem-deploy/tls/tls.crt` | Certificate chain for WebTransport and the sidecar. |
| `GOLEM_TLS_KEY_FILE` | `/run/golem-deploy/tls/tls.key` | Its private key. |
| `GOLEM_TLS_RELOAD_INTERVAL` | `30s` | The engine checks the files every 30 seconds and loads a replaced certificate without a restart. |
| `GOLEM_PUBLIC_URL` | `https://<host>:<port>/wt` | The WebTransport URL in the realtime config. The path is `transport.webtransport_path` when set. |
| `GOLEM_WS_FALLBACK_URL` | `wss://<host>:<port>/api/ws` | The WebSocket fallback URL in the realtime config. The path is `transport.websocket_path` when set. |
| `GOLEM_WT_ALLOWED_ORIGINS` | `https://<host>:<port>`, then the server's allowed origins, then the panel's test origins | Browser origins allowed to connect over WebTransport and WebSocket. |
| `GOLEM_TLS_PINNED` | `1` with a self-signed certificate, `0` otherwise | With `1` the realtime config publishes the certificate's hashes so browsers can pin it. |
| `GOLEM_ADMIN_ADDR` | `:7778` | The admin listener. |
| `GOLEM_ADMIN_TOKEN` | 64 random characters, new for each container | The admin bearer token. Never shown to anyone. |
| `GOLEM_SNAPSHOT_DIR` | `/data` | Snapshots are on: `world.snapshot` lives on the data volume. |
| `GOLEM_SNAPSHOT_INTERVAL` | the server's autosave period, e.g. `300s`, or `0` | How often the running game saves. `0` saves only at stop and for backups. |
| `GOLEM_INSPECTOR_HUB_URL` | `ws://<agent host>:<inspector port>/ws/agent` | Only when the server's Inspector switch is on and the Inspector is on and reachable: where the agent connects. |
| `GOLEM_INSPECTOR_TOKEN` | the server's own agent token | Same condition: authenticates the agent. |
| `GOLEM_INSPECTOR_TARGET_ID` | the server name | Same condition: the name shown in the Inspector. |
| `GOLEM_INSPECTOR_LABELS` | `project=<name>,commit=<commit>,server=<id>` | Same condition. Values containing `,` or `=` are left out. |

**Overriding.** The server's own environment variables (set by a developer on the server page, `PATCH /api/v1/servers/{id}`) are added for the game's own settings. A name may not start with `GOLEM_`, in any case: the whole prefix is reserved, also for engine variables Golem Deploy does not set. So `GOLEM_TICK_RATE` and `GOLEM_INSPECTOR_RATE_HZ` cannot be set per server; set the tick rate in your code. Names match `^[A-Za-z_][A-Za-z0-9_]{0,127}$`, and values are single-line text. A server holds at most 128 variables and 64 KiB in total. Changing them marks a running server "restart required".

Values are encrypted at rest and never shown again, but the game itself can read and print them. They protect secrets from other servers and from panel users, not from whoever writes the game's code.

## 6. The web client

The game serves its own page: `Runtime.Host` serves the built client from `HostOptions.AssetsDir` (or an embedded `Assets` file system) on the sidecar, so players open `https://<host>:<port>/`. The page, the realtime config and both transports then share one origin.

In the browser, read the realtime config and follow it. With the `golem-engine` npm package, as in the reference game's [`web/src/connection.ts`](https://github.com/demiurgos-hub/golem-reference-game/blob/main/web/src/connection.ts):

```ts
import { connectPlanFromRealtimeConfig, fetchRealtimeConfig } from 'golem-engine';

const config = await fetchRealtimeConfig('/api/realtime-config');
const plan = connectPlanFromRealtimeConfig(config);
```

The plan tries **WebTransport** first (over UDP) and falls back to **WebSocket** (over TCP, on the same port) when the browser lacks WebTransport or cannot reach it. Do not hard-code URLs: the server decides them, and with a pinned certificate the realtime config also carries the certificate hashes, which change when the certificate rotates. Fetch the config again on every reconnect.

**Allowed origins.** The game's own origin is always allowed. When the page is served somewhere else (your website, for example), add that origin to the server's **allowed origins** (at most 16, each `https://host[:port]` or `http://host[:port]`, no path). The panel's own origins are added so its connection test works.

**Certificates.** Which certificate mode an admin picks decides what works in a browser:

- `acme` or `custom` (a trusted certificate): the page, the WebSocket fallback and WebTransport all work without warnings. Use this for public play.
- `self_signed` (the default): WebTransport works by pinning the certificate's hash, from a page served over trusted HTTPS. The game's own page shows a certificate warning, and the WebSocket fallback fails unless the browser trusts the certificate. Use it for development, LAN play and native clients that pin.

See [CERTIFICATES.md](CERTIFICATES.md).

## 7. Saved data

- The game's snapshot directory is `/data`, a Docker volume that belongs to the server. It survives restarts, new builds and panel upgrades.
- `Runtime.Host` loads `/data/world.snapshot` before the server starts, saves it on the autosave period (default 300 seconds, 0 or 10 to 86400 seconds) and when the server stops. Golem Deploy stops servers with SIGTERM, so a stop from the panel saves.
- **The schema fingerprint guard.** The engine refuses to load a snapshot written with other schemas and exits at startup, naming both fingerprints. Golem Deploy knows the fingerprint of the saved data (what the game reported when it last became ready) and of each build (from the committed generated code, or from the first game that ran it). When both are known and differ, a start fails before the running server is touched, with three ways out: assign a build with the saved schema, restore a matching backup, or reset the data. So when you change your schemas, plan for the saved world: reset it, or migrate in `AfterRestore` with schemas that still load.
- **Backups** ask the running game for a fresh snapshot first (admin `POST /snapshot`), then archive the regular files of `/data`. Links, pipes and devices are left out. Keep everything the game must remember as regular files in `/data`. See [BACKUPS.md](BACKUPS.md).

## 8. The admin endpoint and the Inspector

`Runtime.Host` opens the admin listener from `GOLEM_ADMIN_ADDR` and `GOLEM_ADMIN_TOKEN`. Golem Deploy reaches it only on `127.0.0.1` and uses:

| Endpoint | Used for |
|----------|----------|
| `GET /ready` | Health-gated starts, recovery after a panel restart, the runtime view. |
| `GET /info` | The schema fingerprint the game reports, and the runtime view. |
| `GET /stats` | Players, entities and tick statistics in the runtime view. |
| `POST /snapshot` | A fresh snapshot before a backup. With snapshots on, the engine provides it. |

You get all of this from `Runtime.Host` without extra code. A game that starts its own admin listener must answer these the way the engine does.

**The Inspector switch.** Each server has an Inspector setting. When it is on, an admin has turned the Inspector on, and containers can reach it, the server gets the `GOLEM_INSPECTOR_*` variables at its next start, and its agent appears in the Inspector. The generated `Runtime.Host` attaches the agent; no code is needed. See [INSPECTOR.md](INSPECTOR.md).

## 9. Static add-ons

Large assets that do not belong in Git can be uploaded to the panel's static file store as ZIP files and attached to a project by an admin, each with a `target` directory below the game root. Every build extracts them before the web build (so the client build can use them) and the image has them at `/app/<target>`. Two add-ons may not provide the same file. Extraction is limited to 4 GiB and 20000 entries per build and 2 GiB per file. See [BUILDS.md](BUILDS.md#static-add-ons).

## 10. Checklist

- [ ] `golem.yaml` and `go.mod` in the game root (or `root` set in `golem-deploy.yaml`).
- [ ] `go.mod` requires `github.com/demiurgos-hub/golem-engine` v0.4.0-rc.1 or newer, with no local `replace`.
- [ ] npm dependencies are published versions, and a lockfile is committed.
- [ ] Generated code committed, and `golem-bake --check` passes on a fresh clone.
- [ ] The server is `package main` in `./cmd/server` (or `server.package` is set).
- [ ] The server builds its config with `golem.ServerConfigFromEnv` and runs with the generated `Runtime.Host`.
- [ ] The primary transport is WebTransport.
- [ ] The world is seeded in `AfterRestore`, not before `Host`.
- [ ] The web client builds to `web/dist` (or `web.dist`) and is served with `HostOptions.AssetsDir`.
- [ ] The browser uses `fetchRealtimeConfig('/api/realtime-config')` and the connect plan, not fixed URLs.
- [ ] Custom endpoint paths are declared under `transport` in `golem-deploy.yaml`.
- [ ] The game writes only to `/data` and `/tmp`.
- [ ] The game's own settings use variable names that do not start with `GOLEM_`.

## 11. Common build and start failures

Build failures appear as the build's detail; the build log has the full output. Start failures appear as the job's detail, and the job log has the game's last lines. The real messages, with `<...>` for the parts that vary:

| Message | What it means |
|---------|---------------|
| `golem.yaml was not found in the game root "<root>"; set root in golem-deploy.yaml if the game lives in a subdirectory` | The game is in a subdirectory, or `golem.yaml` is missing. |
| `go.mod requires github.com/demiurgos-hub/golem-engine <version>; Golem Deploy needs v0.4.0-rc.1 or newer` | Upgrade the engine. |
| `go.mod replaces github.com/demiurgos-hub/golem-engine with a local path; remove the replace directive and require a released Golem Engine version` | See [Released packages only](#released-packages-only). |
| `several commands were found (<list>); set server.package in golem-deploy.yaml` | More than one directory under `./cmd` has Go files. |
| `the server package was not found (looked for ./cmd/server, a single ./cmd/* command and Go files in the game root); set server.package in golem-deploy.yaml` | No server package where Golem Deploy looks. |
| `golem-deploy.yaml is not valid: <parser message>` | A YAML error or an unknown key. |
| `<path> is a symbolic link; symbolic links are not followed` | A file the preflight reads, or a directory on its path, is a link. |
| `the repository requests a custom Dockerfile; ask an admin to enable allow_custom_dockerfile for this project` | See [The custom Dockerfile](#the-custom-dockerfile). |
| `docker build failed; see the build log` | A build step failed. Common lines in the log follow. |
| `golem-bake: generated code is out of date (<n> difference(s)):` (in the build log) | Run `golem-bake` and commit. |
| `golem-deploy: the web client directory web/dist (web.dist) does not exist after the web build` (in the build log) | The web build writes somewhere else; set `web.dist`. |
| `the server must be a package main` (in the build log) | The server package is a library. |
| `the build exceeded the time limit of 30m0s` | The build took longer than `--build-timeout`. |
| `the game did not become ready within 1m0s and its admin endpoint never answered: the game must use the Golem Engine deploy contract (golem.ServerConfigFromEnv and the generated Runtime.Host); see the server log (GET /api/v1/servers/<id>/logs) and the job log` | The game does not open the admin listener: it does not use `Runtime.Host`, or it is stuck before it. |
| `the container exited before the game became ready (exit code <n>); see the server log (GET /api/v1/servers/<id>/logs) and the job log` | The game crashed at startup. Its last lines are in the job log. |
| `golem/host: snapshot /data/world.snapshot has schema fingerprint <a> but this server's schema fingerprint is <b>; ...` (in the game's log) | The saved world was written with other schemas. See [Saved data](#7-saved-data). |
| `golem/host: a WebSocket fallback URL (GOLEM_WS_FALLBACK_URL) requires a WebTransport primary transport, ...` (in the game's log) | Use `golem.TransportWebTransport`. |
| `the server's saved data has schema <a> but build <n> has schema <b>, so the game cannot load it; ...` | The schema guard stopped the start; the message lists the three ways out. |

More failures, with how to check each, are in [TROUBLESHOOTING.md](TROUBLESHOOTING.md#builds-fail).
