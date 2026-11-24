# Game servers: operator notes

Golem Deploy runs each game server as one Docker container through the `docker` CLI. The daemon user must be able to use Docker (on Linux: a member of the `docker` group, which is root-equivalent on that host). This note explains what the daemon touches, how containers are confined, how ports and networking work and what server environment variables are protected from. The HTTP contract is in [API.md](API.md).

## Managed objects

Every installation has a random **instance id** (32 hex characters, generated on first start and kept in the database; it is never returned by the API). Everything Golem Deploy creates carries the label `dev.golemengine.deploy.managed=true` and `dev.golemengine.deploy.instance=<instance id>`; every listing filters on both and every inspect-before-act checks both, so two installations sharing a Docker host, or objects left behind by a wiped data directory, never act on each other's objects.

| Object | Name | Labels (prefix `dev.golemengine.deploy.`) |
|--------|------|--------|
| Server container | `golem-deploy-<server name>` | `managed`, `instance`, `server-id`, `server`, `project-id`, `commit`, `config-hash` — all set explicitly on every run (empty when not applicable), so no value can be inherited from the image |
| Network | `golem-deploy` (user-defined bridge, no inter-container traffic) | `managed`, `instance` |
| Image | built by Golem Deploy (build pipeline: next milestone) | `managed`, `project-id`, `commit` |
| Volume | server data volumes (next milestone) | `managed`, `instance`, `server-id`, `server` |

**A server's container is the container whose id the daemon recorded when it created it** (column `servers.container_id`), never "a container with the right labels": containers inherit the labels of their image, and once images are built from repositories, repository authors control image labels. The recorded container's labels are verified on every inspect as a second check. A container that carries the labels but is not a recorded container is ignored (not reported, read, stopped or removed) and logged once as a foreign container. Containers, images and volumes without the labels are invisible to the API and never touched. If a container named `golem-deploy-<name>` or a network named `golem-deploy` exists that this installation does not manage, starting a server fails with a message instead of touching it.

A server can only run a managed image. **Image trust is provisional:** until the build pipeline records the ids of the images it builds, any image carrying `dev.golemengine.deploy.managed=true` is accepted (images are not labelled with the instance id). Until then you can label an image yourself:

```sh
docker build --label dev.golemengine.deploy.managed=true -t mygame:dev .
```

Images are deleted by their immutable id after the label check, never by tag.

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
- no restart policy while the start job waits for the container to settle (so a crash keeps its real exit code), then restart policy `on-failure:5` (applied with `docker update`);
- `json-file` logs capped at 3 × 10 MB;
- the `golem-deploy` bridge network.

## Environment variables

Values are encrypted in the database, never logged and never returned by the API (only the names are).

They reach `docker run` only through `--env-file`, **never through the command line or the docker CLI's own environment** (a variable such as `LD_PRELOAD` or `GODEBUG` there would change how the CLI itself runs as the daemon user). On Linux the env file is an anonymous pipe passed to the CLI as `/dev/fd/3`, so values never touch a disk; on other platforms (Docker Desktop on Windows or macOS) it is a 0600 file in a fresh 0700 directory under the data directory's `tmp/`, removed as soon as the command returns (leftovers of a crash, `tmp/docker-envfile-*`, are removed at startup). The CLI process always gets the same fixed allowlist of the daemon's own environment (`PATH`, `HOME`, `DOCKER_HOST`, `DOCKER_CONTEXT`, ...).

The env-file format is one `KEY=value` per line with the value taken literally, so values must be single-line UTF-8 (no NUL, CR or LF); every other character, including spaces, quotes, `#`, `=`, `$` and backslashes, arrives unchanged. Names match `^[A-Za-z_][A-Za-z0-9_]{0,127}$`; names starting with `GOLEM_` are reserved for variables the daemon injects. Names like `PATH` or `LD_PRELOAD` are ordinary container variables.

**What env values are protected from — and what not.** They are protected from other servers and from the API surface: nobody can read them through the API, the daemon never logs them, and the last log lines copied into the job log of a failed start have every line of every value (of at least 4 bytes) replaced with `[redacted]`. That redaction is best effort: a game that prints a value encoded or split is not caught, and `GET /servers/{id}/logs` is the game's raw output (sanitised, never redacted). Above all, **anyone who can change a server's image or the game's code can read that server's environment** — the game runs with it. Env values protect secrets from other servers and from people who only use the panel, not from the developers of that game.

## Logs

Container output shown to users — `GET /servers/{id}/logs` and the excerpt in the job log of a failed start — is sanitised: invalid UTF-8 becomes U+FFFD, control characters (C0 except tab and newline, DEL, C1, ESC, Unicode line separators and bidirectional overrides) are shown escaped (`\x1b`, `\u202e`), and lines are cut at 16 KiB with ` [line truncated]`. In job logs every container line starts with `container | ` so it cannot be mistaken for a daemon message.

Job logs and job details only contain short, fixed messages for Docker failures (image missing, name in use by a container this installation does not manage, port busy, Docker unreachable, timeout, other). Docker's own error output goes to the daemon log together with the job id (`job_id=<n>`).

## Ports

Each server has one host port `N` from the configured range, published as both `N/udp` and `N/tcp` to port 7777 inside the container (the game port).

- The range is `--port-range` / `GOLEM_DEPLOY_PORT_RANGE`, default `7000-7099`. It must lie within 1024–32767 (below 1024 is privileged; 32768 and up is the Linux ephemeral range) and must not contain the panel's own listen port. Ports browsers refuse to connect to (for example 6000, 6566, 6665–6669, 6697, 10080) are skipped.
- Change the range by restarting the daemon with the new flag. Existing servers keep their port; a start checks the port against the current range, the browser-unsafe list and the panel's port and fails with a message to assign a new port (`PATCH /servers/{id}` with `port`) when it is no longer allowed.
- A server's port cannot change while its container is running (`409 conflict`: stop it first), so a running container never holds a port that was given to another server.
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
