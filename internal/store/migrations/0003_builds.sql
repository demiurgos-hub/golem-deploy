-- Builds: one row per build of a project's checkout into a managed image.
-- The image a server may run is always the image_id of a successful build
-- of this table; labels on an image alone are never trusted.
CREATE TABLE builds (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id         INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- Branch and commit of the checkout that was built. commit_hash is
    -- empty until the build job has read the checkout.
    branch             TEXT    NOT NULL DEFAULT '',
    commit_hash        TEXT    NOT NULL DEFAULT '',
    status             TEXT    NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'success', 'error', 'cancelled')),
    -- Short, user-safe outcome message (empty on success).
    detail             TEXT    NOT NULL DEFAULT '',
    job_id             INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
    -- Set on success: the immutable image id ("sha256:...") and its tag.
    -- Two builds can produce the same image id (identical inputs).
    image_id           TEXT    NOT NULL DEFAULT '',
    image_tag          TEXT    NOT NULL DEFAULT '',
    -- Golem Engine module version the game requires (from its go.mod).
    engine_version     TEXT    NOT NULL DEFAULT '',
    -- Reported by a running server's admin endpoint; empty until a server
    -- built from this image has become ready once.
    schema_fingerprint TEXT    NOT NULL DEFAULT '',
    created_by         INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
    created_at         TEXT    NOT NULL,
    started_at         TEXT,
    finished_at        TEXT
);
CREATE INDEX builds_project ON builds(project_id, id);
CREATE INDEX builds_image ON builds(image_id);

-- Static add-ons of a project: a JSON array of {"name","target"} objects
-- (a ZIP in the static store, extracted at a path relative to the game root).
ALTER TABLE projects ADD COLUMN static_addons TEXT NOT NULL DEFAULT '[]';
