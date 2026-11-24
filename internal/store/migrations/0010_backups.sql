-- Saved data: the Docker volume holding a server's data directory (the
-- name the daemon created it with; '' until the first start and after a
-- reset), the schema fingerprint of the saved world as far as the daemon
-- knows ('' when unknown), the game's autosave period and the server's
-- backup schedule.
ALTER TABLE servers ADD COLUMN data_volume TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN data_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN snapshot_interval_seconds INTEGER NOT NULL DEFAULT 300
    CHECK (snapshot_interval_seconds = 0 OR snapshot_interval_seconds BETWEEN 10 AND 86400);
ALTER TABLE servers ADD COLUMN backup_interval_hours INTEGER NOT NULL DEFAULT 0
    CHECK (backup_interval_hours BETWEEN 0 AND 720);
ALTER TABLE servers ADD COLUMN backup_keep INTEGER NOT NULL DEFAULT 7
    CHECK (backup_keep BETWEEN 1 AND 100);

-- Backups: one row per complete archive in <data-dir>/backups. The row is
-- written only after its file is complete and in place, and deleted before
-- its file is removed, so a row always has its file; files without a row
-- are removed at startup. The references describe where the backup came
-- from and survive the deletion of what they point to.
CREATE TABLE backups (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    kind               TEXT    NOT NULL CHECK (kind IN ('manual', 'scheduled', 'pre_restore', 'pre_reset')),
    -- Base name of the archive below backups/ (32 hex characters plus
    -- ".tar.gz"), chosen before the file is written.
    file               TEXT    NOT NULL UNIQUE,
    server_id          INTEGER REFERENCES servers(id) ON DELETE SET NULL,
    server_name        TEXT    NOT NULL,
    project_id         INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    project_name       TEXT    NOT NULL DEFAULT '',
    build_id           INTEGER REFERENCES builds(id) ON DELETE SET NULL,
    commit_hash        TEXT    NOT NULL DEFAULT '',
    engine_version     TEXT    NOT NULL DEFAULT '',
    schema_fingerprint TEXT    NOT NULL DEFAULT '',
    live               INTEGER NOT NULL DEFAULT 0 CHECK (live IN (0, 1)),
    note               TEXT    NOT NULL DEFAULT '',
    size_bytes         INTEGER NOT NULL,
    content_bytes      INTEGER NOT NULL,
    file_count         INTEGER NOT NULL,
    sha256             TEXT    NOT NULL,
    job_id             INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
    created_by         INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
    created_at         TEXT    NOT NULL
);
CREATE INDEX backups_server ON backups(server_id, created_at);

-- Helper containers: created (never started) from a server's build image to
-- read or write a data volume. The random launch id is recorded before
-- Docker create and passed as the helper label, the container id right
-- after; a row left by a crash identifies exactly the container to remove.
CREATE TABLE volume_helpers (
    launch_id    TEXT PRIMARY KEY,
    server_id    INTEGER NOT NULL,
    container_id TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL
);
