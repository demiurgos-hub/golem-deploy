# Security model

This page is for an operator deciding whether and how to run Golem Deploy. It says what Golem Deploy trusts and what it does not, what each role can do, and what it does not protect against. The details live in the subsystem documents linked from each section; this page consolidates them. To report a vulnerability, see the [security policy](../SECURITY.md).

Golem Deploy is in early development. Read this page as a description of the current design, not as an audited guarantee.

## Summary

| Party | Trusted with |
|-------|--------------|
| Root on the host, and the `golem-deploy` service user | Everything. The service user is in the `docker` group. |
| Panel **admins** | Everything, in effect: treat them as root on the host. |
| Panel **developers** and their API tokens | Building and running code from the projects admins set up, reading every project's and server's logs, saved data and backups. Not the host. |
| **Game repositories**, and everyone who can push to them | Nothing on the host. Their code runs only inside build containers and game containers. |
| **Games** (the code in a running container) | Their own data volume, their own environment, their own certificate mount. Not other games' data, and not the panel. |
| **Players** and the internet | Only the game ports and whatever the panel's login allows. |

What Golem Deploy does **not** protect against, in one place:

- An admin, or anyone who can act as the service user, can take over the host.
- A build runs the repository's code with network access, from the host's network position.
- A game can reach the host's own services, your LAN and cloud metadata services, unless you add firewall rules.
- A game can read its own environment, its certificate's private key and, in `custom` or `acme` certificate mode, the key shared by every game.
- Game containers are separated by Docker, not by virtual machines: a kernel or Docker escape is out of scope.
- Every developer sees every server in the Inspector, every server's logs and every backup.
- Denial of service by a game or a build within its limits (CPU, memory, disk, build cache).

## The host and the service user

Golem Deploy manages the host by running `git` and `docker` as its service user, `golem-deploy` in the systemd installation ([INSTALL.md](INSTALL.md)).

- **The `docker` group is root-equivalent.** The service user must be in it, and any member can start a privileged container that mounts the host's file system. So whoever controls the service user, or its data directory, controls the host.
- **Treat panel admins as root on the host.** Admins choose which repositories are built, can allow a repository's own Dockerfile, and manage certificates, settings and every server. Golem Deploy does not try to stop a malicious admin.
- **Keep the host dedicated.** Do not add people to the `docker` group, and do not run other sensitive services on the host: games and builds can reach the host's network ([below](#game-containers)).
- The systemd unit runs the service with only `CAP_NET_BIND_SERVICE`, `NoNewPrivileges`, a read-only file system except for its data and cache directories, and private `/tmp` and `/dev`. These restrict the daemon process, not what it can ask Docker to do.
- Golem Deploy only touches Docker objects labelled `dev.golemengine.deploy.managed=true` with its own instance id, and finds a server's container by the id it recorded, never by a name or labels alone. This keeps it from acting on other containers on the host, and two installations from acting on each other's objects. It is not a security boundary against someone with Docker access. See [SERVERS.md](SERVERS.md#managed-objects).

## Accounts, roles and API tokens

There are two roles, **admin** and **developer**, and API tokens. Every route in the API has one access level, declared in one table (`internal/httpapi/routes.go`); handlers never re-check roles.

| Can | Developer session | API token (any account) | Admin session |
|-----|:-:|:-:|:-:|
| See projects, builds, servers, jobs, images, volumes, static files | yes | yes | yes |
| Pull a project, switch its branch, start a build | yes | yes | yes |
| Create, change (build, port, environment, allowed origins, Inspector switch), start, stop, restart and delete servers | yes | yes | yes |
| Read server logs and stats, take and download backups | yes | yes | yes |
| Upload and delete static files | yes | yes | yes |
| Open the Inspector | yes | no | yes |
| Change own password, create own API tokens | yes | no | yes |
| Create or change projects (repository URL, credentials, custom Dockerfile, static add-ons) | no | no | yes |
| Restore or reset a server's data, delete without a safety backup, delete backups, builds, images, volumes | no | no | yes |
| Settings, certificates, accounts, the Inspector switch, server export and import | no | no | yes |

What this means in practice:

- **A developer runs arbitrary code.** Starting a build runs the repository's code inside a build container with network access ([Builds](#builds)). Switching a project's branch picks which code that is, and assigning a build to a server runs it as a game. A developer cannot change a project's repository URL, but can build any branch of it.
- **A developer can read every server's environment**, in effect: a developer can deploy any successful build of a server's project to that server, and the game reads its environment. Environment values protect secrets from other servers and from people who only use the panel, not from developers of that game. See [SERVERS.md](SERVERS.md#environment-variables).
- **API tokens never exceed the developer role**, even an admin's. They cannot reach admin routes or session-only routes (password change, token creation, Inspector links). A token is `gdp_` followed by 64 hex characters, stored only as its SHA-256 hash, shown once, and can have an expiry. Tokens of an account that is not approved stop working.
- **Jobs started from admin routes are restricted**: only admins can read their logs or cancel them. Other jobs can be cancelled by the account that started them or by an admin.
- **Self-registration is off by default.** When an admin turns it on, new accounts are developers and cannot sign in until an admin approves them.

### Sessions, passwords and rate limits

- Passwords are 12 to 72 bytes and stored as bcrypt hashes (cost 12).
- A session is a signed token (JWT) sent in the `Authorization` header, not a cookie, so other sites cannot make the browser send it. It lasts 12 hours and can be refreshed for up to 30 days after login. Every request re-checks the account in the database: deleting, unapproving or demoting an account, changing its password or logging out ends its sessions at once. The web client keeps the token in the tab's `sessionStorage`, or in `localStorage` with "Keep me signed in".
- Failed logins are limited to 5 per client address and 20 per username in 15 minutes (addresses that signed in successfully as that user are exempt from the username limit), and concurrent logins are capped. Registration attempts are limited to 5 per address per hour. The same limits apply to password checks for a password change or token creation. The counters live in memory and reset when the daemon restarts.
- `golem-deploy admin reset-password --username <name>` on the host sets a new password, ends all sessions of that account and deletes all of its API tokens. Use it to recover a compromised account.

### The panel's TLS and Host rules

- A panel listener on a non-loopback address serves HTTPS: with your certificate files, with an ACME certificate, or until then with a generated self-signed certificate whose SHA-256 fingerprint is in the daemon's startup log. Plain HTTP on a non-loopback address needs `--insecure-http`, for a TLS-terminating proxy you trust. See [CERTIFICATES.md](CERTIFICATES.md).
- Requests are served only for IP addresses, `localhost` and the names in `--public-host` (or the game host when that list is empty). Any other `Host` gets `421`. This stops web pages from reaching a panel on loopback through DNS rebinding.
- Forwarding headers (`X-Forwarded-For`) are ignored unless the peer is in `--trusted-proxy`. Never list networks untrusted clients can connect from: they could choose their address for the rate limits.
- Pages carry a strict Content Security Policy, `X-Frame-Options: DENY` and, with TLS, `Strict-Transport-Security`. API errors never contain internal error text, command output, host paths or secrets.

## Game repositories

A game repository is **untrusted input**, and so is everyone who can push to it.

- **The host only reads a few files from a checkout**: `golem-deploy.yaml`, `.dockerignore`, `golem.yaml`, `go.mod`, the `package.json` files, a custom Dockerfile and the committed `golem_helpers.go` (for the schema fingerprint). It reads them through a reader contained to the checkout that refuses symbolic links and anything that is not a regular file, and caps each file's size. It never writes into the checkout. See [BUILDS.md](BUILDS.md#security-model).
- **No repository value can become a command.** Values placed in the generated Dockerfile or on the `docker build` command line are constants, numbers, strictly checked versions or allowlisted paths; commands are exec-form arrays.
- **Git runs hardened**: system and global git configuration are ignored, hooks and submodules are off, only the `https` and `ssh` transports are allowed, prompts are disabled, every command has a timeout, and checkouts are size-capped (`--max-repo-size-mb`). Credentials are given only to the exact host and port of the project's URL and never appear on a command line, in a URL or in `.git/config`. SSH uses only the project's key and the daemon's own `known_hosts` (first use is trusted unless you pre-seed it). Git LFS settings from the repository that would redirect LFS traffic are refused. See [REPOSITORIES.md](REPOSITORIES.md).
- **The custom Dockerfile switch.** A repository can ask to be built with its own Dockerfile, but only an admin can allow it (`allow_custom_dockerfile` on the project). A custom Dockerfile still cannot select a BuildKit frontend, gets no build arguments, secrets, SSH agent or entitlements, and its image must not set Golem Deploy's labels. But it controls every build step and can read or write other projects' BuildKit cache mounts if it knows their ids. Allow it only for repositories you trust with the host's shared builder and cache.

## Builds

Builds run `docker build`, so every build step runs **inside BuildKit containers managed by the host's Docker**, outside the panel's systemd sandbox.

What builds are protected from:

- Builds get no build arguments, secrets, SSH agent or BuildKit entitlements (`RUN --network=host` and `--security=insecure` fail). `.git` never enters the build context, and the daemon keeps no credentials in it anyway.
- The Docker CLI runs with a fixed minimal environment and a dedicated client configuration in `<data-dir>/docker-build/`, with no inherited registry credentials or proxy settings. An operator can add registry credentials there on purpose; proxy settings there are refused.
- Generated builds use cache mounts private to each project.
- The result is verified: the image is identified by its exact id and must carry exactly the labels the daemon set. Servers run only the image id recorded on a successful build and check its labels again before every start.

The limits of build isolation:

- **Build steps run the repository's code with network access**: `npm ci` (with install scripts), `golem-bake`, the web build, `go build` and every step of a custom Dockerfile. A malicious repository can use a build to reach the internet and anything reachable from Docker's build network, including services on your LAN or cloud network.
- A build can use CPU, memory and disk until the build timeout (`--build-timeout`, default 30 minutes) and fill Docker's build cache. One build runs at a time. Golem Deploy sets no memory limit on builds.
- Base images (`golang`, `node`, `distroless`) are pulled by tag from their registries at build time. Whoever controls those registries controls what builds start from.
- Docker's layer cache is shared across projects. Prune it with `docker builder prune` when needed.

## Game containers

Each game runs in a container built for running untrusted code ([SERVERS.md](SERVERS.md#container-hardening)):

- only verified, local images (`--pull never`), recorded on a successful build;
- a read-only root file system, with size-limited `noexec,nosuid` tmpfs mounts on `/tmp` and every path the image declares as a volume;
- user 65532, all capabilities dropped, `no-new-privileges`, an init process, a pids limit (512), memory and CPU limits (`--server-memory-mb`, default 2048 MiB without extra swap; `--server-cpus`, default 2);
- its own data volume at `/data`, its own certificate directory read-only, and no other mounts;
- the admin port published on `127.0.0.1` only, behind a random per-container token;
- capped `json-file` logs.

Environment values reach containers only through an env file (on Linux an anonymous pipe), never through a command line. Values are encrypted at rest, never logged and never returned by the API. A failed start's log excerpt has every value redacted, as a best effort.

### Network isolation

Containers join the `golem-deploy` bridge network with traffic between containers turned off.

| From a game container to... | Reachable? |
|------------------------------|-----------|
| Another game container directly (by name or container IP) | No |
| Another game's published port, through the host's address | Yes, like any player |
| The host itself, through the bridge gateway (SSH, a database, the panel when it listens on all interfaces) | Yes, unless you add rules |
| The Inspector listener on the host | Yes: games' Inspector agents use this path |
| Your LAN and cloud metadata services (such as `169.254.169.254`) | Yes, unless you add rules |
| The internet | Yes |

[SERVERS.md](SERVERS.md#network-isolation) has example `iptables` rules that block the host, private networks and the metadata service while keeping players and the Inspector working.

Docker publishes game ports by programming the firewall itself, which **bypasses ufw and firewalld**: a published game port is reachable even when ufw denies it. Use `--port-bind-address` or Docker's `DOCKER-USER` chain to restrict exposure.

## Secrets at rest and the key files

The data directory (`/var/lib/golem-deploy` in the systemd installation) holds every secret. On Linux the daemon refuses to start when it is accessible by group or others. Windows is a development platform only: there the daemon does not check or restrict the directory's permissions.

| File | What it protects | If it leaks |
|------|------------------|-------------|
| `keys/secrets.key` | Encrypts (AES-256-GCM) Git credentials, server environment values, game admin tokens and the Inspector's secrets in the database. | With the database, every stored secret can be decrypted. |
| `keys/jwt.key` | Signs session tokens. | Session tokens can be forged. |
| `golem-deploy.db` | Accounts (bcrypt password hashes, API token hashes), settings, encrypted secrets. | Offline password guessing; with `secrets.key`, all secrets. |
| `acme/`, `certs/panel/`, `certs/game/` | ACME account keys and the private keys of panel and game certificates (mode 0600). | Impersonation of the panel or the games for the names they cover. |
| `backups/` | Archives of the games' saved data. | Whatever the games stored. |

Back up `keys/secrets.key` together with the database: without it, stored credentials and environment values cannot be decrypted. Keep both backups as private as the host itself.

## Certificates and their keys

See [CERTIFICATES.md](CERTIFICATES.md) for modes and renewal.

- **A game can read its certificate's private key**: the key is in its read-only mount so the engine can serve TLS, and the game can print it into its log. In `self_signed` mode each server has its own short-lived key. In `custom` and `acme` mode **all games share one key**.
- **Keep panel names outside the game certificate.** A game holding a certificate that covers a panel name (including through a wildcard) could impersonate the panel. If games are not fully trusted, reach the panel by names (`--public-host`) that the shared game certificate does not cover.
- Panel keys and ACME account keys are private to the daemon and never mounted into a game.
- ACME is off until an admin accepts the CA's terms and switches a service to it. Issuance needs TCP port 80 or 443 reachable for each name.

## The Inspector

See [INSPECTOR.md](INSPECTOR.md#security-model-and-limits).

- It is off until an admin turns it on, and each server has its own switch.
- **Every developer account can open it** and see every server whose switch is on. There is no per-server permission.
- **A game with the switch on can register under another server's name.** Turn the switch on only for games you trust as much as each other.
- The hub runs from the configured image (`--inspector-image`). Whoever controls that image reference controls the hub's code, which sees the entity state of inspected servers and serves the Inspector's web pages. Pin a digest if you need a fixed image.
- Only the daemon talks to the hub. Browsers get one-time links (60 seconds, one use) and a session key held in the tab, not a cookie, so game servers on other ports of the same host cannot read it. Games' agents connect through the daemon with per-server tokens.
- The Inspector listener (TCP 8444 by default) is reachable by anyone who can reach its port, but without a valid session it serves no data.

## Backups

A backup is an archive of a game's data directory, and everything in it was written by the game, so it is **untrusted** ([BACKUPS.md](BACKUPS.md)).

- The daemon stores only regular files and directories, with owners reset and modes masked; links, pipes, devices and unsafe names are left out and only counted, never named.
- It interprets nothing in the archive except the header of the top-level `world.snapshot`, to record the schema fingerprint.
- Archives are size-capped (`--backup-max-mb`, default 1024 MiB, and 100000 entries) and need 1 GiB of free disk space to remain.
- There is no upload: a restore uses only archives the daemon made itself, and checks their size and SHA-256 first. A restore, reset or deletion first takes a safety backup.
- A downloaded archive holds whatever the game wrote. Treat it as untrusted on your own computer, and check its SHA-256 if it travels.

## Restarting or stopping the panel

- **Game servers keep running.** Their containers belong to Docker, not to the service. So does the Inspector hub.
- Open requests finish, running jobs are cancelled, and jobs and builds interrupted by a stop are marked as such at the next start.
- Inspector sessions live in the daemon's memory and end. Login rate limit counters reset.
- At start, the daemon verifies the running containers' images and their games' readiness, and starts servers that should be running.
- After a game crashes, Docker restarts it up to 5 times, also while the panel is down.

## Hardening checklist

- [ ] **A dedicated host** (or virtual machine) for Golem Deploy, with nothing sensitive on it or reachable from it.
- [ ] **A firewall in front of the host** (the provider's security group) that allows only SSH, the panel's ports, the game port range over UDP and TCP, and the Inspector port if you use it. Remember that Docker bypasses ufw for game ports.
- [ ] **Egress rules** for the game network if games or builds are not fully trusted ([SERVERS.md](SERVERS.md#network-isolation)).
- [ ] **A domain name and a trusted certificate** for the panel and the game host, via ACME or your own files, and panel names outside the shared game certificate.
- [ ] **Self-registration off**, unless you need it, and approve only accounts you know.
- [ ] **Few admins.** Give people the developer role, and API tokens with an expiry for automation.
- [ ] **The custom Dockerfile switch off** unless you trust the repository.
- [ ] **The Inspector off**, or on only for servers you trust as much as each other.
- [ ] **Updates** of the host, Docker and Golem Deploy.
- [ ] **Backups of the data directory**, at least `golem-deploy.db`, `keys/` (including `keys/secrets.key`) and `backups/`, stored as privately as the host.
