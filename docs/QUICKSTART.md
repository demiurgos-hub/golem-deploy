# Quick start

This page takes a fresh server to a running game that players can open in a browser. It uses the reference game, [Lantern Ring](https://github.com/demiurgos-hub/golem-reference-game), so you can try the whole path before deploying your own game. Allow about fifteen minutes.

Golem Deploy is in early development. Read the [security model](SECURITY.md) before you put it on a host that matters.

## What you need

- A server running **Ubuntu or Debian** with systemd, amd64 or arm64, and root access. Use a dedicated host: Golem Deploy drives Docker, which makes it, and its admin accounts, equivalent to root there.
- A **public address**. A domain name is optional: Let's Encrypt issues certificates for public IP addresses too.
- These ports open in your provider's firewall (security group), and in the host's firewall if it has one:

  | Port | For |
  |------|-----|
  | TCP 443 | The panel |
  | TCP 80 | Automatic certificates and redirects to HTTPS |
  | UDP and TCP 7000-7099 | Game servers, one port each |
  | TCP 8444 | The Inspector, only if you turn it on |

## 1. Install

On the server:

```sh
curl -fsSL https://github.com/demiurgos-hub/golem-deploy/releases/latest/download/install.sh | sudo bash
```

With a domain name, name it, so the panel and the games use it instead of the IP address:

```sh
curl -fsSL https://github.com/demiurgos-hub/golem-deploy/releases/latest/download/install.sh | sudo bash -s -- --public-host deploy.example.com
```

The installer checks the download against the release's checksums, installs Docker Engine when it is missing, and starts Golem Deploy as the service `golem-deploy`. It asks no questions and never changes firewall rules. [INSTALL.md](INSTALL.md) lists what it does and its options.

If another program already uses port 443 or 80, the installer puts the panel on a loopback port and tells you how to reach it through an SSH tunnel. To keep the panel on the network instead, choose free ports on a new installation: `--listen :8443 --http-listen :8080`. Automatic certificates need port 80 or 443 reachable from the internet, so in that case leave one of them to Golem Deploy, for example `--listen :8443 --http-listen :80`.

## 2. Create the admin account

The installer ends with a link like this:

```
https://203.0.113.5/setup#t=...
```

Open it in your browser. It works once and for one hour. The panel starts with a self-signed certificate, so the browser shows a warning: continue only if the certificate's SHA-256 fingerprint in the browser is the one the installer printed below the link.

Choose a username and a password. You are signed in as the first admin.

If the link expired, make a new one on the server with the command the installer printed (`golem-deploy admin setup-link`).

## 3. Get trusted certificates

Browsers need a certificate they trust to load the game's page and to use its WebSocket fallback. In **Settings**:

1. Under **Automatic certificates**, tick the Let's Encrypt subscriber agreement and save. The contact email is optional.
2. Under **Game certificates**, switch to automatic certificates. A job requests the certificate; its log shows the progress.
3. Under **Panel certificate**, switch to automatic too. Reload the page: the warning is gone.

This works for a domain name and for a public IP address. Certificates for IP addresses last about a week, and Golem Deploy renews them on its own. If the request fails, see [Troubleshooting](TROUBLESHOOTING.md#certificate-problems).

You can skip this step for a first look: with the default self-signed certificates the browser warns about the game's page and can only connect over WebTransport. See [CERTIFICATES.md](CERTIFICATES.md).

## 4. Add the project

On **Projects**, choose **New project**:

- Name: `lantern-ring`
- Repository URL: `https://github.com/demiurgos-hub/golem-reference-game.git`
- Authentication: none (the repository is public)

Golem Deploy clones the repository. The job's log shows the commit it checked out.

## 5. Build

Open the project and choose **Start build**. The build runs inside `docker build`: it installs the dependencies, checks the generated code, builds the web client and the server, and produces a small image. The first build downloads base images and dependencies and takes a few minutes; later builds reuse them. Follow it in the build's log.

## 6. Start a server

On **Servers**, choose **New server**, pick the build and give the server a name, for example `ring-a`. Then start it. The start succeeds once the game reports that it is ready.

The server's page shows its address, `https://<your host>:7000/`. Open it in two browser windows and light a lantern: both windows show it. The page's **connection test** checks HTTPS, the WebSocket fallback and WebTransport from your browser, which is the quickest way to find a closed port.

## Next steps

- **Your own game.** [GAME_INTEGRATION.md](GAME_INTEGRATION.md) says what a Golem Engine game needs to be built and run here.
- **Saved worlds.** Each server keeps its world on a volume of its own. Take a backup from the server's page, set a schedule, and restore when you need to: [BACKUPS.md](BACKUPS.md).
- **A new version of the game.** Pull the project, build again, assign the new build to the server and restart it. The saved world stays. A server restart disconnects its players.
- **The Inspector.** Turn it on in Settings, switch it on for a server, restart that server, and open it from the server's page to see the live entities: [INSPECTOR.md](INSPECTOR.md).
- **More people.** Create accounts under **Accounts**. Developers can build and run servers; admins also manage projects, settings and accounts.
- **Upgrading.** Run the install command again. Game servers keep running while the panel restarts.
- **Removing it.** `sudo golem-deploy-uninstall` removes the service and keeps your data; `--purge` deletes everything Golem Deploy created.
- **When something does not work.** [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
