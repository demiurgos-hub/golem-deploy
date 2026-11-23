# Git repositories (operators)

Each project is a Git repository that Golem Deploy clones into `<data-dir>/repositories/<project id>` using the host's `git` binary (and `git-lfs` when a repository needs it). This page covers what you need to know to connect repositories safely. The HTTP contract is in [API.md](API.md#projects).

## Supported repository URLs

| Form | Example |
|------|---------|
| HTTPS | `https://github.com/acme/game.git`, `https://git.example.com:8443/team/game.git` |
| SSH URL | `ssh://git@github.com/acme/game.git`, `ssh://git@git.example.com:2222/team/game.git` |
| SSH scp-style | `git@github.com:acme/game.git` |

Everything else is rejected, including plain `http://` (code and credentials would travel in clear text), `git://`, `file://`, `ext::…` and other remote helpers, and local paths. HTTPS URLs must not contain a user name or password: put credentials in the project's `auth` settings instead.

## Credentials

- **HTTPS** (`auth.type: "https"`): a user name and a password or access token (for GitHub/GitLab, a fine-grained or project access token with read access to the repository). Git receives them through `GIT_ASKPASS`, which points at the `golem-deploy` binary itself: it answers git's prompts from variables set only on that git process, and only for the host of the project's URL (a redirect to another host gets nothing). Credentials never appear on a command line, in the URL or in `.git/config`, and no credential helper caches them.
- **SSH** (`auth.type: "ssh"`): an unencrypted private key (OpenSSH or PEM format), typically a read-only deploy key. Passphrase-protected keys are rejected. For each operation the key is written to a 0600 file in a fresh 0700 directory under `<data-dir>/tmp/`, used as the only identity (`IdentitiesOnly=yes`, no agent, no `~/.ssh/config`) and deleted when the operation ends; leftovers from a crash are removed at the next start.
- **None**: public repositories.

Stored passwords and keys are encrypted at rest in the database with `<data-dir>/keys/secrets.key`; they are never returned by the API or written to logs. Git output in job logs passes through a filter that redacts the password and any `user:password@` in URLs.

## SSH host keys

SSH connections use `StrictHostKeyChecking=accept-new` with the known-hosts file `<data-dir>/known_hosts`:

- The first connection to a host records its key (trust on first use).
- Later connections must present the same key; a changed key fails the job with "SSH host key verification failed".

To avoid trusting the first connection blindly, pre-seed the file before creating SSH projects, verifying the fingerprints against the provider's published ones:

```sh
ssh-keyscan -t ed25519,ecdsa,rsa github.com >> /path/to/data/known_hosts
ssh-keygen -lf /path/to/data/known_hosts          # compare with the provider's fingerprints
chmod 600 /path/to/data/known_hosts
```

If a host legitimately rotated its key, remove its old entry (`ssh-keygen -R github.com -f /path/to/data/known_hosts`) and pre-seed or reconnect. Deleting the file resets all recorded hosts.

## What pulls do

A pull fetches the project's branch and makes the checkout identical to it: local changes and untracked files are discarded (`checkout -B` and `clean -ffdx`), there is never a merge. Full history and tags are kept so builds can use `git describe`. Switching branch fetches that branch explicitly and does the same. Changing a project's URL takes effect at the next pull without re-cloning.

Git runs non-interactively with an isolated configuration: system and global git config are ignored, hooks and submodules are disabled, only the `https` and `ssh` transports are allowed, and every command has a timeout (15 minutes for network operations, 60 seconds for local ones). Cancelling a job stops git and all processes it started.

## Git LFS

If the checkout's `.gitattributes` files use `filter=lfs`, the job runs `git lfs pull` with the same credentials after cloning or pulling. When `git-lfs` is not installed the job fails with "the repository uses Git LFS but git-lfs is not installed on the host"; install it (`apt install git-lfs`) and restart the daemon (it checks for `git-lfs` once per run).

## Troubleshooting

The job's detail gives the reason (authentication failed, repository not found, branch does not exist, host key verification failed, remote unreachable, timeout); the job log (`GET /api/v1/jobs/{id}/log`) has the git output. A project in status `error` is repaired by pulling it, which re-clones when there is no checkout.
