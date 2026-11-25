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

- **HTTPS** (`auth.type: "https"`): a user name and a password or access token (for GitHub/GitLab, a fine-grained or project access token with read access to the repository). Git receives them through `GIT_ASKPASS`, which points at the `golem-deploy` binary itself: it answers git's prompts from variables set only on that git process, and only when the prompt is exactly the one git prints for the configured host and port (`Username for 'https://host[:port]': ` and `Password for 'https://user@host[:port]': `; `:443` and no port are treated alike). Any other prompt, such as one for another host after a redirect, gets no answer, so credentials are only ever given to the exact host and port of the project's URL. Credentials never appear on a command line, in the URL or in `.git/config`, and no credential helper caches them.
- **SSH** (`auth.type: "ssh"`, required for SSH URLs): an unencrypted private key (OpenSSH or PEM format), typically a read-only deploy key. Passphrase-protected keys are rejected. For each operation the key is written to a 0600 file in a fresh 0700 directory under `<data-dir>/tmp/` together with a generated `ssh_config`, which is ssh's only configuration (`ssh -F`; `~/.ssh/config` and the system file are not read). The key is the only identity offered: the daemon user's own `~/.ssh` keys and SSH agent are never used, and password, keyboard-interactive, agent forwarding, proxy commands and connection sharing are disabled. Both files are deleted when the operation ends; leftovers from a crash are removed at the next start. SSH URLs cannot use `auth.type: "none"`.
- **None**: public repositories over HTTPS.

Changing a project's URL to another scheme, host or port requires sending `auth` in the same request, so credentials set for one host are never sent to another. Changing only the path keeps them.

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

Git runs non-interactively with an isolated configuration: system and global git config are ignored, hooks and submodules are disabled, only the `https` and `ssh` transports are allowed, and every command has a timeout (15 minutes for network operations, 60 seconds for local ones). Cancelling a job stops git and all processes it started; on Linux git is also killed if the daemon dies.

## Size limits

A checkout (working tree and `.git`, including Git LFS content) may not grow beyond `--max-repo-size-mb` (default 10240 MiB; `0` disables the limit). The size is sampled every few seconds while clone, fetch, checkout and `git lfs pull` run, and after each of them; an operation that exceeds it is stopped and the job fails with "the checkout exceeds the maximum repository size configured on the host" (a failed clone leaves nothing behind, a failed pull can leave a partially updated checkout). A clone, pull or branch switch does not start when less than 512 MiB is free on the data directory's filesystem. Each job log is capped at 16 MiB; further output is replaced by a truncation note.

## Git LFS

If the checkout's `.gitattributes` files use `filter=lfs`, the job runs `git lfs pull` with the same credentials after cloning or pulling. When `git-lfs` is not installed the job fails with "the repository uses Git LFS but git-lfs is not installed on the host"; install it (`apt install git-lfs`) and restart the daemon (it checks for `git-lfs` once per run).

The LFS server is always derived from the project's URL, never from the repository: for `https://host/team/game` (with or without `.git`) it is `https://host/team/game.git/info/lfs`, and for an SSH URL it is the SSH remote itself (git-lfs asks the server through `git-lfs-authenticate`). The transport follows the project's URL, so an HTTPS project never makes LFS connections over plain HTTP or SSH. A repository whose `.lfsconfig` sets the LFS server or transfer settings (`lfs.url`, `lfs.pushurl`, `remote.*.lfsurl`, `remote.*.lfspushurl`, `lfs.<url>.access` for another URL, `lfs.customtransfer.*`, `lfs.standalonetransferagent`, `lfs.extension.*`, `url.*.insteadOf`) fails the job with a message naming the keys in the job log; remove them or point the project at the repository that hosts the LFS objects. The LFS server the project points at is trusted: like any Git LFS client, the daemon downloads objects from whatever URLs it returns (often a storage host), without sending the project's credentials there.

## Troubleshooting

The job's detail gives the reason (authentication failed, repository not found, branch does not exist, host key verification failed, remote unreachable, timeout); the job log (`GET /api/v1/jobs/{id}/log`) has the git output. A project in status `error` is repaired by pulling it, which re-clones when there is no checkout. A build requires an idle checkout with a Git HEAD that matches the recorded commit. Use `pull: true` to update the checkout and build in one job.

## See also

- [TROUBLESHOOTING.md](TROUBLESHOOTING.md#before-the-build-starts): messages about the checkout when a build starts.
- [SECURITY.md](SECURITY.md#game-repositories): why repositories are untrusted and what the daemon reads from them.
