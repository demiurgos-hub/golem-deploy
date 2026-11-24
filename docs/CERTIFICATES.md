# Game certificates: operator notes

Every game server serves TLS: WebTransport (QUIC over UDP) and the game's HTTPS sidecar (realtime config, WebSocket fallback, the game's own pages) on the server's port. Golem Deploy manages **one game certificate for the whole installation** and gives it to every server container. This note explains where it lives, the two modes that exist today, how replacement works and what a self-signed certificate can and cannot do. The panel's own TLS (`--tls-cert`, `--tls-key`) is separate and unaffected. The HTTP contract is in [API.md](API.md#game-certificate).

## Where it lives

- `<data>/certs/game/tls.crt` (the chain, leaf first) and `<data>/certs/game/tls.key` (PKCS#8). Back them up with the data directory if you use a custom certificate; a self-signed one is simply regenerated.
- The directory is bind-mounted **read-only** at `/run/golem-deploy/tls` into every server container, and the engine is told where to find the files (`GOLEM_TLS_CERT_FILE`, `GOLEM_TLS_KEY_FILE`). It is never baked into images.
- Permissions on Unix hosts: directory `0750`, key `0640`, certificate `0644`, owned by the daemon user and the daemon's group. Server containers run as uid 65532, so they read the files through the group: each container gets the daemon's group as a supplementary group (`--group-add`). The key is never world-readable. When the daemon runs with group 0 (for example as root), the files get group 65532 (the container user's own group) instead, so no container is ever given group 0. A daemon running with group 0 that is not root cannot do that and refuses to start.
- On Windows development hosts (Docker Desktop) no group handling is done; Docker Desktop's bind mounts do not carry Unix ownership.
- The data directory path must not contain `,`, `"`, `'` or `=` (they cannot be expressed in a bind mount); the daemon refuses to start otherwise.

What this does not cover: hosts with Docker user namespace remapping (`userns-remap`) or SELinux enforcing on the bind mount may still deny the container access; servers then fail to become ready and the server log shows the certificate error.

## Game host

The certificate is issued for the **game host**: the DNS name or IP address players use to reach game servers. Set it with `PATCH /api/v1/settings` (`game_host`); when it is empty the first `--public-host` is used; when neither exists, servers cannot start. Each server's page is `https://<game host>:<port>/`.

## Modes

### `self_signed` (default)

The daemon generates an ECDSA P-256 certificate:

- valid for **13 days** (one hour of it before the moment of creation, to absorb clock skew). WebTransport certificate hash pinning (`serverCertificateHashes`) only accepts certificates valid for at most 14 days;
- subject alternative names: the game host, `localhost`, `127.0.0.1` and `::1`;
- regenerated when the game host changes, and **rotated automatically** when less than 5 days of validity remain: checked at startup, every hour and before every server start (also when the certificate is read through the API).

Servers are started with `GOLEM_TLS_PINNED=1`, so the game's realtime config publishes the certificate's SHA-256 hashes and browser clients pin them instead of validating a chain.

### `custom`

An admin uploads a PEM chain (leaf first, at most 8 certificates and 64 KiB) and an unencrypted private key (PKCS#8, PKCS#1 or SEC 1, at most 16 KiB) with `PUT /api/v1/certificates/game`. The upload is refused unless the key is RSA of at least 2048 bits or ECDSA (P-256, P-384, P-521), matches the leaf, the leaf is currently valid and is usable for TLS servers. The private key is never returned. The daemon never changes a custom certificate: renew it yourself before it expires (the daemon logs a warning from 14 days before expiry; an expired custom certificate makes server starts fail with a message). `GET /api/v1/certificates/game` reports whether it covers the current game host.

Servers are started with `GOLEM_TLS_PINNED=0`: clients validate the chain normally.

### Later: a certificate manager

Automatic certificates from a public CA (ACME) are planned as a third mode on top of the same files and the same replacement mechanism.

## Replacement without restarts

Running servers pick up a replaced certificate **without a restart**: the engine checks the files every 30 seconds (`GOLEM_TLS_RELOAD_INTERVAL`) and reloads them when either file's size or modification time changes; existing connections keep their certificate, new handshakes get the new one. This applies to rotation, a host change in self-signed mode and a new custom upload.

The daemon writes both files completely under temporary names and then renames them over the old ones, **key first, certificate last**. Each rename is atomic, so the engine never reads a half-written file. Between the two renames the directory briefly holds the new key with the old certificate; the engine's reload validates that the pair matches, rejects the mismatched pair, keeps serving the previous certificate and retries on the next check, by which time the certificate rename has completed. A crash between the two renames is completed on the next daemon start. (A server *starting* in exactly that instant would fail to load the pair and its start fails with a message; start it again.)

Switching between modes changes `GOLEM_TLS_PINNED`, so running servers report `restart_required` until they are restarted. A new game host also marks them `restart_required` (their public URL and allowed origins change).

## The honest limit of self-signed certificates

A self-signed, pinned certificate works for:

- **LAN play and development**, where players open a page served by something they trust (or `localhost`);
- **native clients** (desktop, Unity, the Go client), which use the published certificate hashes;
- browser clients whose page is served over **trusted HTTPS** from somewhere else and that connect to the game with `serverCertificateHashes`.

It does **not** give players on the internet a browser page they can simply open: `https://<game host>:<port>/` served with a self-signed certificate shows a certificate warning, and browsers only allow `serverCertificateHashes` pinning from a secure (trusted HTTPS) page. For internet players the page must come from a host with a publicly trusted certificate: upload one in `custom` mode today, or wait for the certificate manager. The WebSocket fallback has the same requirement: browsers do not pin certificates for WebSocket.
