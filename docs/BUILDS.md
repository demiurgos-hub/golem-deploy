# Builds

A build turns a project's checkout into a Docker image that game servers run. It is started with `POST /api/v1/projects/{id}/builds` (see [API.md](API.md#builds)), runs as a `project.build` job and records a `Build` with the image id. This document describes what a game repository must contain, the optional `golem-deploy.yaml`, how the build runs and its security model.

## What a game repository must contain

In the **game root** (the repository root, or the directory `root` names in `golem-deploy.yaml`):

- `golem.yaml`, the Golem Engine project file.
- `go.mod` requiring `github.com/demiurgos-hub/golem-engine` at **v0.4.0-rc.1 or newer** (a release, prerelease or pseudo-version; versions with `+build` metadata are refused). Engine modules must not be replaced with local paths (`replace github.com/demiurgos-hub/golem-engine => ../golem-engine` is refused, as is any replacement of the root engine module): the monorepo examples use such replacements, a standalone game requires released versions instead (for npm: `golem-engine@0.4.0-rc.1` and `golem-threlte@0.4.0-rc.1` from the `next` dist-tag instead of `file:` dependencies). The `go` directive (or a newer `toolchain` directive) selects the Go image (`golang:<version>-bookworm`).
- A server package whose `main` runs the game through the generated `Runtime.Host`, found in this order: `server.package` from `golem-deploy.yaml`, `./cmd/server`, the only directory under `./cmd` with Go files, the game root when it has Go files. The server listens on container port **7777** (TCP for the HTTP sidecar and WebSocket, UDP for WebTransport); the daemon configures it through `GOLEM_*` environment variables (see [SERVERS.md](SERVERS.md)).
- Optionally a browser client: a `package.json` in the game root (or `web.dir`). With a lockfile (`package-lock.json` or `npm-shrinkwrap.json`) dependencies are installed with `npm ci`, otherwise with `npm install` (the build log notes it; commit a lockfile for reproducible builds). When `package.json` has a `build` script, `npm run build` builds the client, which must end up in `web/dist` (or `web.dist`).
- Generated Golem Engine code may be committed or not. When any output `golem.yaml` configures (`proto.out`, an integration's `out`) exists in the checkout, the build verifies it with `golem-bake --check` and fails if it is stale (run `golem-bake` and commit); otherwise the build generates it with `golem-bake`. Baking a JavaScript integration needs `typescript` among the project's npm dependencies (it runs `tsc` from `node_modules`).

`internal/build/testdata/game` in this repository is a minimal working example.

## `golem-deploy.yaml`

Optional, at the **repository root**. Unknown keys are errors; the file is at most 64 KiB and a single YAML document. Every path is relative, uses `/`, consists of segments of letters, digits, `.`, `_` and `-` (not starting with `-`, not `.`/`..`, not `.git`), at most 16 segments and 255 bytes; a leading `./` and a trailing `/` are ignored.

```yaml
# Directory of golem.yaml and go.mod, relative to the repository root (default ".").
root: games/arena

server:
  # Go package of the server, relative to the game root (default: detected, see above).
  package: ./cmd/arena
  # Arguments passed to the server binary (exec form, no shell; default none).
  args: ["-assets", "web/dist"]

web:
  # Directory of package.json, relative to the game root (default: the game root).
  dir: .
  # Web build command, as a list of words or a string split on spaces; no shell
  # (default: "npm run build" when package.json has a build script, otherwise none).
  build: npm run build
  # The built client, relative to the game root; copied into the image at the same
  # relative path (default "web/dist" when there is a web build; set it without a
  # build to ship committed assets).
  dist: web/dist

# A custom Dockerfile, relative to the game root, used instead of the generated one
# (see "Custom Dockerfile").
# dockerfile: deploy/Dockerfile

transport:
  # Only when the game changed the engine's endpoint paths: the WebTransport path
  # (golem.ServerConfig.Path, default "/wt") and the WebSocket fallback path
  # (host.GameOptions.WebSocketPath, default "/api/ws").
  webtransport_path: /wt
  websocket_path: /api/ws
```

`transport` paths are recorded on the build (`webtransport_path`, `websocket_path`) and become the URLs servers running the build publish (`GOLEM_PUBLIC_URL`, `GOLEM_WS_FALLBACK_URL`, see [SERVERS.md](SERVERS.md#engine-deploy-contract)). Each is `/` followed by at most 8 segments of letters, digits, `.`, `_`, `~` and `-` (no `.`/`..`, no empty segment, no trailing `/`, no query), at most 128 bytes. The daemon cannot read the paths from the game's code: if the game serves other paths than the build declares, clients fail to connect while the start still succeeds (the admin endpoint reports ready either way).

Commands and arguments are passed in Dockerfile exec form (a JSON array): there is no shell, no variable expansion and no way to add a line to the Dockerfile. Each word is single-line UTF-8 without control or bidirectional characters, at most 1024 bytes, at most 32 words. To use shell features, write them out: `build: ["sh", "-c", "npm run lint && npm run build"]`.

## How a build runs

1. With `pull: true`, the checkout is updated first (exactly like `POST /projects/{id}/pull`), in the same job.
2. The build waits for the daemon's build slot (one build at a time; at most 4 may be queued or running).
3. The daemon reads the few files above from the checkout and validates them (the *preflight*). Problems are reported as the build's `detail`, for example `golem.yaml was not found in the game root "."`.
4. In a private directory below the data directory's `tmp/` it writes the generated `Dockerfile`, its `Dockerfile.dockerignore` and the extracted static add-ons. Nothing is written into the checkout.
5. `docker build` runs with the repository root as context. The image is tagged `golem-deploy/<project>:<commit[:12]>` and labelled from the command line (managed, instance, project id, commit, build id, engine version). The build's output is the job log (sanitised, host paths replaced by placeholders such as `<repository>`).
6. The image is looked up by its exact id and must carry exactly those labels; then the build is recorded as successful and the retention policy runs (keep the newest `--build-retention` successful builds per project, default 5, plus every build a server uses; keep the newest 20 failed or cancelled builds).

The generated Dockerfile (printed at the top of every build log) is a two-stage build:

- **Build stage** `golang:<go version>-bookworm`, with Node.js 24 and npm copied from `node:24-bookworm-slim` when there is a `package.json`. The repository is copied to `/src`, static add-ons on top of the game root, then: `npm ci` (or `npm install`) in each `package.json` directory, `go run github.com/demiurgos-hub/golem-engine/cmd/golem-bake@<engine version>` (with `--check` when generated code is committed), the web build, a check that the web client directory exists, and `CGO_ENABLED=0 go build -trimpath` of the server package. Go's module and build caches and npm's cache are BuildKit cache mounts whose ids contain a hash of the installation's instance id and the project id, so projects never share (or poison) each other's caches. `GOFLAGS=-mod=mod` lets the go command complete a missing `go.sum` (downloads are still verified against the checksum database).
- **Final stage** `gcr.io/distroless/static-debian12:nonroot`: working directory `/app` (the game root), the server at `/usr/local/bin/golem-server`, the web client at `/app/<web.dist>`, static add-ons at `/app/<target>`, an empty `/data` owned by uid 65532, `USER 65532:65532`, `EXPOSE 7777/udp 7777/tcp` and the exec-form `ENTRYPOINT ["/usr/local/bin/golem-server", <server.args>...]`.

Every build's `.dockerignore` is the repository's own `.dockerignore` (if any) followed by `.git`, `**/.git`, `node_modules` and `**/node_modules`, which therefore always win.

### Custom Dockerfile

With `dockerfile:` set, the repository's Dockerfile is used as it is (copied out of the checkout; a `# syntax=` directive in it makes BuildKit pull and run that frontend image, with the same restrictions; the context is still the repository root, with the `.dockerignore` above). The preflight still requires `golem.yaml` and a valid `go.mod` (the engine version labels the image), and labels are still set from the command line. Your Dockerfile must produce an image that:

- listens on port 7777 (TCP and/or UDP) and is configured through `GOLEM_*` environment variables;
- runs as a non-root user (the daemon runs every server as uid/gid 65532 with a read-only root filesystem, no capabilities and `no-new-privileges`; write only to `/tmp`, `/data` or declared volumes);
- declares at most 8 `VOLUME`s (each becomes a small tmpfs, see [SERVERS.md](SERVERS.md));
- sets no labels starting with `dev.golemengine.deploy.` (the build fails and the image is removed).

The static add-ons are available as the named build context `static` (`COPY --from=static / /app/`). No build arguments, secrets or SSH agent are passed, and no BuildKit entitlements are granted (`RUN --network=host` and `--security=insecure` fail).

## Static add-ons

Large assets that do not belong in Git can be uploaded to the static store (`POST /api/v1/static`, ZIP files) and attached to a project with `PATCH /api/v1/projects/{id}` `{"static_addons":[{"name":"maps.zip","target":"assets/maps"}]}` (admin). Every build extracts each ZIP with the static store's extraction limits (at most 4 GiB in total, 2 GiB per file, 20000 entries, bounded compression ratio, no unsafe paths or links) and makes its files appear at `<target>` below the game root: in the build stage before the web build (so the client build can use them) and at `/app/<target>` in the image. Two add-ons may not provide the same file. A missing or invalid add-on fails the build with a message naming it. A static file that a project references cannot be deleted; uploading a new version with `replace=1` is picked up by the next build.

## Security model

The game repository is **untrusted**, and so is everyone who can push to it.

- **The host never runs repository code.** The daemon only reads `golem-deploy.yaml`, `.dockerignore`, `golem.yaml`, `go.mod`, `package.json` (and a custom Dockerfile) from the checkout, through a reader that is contained to the checkout (`os.Root`), refuses symbolic links anywhere in a path and anything that is not a regular file, and caps sizes (64 KiB to 1 MiB). It never writes to the checkout. A repository cannot make the daemon read or write host files outside it.
- **No injection.** Every value placed in the Dockerfile or the `docker build` command line is a constant, a number, a version matching a strict pattern, or a path from the allowlist above; commands are JSON exec-form arrays. The command line takes `--` before the context, and no repository value can become an option.
- **No secrets in builds.** The build gets no build arguments, secrets, SSH agent or entitlements; `.git` (where the daemon keeps no credentials anyway) never enters the context; the docker CLI runs with the daemon's fixed minimal environment plus fixed BuildKit settings (`BUILDX_GIT_INFO=false`, so the CLI does not run git in the checkout; no attestations).
- **Outputs are verified.** The image is identified by its exact id and must carry exactly the labels the daemon set; servers will only run images recorded by successful builds.

What this does **not** protect against:

- Build steps (`npm ci` with install scripts, `golem-bake`, the web build, `go build`, a custom Dockerfile) run repository code **inside BuildKit containers with network access**: a malicious repository can use the build to reach the network, including services reachable from the Docker host's build network, consume CPU, memory and disk until the build timeout, and fill Docker's build cache. Run Golem Deploy on a host whose network position you are comfortable exposing to the code you build.
- Base images (`golang`, `node`, `distroless`) are pulled by tag from their registries at build time; the docker CLI may use the daemon user's registry credentials for those pulls (and for `FROM` lines of custom Dockerfiles). Do not log the daemon user in to registries holding images you do not want builds to read.
- Docker's layer cache is content-addressed and shared across projects (only the cache mounts are per project); identical inputs produce identical layers, which is not a confidentiality issue but means build cache sizes are host-wide. Prune it with `docker builder prune` when needed.
- The built image runs the game's code: the server hardening in [SERVERS.md](SERVERS.md) is what contains it at run time.
