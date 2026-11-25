# Inspector: operator notes

The Golem Inspector shows the live entities of game servers. It has two parts:

- the **hub**, a small web server with its own web UI, and
- an **agent** inside every game server (part of Golem Engine), which connects to the hub over WebSocket and streams entity state while somebody looks at that server.

Golem Deploy runs the hub as a managed container and stands between it and everything else. Only the daemon talks to the hub. Browsers reach the hub through the daemon's **Inspector listener**, and the games' agents connect to that same listener from their containers. The API is in [API.md](API.md#inspector).

## Turning it on

1. Make sure the Inspector listener's port can be reached (see [Firewall](#firewall)). By default it is the host part of `--listen` with port `8444`.
2. In the panel, an admin turns the Inspector on (`PATCH /api/v1/inspector {"enabled": true}`). The setting is stored first (when that fails, the request fails and nothing starts). Then an `inspector.apply` job follows the setting as it is when the job runs:
   - uses the hub image when the host has it and downloads it otherwise (before anything else changes),
   - makes sure the `golem-deploy` network exists without traffic between containers (see [Agents](#agents)),
   - generates the hub's secrets on first use,
   - creates and starts the hub container, waits until the hub answers and the daemon can log in,
   - opens the Inspector listener.

   When a step fails, the Inspector is `stopped` with a short reason, and nothing talks to the hub until an apply (or the automatic repair) succeeds.
3. Turn on the **Inspector** setting of each server that should appear in it, and restart those servers. A server gets the agent settings only when its own switch is on, the Inspector is on, and containers can reach the listener. Until it is restarted it reports `restart_required`.
4. Open the Inspector from the panel. The panel asks the daemon for a one-time link (`POST /api/v1/inspector/sessions`) and opens it on the Inspector's address. Opening it from a server page shows that server directly.

Turning the Inspector off takes effect when the setting is stored, before the request is answered: new links are refused, unused links stop working, and every Inspector session, open stream and agent connection ends. The `inspector.apply` job then closes the listener and removes the hub container. Servers that were started with the agent settings then report `restart_required`; their agents retry in the background and are refused until the Inspector is on again. A `PATCH` without `enabled` changes no setting. What a turn-off leaves behind (the listener or the hub container, for example when its job could not start or failed) is removed by the automatic repair within about 30 seconds, tried again every minute while that fails, whatever happened to earlier attempts to start the hub. A hub container that should not exist for another reason (the daemon runs with `--inspector-listen off`, or it was left by an older run) is removed when the daemon starts and looked for every 15 minutes.

**Rotate secrets** (`rotate_secrets`) replaces the hub's password, the hub's own agent token and the key of the servers' agent tokens together. Open Inspector sessions end, the hub is recreated, and servers with the Inspector on need a restart to get their new token. With `pull_image` too, the image is downloaded first: a failed download rotates nothing and leaves a running Inspector as it is.

The hub keeps running when the daemon stops or restarts, like game servers. The Inspector sessions do not: they live in the daemon's memory. At start, and at most every 15 minutes after a failed attempt, the daemon repairs a hub that should be on but is not serving (it never downloads the image for that). Every 30 seconds it also asks the hub whether it is ready; after three failures in a row (no answer, or not ready) the Inspector is `stopped` with a reason and the repair replaces the hub container. The repair job reads the setting when it runs and checks it again before it opens the Inspector to browsers: when the Inspector was turned off meanwhile, the same job removes the hub instead. When an apply fails after it found the image, every Inspector session, open stream and agent connection ends.

When the hub refuses the daemon's login (it does not accept the password, or limits logins), the daemon stops logging in at that hub: after a refused password until the hub container or its secrets change, after a limit until the hub's `Retry-After`. The hold lives in the daemon's memory and applies within one run of the daemon; after a restart the daemon tries once more.

The daemon talks to the hub only on the loopback port Docker published for the hub container it created. It sends the hub's password only after Docker confirmed, for that login, that this container runs and publishes that port. It sends a hub session, the hub's agent token or a browser's request for the Inspector's own pages only when Docker confirmed this within the last 10 seconds; an older confirmation is renewed first. It stops using the port at once when Docker reports the container stopped, gone or replaced, when Docker cannot be asked, when the hub does not answer, and after three failed readiness checks. So another program that takes the port after the hub stopped never receives the hub's password, and receives a hub session or the agent token only if it took the port within 10 seconds of Docker's last confirmation.

## Serve options

| Option | Environment | Default | Meaning |
|--------|-------------|---------|---------|
| `--inspector-listen` | `GOLEM_DEPLOY_INSPECTOR_LISTEN` | host of `--listen`, port `8444` | Address of the Inspector listener; `off` makes the Inspector unavailable. The port must differ from the panel's and the HTTP listener's ports and lie outside `--port-range`. The listener is open only while the Inspector is on; a bind failure is reported by the apply job and in the Inspector's `detail`. |
| `--inspector-image` | `GOLEM_DEPLOY_INSPECTOR_IMAGE` | `ghcr.io/demiurgos-hub/golem-inspector:0.4.0-rc.1` | Hub image reference (tag or digest). |
| `--inspector-memory-mb` | `GOLEM_DEPLOY_INSPECTOR_MEMORY_MB` | `512` | Memory limit of the hub container (32-1048576). |
| `--inspector-public-url` | `GOLEM_DEPLOY_INSPECTOR_PUBLIC_URL` | none | The origin browsers use when a reverse proxy fronts the listener, e.g. `https://inspector.example.com`. Links then point there, and only that origin is accepted for WebSocket and state-changing requests (compared in canonical form: lower case, IPv6 in its short form, no default port). Its host name is accepted in the `Host` header. |
| `--inspector-agent-host` | `GOLEM_DEPLOY_INSPECTOR_AGENT_HOST` | derived | The address agents connect to from their containers. On Linux, setting it also lets agents connect from a loopback address. |

## Browsers

When the panel serves HTTPS, the Inspector listener serves the panel's certificate (in every panel certificate mode) on its port. When the panel is plain HTTP, so is the listener. The `Host` header policy of the panel applies (IP addresses, `localhost`, `--public-host` names), and the client address behind `--trusted-proxy` is resolved the same way.

A link works once, for 60 seconds, for the account that asked for it. An account can hold 8 unused links and the installation 1024; beyond that, new links are refused (`429`) until some are used or expire. Its one-time ticket is in the fragment (`/_golem-deploy/enter#t=<ticket>`), so browsers never send it in a request, a `Referer` or a log line. The entry page removes the fragment from the address bar and the history at once and exchanges the ticket at `POST /_golem-deploy/session`. The daemon then logs in at the hub for a new Inspector session and answers a random session key. The page keeps the key in the tab's `sessionStorage` and opens the Inspector (on the requested server, if the link was opened from a server page). Failed exchanges are limited to 10 per client address in 15 minutes. A link issued before the Inspector's secrets were replaced, or before it was turned off, does not work, also when that happens while the link is being exchanged.

The session belongs to that browser tab. A new tab needs a new link from the panel. A small script of the daemon (`/_golem-deploy/boot.js`, inserted at the top of every Inspector page) sends the key with the Inspector's own requests: as `Authorization: Bearer <key>` and, for its WebSocket, as a subprotocol. The key only goes to the Inspector's own origin.

The Inspector listener uses no cookies. Browsers send a cookie to every port of a host name, and game servers are served on other ports of that same name by default, so a game could read and reuse an Inspector cookie. `sessionStorage` is bound to the full origin (scheme, host and port), so no game server receives the key.

Every request with a key is checked after its body was read: the account must still exist, be approved and have the same token version (no password change, logout or role change since). The daemon also checks every 30 seconds and closes the open streams of sessions that became invalid. A session ends after 8 hours (open streams close then too, and no answer is written after that), at sign-out in the Inspector (`POST /auth/logout` without a body; other methods get `405`, a body `400`; the session ends at once, before the hub's own sign-out), and when the daemon restarts; the tab then shows a short page that says so.

Valid requests are relayed to the hub with the browser's cookies, credentials, key and forwarding headers removed and only the daemon-held hub session added. The hub never sees the key, and the hub's own cookies, informational (1xx) responses and trailers never reach the browser. Requests under `/api`, `/auth` and `/ws` need a valid key. The Inspector's web UI itself (its page, scripts and styles) is relayed without any credentials. WebSocket upgrades and requests other than GET and HEAD need exactly one `Origin` header, equal to the Inspector's address. Only WebSocket upgrades under `/ws/` are relayed; any other upgrade, a GET or HEAD with a body, a WebSocket upgrade with a body and a request that announces trailers are refused (`400`), and a refused request with a body closes its connection. Every request that is not a relayed WebSocket has a 2-minute deadline for reading and for writing, also what the daemon answers before it looks at the request (for example a `417` for an `Expect` header it does not support), and the listener holds at most 1024 connections at a time (relayed streams included). When the hub lost its session for a valid daemon session and a new one cannot be opened, the request fails with `503`. When the hub switches protocols for anything but the WebSocket the request asked for, the request fails with `502` and the daemon closes its connection to the hub. `/auth/login`, `/ws/agent`, `/metrics`, `/health`, `/live` and `/ready` are never relayed from browsers. An Inspector page that cannot carry the daemon's script is not served (`502`): before `<head ...>` it may have only a byte order mark, whitespace, one doctype and an `<html ...>` tag (no comment), and it must be at most 2 MiB.

## Agents

A game cannot reach the hub directly: the `golem-deploy` network has traffic between containers turned off, and that stays so. A network created by an older version may still allow that traffic while containers are attached; the hub is then not started (and a hub already there is removed) until every server is stopped and the daemon restarted, which recreates the network. Instead of reaching the hub, the agent connects to the daemon's Inspector listener, which relays it to the hub. A server with the Inspector on gets these variables in its env file (never in argv):

| Variable | Value |
|----------|-------|
| `GOLEM_INSPECTOR_HUB_URL` | `ws://<agent host>:<inspector port>/ws/agent` |
| `GOLEM_INSPECTOR_TOKEN` | the server's own token, `gdi_<server id>_<HMAC>` |
| `GOLEM_INSPECTOR_TARGET_ID` | the server name |
| `GOLEM_INSPECTOR_LABELS` | `project=<name>,commit=<commit>,server=<id>` |

The agent host is, in this order: `--inspector-agent-host`; the `golem-deploy` network's gateway address when the listener is bound to all interfaces (on Linux); the listener's own address when it is bound to one non-loopback address; `host.docker.internal` when the daemon does not run on Linux (development with Docker Desktop). On Linux a listener bound only to loopback cannot be reached from containers: the Inspector still opens, but no server appears in it (`agents_reachable` is false and nothing is injected).

The variables follow the desired configuration (the Inspector on, its secrets and an address containers can reach), not whether the hub runs at that moment: a server started while the hub is down still gets them, and its agent connects once the hub is repaired. The endpoint, the token and the generation in the server's config hash come from one snapshot: when the Inspector is turned off or its secrets are replaced while the daemon looks up the agent address, the server starts without the variables and reports `restart_required` later.

The listener accepts plain HTTP for agents on the same port as the browsers' HTTPS (it tells them apart by the first byte of each connection); on that plain side only the agent endpoint is served. An agent connection is relayed only when it comes directly (no `Forwarded`, `X-Forwarded-For` or `Via` header) from an address inside the `golem-deploy` network, carries a valid token of a server that exists with its Inspector switch on, and stays within 256 relays in total. A loopback address is accepted only when the daemon does not run on Linux (Docker Desktop) or `--inspector-agent-host` is set: otherwise a reverse proxy on the host could turn remote clients into loopback peers. A server keeps one relay: a new valid connection of the same server closes the previous one. Every 30 seconds the daemon closes the relays of servers that were deleted or whose switch is off. The hub only ever sees the hub's own agent token.

The hub accepts at most 4 agents registering at once from one address. All relayed agents share the daemon's address, so when many games start together some are refused for a moment; their agents retry.

## Firewall

Open **one TCP port**: the Inspector listener's (8444 by default). Browsers use it, and the games' agents use it too: they connect from their containers to the host's address on the `golem-deploy` network's gateway. On a host with ufw, allow the port (for example `ufw allow 8444/tcp`) or the agents' connections from the Docker network are dropped. If you added the egress rules from [SERVERS.md](SERVERS.md#network-isolation), keep the rule that accepts this port on the bridge interface. The hub container itself publishes nothing reachable from outside: its port is on `127.0.0.1` only.

## The hub container

The hub runs from the configured image, resolved to its immutable id before the container is created. The apply job uses the image when the host has it and downloads it only when it is missing or `pull_image` is set; downloads run with the daemon's build Docker configuration (the one builds use), never with the operator's own client configuration. That configuration holds no registry credentials unless an operator added some there; a download then uses them, which is fine because the image reference is the operator's own setting. The container gets the same hardening as server containers: read-only root file system, all capabilities dropped, no-new-privileges, user `65532:65532`, pids and memory limits (`--inspector-memory-mb`), a limit of one CPU, bounded logs, size-limited tmpfs mounts on `/tmp` and on every volume the image declares, the `golem-deploy` network, no mounts, and every `dev.golemengine.deploy.*` label set explicitly (with `inspector-hub=true`). Its settings travel in an env file: production mode behind a proxy (the daemon), one fixed internal public origin (`https://inspector.golem-deploy.internal`), a generated agent token, the bcrypt hash (cost 12) of a generated password, and 12-hour hub sessions. Its port 8787 is published on `127.0.0.1` on a port Docker assigns. The container is found by its labels and its name `golem-deploy_inspector`; its image is never offered for deletion in the panel and never removed by build cleanup. Remove it by hand only after turning the Inspector off.

The secrets are stored encrypted in the database and never returned, logged or put in argv. No person ever knows the hub password.

## Security model and limits

- **Every developer account can open the Inspector** and see every server that has its Inspector switch on. There is no per-server permission.
- **A game with the switch on can register under another server's name.** The hub does not check that a target id matches the token; the daemon only checks that the token belongs to a server with the switch on. Turn the switch on only for games you trust as much as each other.
- **The hub image is pulled from the configured reference.** Whoever controls that reference (registry, tag) controls the hub's code, which runs with the hardening above, sees the entity state of inspected servers and serves the Inspector's web UI to browsers. Pin a digest (`name@sha256:...`) if you need a fixed image.
- **The session key lives in the browser tab.** Code that runs on the Inspector's own origin can read it, like the Inspector's web UI, which comes from the hub image. Game servers cannot: they are other origins, and the listener uses no cookies.
- The agent token of a server is visible to the game (it is in its environment) and can appear in the game's log if the game prints it. It only lets that game register as an Inspector target.
- The Inspector listener is reachable by anyone who can reach its port. Without a valid session it serves only the entry page and its script, the boot script, a short "session ended" page, the Inspector's web UI without any data, and JSON errors.
