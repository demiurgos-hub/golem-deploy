# Installing Golem Deploy on a Linux host

The installer sets up Golem Deploy as a systemd service on **Ubuntu or Debian**. It downloads a release, checks its checksum, installs Docker Engine when it is missing, and prints a one-time link to create the first admin account. Running it again upgrades the installation.

Windows with Docker Desktop is supported for development only. On Windows, Golem Deploy leaves the data directory ACL unchecked and unchanged. Certificate keys and temporary environment files inherit its permissions. Run production installations on Linux.

## Requirements

- Ubuntu or Debian with systemd as the init system, on amd64 or arm64, and root access. To have Docker installed by the script, use a release that [Docker supports](https://docs.docker.com/engine/install/ubuntu/#os-requirements) (for example Ubuntu 22.04 or 24.04, Debian 12).
- `curl`, which cloud images include. The installer installs the rest with apt: `git`, `git-lfs`, `openssh-client` (for SSH repository URLs) and `ca-certificates`, and, when Docker is missing, `gpg` (to check Docker's key) and Docker Engine with the buildx plugin.
- Network access to GitHub and, when Docker is installed, to `download.docker.com`. Builds later need Docker Hub, gcr.io, the Go module proxy and npm. For hosts without network access, see [Offline installs](#offline-installs).
- A dedicated host is best: the service user is root-equivalent through Docker (see [Security](#security)).

## Install

```sh
curl -fsSL https://github.com/demiurgos-hub/golem-deploy/releases/latest/download/install.sh | sudo bash
```

Options go after `bash -s --`:

```sh
curl -fsSL https://github.com/demiurgos-hub/golem-deploy/releases/latest/download/install.sh | sudo bash -s -- --public-host deploy.example.com
```

The script never asks questions, so it can run from a pipe or from provisioning tools. Everything in the script is in functions called from its last line, so a download cut off before that line is complete runs nothing. To read it first, download it, then run `sudo bash install.sh [options]`.

Where the program comes from:

| Option | Meaning |
|--------|---------|
| (none) | Download the release the script belongs to; for `install.sh` from `releases/latest`, that is the latest release. A script from a source checkout downloads the latest release |
| `--version TAG` | Download release `TAG`, for example `v0.1.0` |
| `--archive FILE` | Install from a downloaded release archive ([offline](#offline-installs)); it needs `--sha256` or a `SHA256SUMS` next to it |
| `--sha256 HEX` | The expected SHA-256 of the archive, checked instead of the release's `SHA256SUMS` ([verifying downloads](#verifying-downloads)) |
| `--binary FILE` | Install a binary you built, from a source checkout ([building from source](#building-from-source)) |

Host changes:

| Option | Meaning |
|--------|---------|
| `--no-docker` | Never install Docker Engine or the buildx plugin; stop when they are missing |
| `--no-sysctl` | Leave the UDP buffer limits alone ([kernel settings](#kernel-settings)) |

Settings. They only apply when the settings file is created; see [Configuration](#configuration):

| Option | Default | Meaning |
|--------|---------|---------|
| `--public-host NAMES` | this host's public address, if it has one | Comma-separated host names the panel is reached by (`GOLEM_DEPLOY_PUBLIC_HOST`); IP addresses and `localhost` always work |
| `--listen ADDR` | `:443` (with HTTP on `:80`) when both ports are free, a free loopback port from 8000 to 8099 on conflict, or `:8443` with TLS files | Panel listen address (`GOLEM_DEPLOY_LISTEN`) |
| `--http-listen ADDR` | `:80` with the default `--listen`, otherwise none | HTTP listener for certificate challenges and redirects to HTTPS (`GOLEM_DEPLOY_HTTP_LISTEN`). Give it together with `--listen`, for example `--listen :8443 --http-listen :80` when another program holds port 443. It needs a panel that serves HTTPS |
| `--port-range LO-HI` | `7000-7099` | Host ports for game servers, within 1024-32767 (`GOLEM_DEPLOY_PORT_RANGE`) |
| `--tls-cert FILE`, `--tls-key FILE` | none | Existing PEM certificate (chain) and private key, copied to `/etc/golem-deploy/tls/` |

Examples:

```sh
# Managed HTTPS on 443 with redirects and HTTP-01 on 80, when both are free.
curl -fsSL .../install.sh | sudo bash -s -- --public-host deploy.example.com

# Panel on loopback, reached through an SSH tunnel or a reverse proxy.
curl -fsSL .../install.sh | sudo bash -s -- --listen 127.0.0.1:8000

# Panel on the network with your own certificate.
sudo bash install.sh --public-host deploy.example.com \
  --tls-cert /etc/letsencrypt/live/deploy.example.com/fullchain.pem \
  --tls-key /etc/letsencrypt/live/deploy.example.com/privkey.pem
```

`GOLEM_DEPLOY_RELEASE_URL` replaces the download location with a directory that holds one release's files (`https://` for a mirror, `file://` for a local copy). The checksum check still applies.

## The first admin account

When the service has started, the installer runs `golem-deploy admin setup-link` as the service user and prints the link as the first next step:

```
1. Create the first admin account: open this link in your browser.
   It works once and for one hour.

     https://deploy.example.com/setup#t=...
```

The link is a secret: whoever opens it first can choose the first admin's username and password and submit them. Opening it alone creates nothing. The installer only prints it; it never writes it to a file or to the journal, and it turns shell tracing off while it holds the link, so `bash -x` does not show it either. To make a new one (for example when it expired):

```sh
sudo runuser -u golem-deploy -- /usr/local/bin/golem-deploy admin setup-link --data-dir /var/lib/golem-deploy --origin https://deploy.example.com
```

The installer uses the data directory the settings file names for this command and the ones it prints, quoted for the shell where needed. It accepts only one plain line `GOLEM_DEPLOY_DATA_DIR=/path` with an absolute path the daemon accepts (no quotes, `$`, backslash, line break, `,` or `=`). For any other value it makes no link, says why and prints the command with `<data directory>` in its place, because another directory could hold another database. The address in the link is built from the settings file: `https` unless the panel listens on loopback or with plain HTTP, the first public host (or the detected address), and the port unless it is the default one. When the panel listens on loopback, the link starts with `http://localhost:<port>` and the installer first prints the SSH tunnel to open:

```sh
ssh -N -L 8000:127.0.0.1:8000 <user>@<host>
```

When the panel serves its generated self-signed certificate, the browser warns about it. The installer prints the certificate's SHA-256 fingerprint (the daemon also logs it at start as `panel self-signed TLS fingerprint`); continue only when the browser shows the same one. Set up an automatic certificate in the panel's settings afterwards (see [CERTIFICATES.md](CERTIFICATES.md)).

When the service is active but did not log that it is listening within 30 seconds, the installer makes no link either; it says how to check the service and prints the command to make the link afterwards. When an admin account already exists, the installer just shows the panel's address. With a binary that has no `setup-link` command, it prints the command to create an admin on the host instead:

```sh
sudo runuser -u golem-deploy -- /usr/local/bin/golem-deploy admin create --data-dir /var/lib/golem-deploy --username admin
```

It prompts for the password twice; `--password-stdin` reads it from the first line of standard input instead. Run admin commands as the service user so the files they create stay owned by it.

## What the installer does

It first runs these checks and stops at the first one that fails, before changing anything: the options (it copies `--tls-cert` and `--tls-key` into a private temporary directory and checks those copies, including, when `openssl` is installed, that they belong together; the copies are what it installs), root, Ubuntu or Debian (`/etc/os-release`, and the release's code name when Docker's repository is to be added), amd64 or arm64, systemd running, that none of the files it writes (the binary, the uninstaller, the unit, the settings, the sysctl file, the TLS copies) is a directory, the state of Docker and of Docker's apt files (below), then the download, its checksum and its contents, and finally that the binary runs (`golem-deploy version`) and, for a download of a given version, reports that version.

Some failures can only show up after changes began. They stop the installer with an error, and running it again after fixing the cause continues from there:

- **Installing packages** (`apt-get update` or `apt-get install` fails): packages apt installed so far stay installed. apt runs with `--no-remove`, so it never removes a package.
- **Docker's repository key** has the wrong fingerprint, cannot be downloaded, or `/etc/apt/keyrings/docker.asc` already exists with other content: `git`, `git-lfs`, `openssh-client`, `ca-certificates` and `gpg` may already be installed; Docker's keyring and source file are not written.
- **Docker's packages** would not be downloaded from Docker's repository alone (another source offers them with the same or a higher priority, or a pin wins), or installing them fails: the keyring and `/etc/apt/sources.list.d/docker.sources` stay and are used by the next run; no Docker package is installed in the first case.
- **The service does not start:** the user, the data directory, the binary, the uninstaller, the unit and the UDP buffer settings are in place, and the unit is enabled. When this run wrote the settings file, it is moved to `/etc/golem-deploy/golem-deploy.env.failed` (same owner and mode) and the service is stopped, so that the next run writes a new one from its options; the installer says so. An existing settings file is kept as it is.

Then it:

| Creates or changes | Details |
|--------------------|---------|
| apt packages | `git`, `git-lfs`, `openssh-client`, `ca-certificates` if missing (`--no-install-recommends`) |
| Docker Engine | Only when the `docker` command is missing: Docker's apt repository and its packages ([Docker](#docker)) |
| `/etc/sysctl.d/60-golem-deploy.conf` | Larger UDP buffer limits ([kernel settings](#kernel-settings)) |
| User and group `golem-deploy` | System account, shell `nologin`, home `/var/lib/golem-deploy`; added to the `docker` group; the script then checks that this user can reach Docker |
| `/usr/local/bin/golem-deploy` | The binary (0755 root), replaced atomically |
| `/usr/local/sbin/golem-deploy-uninstall` | The uninstaller ([Uninstall](#uninstall)) |
| `/var/lib/golem-deploy` | The data directory (0700, owned by `golem-deploy`); the daemon fills it on first start (see the data directory notes in the [README](../README.md#run)) |
| `/etc/golem-deploy/` | 0750 `root:golem-deploy` |
| `/etc/golem-deploy/golem-deploy.env` | The settings (0640 `root:golem-deploy`) |
| `/etc/golem-deploy/tls/cert.pem`, `key.pem` | Only with `--tls-cert`/`--tls-key`: certificate 0644, key 0640 `root:golem-deploy` |
| `/etc/systemd/system/golem-deploy.service` | The unit from [`deploy/golem-deploy.service`](../deploy/golem-deploy.service), enabled and (re)started |
| `/var/cache/golem-deploy` | Created by systemd for Docker client state used by operations outside the build pipeline (`DOCKER_CONFIG`) |

Downloads use HTTPS only (TLS 1.2 or newer, no redirect to plain HTTP) with retries and a size cap (256 MiB for the archive, 1 MiB for `SHA256SUMS` and Docker's key) that holds also when the server sends no length. The archive is checked against `SHA256SUMS` before it is opened, and only its four expected files are extracted, as plain files, into a private temporary directory: anything else in the archive (other names, links, directories) stops the installation. The installer sets file modes itself and ignores those in the archive.

Downloads ignore root's `~/.curlrc` (`curl -q`), so a setting there cannot turn off certificate checks. `--archive` copies at most one byte more than the cap and stops when the copy is larger, and archive listings use numeric owners, so an owner name cannot hide a member's size. Fixed destinations are written with `install -T` and `mv -T`, so a directory at such a path is an error, never a place a file is dropped into.

After restarting the service it waits up to 30 seconds for the daemon to log `golem-deploy listening`. If the restart or the service fails instead, it prints `systemctl status` and the log of that start and exits with an error. If the service is active but never logs that line, it warns and makes no setup link.

It never changes firewall rules. When ufw is active, it prints the `ufw allow` commands for the panel's ports.

### Docker

- **Docker works:** the installed Docker is kept as it is, whatever its version or source. Builds need the buildx plugin: when `docker buildx version` fails, the installer installs `docker-buildx-plugin` if Docker's apt repository is configured (with the same `apt-cache policy` check as below), and stops otherwise.
- **Docker is missing:** the installer follows [Docker's instructions for Ubuntu](https://docs.docker.com/engine/install/ubuntu/) and [Debian](https://docs.docker.com/engine/install/debian/): it downloads Docker's repository key, checks that it has Docker's fingerprint `9DC8 5822 9FC7 DD38 854A E2D8 8D81 803C 0EBF CD88` (installing the distribution's `gpg` package first when `gpg` is missing) and stops on a mismatch. Only then does it write the key to `/etc/apt/keyrings/docker.asc` and `/etc/apt/sources.list.d/docker.sources` with `Signed-By:` that key. After `apt-get update` it checks with `apt-cache policy`, for each of `docker-ce docker-ce-cli containerd.io docker-buildx-plugin`, the sources that offer the candidate version: apt downloads it from one with the highest priority among them, so every source with that priority must be `https://download.docker.com/linux/ubuntu` (or `/debian`), compared as the whole URL. A mirror or local repository offering the same version with the same or a higher priority, or a pin that makes another version the candidate, stops it before any Docker package is installed. Then it installs them with `--no-remove`, enables and starts Docker and waits until it answers. When the repository is already configured, it is used as it is. Only enabled entries with a URI of Docker's repository count, read the way apt reads them: `deb` lines in `/etc/apt/sources.list` and `*.list` files (everything after `#` is a comment, and the URI field is compared, not the whole line), and stanzas in `*.sources` files that apt does not disable (`Enabled:` `no`, `false`, `0` or `off`, in any letter case). Backups such as `docker.sources.save` do not count. The installer never overwrites an existing `/etc/apt/sources.list.d/docker.sources` or `/etc/apt/keyrings/docker.asc`: when `docker.sources` exists but is disabled, it stops before changing anything and says to enable or remove it; when the keyring exists with content other than the key it just downloaded, it stops before writing anything for Docker and says to remove it.
- **Docker's packages are installed but Docker is stopped:** it is enabled and started.
- **Conflicting packages:** when distribution packages that conflict with Docker's are installed (`docker.io`, `docker-compose`, `docker-compose-v2`, `docker-doc`, `docker-buildx`, `podman-docker`, `containerd`, `runc`) and Docker does not work, the installer stops and prints the `apt-get remove` command. It never removes packages itself.
- Docker from a snap and rootless Docker are not supported.
- `--no-docker` turns all of this off: the installer then stops when Docker or buildx is missing.

### Kernel settings

Games use QUIC (WebTransport), which works better with UDP buffers larger than the Linux default. The installer writes `/etc/sysctl.d/60-golem-deploy.conf` with `net.core.rmem_max` and `net.core.wmem_max` set to 7500000 bytes, the value quic-go recommends, and applies that file. A value that is already higher is kept. Inside a container the values usually cannot be changed; the installer then says so, and they apply at the next boot of a real host. `--no-sysctl` skips this.

### Public host

On a new installation without `--public-host`, the installer asks the kernel which address it uses for outgoing traffic (`ip route get`, which sends nothing). When that is a public address, it becomes `GOLEM_DEPLOY_PUBLIC_HOST`, which also makes it the default game host players connect to. When it is a private address, as on cloud hosts behind NAT, the setting stays empty: pass `--public-host` with the host's public name or address, or set the game host in the panel's settings.

### The service

The unit runs `golem-deploy serve` as `golem-deploy` with the settings file as its environment, restarts it on failure (5 s apart, at most 5 times in 5 minutes) and gives it 30 seconds to stop. Stopping or restarting the panel **does not stop game servers**: their containers belong to Docker, not to the service, and keep running; the daemon only finishes open requests and cancels running jobs.

The unit grants only `CAP_NET_BIND_SERVICE`, which lets the daemon bind ports below 1024. With no listen override or TLS files and both ports free, a fresh install uses TCP 443 for the panel and TCP 80 for ACME HTTP-01 and HTTPS redirection. The unit also disables privilege escalation, protects the kernel settings, uses a read-only filesystem except for the data and cache directories, and gives the service private `/tmp` and `/dev` directories. The unit file explains each setting.

### Configuration

`/etc/golem-deploy/golem-deploy.env` holds one `KEY=value` per line (no quotes) for the `GOLEM_DEPLOY_*` variables of `golem-deploy serve`, listed in the [README](../README.md#run): data directory, HTTPS and optional HTTP listener addresses, public hosts, port range, optional certificate paths and limits such as `GOLEM_DEPLOY_BACKUP_MAX_MB` (the largest data directory a server backup accepts, default 1024 MiB; see [BACKUPS.md](BACKUPS.md)). Change settings by editing it, then:

```sh
sudo systemctl restart golem-deploy
```

The installer never overwrites this file. When it already exists, `--public-host`, `--listen`, `--http-listen` and `--port-range` are ignored with a warning. The one exception is a file the same run wrote: when the service then does not start (for example because the daemon refuses an option), the file is moved to `golem-deploy.env.failed`, so that running the installer again with corrected options writes a new one.

To run the panel behind a TLS-terminating reverse proxy on the same host, keep it on loopback and add the proxy and host name, for example:

```sh
GOLEM_DEPLOY_LISTEN=127.0.0.1:8000
GOLEM_DEPLOY_PUBLIC_HOST=deploy.example.com
GOLEM_DEPLOY_TRUSTED_PROXY=127.0.0.1/32
```

Only if the proxy runs on another machine does the panel need a non-loopback address, which without TLS also needs `GOLEM_DEPLOY_INSECURE_HTTP=true`; never expose plain HTTP to untrusted networks.

### Firewall

The installer does not change firewall rules. Allow in your provider's firewall (security groups) and in the host firewall:

- the panel's TCP port (443, and 80 for automatic certificates, on a default installation); a loopback panel needs none;
- the game servers' port range over **UDP and TCP**. Docker publishes these ports itself and bypasses ufw and firewalld; see [SERVERS.md](SERVERS.md#firewall), and [TROUBLESHOOTING.md](TROUBLESHOOTING.md#players-cannot-connect) when players cannot connect;
- TCP 8444 when you turn the Inspector on (below).

### Inspector

The Inspector (see [INSPECTOR.md](INSPECTOR.md)) is off until an admin turns it on. It then runs one more container (`golem-deploy_inspector`, from `GOLEM_DEPLOY_INSPECTOR_IMAGE`, downloaded on first use with the daemon's build Docker configuration) and opens the Inspector listener, by default on TCP port 8444 of the panel's address (`GOLEM_DEPLOY_INSPECTOR_LISTEN`; `off` disables the Inspector). Allow that one port in the host firewall and in the provider's firewall: browsers open the Inspector on it, and the game servers' Inspector agents connect to it from their containers through the Docker network's gateway, so with ufw it must be allowed (`ufw allow 8444/tcp`) even for the games.

### TLS certificates

To enable ACME, make TCP port 80 or 443 reachable for each requested identifier and accept the CA terms. Point `GOLEM_DEPLOY_HTTP_LISTEN` at the HTTP-01 listener if you use port 80 (`--http-listen` on a new installation). See [CERTIFICATES.md](CERTIFICATES.md).

With `--tls-cert` and `--tls-key`, the installer copies the files into `/etc/golem-deploy/tls/`. After renewing them, re-run the installer with both options to replace the copies and restart the service. On an existing installation, add the printed environment lines if needed and adjust `GOLEM_DEPLOY_LISTEN` before restarting.

## Upgrade

Run the installer again, with the same one-line command or `--version TAG`:

```sh
curl -fsSL https://github.com/demiurgos-hub/golem-deploy/releases/latest/download/install.sh | sudo bash
```

It replaces the binary, the unit and the uninstaller, keeps the user, the settings file and the data directory (it says what it kept), leaves a working Docker alone and restarts the service. Running game servers keep running during the restart. Before upgrading, back up the data directory (at least `golem-deploy.db`, `keys/` and `backups/`), preferably with the service stopped; there are no migration paths between versions before 1.0.0 yet. Game worlds live in the servers' data volumes in Docker (see [BACKUPS.md](BACKUPS.md)); take a backup of each server first if you want a copy in the data directory. [CHANGELOG.md](../CHANGELOG.md) lists what changed.

## Uninstall

```sh
sudo golem-deploy-uninstall
```

This stops and disables the service and removes the unit, the binary, `/var/cache/golem-deploy`, `/etc/sysctl.d/60-golem-deploy.conf` (the UDP buffer limits stay in effect until the next reboot) and the uninstaller itself. It keeps the data directory, the settings, the `golem-deploy` user and every Docker object, so installing again picks up where it left off. Game servers keep running without the panel: the uninstaller lists the running managed containers and prints the commands to stop and remove them.

To delete everything Golem Deploy made:

```sh
sudo golem-deploy-uninstall --purge
```

Before it changes anything it prints what it will delete (counts of Docker objects, the directories and the user) and asks you to type `purge`. Without a terminal it refuses unless `--yes` is given. It then also removes:

- this installation's Docker containers, volumes (the game servers' saved worlds), image tags and the `golem-deploy` network: only objects with the label `dev.golemengine.deploy.managed=true` **and** this installation's instance id in `dev.golemengine.deploy.instance` (see below). Of the containers, only those the daemon created count: they also carry one of its own label keys (`server-id`, `inspector-hub` or `helper`), which build images never have. Containers that someone else started from Golem Deploy's images are left alone;
- of the images, only the tags in Golem Deploy's own namespace (`golem-deploy/...`), with `--no-prune` and never with `-f`. Right before removing a tag it checks that the tag still names the image it selected by the labels. Removing an image's last tag removes the image. Images are never removed by id: an image that is left without tags, still has other tags or that another container uses is left, and the uninstaller lists it. An image you built `FROM` one of Golem Deploy's inherits its labels (Docker copies a base image's labels into images built from it), so the uninstaller finds it, but it has none of Golem Deploy's tags: it is left and listed, with its own tags or as an untagged image (`docker image rm` removes it). If Docker keeps one of Golem Deploy's images after its last tag is removed because such an image still uses it, that one is listed as untagged too;
- `/var/lib/golem-deploy`: the database, the encryption keys (stored credentials cannot be recovered without `keys/secrets.key`), every project checkout and the backups;
- `/etc/golem-deploy` and the user and group `golem-deploy`.

Every `docker` command of the uninstaller talks to the Docker daemon the service uses, on the default socket `/var/run/docker.sock`, whatever `DOCKER_HOST`, `DOCKER_CONTEXT` or root's Docker contexts select.

**The instance id.** Docker objects are deleted only when the uninstaller knows this installation's instance id, so that a purge never reaches another installation's objects, or those of an older database. It takes the id from `--instance ID` (32 lower-case hexadecimal characters), otherwise from the installed binary (`golem-deploy admin instance-id`, run as the service user, and only when it succeeds) for the data directory the settings file gives the service (`/var/lib/golem-deploy` when there is no settings file). When the settings file sets `GOLEM_DEPLOY_DATA_DIR` to anything other than one plain line with an absolute path (quotes, for example), the id is unknown and the uninstaller says why. When the id is unknown and Docker holds objects with the managed label (images without tags included), it stops before changing anything and lists the instance ids it found, with how many containers, volumes, images and networks carry each:

```sh
sudo golem-deploy-uninstall --purge --instance 0123456789abcdef0123456789abcdef
```

When Docker holds no managed objects at all, the purge goes on without a Docker step.

**What it refuses.** It refuses to start when Docker is installed but not reachable, when `/proc/self/mountinfo` cannot be read, and when anything is mounted at or below a directory it would delete (bind mounts count too); it names the mount points. It checks the mounts again after the confirmation, right before it deletes directories. Directories are deleted without crossing into other file systems. Where it expects a file (the unit, the binary, the uninstaller, the sysctl file) and finds a directory, it leaves it and warns.

**When something fails.** The uninstaller stops the service before it deletes anything. It goes on only when `systemctl show` says that the unit is stopped (or unknown to systemd) and no `golem-deploy serve` holds the lock on the data directory's `.lock` file (which the daemon holds while it runs, however it was started); otherwise it stops and prints the command to run again. When a Docker listing fails or a Docker object cannot be deleted (for example a network another container is still attached to), it stops: the data directory, the settings, the user, the binary and the uninstaller are kept, the service stays stopped (`systemctl start golem-deploy` starts it again), and it prints the command to run again after fixing the cause, with `--instance` and, if you gave it, `--yes`. When a file, directory, the user or the group cannot be removed, it keeps the uninstaller, lists what is left and prints that command instead of saying it is uninstalled. For a copy piped into bash it says to save `uninstall.sh` from the release archive and run that.

The Inspector hub image carries no Golem Deploy label and is left; remove it with `docker image rm` and the configured `GOLEM_DEPLOY_INSPECTOR_IMAGE`. Docker itself, Docker's apt repository and every apt package stay installed, and no prune command is run.

The uninstaller removes itself once everything is removed. To purge after a plain uninstall, run `uninstall.sh` from a release archive: `sudo bash uninstall.sh --purge --instance ID`. The binary is gone by then, so the id cannot be read; a plain uninstall prints the command with the id when it knows it. It copes with a half-installed or partly removed installation.

## Offline installs

Download the archive for the host's architecture and `SHA256SUMS` from the [releases page](https://github.com/demiurgos-hub/golem-deploy/releases), together with `install.sh`, and copy them to the host. Then:

```sh
sudo bash install.sh --archive ./golem-deploy_linux_amd64.tar.gz
```

The archive is checked against a `SHA256SUMS` next to it, or against `--sha256 HEX`; without either the installer refuses it. It copies the archive into a private temporary directory first and checks, lists and unpacks only that copy, so the file it checked is the file it installs. Docker must already be installed, or the host must reach Docker's apt repository.

Each archive holds exactly `golem-deploy` (the binary with the web client), `golem-deploy.service`, `uninstall.sh` and `LICENSE`.

## Verifying downloads

The installer checks the archive's SHA-256 against the release's `SHA256SUMS`. That proves the download is complete and is the file the release lists. It does not prove who made the release: `SHA256SUMS` comes from the same place as the archive, so someone who could replace one could replace both.

For more:

- **Build provenance.** Each release's archives and `install.sh` have a signed attestation that they were built by this repository's release workflow from the tagged commit. With the [GitHub CLI](https://cli.github.com/):

  ```sh
  gh attestation verify golem-deploy_linux_amd64.tar.gz --repo demiurgos-hub/golem-deploy \
    --signer-workflow demiurgos-hub/golem-deploy/.github/workflows/release.yml
  ```

- **Pinning the checksum.** Verify an archive once (for example with the attestation), then pass its SHA-256 with `--sha256` on other hosts. The installer then accepts only that exact file.
- Read `install.sh` before running it, and check it with `sha256sum -c SHA256SUMS --ignore-missing` or the attestation as well.

## Building from source

`scripts/release.sh` builds the same files a release has (see [RELEASING.md](RELEASING.md)):

```sh
scripts/release.sh v0.0.0-dev.1          # into dist/release
sudo bash dist/release/install.sh --archive dist/release/golem-deploy_linux_amd64.tar.gz
```

Or build just the binary and install it from the checkout; the unit comes from `deploy/` and the uninstaller from `scripts/`:

```sh
(cd frontend && npm ci && npm run build)          # embed the web client
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/golem-deploy ./cmd/golem-deploy   # GOARCH=arm64 for ARM hosts
sudo scripts/install.sh --binary ./bin/golem-deploy
```

Without the web client build the binary works, but the panel only shows a "web client not built" notice.

## Security

The security model is described in [SECURITY.md](SECURITY.md); how to report a vulnerability is in the repository's [SECURITY.md](../SECURITY.md). For the installation:

- **The `docker` group is root-equivalent.** The service user must be in it to drive Docker, and any member of the group can start a privileged container that mounts the host's file system. Treat the `golem-deploy` account, and the panel's admin accounts, as root on this host. Do not add people to the `docker` group, and keep the host dedicated.
- **Managed TLS on network listeners.** A non-loopback listener without TLS files serves HTTPS with a generated certificate until ACME is configured. Keep the panel on loopback if you reach it through an SSH tunnel or a reverse proxy on the same host.
- The settings file and the TLS key are readable by root and the service group only; the data directory (database and keys) by the service user only.
- **Builds and the service sandbox.** `docker build` runs as the service user under the unit's restrictions: the buildx client uses the dedicated `DOCKER_CONFIG` at `/var/lib/golem-deploy/docker-build`, its scratch files in the private `/tmp`, and the daemon writes the generated Dockerfile, `--iidfile` and `--metadata-file` below the data directory's `tmp/`, all writable under `ProtectSystem=strict`. The build itself (and every `RUN` step) executes inside dockerd's BuildKit, outside the sandbox. Keep the default `docker` builder in the dedicated configuration. To grant registry access deliberately, run `sudo -u golem-deploy env DOCKER_CONFIG=/var/lib/golem-deploy/docker-build docker login <registry>`. Keep proxy settings out of this configuration. Docker discovers buildx in its system plugin paths and the original client's `cli-plugins/`. For other plugin locations, install buildx under the dedicated `cli-plugins/` or set `cliPluginsExtraDirs` there. If the primary Docker client uses a named TLS context, import that context into the dedicated directory as well. Each game's certificate directory (`/var/lib/golem-deploy/certs/servers/<id>`) is bind-mounted by dockerd from its real path, which the unit leaves unchanged.

Game server isolation and the Inspector's limits are covered in [SERVERS.md](SERVERS.md#network-isolation) and [INSPECTOR.md](INSPECTOR.md#security-model-and-limits).

## Troubleshooting

Installation and service problems are below. For builds, game servers and connectivity, see [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

- **Logs:** `journalctl -u golem-deploy -f` (the daemon's own log, including Docker errors of failed jobs); `systemctl status golem-deploy` for the state.
- **The download or the checksum fails:** nothing was changed. Check that the host reaches `github.com` and GitHub's download hosts (`*.githubusercontent.com`) over HTTPS, and that the version exists. A checksum mismatch means the file is not the one the release lists; download it again and do not work around the check.
- **`packages that conflict with Docker's are installed`:** remove the listed packages with the printed `apt-get remove` command (images, containers and volumes in `/var/lib/docker` are kept) and run the installer again, or install Docker yourself and run it again.
- **Installing Docker fails:** the release may not be one Docker publishes packages for. Install Docker Engine following Docker's instructions, then run the installer again. After a failure at this step, `/etc/apt/keyrings/docker.asc` and `/etc/apt/sources.list.d/docker.sources` may already exist; the next run uses them.
- **`the downloaded Docker repository key does not have the fingerprint Docker documents`:** something between the host and `download.docker.com` changed the key. Nothing was written for Docker's repository. Do not work around the check.
- **`apt would not download docker-ce only from https://download.docker.com/linux/...`:** another apt source (a mirror or a local repository) offers the candidate version with the same or a higher priority, or an apt pin (`/etc/apt/preferences.d/`) prefers another origin. The installer prints the start of `apt-cache policy`; remove the other source or lower its priority, or fix the pin, and run it again.
- **`docker.sources exists but has no enabled entry` or `docker.asc exists, but does not hold the key`:** the installer does not overwrite these files. Enable Docker's source (remove its `Enabled:` line) or remove the file, and remove a keyring no source uses, then run it again.
- **No setup link, `did not log that it is listening`:** the service is active but did not report a listening panel within 30 seconds. Check `systemctl status golem-deploy` and `journalctl -u golem-deploy`, then make the link with the printed command.
- **The first start fails and the settings file was moved to `golem-deploy.env.failed`:** the daemon refused a setting (the log shows which, for example a port range with no usable port). Run the installer again with corrected options; it writes a new settings file. Delete the `.failed` file when you no longer need it.
- **`the Docker buildx plugin is missing`:** install `docker-buildx-plugin` (Docker's packages) or the distribution's `docker-buildx` package (with `docker.io`).
- **The service does not start, `bind: address already in use`:** another process holds the listen port. Find it with `ss -ltnp 'sport = :8000'`, then stop it or change `GOLEM_DEPLOY_LISTEN` and restart. A game port range that contains the listen port is refused at startup.
- **A listener port is busy or forbidden.** Free it in the other service or update the corresponding listener in the environment file. For a manually configured daemon on Linux, binding low ports may require `CAP_NET_BIND_SERVICE`.
- **Docker unreachable in the panel, or `permission denied ... /var/run/docker.sock` in the log:** check that the user is in the group (`id golem-deploy` lists `docker`), that the socket belongs to that group (`ls -l /var/run/docker.sock` shows `root docker`), and that Docker runs (`systemctl status docker`). Group changes apply when the service restarts: `systemctl restart golem-deploy`. The installer checks this with `runuser -u golem-deploy -- docker version`.
- **Data directory refused** (`has mode ...`, `not empty and was not initialised by Golem Deploy`): the data directory must be owned by `golem-deploy`, mode 0700, and contain only what Golem Deploy put there. The installer refuses an existing `/var/lib/golem-deploy` with another owner or mode and prints the `chown`/`chmod` to run if it is the right directory.
- **`start request repeated too quickly`:** the service failed 5 times in 5 minutes. Fix the cause shown in the log, then `systemctl reset-failed golem-deploy && systemctl start golem-deploy` (the installer resets the counter itself).
- **The setup link expired or was used:** make a new one with the command under [The first admin account](#the-first-admin-account).
- **`admin create` or `admin setup-link` fails with permission errors:** run it as the service user with `runuser -u golem-deploy --` (or `sudo -u golem-deploy`) and pass `--data-dir /var/lib/golem-deploy`. Running it as root can leave root-owned files the service cannot read; fix them with `chown -R golem-deploy:golem-deploy /var/lib/golem-deploy`.
