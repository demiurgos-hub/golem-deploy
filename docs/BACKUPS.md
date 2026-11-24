# Saved data and backups

Every game server keeps its game's data directory on a Docker volume of its own, so the world survives restarts, new builds and daemon upgrades. Backups copy that directory into archives stored by the daemon, which can be downloaded, restored into the same or another server, and taken on a schedule. The HTTP contract is in [API.md](API.md#saved-data-and-backups); how servers run is in [SERVERS.md](SERVERS.md).

## The data volume

- The daemon creates a server's data volume on its first start: a named volume `golem-deploy-data-<server id>-<random>` with the labels `dev.golemengine.deploy.managed`, `instance`, `server-id` and `server`. Its name is recorded on the server row (`Server.data.volume`); a restore or reset replaces it with a new name, so a volume name always means one set of data.
- The volume is mounted read-write at `/data`, and the game gets `GOLEM_SNAPSHOT_DIR=/data` and `GOLEM_SNAPSHOT_INTERVAL` (the server's `snapshot_interval_seconds`, default 300; `0` turns autosave off). Golem Engine then loads `world.snapshot` when it starts, saves it every interval and when it stops on SIGTERM (a stop or restart from the panel is graceful), and may keep other files next to it. Changing the interval marks a running server `restart_required`.
- A fresh volume takes the owner and mode of the image's `/data` when it is first mounted. Generated images have an empty `/data` owned by uid 65532 (the user every server runs as). **An image built from a custom Dockerfile must provide a `/data` directory writable by uid 65532**, otherwise the game cannot save.
- If the recorded volume disappears (removed outside the daemon), the next start creates a new, empty one, says so in the job log and forgets the recorded schema fingerprint.
- Deleting a server removes its container, then its data volume, then the server; its backups stay. `DELETE /api/v1/volumes/{name}` refuses a volume that is a server's current data volume (`Volume.current`) even when no container mounts it. Volumes left behind (a restore or reset whose old volume could not be removed, a crash in the middle of a restore) are listed with `current: false` and can be deleted there.

## What a backup contains

A backup is one gzip-compressed tar of the data directory, stored as `<data-dir>/backups/<random>.tar.gz` (directory 0700, files 0600) with a database record (`Backup`). The archive holds:

- regular files and directories, with their names and modification times, owner and group set to 0 and modes masked (the owner can read and write, nobody else can write, no special bits);
- nothing else. Symbolic links, hard links, named pipes, devices and entries whose paths are not clean relative paths of at most 32 levels and 1024 bytes are left out; the job log lists them (names sanitised). A file below a directory that was left out is left out too.

Everything in the data directory was written by the game, which runs repository code, so the daemon treats it as **untrusted**: it never follows a link or interprets a file, it reads only the header of the top-level `world.snapshot` (magic, format version 1 or 2, timestamp, fingerprint of at most 128 bytes) to record the backup's schema fingerprint, and anything unexpected there means "unknown" (`schema_fingerprint: ""`). Download and restore return the archive's exact bytes; check them against `sha256` if they travel.

Limits: the files of a data directory may hold at most `--backup-max-mb` MiB (default 1024; `BackupList.max_content_bytes`) and at most 100000 entries. Reaching either stops the copy at once and fails the job without leaving a file. A backup also needs the free space the static store keeps (1 GiB on the file system of the data directory), checked before and while it is written. Backups are not counted against the static store's quota; `BackupList.total_bytes` is their total size.

## Taking a backup

`POST /api/v1/servers/{id}/backups` (developer, optional `note`) starts a `server.backup` job, serialised with the server's other jobs:

1. When the server's container runs and its admin endpoint answers, the daemon asks the game for a fresh snapshot (`POST /snapshot` on the admin endpoint, up to 75 seconds). While the game reports a snapshot in progress the daemon waits and asks again. A game without the snapshot hook (501), a stopped server or an admin endpoint that does not answer gives a backup of the files as they are (`live: false`). Any other answer (the snapshot failed or timed out) fails the job with a detail that says so.
2. A helper container mounts the volume read-only (see below) and `docker cp` streams the directory into the normaliser, which writes the archive under a temporary name.
3. The archive is synced and moved to its final name, then recorded. A server without a data volume, or whose data directory holds no files, fails with a detail that says there is nothing to back up.

A crash at any point leaves either a complete, recorded backup or a file without a record, which the daemon removes when it starts. Deleting a backup (`DELETE /api/v1/backups/{id}`, admin) removes the record and then the file; a backup that a running restore reads cannot be deleted (409).

## Helper containers

All reading and writing of a volume goes through a helper container: created from the server's verified build image (the same check as a start) and **never started**, with only the data volume mounted (read-only for a backup), no network, a read-only root filesystem, all capabilities dropped, no-new-privileges, user 65532, a pids limit, no ports, no environment, `--pull never`, an entrypoint that does not exist and every `dev.golemengine.deploy.*` label set explicitly, including `helper=<launch id>`. Its name is `golem-deploy_helper_<launch id>`. The random launch id is recorded in the database before the create and the container id right after; the helper is removed when the operation ends, also when it fails or is cancelled. Helpers a crash left behind are removed at startup (and retried every minute) by exactly that recorded id, never by labels alone, and they are neither reported as servers nor logged as foreign containers.

## Restore

`POST /api/v1/servers/{id}/restore` (admin) with `{backup_id, force?, skip_safety_backup?}` starts a restricted `server.restore` job. A backup can be restored into any server, also one other than it was taken from.

The handler checks, and the job checks again: the backup exists and its file matches its recorded size and SHA-256; the server has a successful build whose image exists; and the schema rule (below). Then the job:

1. stops the server gracefully without changing its desired state (the game saves its snapshot);
2. takes a safety backup of the current data (kind `pre_restore`), unless `skip_safety_backup` is set or there is no data; if it fails, the server is started again when it is meant to run and the job fails;
3. creates a fresh volume and copies the backup into it through a writable helper, with every file owned by uid and gid 65532 so the game can change it;
4. only then switches the server to the new volume and to the backup's schema fingerprint, in one database write;
5. removes the server's previous container and old volume (a failure is logged and leaves a non-current volume an admin can delete);
6. starts the server through the normal health-gated start when its desired state is `running`.

A failure or a daemon crash before step 4 leaves the server on its old data (a failed copy also starts it again when it is meant to run; a cancelled job leaves it stopped). A cancelled or crashed restore can leave an unused volume behind, listed with `current: false`.

## Reset

`POST /api/v1/servers/{id}/data/reset` (admin, optional `{skip_safety_backup}`) starts a restricted `server.data_reset` job: it stops the server, takes a safety backup (kind `pre_reset`, with the same rules), detaches the data volume and forgets the recorded fingerprint, removes the old container and volume, and starts the server again with an empty world when it is meant to run (the start creates a new volume).

## The schema guard

A Golem Engine game refuses to load a snapshot whose schema fingerprint differs from its own: it exits at startup with an error naming both. The daemon knows two fingerprints:

- the **build's** (`Build.schema_fingerprint`): read at build time from the committed generated code (the `SchemaFingerprint` constant in `golem_helpers.go` inside the directory `integrations.go-server.out` of `golem.yaml` names, only for generated Dockerfiles whose committed code the build verifies with `golem-bake --check`), and replaced by what the game reports in `/info` once a server running the build becomes ready;
- the **saved data's** (`Server.data.schema_fingerprint`): what the game reported when a start reached ready, the backup's fingerprint after a restore, empty after a reset.

When both are known and differ (`Server.data.schema_mismatch`), a start or restart fails at once, before the running container is stopped or removed, with a detail that names both fingerprints and the three ways out: assign a build with the saved schema, restore a backup with the build's schema, or reset the data. An unknown fingerprint on either side is never blocked. A restore of a backup whose known fingerprint differs from the build's known fingerprint is refused with 409 `schema_mismatch` unless `force` is set; after a forced restore the automatic start is skipped (the job says so and still succeeds), so the operator can assign a matching build first.

## Schedule and retention

`Server.backup_schedule` is `{interval_hours, keep}`: `interval_hours` 0 (off, the default) or 1 to 720, `keep` 1 to 100 (default 7). It applies at once and never needs a restart. About once a minute the daemon starts a `server.backup` job (kind `scheduled`, no creator) for every server with a schedule whose container is running and that has no active job, when its newest backup of any kind is older than the interval (or it has none). After a successful scheduled backup the server's oldest scheduled backups beyond `keep` are deleted; manual and safety backups are never deleted automatically. After a failed scheduled backup the next attempt waits at least 15 minutes. Stopped servers are not backed up.

## Backing up the daemon itself

Backups live in the data directory. Copy `<data-dir>/backups/` together with `golem-deploy.db` and `keys/` when you back up the daemon (see [INSTALL.md](INSTALL.md#upgrade)). The data volumes themselves stay in Docker; a backup of the host should include Docker's volumes or rely on the archives above.
