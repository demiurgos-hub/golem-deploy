# Saved data and backups

Every game server keeps its game's data directory on a Docker volume of its own, so the world survives restarts, new builds and daemon upgrades. Backups copy that directory into archives stored by the daemon, which can be downloaded, restored into the same or another server, and taken on a schedule. The HTTP contract is in [API.md](API.md#saved-data-and-backups); how servers run is in [SERVERS.md](SERVERS.md).

## The data volume

- The daemon creates a server's data volume on its first start: a named volume `golem-deploy-data-<server id>-<random>` with the labels `dev.golemengine.deploy.managed`, `instance`, `server-id` and `server`. Its name is recorded on the server row (`Server.data.volume`); a restore or reset replaces it with a new name, so a volume name always means one set of data.
- The volume is mounted read-write at `/data`, and the game gets `GOLEM_SNAPSHOT_DIR=/data` and `GOLEM_SNAPSHOT_INTERVAL` (the server's `snapshot_interval_seconds`, default 300; `0` turns autosave off). Golem Engine then loads `world.snapshot` when it starts, saves it every interval and when it stops on SIGTERM (a stop or restart from the panel is graceful), and may keep other files next to it. Changing the interval marks a running server `restart_required`.
- A fresh volume takes the owner and mode of the image's `/data` when it is first mounted, and Docker copies whatever the image has in `/data` into it (standard Docker behaviour). So a normal first start, or a start after a reset, begins with whatever the image has in `/data`. Generated images have an empty `/data` owned by uid 65532 (the user every server runs as). **An image built from a custom Dockerfile must provide a `/data` directory writable by uid 65532**, otherwise the game cannot save, and should leave it empty, otherwise a restore refuses to run (see below).
- If the recorded volume disappears (removed outside the daemon), the next start creates a new, empty one, says so in the job log and forgets the recorded schema fingerprint.
- Deleting a server stops its container, stores a last backup of its data (see [Deleting a server](#deleting-a-server)), then removes the container, the data volume and the server; its backups stay. `DELETE /api/v1/volumes/{name}` refuses a volume that is a server's current data volume (`Volume.current`) even when no container mounts it. Volumes left behind (a restore or reset whose old volume could not be removed, a crash in the middle of a restore) are listed with `current: false` and can be deleted there.

## What a backup contains

A backup is one gzip-compressed tar of the data directory, stored as `<data-dir>/backups/<random>.tar.gz` (directory 0700, files 0600) with a database record (`Backup`). The archive holds:

- regular files and directories, with their names and modification times, owner and group set to 0 and modes masked (the owner can read and write, nobody else can write, no special bits);
- nothing else. Symbolic links, hard links, named pipes, devices and entries whose paths are not clean relative paths of at most 32 levels and 1024 bytes are left out; the job log says how many per reason (for example "left out 5 entries: 2 symbolic links, 1 named pipe, 2 with names that are not allowed") and `Backup.omitted_entries` counts them, so 0 means a complete copy. Their names are never written anywhere, not to the job log and not to the daemon log: the game chose them, and they can hold values of the server's environment that no redaction could know. A file below a directory that was left out is left out too.

Docker produces the copy stream itself and can leave out files it cannot read; those are not counted.

Everything in the data directory was written by the game, which runs repository code, so the daemon treats it as **untrusted**: it never follows a link or interprets a file, it reads only the header of the top-level `world.snapshot` (magic, format version 1 or 2, timestamp, fingerprint of at most 128 bytes) to record the backup's schema fingerprint, and anything unexpected there means "unknown" (`schema_fingerprint: ""`). Download and restore return the archive's exact bytes; check them against `sha256` if they travel.

A backup describes the build that wrote the data (`build_id`, `commit`, `engine_version`, `project_id`, `project_name`): when the server has a recorded container, running or stopped, the build named by the labels the daemon set on that container (checked against the build's record and image; a deleted build leaves the commit and engine version without a build id), otherwise the server's assigned build. After a new build is assigned and before the server is restarted, a backup therefore still names the build whose game wrote the data.

Limits: the files of a data directory may hold at most `--backup-max-mb` MiB (default 1024; `BackupList.max_content_bytes`) and at most 100000 entries. Every entry's content counts, also that of an entry that is left out, and an entry too large for what is left of the limit is refused before it is read. Every header in the copy stream counts towards the entries, including the PAX or GNU records in front of an entry, and the stream as a whole may be at most the size limit plus 16 KiB per entry. Reaching any of these stops the copy at once (it is cut off, not read to its end) and fails the job without leaving a file. A backup also needs the free space the static store keeps (1 GiB on the file system of the data directory), checked before it is written and every 64 MiB while it is written. The reserve is approximate: it is not coordinated between backups running at the same time. Backups are not counted against the static store's quota; `BackupList.total_bytes` is their total size.

## Taking a backup

`POST /api/v1/servers/{id}/backups` (developer, optional `note`) starts a `server.backup` job, serialised with the server's other jobs:

1. When the server's container runs and its admin endpoint answers, the daemon asks the game for a fresh snapshot (`POST /snapshot` on the admin endpoint). The whole request, retries and the inspections between them included, has one deadline of three minutes. While the game reports a snapshot in progress (409) the daemon waits, inspects the recorded container again and asks again on the port that inspection reports, so the admin token only goes to a port the container owns; a container that stopped meanwhile is copied as it is. A game without the snapshot hook (501), a stopped server or an admin endpoint that does not answer gives a backup of the files as they are (`live: false`). A game that is starting or stopping (503) fails the job with a detail that says to try again shortly. Any other answer (the snapshot failed or timed out, or the deadline passed) fails the job with a detail that says so.
2. A helper container mounts the volume read-only (see below) and `docker cp` streams the directory into the normaliser, which writes the archive under a temporary name.
3. The archive is synced and moved to its final name, then recorded. A server without a data volume, or whose data directory holds no regular files, fails with a detail that says there is nothing to back up.

A crash at any point leaves either a complete, recorded backup or a file without a record, which the daemon removes when it starts. Deleting a backup (`DELETE /api/v1/backups/{id}`, admin) removes the record and then the file; a backup that a running restore reads cannot be deleted (409).

## Downloading a backup

`GET /api/v1/backups/{id}/download` returns the archive's exact bytes. The daemon opens the archive once, without following links, and serves that open file. The web client holds a download in memory before it saves it, so large archives are best downloaded with an API token and `curl`:

```sh
curl -fSL -H "Authorization: Bearer $TOKEN" -o backup.tar.gz https://panel.example.com/api/v1/backups/42/download
```

## Helper containers

All reading and writing of a volume goes through a helper container: created from a verified build image (the same check as a start) and **never started**, with only the data volume mounted (read-only for a backup), no network, a read-only root filesystem, all capabilities dropped, no-new-privileges, user 65532, a pids limit, no ports, no environment, `--pull never`, an entrypoint that does not exist and every `dev.golemengine.deploy.*` label set explicitly, including `helper=<launch id>`. Its name is `golem-deploy_helper_<launch id>`. A backup's helper uses the image of the build that wrote the data when it still exists and verifies, otherwise the image of the assigned build, so a newly assigned build whose image is missing does not block backing up the data the running build wrote; a restore's helper uses the assigned build's image.

The random launch id is recorded in the database before the create and the container id right after; the helper is removed when the operation ends, also when it fails or is cancelled. Helpers a crash left behind are removed at startup (and retried every minute) by exactly that recorded id, never by labels alone, and they are neither reported as servers nor logged as foreign containers. When a create fails without saying whether Docker created the container (a lost reply), the record is kept and looked for again for ten minutes before it is forgotten.

## Safety backups

A restore, a reset and a deletion first take a backup of the server's current data (kind `pre_restore`, `pre_reset` or `pre_delete`), after the server is stopped and before anything is replaced or removed:

- it is skipped silently when the server has no data volume, the volume is missing, or the data directory holds no entries at all;
- it is skipped on request with `skip_safety_backup` (a deletion takes it as the query `skip_safety_backup=1` or `true`, admins only; `0` and `false` keep it, and any other value, also an empty one, is 400 `bad_request`);
- when the copy would leave entries out (`omitted_entries` above 0), the backup is stored if it has at least one file, and then the operation fails before anything is replaced, with a detail that says how many entries a backup cannot hold, the id of the incomplete backup and that repeating the request with `skip_safety_backup` goes ahead. A data directory with entries a backup cannot hold but no regular files fails the same way, without a stored backup;
- when it cannot be taken at all, the operation fails with a detail that says why.

After a failed safety backup the server is put back (see below).

## Putting the server back

A restore, a reset and a deletion first note whether the server's recorded container is running, then stop it gracefully without changing the server's desired state. Every way such a job can end before it replaces or removes the data puts the server back: an error or a cancellation of the stop itself, a failed or incomplete safety backup, a failure creating the new volume or copying into it, a restore refused because the image has files in `/data`, a cancellation before the switch, a failed database switch, and on a deletion a failure removing the container after the last backup was stored. Only the jobs that switched the data (a restore or reset past its database write) or removed it (a deletion that removed the container) leave the container stopped.

To put the server back, the daemon inspects the recorded container again. When it was running before the stop and is not running now, the daemon starts that same container (`docker start` of the recorded id) and waits until its game reports ready with the recorded admin token, the way a running container is verified after a daemon restart; the job log says whether the server runs again, or why not (Docker failures go to the daemon log). Nothing is created or removed and the assigned build is not consulted, so a build assigned since, which the next start may refuse (for example by the schema guard), does not keep a server that was serving players down. This runs under its own deadline of `--start-timeout` plus a minute, also after a cancellation. A server whose container was not running before stays as it was.

## Restore

`POST /api/v1/servers/{id}/restore` (admin) with `{backup_id, force?, skip_safety_backup?}` starts a restricted `server.restore` job. A backup can be restored into any server, also one other than it was taken from.

The handler checks, and the job checks again: the backup exists and its file matches its recorded size and SHA-256; the server has a successful build whose image exists; and the schema rule (below). The job verifies the archive on the same open file it later copies from. Then the job:

1. stops the server gracefully without changing its desired state (the game saves its snapshot);
2. takes a safety backup of the current data (see [Safety backups](#safety-backups));
3. creates a fresh volume and a writable helper on it. Docker fills the empty volume with the image's `/data` when the helper is created; when that holds anything besides the directory itself, the restore fails here with a detail that says so, because the restored volume would not hold exactly the backup's files. Otherwise the job copies the backup into it, with every file owned by uid and gid 65532 so the game can change it;
4. checks for cancellation, and only then switches the server to the new volume and to the backup's schema fingerprint, in one database write;
5. removes the server's previous container and old volume (a failure is logged and leaves a non-current volume an admin can delete);
6. starts the server through the normal health-gated start when its desired state is `running`.

A failure, a cancellation or a daemon crash before step 4 leaves the server on its old data; after a failure or a cancellation the job also puts the server back (see [Putting the server back](#putting-the-server-back)). A daemon crash after step 1 leaves the container stopped; when the daemon starts again, a server meant to run is recovered through the normal start, and a start that fails before it replaces the recorded container (the schema guard, for one) leaves that container stopped, so such a server has to be started by hand once the cause is resolved. After step 4 the job does not end as cancelled: it finishes steps 5 and 6, except that a cancellation skips the automatic start with a line in the job log, and it succeeds. A cancelled or crashed restore can leave an unused volume behind, listed with `current: false`.

## Reset

`POST /api/v1/servers/{id}/data/reset` (admin, optional `{skip_safety_backup}`) starts a restricted `server.data_reset` job: it stops the server, takes a safety backup (kind `pre_reset`, see [Safety backups](#safety-backups)), checks for cancellation, detaches the data volume and forgets the recorded fingerprint, removes the old container and volume, and starts the server again when it is meant to run (the start creates a new volume, which begins with whatever the image has in `/data`). Failures before the data is detached put the server back, and cancellation and crashes follow the rules of a restore.

## Deleting a server

`DELETE /api/v1/servers/{id}` stops the server's container, takes a last backup of its data (kind `pre_delete`, see [Safety backups](#safety-backups)) and only then removes the container, the data volume and the server. When the last backup fails, nothing is removed, the server is put back (see [Putting the server back](#putting-the-server-back)) and the request answers 503 `service_unavailable` when the backup failed because Docker is unreachable, otherwise 409 `conflict` with a message that says why. A failure removing the container also puts the server back. An admin can delete without the last backup with `DELETE /api/v1/servers/{id}?skip_safety_backup=1` (or `true`); other accounts get 403 `forbidden`. `skip_safety_backup` accepts only `1`, `true`, `0` and `false`; any other value, also an empty or a repeated one, is 400 `bad_request`. A server without a data volume is deleted without a backup.

## The schema guard

A Golem Engine game refuses to load a snapshot whose schema fingerprint differs from its own: it exits at startup with an error naming both. The daemon knows two fingerprints:

- the **build's** (`Build.schema_fingerprint`): read at build time from the committed generated code (the `SchemaFingerprint` constant in `golem_helpers.go` inside the directory `integrations.go-server.out` of `golem.yaml` names, only for generated Dockerfiles whose committed code the build verifies with `golem-bake --check`). When the build has none, the first value a game running the build reports in `/info` once its server becomes ready is filled in (one conditional write). A recorded value is never replaced: the game is untrusted, and one server's game must not change the fingerprint every other server of the build is judged by. A report that differs from the recorded value is noted in the job log of the start;
- the **saved data's** (`Server.data.schema_fingerprint`): what the server's own game reported when a start reached ready, the backup's fingerprint after a restore, empty after a reset.

When both are known and differ (`Server.data.schema_mismatch`), a start or restart fails at once, before the running container is stopped or removed, with a detail that names both fingerprints and the three ways out: assign a build with the saved schema, restore a backup with the build's schema, or reset the data. Crash recovery after a daemon restart follows the same rule: a start refused this way, like any start that fails before it replaces the recorded container, leaves that container as it is, also when it is running. A game whose report differs from its build's recorded value therefore makes the next start of its server fail until they agree. An unknown fingerprint on either side is never blocked. A restore of a backup whose known fingerprint differs from the build's known fingerprint is refused with 409 `schema_mismatch` unless `force` is set; after a forced restore the automatic start is skipped (the job says so and still succeeds), so the operator can assign a matching build first.

## Schedule and retention

`Server.backup_schedule` is `{interval_hours, keep}`: `interval_hours` 0 (off, the default) or 1 to 720, `keep` 1 to 100 (default 7). It applies at once and never needs a restart. About once a minute the daemon starts a `server.backup` job (kind `scheduled`, no creator) for every server with a schedule whose container is running and that has no active job, when its newest backup of any kind is older than the interval (or it has none). Once the job holds the server's job slot it checks again that the schedule is still on, no scheduled backup failed within the last 15 minutes (a failure recorded after the check that started the job counts), the container still runs and the newest backup is still older than the interval; otherwise it ends successfully with a line saying why. When that check itself fails (other than by cancellation), the failure is recorded like a failed backup. After a successful scheduled backup the server's oldest scheduled backups beyond `keep` are deleted; manual and safety backups are never deleted automatically. After a failed scheduled backup the next attempt waits at least 15 minutes; the time of the failure is stored with the server, so the wait also holds across a daemon restart. Stopped servers are not backed up.

## Backing up the daemon itself

Backups live in the data directory. Copy `<data-dir>/backups/` together with `golem-deploy.db` and `keys/` when you back up the daemon (see [INSTALL.md](INSTALL.md#upgrade)). The data volumes themselves stay in Docker; a backup of the host should include Docker's volumes or rely on the archives above.
