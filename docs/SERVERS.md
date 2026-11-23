# Game servers: operator notes

Golem Deploy runs each game server as one Docker container through the `docker` CLI. The daemon user must be able to use Docker (on Linux: a member of the `docker` group, which is root-equivalent on that host). This note explains what the daemon touches, how containers are confined and how ports work. The HTTP contract is in [API.md](API.md).

## Managed objects

Everything Golem Deploy creates carries the label `dev.golemengine.deploy.managed=true`, and the daemon refuses to act on anything without it:

| Object | Name | Labels |
|--------|------|--------|
| Server container | `golem-deploy-<server name>` | `managed`, `server-id`, `server`, `project-id` (when set), `config-hash` |
| Network | `golem-deploy` (user-defined bridge) | `managed` |
| Image | built by Golem Deploy (build pipeline: next milestone) | `managed`, `project-id`, `commit` |
| Volume | server data volumes (next milestone) | `managed`, `server-id`, `server` |

All labels are prefixed `dev.golemengine.deploy.`. Containers are found by label from the server id, never by a name or id sent by a client; images and volumes are inspected and their label checked before deletion. Other containers, images and volumes on the host are invisible to the API and are never stopped, removed or read. If a container named `golem-deploy-<name>` or a network named `golem-deploy` exists without the label, starting a server fails with a message instead of touching it.

A server can only run a managed image. Until the build pipeline exists you can label an image yourself, for example:

```sh
docker build --label dev.golemengine.deploy.managed=true -t mygame:dev .
```

Managed containers whose server was deleted outside the API are logged at startup and left alone.

## Container hardening

Every server container is created with:

- a read-only root filesystem and a 64 MiB `noexec,nosuid` tmpfs on `/tmp`;
- all Linux capabilities dropped and `no-new-privileges`;
- user `65532:65532` (distroless "nonroot"), never root;
- `--pids-limit 512`, and memory/CPU limits when `--server-memory-mb` / `--server-cpus` are set (default: unlimited);
- restart policy `on-failure:5`, a 30 s graceful stop timeout;
- `json-file` logs capped at 3 × 10 MB;
- the `golem-deploy` bridge network.

Environment values are encrypted in the database, passed to `docker run` through the CLI process environment (only `--env KEY` appears on the command line), and never logged or returned by the API.

## Ports

Each server has one host port `N` from the configured range, published as both `N/udp` and `N/tcp` to port 7777 inside the container (the game port).

- The range is `--port-range` / `GOLEM_DEPLOY_PORT_RANGE`, default `7000-7099`. It must lie within 1024–32767 (below 1024 is privileged; 32768 and up is the Linux ephemeral range) and must not contain the panel's own listen port. Ports browsers refuse to connect to (for example 6000, 6566, 6665–6669, 6697, 10080) are skipped. Change the range by restarting the daemon with the new flag; existing servers keep their port, so pick a range that still contains them (or PATCH their port).
- `--port-bind-address` / `GOLEM_DEPLOY_PORT_BIND_ADDRESS` publishes on one host IP instead of all interfaces.
- New servers get the lowest free port unless the request names one. Before each start the daemon checks that nothing else on the host listens on the port (TCP and UDP).

### Firewall

Docker publishes ports by programming iptables/nftables directly, **bypassing ufw and firewalld rules**: a published game port is reachable even if ufw denies it. Restrict exposure with `--port-bind-address` or Docker's `DOCKER-USER` chain. Conversely, cloud or provider firewalls (security groups) sit in front of the host and must allow the whole port range for **both UDP and TCP** for players to connect.
