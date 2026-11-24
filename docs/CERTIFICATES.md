# Game and panel certificates

Game servers use TLS for WebTransport over UDP and for HTTPS on their TCP port. The panel also supports managed TLS. Admins configure certificates through the [certificate API](API.md#game-certificate).

## Choose a mode

| Service | Mode | Use |
|---------|------|-----|
| Game | `self_signed` | Default for development, LAN play and clients that pin a certificate. Each server receives a separate ECDSA P-256 key and a 13-day certificate. The daemon rotates it when the game host changes or when fewer than five days remain. |
| Game | `custom` | Upload a matching PEM certificate chain and private key. All servers share the pair. Renew it yourself. The daemon warns when expiry is within 14 days. |
| Game | `acme` | Obtain and renew a publicly trusted pair for the game host. All game servers share it. |
| Panel | `none` | Plain HTTP on loopback without TLS files, or with explicit `--insecure-http` for a trusted proxy setup. |
| Panel | `files` | Serve the certificate and key configured through `--tls-cert` and `--tls-key`. The operator renews these and restarts the daemon. |
| Panel | `self_signed` | Initial TLS mode on a non-loopback listener without TLS files. Compare the SHA-256 fingerprint in the startup log with the browser's certificate before accepting it. |
| Panel | `acme` | Obtain a certificate for each panel host. TLS picks the certificate for the client SNI. Without SNI, it tries the connection's local address, then the sole managed IP certificate if available, then the first panel host. The IP fallback covers clients connecting through NAT. The daemon falls back to a generated certificate for names without a usable ACME pair. |

The effective game host is the admin's `game_host` setting, or the first `--public-host` when the setting is empty. Each server's page is at `https://<game host>:<port>/`. The panel uses the `--public-host` list, with the effective game host as its fallback when the list is empty. The panel's generated fallback covers its hosts, the game host, `localhost`, `127.0.0.1` and `::1`.

Browser players need a trusted HTTPS page. A self-signed game certificate enables WebTransport hash pinning from a page already served over trusted HTTPS, and works with native clients that support pinning. Browsers still warn when opening the game server's own page with a self-signed certificate. Browser WebSockets use normal chain validation.

## Configure automatic certificates

1. Point each public DNS name at the daemon, or use a publicly reachable IP address. The default CA rejects identifiers such as loopback, private and other non-public addresses, and local or single-label names.
2. Make TCP port 80 or 443 reachable **for each identifier**. Set `--http-listen` for HTTP-01 and route public port 80 there. TLS-ALPN-01 requires a TLS panel listener on port 443 and a direct TLS connection from the CA, including through any TCP passthrough proxy. HTTP-01 handles active challenges and redirects other allowed requests to the HTTPS panel. There is no DNS-01 solver.
3. Read and accept the CA's subscriber agreement in the ACME account settings. You may provide a contact email. ACME is off until an admin asks for it.
4. Switch the game or panel to `acme`. The restricted issuance job reports progress. The previous mode and pair stay active until the new pair is staged and the mode commits.

The default CA is Let's Encrypt. DNS names use its default profile, with a lifetime set by the CA (typically up to 90 days). IP certificates use the `shortlived` profile, about 160 hours. Staging exercises validation with higher rate limits and certificates browsers do not trust. To change staging or withdraw agreement to the subscriber terms, first switch the game and panel away from ACME. `--acme-directory` overrides both directories, including for a private CA. `--acme-root-ca` sets the roots used to connect to that CA. Private directory overrides permit local identifiers, subject to the CA's own policy.

One identifier uses one ACME pair across the installation. Every game container can read the game certificate's private key in its mount and can print it into its log. If the game host is also a panel host, the shared game certificate in `acme` or `custom` mode is valid for the panel too. A separate certificate for the same name offers no isolation because browsers accept certificates for that name. If games are not fully trusted, use `--public-host` to give the panel a hostname different from the game host. Let's Encrypt limits certificates for an identical identifier set to five per week. Prefer staging for experiments and use manual renewal only when needed.

## Renewal and failures

The daemon uses ACME Renewal Information when available and otherwise schedules renewal around half the lifetime. Renewal leaves at least one quarter of the certificate lifetime for retries, with a minimum of 12 hours and a maximum of seven days for that margin. Local retry delays are capped at 30 minutes, subject to any later retry time requested by the CA. Status records a failed or blocked renewal and its most recent attempt. The installed pair stays in use. An expired pair is reported as an error. A missing or expired shared game certificate prevents a new server start. An expired ACME panel certificate gives way to the self-signed fallback.

When the effective game host changes in ACME mode, the daemon obtains a certificate for the new host in the background. Existing servers keep the installed certificate. Servers started before the new pair arrives also serve the previous certificate while it remains valid. Their certificate metadata shows `covers_game_host: false` when it does not cover the new name, and the start job log warns about the mismatch. The game URL and allowed origins also change, so running servers may show `restart_required`. To force renewal for all active identifiers, call `POST /api/v1/certificates/acme/renew`. When neither service uses ACME and no certificate job was requested, the daemon makes no CA requests.

## Storage and replacement

- `<data>/acme/` holds private certificate and ACME account assets. `<data>/certs/panel/` holds the generated fallback and copies of installed panel pairs. Both are private to the daemon and never mounted into a game.
- `<data>/certs/game/` holds the canonical custom or ACME pair. Game mounts are `<data>/certs/servers/<id>/`, mounted read-only at `/run/golem-deploy/tls`. They contain only that server's `tls.crt` (leaf first) and `tls.key`. The mount is removed when the server is deleted.
- On Unix, private keys outside game mounts are mode `0600`. A game mount uses directory `0750`, key `0640` and certificate `0644`. Containers read through the daemon's group, or group 65532 when the daemon runs as root with group 0. A non-root daemon with group 0 cannot assign that group. Windows development hosts inherit the data ACL. Docker user namespace remapping and SELinux policy may require host configuration for bind-mount access.

Game changes stage complete pairs outside the mounts and commit the mode and generation together. Startup discards uncommitted stages and finishes committed ones. An incomplete copy after commit is reported with the new mode active, and the daemon retries promptly. Each publication atomically renames the key and then the chain. The engine checks both files every 30 seconds and keeps its previous certificate when a poll sees a transient mismatch. It loads the new pair on a later poll without restarting. A server start repairs an interrupted replacement and keeps its mount stable until the game loads its initial pair. Startup and periodic reconciliation adopt a newer ACME pair saved just before an interrupted install. The next check installs a renewal the CA has already issued and the daemon has saved, even if the renewal job was cancelled.

Self-signed games start with `GOLEM_TLS_PINNED=1`. Custom and ACME use `GOLEM_TLS_PINNED=0` and clients validate the certificate chain normally. Switching between those pinning settings reports `restart_required` until the server restarts.
