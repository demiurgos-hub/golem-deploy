# Releasing Golem Deploy

A release is a Git tag `vX.Y.Z`, or `vX.Y.Z-rc.N` for a release candidate, pushed to GitHub. The [release workflow](../.github/workflows/release.yml) checks, builds and publishes it. Versions follow [Semantic Versioning](https://semver.org/); before 1.0.0, a minor version may break compatibility.

## Files of a release

| File | Contents |
|------|----------|
| `golem-deploy_linux_amd64.tar.gz`, `golem-deploy_linux_arm64.tar.gz` | Exactly four files: `golem-deploy` (static binary with the web client, built with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<tag>"`), `golem-deploy.service`, `uninstall.sh`, `LICENSE` |
| `install.sh` | `scripts/install.sh` with `RELEASE_VERSION` set to the tag, so it installs this release by default |
| `SHA256SUMS` | `sha256sum` lines for the two archives and `install.sh` |

The archives are reproducible: the same commit, Go version and web client build give the same bytes (sorted names, owner 0, modification time of the tagged commit, `gzip -n`). Each archive and `install.sh` also get a build provenance attestation. Users install with:

```sh
curl -fsSL https://github.com/demiurgos-hub/golem-deploy/releases/latest/download/install.sh | sudo bash
```

`releases/latest` points at the newest release that is not a prerelease. A release candidate is installed with `--version vX.Y.Z-rc.N` or its own `install.sh`.

## Protect the release tags

A pushed `v*` tag publishes a release, and the release workflow checks that the tag did not move while it ran. To keep anyone with plain write access from creating, moving or deleting release tags, add a tag ruleset once: in the repository's **Settings → Rules → Rulesets**, create a tag ruleset that targets `refs/tags/v*`, restricts creations, updates and deletions, and lets only the maintainers who release bypass it.

## Steps

1. **Check `main`.** CI passes on the commit you want to release. With Docker available, also run the opt-in Docker tests (see `AGENTS.md`).
2. **Update `CHANGELOG.md`.** Rename `## [Unreleased]` to the version with today's date, for example `## [0.1.0] - 2026-11-25` (no `v`), and start a new empty `## [Unreleased]` above it. Keep the entries short and written for operators. Update the link definitions at the end of the file. The workflow takes the release notes from this section and fails without it. Preview them with:

   ```sh
   scripts/release-notes.sh v0.1.0
   ```

3. **Try the build locally** (optional; works on Linux and in Git Bash on Windows):

   ```sh
   scripts/release.sh v0.1.0
   ```

   It writes the release files to `dist/release`. Add `--skip-web` when `frontend/dist` is already built.
4. **Commit and tag.** Commit the changelog to `main` (through a pull request if `main` is protected), then tag that commit and push the tag:

   ```sh
   git tag -a v0.1.0 -m "Golem Deploy v0.1.0"
   git push origin v0.1.0
   ```

## What the workflow does

1. **Tag:** checks that the tag looks like `vX.Y.Z` or `vX.Y.Z-rc.N` and that `CHANGELOG.md` has notes for it.
2. **Checks:** runs the CI workflow (`ci.yml`): frontend lint, tests and build, shellcheck on every script, generated types, hidden characters, `go vet`, staticcheck, govulncheck and `go test -race`.
3. **Build:** runs `scripts/release.sh` without dependency caches, takes the notes with `scripts/release-notes.sh`, and creates the build provenance attestation for the archives and `install.sh`.
4. **Publish:** checks that `SHA256SUMS` holds exactly one entry for each of the three assets and nothing else, with the digests of the files about to be uploaded; checks with `git ls-remote` that the tag on the remote still points at the commit the run built, and stops if it moved; then creates the GitHub release with `gh release create`, marked as a prerelease when the tag has a `-` part.

Each job only gets the permissions it needs: reading the repository, plus the attestation permissions for the build and writing releases for the publish job.

## Check the result

- The release page lists the four files and the notes from the changelog.
- Verify a download:

  ```sh
  gh release download v0.1.0 --repo demiurgos-hub/golem-deploy
  sha256sum -c SHA256SUMS
  gh attestation verify golem-deploy_linux_amd64.tar.gz --repo demiurgos-hub/golem-deploy \
    --signer-workflow demiurgos-hub/golem-deploy/.github/workflows/release.yml
  ```

- Install it on a fresh Ubuntu or Debian host with the one-line command (or `--version` for a release candidate) and open the setup link it prints.

## When something goes wrong

- **The workflow failed before Publish:** nothing was published. Fix the cause on `main`, then move the tag: `git tag -d v0.1.0 && git push origin :refs/tags/v0.1.0`, tag the new commit and push again. Only do this while no release exists for the tag.
- **A published release is broken:** do not replace its files; users and checksums rely on them. Publish a new patch version instead, and mark the broken release in its notes.
