# Installing Golem Deploy on a Linux host

`scripts/install.sh` installs an already built `golem-deploy` binary as a systemd service on **Ubuntu or Debian**. It is a minimal installer: it does not download releases, install Docker, configure certificates or touch the firewall. A full installer (checksum-verified releases, Docker installation, setup link, certificate management) will replace it.

Windows with Docker Desktop is supported for development only. On Windows, Golem Deploy leaves the data directory ACL unchecked and unchanged. Certificate keys and temporary environment files inherit its permissions. Run production installations on Linux.

## Requirements

- Ubuntu or Debian with systemd as the init system, and root access.
- **Docker Engine** installed and running, from Docker's packages: follow [Install Docker Engine on Ubuntu](https://docs.docker.com/engine/install/ubuntu/) or [on Debian](https://docs.docker.com/engine/install/debian/). The installer needs the `docker` command, a reachable daemon and the `docker` group those packages create. Snap and rootless Docker are not supported.
- A `golem-deploy` binary built for the host's architecture, and the source checkout it came from (the installer takes the unit file from `deploy/`). To build it on another machine:

  ```sh
  (cd frontend && npm ci && npm run build)          # embed the web client
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o golem-deploy ./cmd/golem-deploy   # GOARCH=arm64 for ARM hosts
  ```

  Without the web client build the binary works, but the panel only shows a "web client not built" notice.

The installer installs `git`, `git-lfs`, `openssh-client` (for SSH repository URLs) and `ca-certificates` with apt when they are missing.

## Install

Copy the checkout (or at least `scripts/install.sh` and `deploy/golem-deploy.service`, keeping that layout) and the binary to the host, then:

```sh
sudo scripts/install.sh /path/to/golem-deploy
```

Options (they only apply when the settings file is created; see [Configuration](#configuration)):

| Option | Default | Meaning |
|--------|---------|---------|
| `--public-host NAMES` | none | Comma-separated host names the panel is reached by (`GOLEM_DEPLOY_PUBLIC_HOST`); IP addresses and `localhost` always work |
| `--listen ADDR` | `127.0.0.1:8000`, or `:8443` with TLS | Panel listen address (`GOLEM_DEPLOY_LISTEN`); the port must be 1024 or higher |
| `--port-range LO-HI` | `7000-7099` | Host ports for game servers, within 1024-32767 (`GOLEM_DEPLOY_PORT_RANGE`) |
| `--tls-cert FILE`, `--tls-key FILE` | none | Existing PEM certificate (chain) and private key, copied to `/etc/golem-deploy/tls/` |

Examples:

```sh
# Panel on loopback only, reached through an SSH tunnel.
sudo scripts/install.sh ./golem-deploy

# Panel on the network with your own certificate.
sudo scripts/install.sh --public-host deploy.example.com \
  --tls-cert /etc/letsencrypt/live/deploy.example.com/fullchain.pem \
  --tls-key /etc/letsencrypt/live/deploy.example.com/privkey.pem \
  ./golem-deploy
```

At the end the script prints the next steps:

1. **Create the first admin**, as the service user so the files it creates stay owned by it:

   ```sh
   sudo runuser -u golem-deploy -- /usr/local/bin/golem-deploy admin create --data-dir /var/lib/golem-deploy --username admin
   ```

   It prompts for the password twice; `--password-stdin` reads it from the first line of standard input instead. It works while the service is running.

2. **Reach the panel.** Without TLS it listens on `127.0.0.1:8000` with plain HTTP and is reachable from the host only. From your computer:

   ```sh
   ssh -N -L 8000:127.0.0.1:8000 <user>@<host>
   ```

   then browse to `http://localhost:8000/`. With TLS, browse to `https://<public host>:8443/` and allow that TCP port in your firewall.

3. **Open the game ports.** Players connect to game servers on the port range over **UDP and TCP**. The script does not change firewall rules: allow the range for both protocols in your provider's firewall (security groups). Docker publishes the ports itself and bypasses ufw and firewalld; see [SERVERS.md](SERVERS.md#firewall).

## What the installer does

It stops at the first failed check, before changing anything: root, Ubuntu or Debian (`/etc/os-release`), systemd running, the `docker` command, a reachable Docker daemon, the `docker` group, a Linux executable that runs (`golem-deploy version`), the unit file next to the script, and the options. Then it:

| Creates or changes | Details |
|--------------------|---------|
| apt packages | `git`, `git-lfs`, `openssh-client`, `ca-certificates` if missing (`--no-install-recommends`) |
| User and group `golem-deploy` | System account, shell `nologin`, home `/var/lib/golem-deploy`; added to the `docker` group; the script then checks that this user can reach Docker |
| `/usr/local/bin/golem-deploy` | The binary (0755 root), replaced atomically |
| `/var/lib/golem-deploy` | The data directory (0700, owned by `golem-deploy`); the daemon fills it on first start (see the data directory notes in the [README](../README.md#run)) |
| `/etc/golem-deploy/` | 0750 `root:golem-deploy` |
| `/etc/golem-deploy/golem-deploy.env` | The settings (0640 `root:golem-deploy`) |
| `/etc/golem-deploy/tls/cert.pem`, `key.pem` | Only with `--tls-cert`/`--tls-key`: certificate 0644, key 0640 `root:golem-deploy` |
| `/etc/systemd/system/golem-deploy.service` | The unit from [`deploy/golem-deploy.service`](../deploy/golem-deploy.service), enabled and (re)started |
| `/var/cache/golem-deploy` | Created by systemd for Docker client state used by operations outside the build pipeline (`DOCKER_CONFIG`) |

After restarting the service it waits up to 30 seconds for the daemon to log `golem-deploy listening`; if the service fails instead, it prints `systemctl status` and the log of that start and exits with an error.

### The service

The unit runs `golem-deploy serve` as `golem-deploy` with the settings file as its environment, restarts it on failure (5 s apart, at most 5 times in 5 minutes) and gives it 30 seconds to stop. Stopping or restarting the panel **does not stop game servers**: their containers belong to Docker, not to the service, and keep running; the daemon only finishes open requests and cancels running jobs.

The unit is sandboxed with systemd's hardening options: no capabilities and no privilege escalation, a read-only file system except the data directory, no access to home directories, a private `/tmp` and `/dev`, kernel settings protected, and a system call filter. Each option is explained in the unit file. This limits what a bug in the daemon or in git can do directly; it does not limit what the service can do through Docker (see [Security notes](#security-notes)). Because the service has no capabilities, the panel cannot listen on ports below 1024.

### Configuration

`/etc/golem-deploy/golem-deploy.env` holds one `KEY=value` per line (no quotes) for the `GOLEM_DEPLOY_*` variables of `golem-deploy serve`, listed in the [README](../README.md#run): data directory, listen address, public hosts, port range and, with TLS, the certificate paths. Change settings by editing it, then:

```sh
sudo systemctl restart golem-deploy
```

The installer never overwrites this file. When it already exists, `--public-host`, `--listen` and `--port-range` are ignored with a warning.

To run the panel behind a TLS-terminating reverse proxy on the same host, keep it on loopback and add the proxy and host name, for example:

```sh
GOLEM_DEPLOY_LISTEN=127.0.0.1:8000
GOLEM_DEPLOY_PUBLIC_HOST=deploy.example.com
GOLEM_DEPLOY_TRUSTED_PROXY=127.0.0.1/32
```

Only if the proxy runs on another machine does the panel need a non-loopback address, which without TLS also needs `GOLEM_DEPLOY_INSECURE_HTTP=true`; never expose plain HTTP to untrusted networks.

### TLS certificates

Certificates are copied, not referenced, so the service user never needs access to the directory they come from (Let's Encrypt keeps keys readable by root only). They are **not renewed automatically**: after each renewal, re-run the installer with `--tls-cert` and `--tls-key` (it replaces the copies and restarts the service). If the settings file was created without TLS, add the two lines the installer prints, adjust `GOLEM_DEPLOY_LISTEN` (for example `:8443`) and restart. Certificate management is planned for a later version.

## Upgrade

Build or copy the new binary and the matching checkout, then run the installer again with the same command:

```sh
sudo scripts/install.sh /path/to/new/golem-deploy
```

It replaces the binary and the unit, keeps the user, the settings file and the data directory (it says what it kept) and restarts the service. Running game servers keep running during the restart. Before upgrading, back up the data directory (at least `golem-deploy.db` and `keys/`), preferably with the service stopped; there are no migration paths between development versions yet.

## Uninstall

There is no uninstall script. As root:

```sh
systemctl disable --now golem-deploy
rm /etc/systemd/system/golem-deploy.service
systemctl daemon-reload
rm /usr/local/bin/golem-deploy
rm -r /var/cache/golem-deploy
```

That keeps the data and the settings. To remove them too (this deletes every project checkout, the database and the encryption keys; stored credentials cannot be recovered without `keys/secrets.key`):

```sh
rm -r /var/lib/golem-deploy /etc/golem-deploy
userdel golem-deploy        # also removes the golem-deploy group if nothing else uses it
```

Game server containers, images, volumes and the `golem-deploy` network stay in Docker. List and remove them by label if you no longer need them:

```sh
docker ps -a --filter label=dev.golemengine.deploy.managed=true
docker rm -f $(docker ps -aq --filter label=dev.golemengine.deploy.managed=true)
docker network rm golem-deploy
docker image ls --filter label=dev.golemengine.deploy.managed=true
docker volume ls --filter label=dev.golemengine.deploy.managed=true
```

The apt packages and Docker itself are left installed.

## Security notes

- **The `docker` group is root-equivalent.** The service user must be in it to drive Docker, and any member of the group can start a privileged container that mounts the host's file system. Treat the `golem-deploy` account, and the panel's admin accounts, as root on this host. Do not add people to the `docker` group, and keep the host dedicated.
- **Plain HTTP on loopback until certificates are configured.** Without `--tls-cert`/`--tls-key` the panel serves unencrypted HTTP and the daemon refuses to listen on anything but a loopback address. Reach it through an SSH tunnel (or a reverse proxy that terminates TLS on the same host). With TLS, the panel is reachable on the network; consider restricting the panel port to your own addresses.
- The settings file and the TLS key are readable by root and the service group only; the data directory (database and keys) by the service user only.
- Game servers can reach the host and its networks; see [SERVERS.md](SERVERS.md#network-isolation) for the isolation model and optional egress rules.
- **Builds and the service sandbox.** `docker build` runs as the service user under the unit's restrictions: the buildx client uses the dedicated `DOCKER_CONFIG` at `/var/lib/golem-deploy/docker-build`, its scratch files in the private `/tmp`, and the daemon writes the generated Dockerfile, `--iidfile` and `--metadata-file` below the data directory's `tmp/`, all writable under `ProtectSystem=strict`. The build itself (and every `RUN` step) executes inside dockerd's BuildKit, outside the sandbox. Keep the default `docker` builder in the dedicated configuration. Builds use public base images by default. To grant registry access deliberately, run `sudo -u golem-deploy env DOCKER_CONFIG=/var/lib/golem-deploy/docker-build docker login <registry>`. Keep proxy settings out of this configuration. Docker discovers buildx in its system plugin paths and the original client's `cli-plugins/`. For other plugin locations, install buildx under the dedicated `cli-plugins/` or set `cliPluginsExtraDirs` there. If the primary Docker client uses a named TLS context, import that context into the dedicated directory as well. The game certificate (`/var/lib/golem-deploy/certs/game`) is bind-mounted by dockerd from its real path, which the unit leaves unchanged.

## Troubleshooting

- **Logs:** `journalctl -u golem-deploy -f` (the daemon's own log, including Docker errors of failed jobs); `systemctl status golem-deploy` for the state.
- **The service does not start, `bind: address already in use`:** another process holds the listen port. Find it with `ss -ltnp 'sport = :8000'`, then stop it or change `GOLEM_DEPLOY_LISTEN` and restart. A game port range that contains the listen port is refused at startup.
- **`refusing to serve plaintext HTTP on a non-loopback address`:** configure TLS (see above) or set the listen address back to `127.0.0.1:8000`.
- **Docker unreachable in the panel, or `permission denied ... /var/run/docker.sock` in the log:** check that the user is in the group (`id golem-deploy` lists `docker`), that the socket belongs to that group (`ls -l /var/run/docker.sock` shows `root docker`), and that Docker runs (`systemctl status docker`). Group changes apply when the service restarts: `systemctl restart golem-deploy`. The installer checks this with `runuser -u golem-deploy -- docker version`.
- **Data directory refused** (`has mode ...`, `not empty and was not initialised by Golem Deploy`): the data directory must be owned by `golem-deploy`, mode 0700, and contain only what Golem Deploy put there. The installer refuses an existing `/var/lib/golem-deploy` with another owner or mode and prints the `chown`/`chmod` to run if it is the right directory.
- **`start request repeated too quickly`:** the service failed 5 times in 5 minutes. Fix the cause shown in the log, then `systemctl reset-failed golem-deploy && systemctl start golem-deploy`.
- **`admin create` fails with permission errors:** run it as the service user with `runuser -u golem-deploy --` (or `sudo -u golem-deploy`) and pass `--data-dir /var/lib/golem-deploy`. Running it as root can leave root-owned files the service cannot read; fix them with `chown -R golem-deploy:golem-deploy /var/lib/golem-deploy`.
- **Players cannot connect:** check that the server runs in the panel, then the provider firewall for the port range over **both UDP and TCP**.
