-- Backups record how many entries of the data directory they leave out
-- (links, pipes, devices, names that are not allowed), and a server's
-- deletion takes a last backup of kind 'pre_delete'. SQLite cannot change
-- a CHECK constraint, so the table is rebuilt with the same rows. No other
-- table references backups.
CREATE TABLE backups_new (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    kind               TEXT    NOT NULL CHECK (kind IN ('manual', 'scheduled', 'pre_restore', 'pre_reset', 'pre_delete')),
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
    -- Entries of the data directory the archive leaves out (0: complete).
    omitted_entries    INTEGER NOT NULL DEFAULT 0 CHECK (omitted_entries >= 0),
    sha256             TEXT    NOT NULL,
    job_id             INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
    created_by         INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
    created_at         TEXT    NOT NULL
);
INSERT INTO backups_new (id, kind, file, server_id, server_name, project_id, project_name, build_id, commit_hash, engine_version,
    schema_fingerprint, live, note, size_bytes, content_bytes, file_count, sha256, job_id, created_by, created_at)
SELECT id, kind, file, server_id, server_name, project_id, project_name, build_id, commit_hash, engine_version,
    schema_fingerprint, live, note, size_bytes, content_bytes, file_count, sha256, job_id, created_by, created_at FROM backups;
-- Keep the id sequence, so the id of a deleted backup is never reused.
DELETE FROM sqlite_sequence WHERE name = 'backups_new';
INSERT INTO sqlite_sequence (name, seq) SELECT 'backups_new', seq FROM sqlite_sequence WHERE name = 'backups';
DROP TABLE backups;
ALTER TABLE backups_new RENAME TO backups;
CREATE INDEX backups_server ON backups(server_id, created_at);

-- When the last scheduled backup of a server failed (NULL after a success
-- or when none failed): the next attempt waits ScheduleRetryDelay, also
-- across daemon restarts.
ALTER TABLE servers ADD COLUMN backup_failed_at TEXT;
