# Game servers: operator notes

Golem Deploy runs each game server as one Docker container through the `docker` CLI, from the image of a successful build of a project. The daemon user must be able to use Docker (on Linux: a member of the `docker` group, which is root-equivalent on that host). This note explains what the daemon touches, how containers are confined and configured for the Golem Engine, how a start is health-gated, how ports and networking work and what server environment variables are protected from. The HTTP contract is in [API.md](API.md); the game certificate is described in [CERTIFICATES.md](CERTIFICATES.md).

The path from a repository to a running game has three steps: a **project** clones the game's Git repository ([REPOSITORIES.md](REPOSITORIES.md), `POST /api/v1/projects`); a **build** turns a commit of its checkout into a verified image ([BUILDS.md](BUILDS.md), `POST /api/v1/projects/{id}/builds`); a **server** runs a successful build (this note, `POST /api/v1/servers` with `build_id`, then `POST /api/v1/servers/{id}/start`). Changing a server to another successful build of its own project (`PATCH /api/v1/servers/{id}` with `build_id`) marks a running server `restart_required`; the next start runs the new build.

## Managed objects

Every installation has a random **instance id** (32 hex characters, generated on first start and kept in the database; it is never returned by the API). Everything Golem Deploy creates carries the label `dev.golemengine.deploy.managed=true` and `dev.golemengine.deploy.instance=<instance id>`; every listing filters on both and every inspect-before-act checks both, so two installations sharing a Docker host, or objects left behind by a wiped data directory, never act on each other's objects.

| Object | Name | Labels (prefix `dev.golemengine.deploy.`) |
|--------|------|--------|
| Server container | `golem-deploy-<server name>` | `managed`, `instance`, `server-id`, `server`, `project-id`, `commit`, `build-id`, `engine-version`, `config-hash`, `launch-id` — all set explicitly on every create (empty when not applicable), so no value can be inherited from the image |
| Network | `golem-deploy` (user-defined bridge, no inter-container traffic) | `managed`, `instance` |
| Image | `golem-deploy/<project>:<commit[:12]>-b<build id>`, built by a build ([BUILDS.md](BUILDS.md)) | `managed`, `instance`, `project-id`, `commit`, `build-id`, `engine-version` |
| Volume | server data volumes (next milestone) | `managed`, `instance`, `server-id`, `server` |

**A server's container is the container whose id the daemon recorded when it created it** (column `servers.container_id`), never "a container with the right labels": containers inherit the labels of their image, and once images are built from repositories, repository authors control image labels. The recorded container's labels are verified on every inspect as a second check. Before create the daemon records a random launch identity and passes it as an explicit label. If Docker's response is lost, recovery can remove the resulting container only when its name and server, instance, managed and launch labels match the recorded intent. Other unrecorded containers are ignored and logged once as foreign. Containers, images and volumes without the labels are invisible to the API and never touched. If a container named `golem-deploy-<name>` or a network named `golem-deploy` exists that this installation does not manage, starting a server fails with a message instead of touching it.

**A server runs a build, never an image reference.** Its image is the immutable `image_id` recorded on a `success` row of the builds table; labels alone are never trusted (anyone who builds an image can set them). Before every `docker create` the daemon also inspects that image and requires the managed label, this installation's instance label and exactly the build labels the build set (`build-id`, `project-id`, `commit` and `engine-version` equal to the build's, no other label in the namespace: the same check the build made when it finished), so an id that no longer names the image that build produced is refused. A hand-labelled image cannot be run. Servers created before servers ran builds have no build. Clear their old environment before or during the first build assignment (`PATCH /servers/{id}` with `build_id` and, if needed, `env_unset`). The old environment has no provable project association.

`GET /images` lists the images carrying the managed and instance labels. One rule decides whether an image is in use, for `GET /images`, `DELETE /images/{id}`, build deletion, build retention and project deletion: a successful build records it, or any container (managed or not, running or stopped) was created from it. A build's image is removed by deleting the build (or by retention, or with its project), never through `/images`; that is refused while the image is in use by a container. A stopped server keeps its container (for its logs), and a server switched to another build keeps the old build's container until its next start, so that old build cannot be deleted until then (the `409` names the server). Images are deleted by their immutable id after the label check, never by tag.

### Upgrading from a version without recorded container ids

Servers created before container ids were recorded have no container as far as the daemon is concerned (`state: "missing"`), even if an old container is still running. Starting such a server fails with "a container named golem-deploy-&lt;name&gt; already exists and this installation does not manage it" while the old container exists. Check it and remove it, then start again:

```sh
docker ps -a --filter name=golem-deploy-
docker rm -f golem-deploy-<name>
```

The old `golem-deploy` network (without an instance label, with inter-container traffic allowed) is removed and recreated on the next start once no container is attached to it.

## Container hardening

Every server container is created with:

- `--pull never` (only local, verified images run);
- a read-only root filesystem and a 64 MiB `noexec,nosuid` tmpfs on `/tmp`;
- a 64 MiB `noexec,nosuid` tmpfs on **every path the image declares with `VOLUME`** (at most 8; images declaring more are refused), so Docker never creates an anonymous volume; a daemon-provided mount at the same path (future data volume, read-only binds) takes precedence. Containers are removed with `--volumes`;
- all Linux capabilities dropped and `no-new-privileges`;
- user `65532:65532` (distroless "nonroot"), never root;
- `--init`, `--no-healthcheck` and `--stop-signal SIGTERM`, so image metadata cannot change them; a 30 s graceful stop timeout;
- `--pids-limit 512`, `--memory` from `--server-memory-mb` (default 2048 MiB, with `--memory-swap` equal to it: no extra swap) and `--cpus` from `--server-cpus` (default 2); `0` means unlimited;
- no restart policy until the game reports ready (so a crash is never retried during a start and keeps its real exit code), then restart policy `on-failure:5` (applied with `docker update`);
- the server’s own certificate directory bind-mounted read-only at `/run/golem-deploy/tls`, and on Unix hosts the daemon's group as a supplementary group (`--group-add`) so uid 65532 can read the key (see [CERTIFICATES.md](CERTIFICATES.md));
- the game's admin port (container TCP 7778) published on `127.0.0.1` only, on a port Docker assigns;
- `json-file` logs capped at 3 × 10 MB;
- the `golem-deploy` bridge network.

## Engine deploy contract

The daemon configures every container for the Golem Engine deploy contract (golem-engine v0.4: `golem.ServerConfigFromEnv` and the generated `Runtime.Host`). The game must use it, with **WebTransport as its primary transport** (the WebSocket fallback needs it; a WebSocket-primary game fails to start with these variables). These variables are injected by the daemon and override operator variables (whose names may not start with `GOLEM_` anyway):

| Variable | Value |
|----------|-------|
| `GOLEM_ADDR` | `:7777` (WebTransport, UDP) |
| `GOLEM_HTTP_ADDR` | `:7777` (HTTPS sidecar: realtime config, WebSocket fallback, the game's pages; TCP) |
| `GOLEM_TLS_CERT_FILE`, `GOLEM_TLS_KEY_FILE` | `/run/golem-deploy/tls/tls.crt`, `/run/golem-deploy/tls/tls.key` |
| `GOLEM_TLS_RELOAD_INTERVAL` | `30s` (replaced certificates are picked up without a restart) |
| `GOLEM_PUBLIC_URL` | `https://<game host>:<port><webtransport path>`: the engine's default `/wt` unless the build recorded another `transport.webtransport_path` from `golem-deploy.yaml` (see [BUILDS.md](BUILDS.md#golem-deployyaml)) |
| `GOLEM_WS_FALLBACK_URL` | `wss://<game host>:<port><websocket path>`: `/api/ws` unless the build recorded another `transport.websocket_path` |
| `GOLEM_WT_ALLOWED_ORIGINS` | `https://<game host>:<port>` plus the server's `allowed_origins` (WebTransport and WebSocket) |
| `GOLEM_TLS_PINNED` | `1` with a self-signed certificate (publish its hashes for pinning), `0` with a custom one |
| `GOLEM_ADMIN_ADDR` | `:7778` |
| `GOLEM_ADMIN_TOKEN` | a random 64-character token generated for each container |

`<port>` is the server's host port and `<game host>` the `game_host` setting (or the first `--public-host`). Everything except the admin token is part of the server's config hash, so changing the game host, the allowed origins or the certificate mode marks running servers `restart_required`.

The admin token is stored encrypted on the server row (never returned, never logged), reaches the container only through the env file like other values and is redacted from failed-start excerpts. The daemon talks to the admin endpoint only on `127.0.0.1:<assigned port>` with a strict client with short timeouts, no proxy, no redirects, no compressed bodies and at most 64 KiB per answer. It never logs raw HTTP protocol errors. Strings containing the server's admin token are discarded and other reported strings are bounded and stripped of control and formatting characters.

## Starting a server

A start (and restart) job checks the build (successful, with an image), the port, the game host and the certificate (a self-signed one is generated or renewed here), verifies the build's image, removes the server's previous container, ensures the network, checks that the port is free, creates the container, records its id and sealed token, and then starts it. It then waits up to `--start-timeout` (default 60 s) for the game's admin `/ready` to answer ready. The deadline covers inspection and probes. Stopping a failed container can take additional time.

- ready: the schema fingerprint reported by the game (admin `/info`) is recorded on the build. The restart policy is applied and the completed start is persisted before the job succeeds;
- the container exits first, or the timeout passes: the job fails with a short message pointing at the server log (`GET /servers/{id}/logs`); when the admin endpoint never answered at all, it also says that the game must use the deploy contract (`golem.ServerConfigFromEnv` and `Runtime.Host`). The container is stopped and kept, without a restart policy, so its logs stay readable; its last lines (sanitised, env values and the admin token redacted) are in the job log.

Startup recovery jobs verify the running container's image matches the configured successful build and ask the game to report readiness. They run the normal start path for interrupted starts and containers that fail verification.

`GET /servers/{id}/runtime` reads the running game's admin `/ready`, `/stats` and `/info` over one TCP connection. All returned values, including the schema fingerprint, are reported by the game. A failed or closed connection ends the sample. Every readiness probe and runtime sample needs a fresh inspect that reports a running container and its loopback binding. Another local process could take a just-freed port in the short window between inspection and connection.

## Environment variables

Values are encrypted in the database, never logged and never returned by the API (only the names are).

They reach `docker create` only through `--env-file`, **never through the command line or the docker CLI's own environment** (a variable such as `LD_PRELOAD` or `GODEBUG` there would change how the CLI itself runs as the daemon user). On Linux the env file is an anonymous pipe passed to the CLI as `/dev/fd/3`, so values never touch a disk; on other platforms (Docker Desktop on Windows or macOS) it is a 0600 file in a fresh 0700 directory under the data directory's `tmp/`, removed as soon as the command returns (leftovers of a crash, `tmp/docker-envfile-*`, are removed at startup). The CLI process always gets the same fixed allowlist of the daemon's own environment (`PATH`, `HOME`, `DOCKER_HOST`, `DOCKER_CONTEXT`, ...).

The env-file format is one `KEY=value` per line with the value taken literally, so values must be single-line UTF-8 (no NUL, CR or LF); every other character, including spaces, quotes, `#`, `=`, `$` and backslashes, arrives unchanged. Names match `^[A-Za-z_][A-Za-z0-9_]{0,127}$`; names starting with `GOLEM_` are reserved for variables the daemon injects. Names like `PATH` or `LD_PRELOAD` are ordinary container variables.

**What env values are protected from — and what not.** They are protected from other servers and from the API surface: nobody can read them through the API, the daemon never logs them, and the last log lines copied into the job log of a failed start have every line of every value (of at least 4 bytes) replaced with `[redacted]`. That redaction is best effort: a game that prints a value encoded or split is not caught, and `GET /servers/{id}/logs` is the game's raw output (sanitised, never redacted). Above all, **anyone who can change a server's build or the game's code can read that server's environment** — the game runs with it. Env values protect secrets from other servers and from people who only use the panel, not from the developers of that game.

## Logs

Container output shown to users — `GET /servers/{id}/logs` and the excerpt in the job log of a failed start — is sanitised like build output: invalid UTF-8 becomes U+FFFD, a carriage return ends a line (so it cannot overwrite one), other control characters (C0 except tab, DEL, C1, ESC, Unicode line separators and bidirectional overrides) are shown escaped (`\x1b`, `\u202e`), and lines are cut at 16 KiB with ` [line truncated]`. In job logs every container line starts with `container | ` so it cannot be mistaken for a daemon message.

Job logs and job details only contain short, fixed messages for Docker failures (image missing, name in use by a container this installation does not manage, port busy, Docker unreachable, timeout, other). Docker's own error output goes to the daemon log together with the job id (`job_id=<n>`).

## Ports

Each server has one host port `N` from the configured range, published as both `N/udp` and `N/tcp` to port 7777 inside the container (the game port). The admin port 7778 is published on `127.0.0.1` only, on a port Docker picks from the host's ephemeral range (read back from `docker inspect`; it can change when Docker restarts the container).

- The range is `--port-range` / `GOLEM_DEPLOY_PORT_RANGE`, default `7000-7099`. It must lie within 1024–32767 (below 1024 is privileged; 32768 and up is the default Linux ephemeral range) and must not contain the panel's own listen port. Ports browsers refuse to connect to (for example 6000, 6566, 6665–6669, 6697, 10080) are skipped. On Linux the daemon reads the host's actual ephemeral range (`net.ipv4.ip_local_port_range`) at startup and logs a warning when the range overlaps it: the kernel and Docker can hand those ports out at any time.
- Change the range by restarting the daemon with the new flag. Existing servers keep their port; a start checks the port against the current range, the browser-unsafe list and the panel's port and fails with a message to assign a new port (`PATCH /servers/{id}` with `port`) when it is no longer allowed.
- Build and port changes are refused while a server job is queued or running (`409 job_in_progress`). A server's port also cannot change while its container is running (`409 conflict`: stop it first), so a running container never holds a port that was given to another server.
- `--port-bind-address` / `GOLEM_DEPLOY_PORT_BIND_ADDRESS` publishes on one host IP instead of all interfaces.
- New servers get the lowest free port unless the request names one. Before each start the daemon checks that nothing else on the host listens on the port (TCP and UDP).

## Network isolation

Servers join the `golem-deploy` bridge network, created with `com.docker.network.bridge.enable_icc=false`: **servers cannot reach each other** directly, by container name or IP. (A network created by an older version is recreated without inter-container traffic on the next start once no container is attached; while containers are attached it is kept and the start job logs a warning.)

What this does **not** isolate on Linux:

- Servers can reach each other's *published* ports through the host's addresses, exactly like players can.
- Servers can reach the host itself through the bridge gateway address, including anything listening on all interfaces — such as the panel when `--listen` is `0.0.0.0:…`, SSH or a database.
- Servers can reach anything routable from the host: the internet, your LAN, and cloud metadata services such as `169.254.169.254`.

Operators who want egress filtering can add rules (Docker's iptables backend; rules are not persistent, and must be re-applied when the network is recreated):

```sh
BR="br-$(docker network inspect -f '{{printf "%.12s" .Id}}' golem-deploy)"
# Servers -> the host itself (panel, SSH, ...): replies only.
iptables -I INPUT 1 -i "$BR" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -I INPUT 2 -i "$BR" -j DROP
# Servers -> metadata service and private networks; players and the
# internet stay reachable.
iptables -I DOCKER-USER 1 -i "$BR" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
iptables -I DOCKER-USER 2 -i "$BR" -d 169.254.0.0/16 -j DROP
iptables -I DOCKER-USER 3 -i "$BR" -d 10.0.0.0/8 -j DROP
iptables -I DOCKER-USER 4 -i "$BR" -d 172.16.0.0/12 -j DROP
iptables -I DOCKER-USER 5 -i "$BR" -d 192.168.0.0/16 -j DROP
```

Use the matching `ip6tables` rules if IPv6 is enabled for Docker.

### Firewall

Docker publishes ports by programming iptables/nftables directly, **bypassing ufw and firewalld rules**: a published game port is reachable even if ufw denies it. Restrict exposure with `--port-bind-address` or Docker's `DOCKER-USER` chain. Conversely, cloud or provider firewalls (security groups) sit in front of the host and must allow the whole port range for **both UDP and TCP** for players to connect.
