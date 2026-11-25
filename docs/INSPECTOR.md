# Inspector: operator notes

The Golem Inspector shows the live entities of game servers. It has two parts:

- the **hub**, a small web server with its own web UI, and
- an **agent** inside every game server (part of Golem Engine), which connects to the hub over WebSocket and streams entity state while somebody looks at that server.

Golem Deploy runs the hub as a managed container and stands between it and everything else. Only the daemon talks to the hub. Browsers reach the hub through the daemon's **Inspector listener**, and the games' agents connect to that same listener from their containers. The API is in [API.md](API.md#inspector).

## Turning it on

1. Make sure the Inspector listener's port can be reached (see [Firewall](#firewall)). By default it is the host part of `--listen` with port `8444`.
2. In the panel, an admin turns the Inspector on (`PATCH /api/v1/inspector {"enabled": true}`). This starts an `inspector.apply` job that:
   - generates the hub's secrets on first use,
   - makes sure the `golem-deploy` network exists,
   - uses the hub image when the host has it and downloads it otherwise,
   - creates and starts the hub container, waits until the hub answers and the daemon can log in,
   - opens the Inspector listener.
3. Turn on the **Inspector** setting of each server that should appear in it, and restart those servers. A server gets the agent settings only when its own switch is on, the Inspector is on, and containers can reach the listener. Until it is restarted it reports `restart_required`.
4. Open the Inspector from the panel. The panel asks the daemon for a one-time link (`POST /api/v1/inspector/sessions`) and opens it on the Inspector's address. Opening it from a server page shows that server directly.

Turning the Inspector off closes the listener, ends every Inspector session and agent connection, and removes the hub container. Servers that were started with the agent settings then report `restart_required`; their agents retry in the background and are refused until the Inspector is on again.

**Rotate secrets** (`rotate_secrets`) replaces the hub's password, the hub's own agent token and the key of the servers' agent tokens together. Open Inspector sessions end, the hub is recreated, and servers with the Inspector on need a restart to get their new token.

The hub keeps running when the daemon stops or restarts, like game servers. The Inspector sessions do not: they live in the daemon's memory. At start, and at most every 15 minutes after an attempt, the daemon repairs a hub that should be on but is not serving (it never downloads the image for that).

## Serve options

| Option | Environment | Default | Meaning |
|--------|-------------|---------|---------|
| `--inspector-listen` | `GOLEM_DEPLOY_INSPECTOR_LISTEN` | host of `--listen`, port `8444` | Address of the Inspector listener; `off` makes the Inspector unavailable. The port must differ from the panel's and the HTTP listener's ports and lie outside `--port-range`. The listener is open only while the Inspector is on; a bind failure is reported by the apply job and in the Inspector's `detail`. |
| `--inspector-image` | `GOLEM_DEPLOY_INSPECTOR_IMAGE` | `ghcr.io/demiurgos-hub/golem-inspector:0.4.0-rc.1` | Hub image reference (tag or digest). |
| `--inspector-memory-mb` | `GOLEM_DEPLOY_INSPECTOR_MEMORY_MB` | `512` | Memory limit of the hub container (32-1048576). |
| `--inspector-public-url` | `GOLEM_DEPLOY_INSPECTOR_PUBLIC_URL` | none | The origin browsers use when a reverse proxy fronts the listener, e.g. `https://inspector.example.com`. Links then point there, and only that origin is accepted for WebSocket and state-changing requests. Its host name is accepted in the `Host` header. |
| `--inspector-agent-host` | `GOLEM_DEPLOY_INSPECTOR_AGENT_HOST` | derived | The address agents connect to from their containers. |

## Browsers

When the panel serves HTTPS, the Inspector listener serves the panel's certificate (in every panel certificate mode) on its port. When the panel is plain HTTP, so is the listener. The `Host` header policy of the panel applies (IP addresses, `localhost`, `--public-host` names), and the client address behind `--trusted-proxy` is resolved the same way.

A link works once, for 60 seconds, for the account that asked for it. Its one-time ticket is in the fragment (`/_golem-deploy/enter#t=<ticket>`), so browsers never send it in a request, a `Referer` or a log line. The entry page removes the fragment from the address bar and the history at once and exchanges the ticket at `POST /_golem-deploy/session`. The daemon then logs in at the hub for a new Inspector session and answers a random session key. The page keeps the key in the tab's `sessionStorage` and opens the Inspector (on the requested server, if the link was opened from a server page). Failed exchanges are limited to 10 per client address in 15 minutes.

The session belongs to that browser tab. A new tab needs a new link from the panel. A small script of the daemon (`/_golem-deploy/boot.js`, inserted at the top of every Inspector page) sends the key with the Inspector's own requests: as `Authorization: Bearer <key>` and, for its WebSocket, as a subprotocol. The key only goes to the Inspector's own origin.

The Inspector listener uses no cookies. Browsers send a cookie to every port of a host name, and game servers are served on other ports of that same name by default, so a game could read and reuse an Inspector cookie. `sessionStorage` is bound to the full origin (scheme, host and port), so no game server receives the key.

Every request with a key is checked: the account must still exist, be approved and have the same token version (no password change, logout or role change since). The daemon also checks every 30 seconds and closes the open streams of sessions that became invalid. A session ends after 8 hours, at sign-out in the Inspector, and when the daemon restarts; the tab then shows a short page that says so.

Valid requests are relayed to the hub with the browser's cookies, credentials, key and forwarding headers removed and only the daemon-held hub session added. The hub never sees the key, and the hub's own cookies never reach the browser. Requests under `/api`, `/auth` and `/ws` need a valid key. The Inspector's web UI itself (its page, scripts and styles) is relayed without any credentials. WebSocket upgrades and requests other than GET and HEAD need an `Origin` equal to the Inspector's address. `/auth/login`, `/ws/agent`, `/metrics`, `/health`, `/live` and `/ready` are never relayed from browsers. An Inspector page that cannot carry the daemon's script (no `<head>`, or larger than 2 MiB) is not served.

## Agents

A game cannot reach the hub directly: the `golem-deploy` network has traffic between containers turned off, and that stays so. Instead the agent connects to the daemon's Inspector listener, which relays it to the hub. A server with the Inspector on gets these variables in its env file (never in argv):

| Variable | Value |
|----------|-------|
| `GOLEM_INSPECTOR_HUB_URL` | `ws://<agent host>:<inspector port>/ws/agent` |
| `GOLEM_INSPECTOR_TOKEN` | the server's own token, `gdi_<server id>_<HMAC>` |
| `GOLEM_INSPECTOR_TARGET_ID` | the server name |
| `GOLEM_INSPECTOR_LABELS` | `project=<name>,commit=<commit>,server=<id>` |

The agent host is, in this order: `--inspector-agent-host`; the `golem-deploy` network's gateway address when the listener is bound to all interfaces (on Linux); the listener's own address when it is bound to one non-loopback address; `host.docker.internal` when the daemon does not run on Linux (development with Docker Desktop). On Linux a listener bound only to loopback cannot be reached from containers: the Inspector still opens, but no server appears in it (`agents_reachable` is false and nothing is injected).

The listener accepts plain HTTP for agents on the same port as the browsers' HTTPS (it tells them apart by the first byte of each connection); on that plain side only the agent endpoint is served. An agent connection is relayed only when it comes from an address inside the `golem-deploy` network (or from loopback), carries a valid token of a server that exists with its Inspector switch on, and stays within 2 connections per server and 256 in total. The hub only ever sees the hub's own agent token.

## Firewall

Open **one TCP port**: the Inspector listener's (8444 by default). Browsers use it, and the games' agents use it too: they connect from their containers to the host's address on the `golem-deploy` network's gateway. On a host with ufw, allow the port (for example `ufw allow 8444/tcp`) or the agents' connections from the Docker network are dropped. If you added the egress rules from [SERVERS.md](SERVERS.md#network-isolation), keep the rule that accepts this port on the bridge interface. The hub container itself publishes nothing reachable from outside: its port is on `127.0.0.1` only.

## The hub container

The hub runs from the configured image, resolved to its immutable id before the container is created. The apply job uses the image when the host has it and downloads it only when it is missing or `pull_image` is set; downloads run anonymously with the daemon's dedicated Docker client configuration (the one builds use), never with credentials stored for the operator. The container gets the same hardening as server containers: read-only root file system, all capabilities dropped, no-new-privileges, user `65532:65532`, pids and memory limits (`--inspector-memory-mb`), bounded logs, size-limited tmpfs mounts on `/tmp` and on every volume the image declares, the `golem-deploy` network, no mounts, and every `dev.golemengine.deploy.*` label set explicitly (with `inspector-hub=true`). Its settings travel in an env file: production mode behind a proxy (the daemon), one fixed internal public origin (`https://inspector.golem-deploy.internal`), a generated agent token, the bcrypt hash (cost 12) of a generated password, and 12-hour hub sessions. Its port 8787 is published on `127.0.0.1` on a port Docker assigns. The container is found by its labels and its name `golem-deploy_inspector`; its image is never offered for deletion in the panel and never removed by build cleanup. Remove it by hand only after turning the Inspector off.

The secrets are stored encrypted in the database and never returned, logged or put in argv. No person ever knows the hub password.

## Security model and limits

- **Every developer account can open the Inspector** and see every server that has its Inspector switch on. There is no per-server permission.
- **A game with the switch on can register under another server's name.** The hub does not check that a target id matches the token; the daemon only checks that the token belongs to a server with the switch on. Turn the switch on only for games you trust as much as each other.
- **The hub image is pulled from the configured reference.** Whoever controls that reference (registry, tag) controls the hub's code, which runs with the hardening above, sees the entity state of inspected servers and serves the Inspector's web UI to browsers. Pin a digest (`name@sha256:...`) if you need a fixed image.
- **The session key lives in the browser tab.** Code that runs on the Inspector's own origin can read it, like the Inspector's web UI, which comes from the hub image. Game servers cannot: they are other origins, and the listener uses no cookies.
- The agent token of a server is visible to the game (it is in its environment) and can appear in the game's log if the game prints it. It only lets that game register as an Inspector target.
- The Inspector listener is reachable by anyone who can reach its port. Without a valid session it serves only the entry page and its script, the boot script, a short "session ended" page, the Inspector's web UI without any data, and JSON errors.
