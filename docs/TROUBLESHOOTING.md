# Troubleshooting

This page is organised by symptom. Each entry says how to check and how to fix. Messages are quoted exactly as Golem Deploy writes them, with `<...>` for the parts that vary, so you can search this page for what you see.

Installation and service problems (the service does not start, a port is in use, Docker is unreachable, the data directory is refused) are in [INSTALL.md](INSTALL.md#troubleshooting).

## Where to look

| What | Where |
|------|-------|
| A job's outcome | The job's **detail** in the panel: one short message. |
| A job's full output (build steps, a failed start's last game lines) | The job log in the panel, or `GET /api/v1/jobs/{id}/log`. |
| A game's own output | The server's **Logs** tab, `GET /api/v1/servers/{id}/logs`, or `docker logs golem-deploy-<server name>` on the host. |
| The daemon's log (Docker's own error messages, under `job_id=<n>`) | `journalctl -u golem-deploy` in the systemd installation. |

Job details and job logs carry short, fixed messages for Docker failures. When a detail says "details in the daemon log under job_id=<n>", search the daemon log for that id.

## Players cannot connect

Players reach a game server on its host port `N`, over **UDP** (WebTransport) and **TCP** (the page, the realtime config and the WebSocket fallback). Both must be open from the player to the host.

### Check with the panel's connection test

On the server page, **Connection test** probes the game from your browser in four steps. Run it from the network where players have trouble. The game may count the short test connections as players joining and leaving.

| Step | What it checks | When it fails |
|------|----------------|---------------|
| Server and browser checks | The server runs and its game reports ready, its certificate is valid and covers the game host, and this panel's address is one of the allowed test origins. | Follow the message. `The server is not running...` or `The game is not ready...`: start it and read its log. `Open the panel at one of these addresses to test this server: <list>`: open the panel by one of those addresses. `The running container may not allow the panel origin yet. Restart the server and try again.`: the server was started before the panel's address was known. `The certificate does not cover the game host...`: see [Certificates](#certificate-problems). |
| HTTPS | The browser gets an HTTPS answer from `https://<game host>:N/`. | DNS of the game host, TCP port `N` in the firewall, or a certificate this browser does not trust. Reload the panel if the game host changed. |
| WebSocket fallback | The browser opens the WebSocket fallback. | If HTTPS passed: the game does not serve the fallback at that path, or does not accept the panel's origin, or a proxy blocks the upgrade. If HTTPS failed: the same causes as HTTPS. |
| WebTransport over UDP | The browser opens a WebTransport session. | If WebSocket passed: almost always **UDP port `N` is blocked** somewhere between the browser and the host. It can also mean the game serves WebTransport on another path than the build declares. |

With a self-signed (pinned) game certificate, the HTTPS and WebSocket steps are skipped: the browser can use the certificate's fingerprint for WebTransport only. Browsers without WebTransport skip the last step.

### Check the firewalls

- **The cloud provider's firewall** (security groups, network ACLs) sits in front of the host. It must allow the whole game port range (`--port-range`, default `7000-7099`) for **both UDP and TCP**. This is the most common cause.
- **A home or office router** in front of the host must forward the range, over both protocols, to the host.
- **ufw and firewalld do not apply to game ports.** Docker publishes ports by programming the firewall itself, bypassing them: a game port is reachable even when ufw denies it, and allowing it in ufw changes nothing. To restrict game ports, use `--port-bind-address` or Docker's `DOCKER-USER` chain ([SERVERS.md](SERVERS.md#firewall)).
- **`--port-bind-address`**, when set, publishes game ports on that one host address only.

### Test UDP from outside

The game speaks QUIC on UDP, so a plain UDP probe gets no answer even when the port is open. Watch for the packet on the host instead. On the host:

```sh
sudo tcpdump -ni any udp port 7000
```

From a machine outside the host's network:

```sh
echo probe | nc -u -w1 <host address> 7000
```

If `tcpdump` shows the packet, UDP reaches the host and the game's port is open. If it shows nothing, a firewall on the way drops it. Repeat over TCP with `curl -kv https://<host address>:7000/` while a server runs on that port.

### Check the server

- The server must be running and ready: its page shows its state, and the runtime view shows the game's players and ticks.
- The **game host** (the admin's `game_host` setting, or the first `--public-host`) must be the name or address players use. The game publishes its URLs with it. After changing it, restart the servers (they show "restart required") and reload the panel.
- On IPv6: the browser cannot test a literal IPv6 game host. Use a DNS name that points to the address.

## Networks that block UDP or only allow port 443

When UDP is blocked, the browser client falls back to **WebSocket over TCP, on the same game port**. Players on such networks can still play, with more latency under packet loss. In the connection test, WebSocket passes and WebTransport fails.

Networks that only allow port 443 (some corporate and public networks) cannot reach any game server in this version. Game ports come from the port range, which must lie within 1024-32767, so no game listens on 443. There is no single-port mode (all games behind one port) in this version.

## Certificate problems

The modes are described in [CERTIFICATES.md](CERTIFICATES.md).

**The browser warns about the panel's certificate.** A panel on a network address without TLS files or ACME uses a generated self-signed certificate. Compare its SHA-256 fingerprint, logged when the daemon starts, with the browser's before you accept it. For a lasting fix, give the panel a domain name in `--public-host` and switch the panel certificate to ACME, or use your own files (`--tls-cert`, `--tls-key`).

**The browser warns about the game's page.** The game certificate is self-signed (the default). For public browser play, switch the game certificate to `acme` (or upload a `custom` one).

**WebTransport fails with a self-signed certificate.** Browsers accept a self-signed WebTransport certificate only by pinning its hash, which needs a short-lived certificate (Golem Deploy issues 13-day ones and rotates them) and a page the browser already trusts. Opening the game's own page over its self-signed certificate is not enough for the WebSocket fallback, which needs normal certificate validation. If the certificate just rotated, wait a minute: the game reloads the files every 30 seconds.

**Issuance fails.** The certificate job's log and the certificate status say why. The messages:

| Message | Fix |
|---------|-----|
| `ACME <type>: <CA detail> Check that DNS points to this host.` | The name's DNS record does not point at this host yet. |
| `ACME <type>: <CA detail> Check DNS and that the internet can reach port 80 (HTTP-01) or port 443 (TLS-ALPN-01).` | The CA could not reach the panel. Open TCP 80 (with `--http-listen` on port 80) or 443 (with the panel listening on 443) in every firewall, for each name. |
| `ACME <type>: <CA detail> The CA rate limited this request.` | Let's Encrypt limits certificates for the same set of names to five per week. Wait, and use staging for experiments. Golem Deploy retries after the CA's `Retry-After`. |
| `The ACME CA could not be reached or did not respond in time. Check network access to the CA and try again.` | The host cannot reach the CA. Check outbound HTTPS. |
| `The Let's Encrypt directory does not advertise the shortlived profile required for IP certificates.` | Certificates for IP addresses need the CA's short-lived profile. |

Let's Encrypt refuses names it cannot validate publicly: private and loopback addresses, `localhost` and single-label names. A certificate for a public IP address is short-lived (about 160 hours) and renewed automatically. There is no DNS-01 challenge, so wildcard certificates cannot be issued.

**Renewal.** Renewal runs on its own; the installed certificate stays in use while renewal fails, and the status shows the failure. A missing or expired shared game certificate stops new server starts with `no valid automatic game certificate is installed; renew it (POST /api/v1/certificates/acme/renew)` or `the automatic game certificate has expired; renew it (POST /api/v1/certificates/acme/renew)` (for an uploaded one, `the custom game certificate has expired; upload a new one (PUT /api/v1/certificates/game)`). Fix the cause, then renew from the settings page.

**The game host changed.** In ACME mode the daemon requests a certificate for the new name in the background. Until it arrives, servers keep the old certificate, their certificate shows that it does not cover the game host, and the connection test fails with `The certificate does not cover the game host. Install a certificate issued for that host.`

## Builds fail

The build's detail names the problem, and the build log has the output. Build a fresh clone of your game locally first: most build failures also happen there. Making a game buildable is described in [GAME_INTEGRATION.md](GAME_INTEGRATION.md).

### Before the build starts

| Message | Fix |
|---------|-----|
| `the project has no checkout yet; pull it first (or build with pull)` | Pull the project, or start the build with "pull". |
| `the project checkout is not idle; pull it first (or build with pull)` | Another project job is running, or the last one failed. Pull the project. |
| `the checkout HEAD could not be verified against the recorded commit; pull the project first` | The checkout is incomplete or changed. Pull the project. |
| `another job is already running for this project` | Wait for the clone, pull or branch job to finish. |
| `too many builds are queued (at most 4); wait for one to finish` | One build runs at a time and at most 4 wait. |

### The preflight

The preflight reads a few files from the checkout before Docker runs:

| Message | Fix |
|---------|-----|
| `golem.yaml was not found in the game root "<root>"; set root in golem-deploy.yaml if the game lives in a subdirectory` | Set `root`, or add `golem.yaml`. |
| `go.mod was not found in the game root "<root>"` | `go.mod` must be next to `golem.yaml`. |
| `go.mod does not require github.com/demiurgos-hub/golem-engine; a Golem Engine game is required` | Require the engine. |
| `go.mod requires github.com/demiurgos-hub/golem-engine <version>; Golem Deploy needs v0.4.0-rc.1 or newer` | Upgrade the engine. |
| `go.mod requires github.com/demiurgos-hub/golem-engine at a version Golem Deploy does not accept (<version>)` | Use a release, prerelease or pseudo-version without `+build` metadata. |
| `go.mod replaces <module> with a local path; remove the replace directive and require a released Golem Engine version` | Remove the `replace`. |
| `go.mod has no go directive` or `go.mod asks for a Go version Golem Deploy does not accept (<version>)` | Add a `go 1.x.y` directive. |
| `several commands were found (<list>); set server.package in golem-deploy.yaml` | Name the server package. |
| `the server package was not found (looked for ./cmd/server, a single ./cmd/* command and Go files in the game root); set server.package in golem-deploy.yaml` | Name the server package, or move it to `./cmd/server`. |
| `golem-deploy.yaml is not valid: <parser message>` | A YAML error or an unknown key. |
| `golem-deploy.yaml: <field> <problem>` | A field's value breaks the rules in [GAME_INTEGRATION.md](GAME_INTEGRATION.md#golem-deployyaml). |
| `<file> exceeds the YAML complexity limits (1024 nodes, 32 levels, 128 keys per mapping, no aliases)` | Simplify the file; aliases are not allowed. |
| `<path> is a symbolic link; symbolic links are not followed` | Replace the link with the real file or directory. |
| `<path> is larger than <n> KiB` | Read limits: `golem-deploy.yaml` 32 KiB, `golem.yaml` and `.dockerignore` 64 KiB, `go.mod`, `package.json` and a custom Dockerfile 1024 KiB. |
| `the repository requests a custom Dockerfile; ask an admin to enable allow_custom_dockerfile for this project` | An admin decides; see [SECURITY.md](SECURITY.md#game-repositories). |
| `a custom Dockerfile cannot select a BuildKit frontend; remove the syntax directive` | Remove the `# syntax=` line. |
| `the static add-on "<name>" does not exist in the static store; upload it or remove it from the project` | Upload the file, or detach it. |
| `there is not enough free disk space to extract the static add-on "<name>"` | Free disk space on the data directory's file system (1 GiB must remain). |

### During `docker build`

`docker build failed; see the build log` means a step failed. Look for these lines in the log:

| Line in the build log | Cause and fix |
|-----------------------|---------------|
| `golem-bake: generated code is out of date (<n> difference(s)):` | The committed generated code does not match the schemas. Run `golem-bake` locally and commit. |
| `the server must be a package main` | The server package is a library. Point `server.package` at the `main` package. |
| `golem-deploy: the web client directory web/dist (web.dist) does not exist after the web build` | The web build writes elsewhere. Set `web.dist`. |
| npm errors about a missing `file:` path | A dependency points outside the repository. Require a published version. |
| Download errors from Docker Hub, gcr.io, the Go module proxy or npm | Builds need network access to all of these. Check the host's outbound connections and DNS. |
| A step killed with exit code 137 | This usually means the kernel ended the step for lack of memory. Golem Deploy sets no memory limit on builds; Go and web builds can need more memory than a small host has. Add memory or swap, or stop running servers during builds. |
| `no space left on device` | Docker's storage is full. Remove old images (delete builds), and prune the build cache with `docker builder prune`. |

Other build failures:

| Message | Fix |
|---------|-----|
| `the build exceeded the time limit of <duration>` | Raise `--build-timeout` (1 minute to 24 hours, default 30 minutes), or make the build faster. The first build of a project is the slowest: later ones reuse the Go and npm caches. |
| `the image sets labels in the dev.golemengine.deploy namespace, which is reserved for golem-deploy; remove those LABEL instructions` | Only with a custom Dockerfile. |
| `the dedicated Docker build configuration must contain explicit registry settings and no proxies; see the installation guide` | `<data-dir>/docker-build/config.json` was changed. Remove proxy settings from it. |
| `Docker is not reachable on the host` | See [INSTALL.md](INSTALL.md#troubleshooting). |

## Servers that do not start

A start succeeds only when the game reports ready on its admin endpoint within `--start-timeout` (default 60 seconds, from 1 second to 30 minutes). A failed start keeps the stopped container so its log stays readable, and the job log holds the game's last lines.

| Message | Cause and fix |
|---------|---------------|
| `no game host is configured: set game_host (PATCH /api/v1/settings) or start the daemon with --public-host` | Set the game host in the settings. |
| `the game did not become ready within <duration> and its admin endpoint never answered: the game must use the Golem Engine deploy contract (golem.ServerConfigFromEnv and the generated Runtime.Host); see the server log (GET /api/v1/servers/<id>/logs) and the job log` | The game never opened its admin listener. It does not use `Runtime.Host`, or it is stuck before it. See [GAME_INTEGRATION.md](GAME_INTEGRATION.md#2-the-two-calls-in-the-games-server). |
| `the game did not report ready within <duration>; see the server log (GET /api/v1/servers/<id>/logs) and the job log` | The game answers but is still starting. A large world takes time to load: raise `--start-timeout`. |
| `the container exited before the game became ready (exit code <n>); see the server log (GET /api/v1/servers/<id>/logs) and the job log` | The game crashed. Read its last lines in the job log. A common cause is writing outside `/data` and `/tmp` (the root file system is read-only). |
| `the game's admin endpoint answered outside the Golem Engine admin contract; still waiting` (in the job log) | Something other than the engine's admin listener answers on port 7778. |
| `the server's saved data has schema <a> but build <n> has schema <b>, so the game cannot load it; ...` | **The schema guard.** The saved world was written by a build with other schemas. Assign a build with the saved schema, restore a backup with the build's schema, or reset the data; the message names the requests. See [BACKUPS.md](BACKUPS.md#the-schema-guard). |
| `golem/host: snapshot /data/world.snapshot has schema fingerprint <a> but this server's schema fingerprint is <b>; ...` (in the game's log) | The same mismatch, found by the game because Golem Deploy did not know one of the fingerprints. Same fixes. |
| `port <n> is in use by another process on the host` | Another program holds the port. Find it with `ss -lntup 'sport = :<n>'`, or give the server another port (`PATCH /api/v1/servers/{id}` with `port`, while it is stopped). |
| `port <n> is no longer allowed (outside the configured range <range>, browser-unsafe or the panel's own port); assign a new port with PATCH /api/v1/servers/<id> and start again` | The port range changed. Assign a new port. |
| `the server has no build; assign a successful build with PATCH /api/v1/servers/<id> (build_id) and start again` | Assign a build. |
| `the image of build <n> no longer exists; build the project again` | The image was removed outside Golem Deploy. Build again and assign the new build. |
| `a container named golem-deploy-<name> already exists and this installation does not manage it; check what it is, then remove it (docker rm -f golem-deploy-<name>) and start again` | A container with the server's name exists that this installation did not create. Check it before removing it. |
| `the Docker network golem-deploy belongs to another golem-deploy installation (or to an older version and still has containers attached); ...` | Stop the containers on that network, then start again; an old network is recreated. |
| Certificate messages (`no valid ... game certificate is installed`, `... has expired`) | See [Certificates](#certificate-problems). |

**A server shows "restart required".** Something it was started with changed, for example its build, environment, port, the game host, allowed origins, the certificate mode, the autosave period or the Inspector settings. Restart it to apply the change.

## The Inspector

See [INSPECTOR.md](INSPECTOR.md).

**The Inspector page does not open.** Browsers open it on the Inspector listener, TCP port 8444 of the panel's address by default (`--inspector-listen`). Allow that port in the provider's firewall and on the host (`ufw allow 8444/tcp`). The apply job reports a listener that cannot open: `the Inspector listener could not be opened: TCP port <port> is in use by another program`, `... permission denied for TCP port <port>` or `... the address of --inspector-listen is not an address of this host`. With `--inspector-listen off`, it says `the Inspector is unavailable: the daemon runs with --inspector-listen off`.

**A server does not appear.** Check, in order:

1. The server's own Inspector switch is on, and the server was restarted after it was turned on (it shows "restart required" until then).
2. The Inspector page does not warn **Game servers cannot reach the Inspector**. That warning means the listener is bound to a loopback address, which containers cannot reach; give `--inspector-listen` an address Docker's networks can reach (or set `--inspector-agent-host`).
3. With ufw, port 8444 is allowed: the games' agents connect from their containers to the host through the Docker network's gateway, and ufw drops that traffic otherwise. Egress rules you added must accept this port too ([SERVERS.md](SERVERS.md#network-isolation)).
4. The game uses the generated `Runtime.Host`, which attaches the agent.

When many games start at once, the hub refuses some agents for a moment; they retry on their own.

**The image cannot be downloaded.** `the Inspector image <ref> could not be downloaded; check the reference and the host's network, see the daemon log`. Check the reference in `--inspector-image` and that the host can reach the registry. Images are pulled with the daemon's build Docker configuration, not with your own `docker login`.

**The hub does not become ready or refuses the daemon.** `the Inspector hub does not accept the daemon's password; rotate the Inspector's secrets to replace the hub`: use **Replace secrets** on the Inspector page (`rotate_secrets`). `the Inspector hub limits the daemon's logins for now; apply the Inspector again later`: wait and apply again.

**"Your Inspector session has ended."** A session belongs to one browser tab and ends after 8 hours, at sign-out, when the daemon restarts, when the account's password or role changes, or when the Inspector is turned off or its secrets are replaced. Open the Inspector again from the panel. A link from the panel works once, for 60 seconds.

## Backups and restore

Archives live in `<data-dir>/backups/` (`/var/lib/golem-deploy/backups` in the systemd installation). See [BACKUPS.md](BACKUPS.md).

| Message | Cause and fix |
|---------|---------------|
| `the data directory is larger than the backup size limit of <n> MiB (--backup-max-mb)` | Raise `--backup-max-mb` (default 1024). |
| `the data directory has more than 100000 entries` | The game keeps too many files in `/data`. |
| `not enough free disk space for the backup` | Free space on the data directory's file system; 1 GiB must remain. Delete old backups. |
| `the game is not ready (it is starting or stopping); try again shortly` | Wait for the server to finish starting or stopping. |
| `the game did not finish its snapshot in time; see the server log` or `the game could not write its snapshot; see the server log` | The game's snapshot failed. Read its log. |
| `the backup has schema <a> but build <n> has schema <b>; the server could not load it (restore with force to do it anyway)` | The backup was written by a build with other schemas. Assign a build with the backup's schema first, or restore with force; after a forced restore the server is not started automatically. |
| `the server's image already has files in /data, which Docker copies into every new data volume, so the restored ...` | Only with a custom Dockerfile: leave `/data` empty in the image. |
| `the backup's archive is missing or does not match its checksum` | The file in `backups/` was removed or changed. |
| A restore, reset or deletion stops with a detail about entries a backup cannot hold | The data directory holds links, pipes or devices, which the safety backup cannot store. The detail names the incomplete backup; repeat with **Skip the safety backup** (`skip_safety_backup`) to go ahead anyway. |

The web client holds a download in memory before saving it. Download large archives with an API token and `curl` instead ([BACKUPS.md](BACKUPS.md#downloading-a-backup)).

## Accounts

**Locked out, or a forgotten admin password.** On the host, as the service user:

```sh
sudo runuser -u golem-deploy -- /usr/local/bin/golem-deploy admin reset-password --data-dir /var/lib/golem-deploy --username <name>
```

It prompts for the new password twice (`--password-stdin` reads it from standard input), ends all of that account's sessions and deletes all of its API tokens. It works while the service runs. `admin create` adds a new admin the same way.

**`too many attempts; try again later` (429).** Login failures are limited to 5 per client address and 20 per username in 15 minutes, and registrations to 5 per address per hour. The answer's `Retry-After` header says how long to wait; restarting the daemon also resets the counters. Behind a reverse proxy, all clients share the proxy's address unless the proxy is listed in `--trusted-proxy`, so one user's failures can block everyone.

**`this host name is not served here; open the panel by IP address, by localhost or by a name configured with --public-host` (421).** Add the name you use to `--public-host` and restart the daemon.

**`self-registration is disabled; ask an administrator for an account`.** An admin creates accounts, or turns self-registration on in the settings. Registered accounts can sign in only after an admin approves them.
