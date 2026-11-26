# Changelog

All notable changes to Golem Deploy are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/). Until 1.0.0, any release may change the API, the storage formats and the configuration without a migration path.

## [Unreleased]

## [0.1.0-rc.3] - 2026-11-26

### Added

- Profiling: a per-server Profiling switch, on by default, sets `GOLEM_ADMIN_PROFILING=1` so the game's admin endpoint serves Go profiles (golem-engine with the admin profile endpoint). `GET /api/v1/servers/{id}/profile?kind=cpu|trace|heap|allocs|goroutine&seconds=N` captures one and answers with the file, and the server page has a capture form that downloads it. New error codes `profiling_disabled` and `profile_in_progress`.

### Changed

- Existing servers get the Profiling switch turned on by the upgrade, so a running server reports `restart_required` once until it is restarted.

## [0.1.0-rc.2] - 2026-11-26

### Added

- `server.module` in `golem-deploy.yaml`: the directory of `go.mod`, relative to the game root, for a monorepo whose Go server lives in a subdirectory. The preflight reads `go.mod` there and the generated Dockerfile builds the server there.
- `lfs.include` in `golem-deploy.yaml`: the Git LFS path patterns that clones and pulls download, so a large monorepo stays under the checkout size limit. A `golem-deploy.yaml` that cannot be read or has a bad `lfs` section fails the clone or pull of a repository that uses Git LFS.

### Fixed

- Notifications in the web client cancel their auto-dismiss timer when they are dismissed or the page is left.

## [0.1.0-rc.1] - 2026-11-26

The first release candidate.

### Added

- A single `golem-deploy` binary that serves the HTTP API under `/api/v1` and the embedded web client, run as a systemd service on Ubuntu and Debian.
- An installer that downloads a checksum-verified release, installs Docker Engine when it is missing, raises the UDP buffer limits for QUIC and prints a one-time link to create the first admin account. An uninstaller that keeps the data, or purges this installation's data and Docker objects (only those with its instance id).
- Release archives for linux/amd64 and linux/arm64 with SHA-256 checksums and build provenance attestations.
- Accounts with the roles admin and developer, optional self-registration with admin approval, sessions and revocable API tokens.
- Projects: Git repositories cloned over HTTPS or SSH, with pull and branch switching as jobs and encrypted credentials.
- Builds: a project's checkout built into a hardened, labelled image with `docker build`, with an optional `golem-deploy.yaml`, static add-ons and retention of old builds.
- Game servers: hardened containers that run successful builds with the Golem Engine deploy contract, health-gated starts, logs, stats, a runtime view and export and import.
- Saved data: one Docker volume per server, with backups, restore, reset and scheduled backups.
- Certificates: managed TLS for the panel, and self-signed, uploaded or automatic ACME certificates for games.
- The Golem Inspector hub as a managed container, opened through one-time links from the panel.
- Static file store, image and volume management, and a web client that covers the path from a repository to a running game.

[Unreleased]: https://github.com/demiurgos-hub/golem-deploy/compare/v0.1.0-rc.3...HEAD
[0.1.0-rc.3]: https://github.com/demiurgos-hub/golem-deploy/releases/tag/v0.1.0-rc.3
[0.1.0-rc.2]: https://github.com/demiurgos-hub/golem-deploy/releases/tag/v0.1.0-rc.2
[0.1.0-rc.1]: https://github.com/demiurgos-hub/golem-deploy/releases/tag/v0.1.0-rc.1
