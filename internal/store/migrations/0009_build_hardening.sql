ALTER TABLE projects ADD COLUMN allow_custom_dockerfile INTEGER NOT NULL DEFAULT 0
    CHECK (allow_custom_dockerfile IN (0, 1));

CREATE TABLE build_image_cleanup (
    image_id TEXT PRIMARY KEY
);

CREATE TRIGGER build_image_cleanup_on_delete AFTER DELETE ON builds
WHEN OLD.status = 'success' AND OLD.image_id <> ''
BEGIN
    INSERT OR IGNORE INTO build_image_cleanup (image_id) VALUES (OLD.image_id);
END;

CREATE TABLE build_exports (
    build_id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    project_name TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    engine_version TEXT NOT NULL,
    recorded_at TEXT NOT NULL
);

CREATE TRIGGER build_export_published AFTER UPDATE OF status ON builds
WHEN NEW.status = 'success'
BEGIN
    DELETE FROM build_exports WHERE build_id = NEW.id;
END;

CREATE TRIGGER build_export_unpublished AFTER UPDATE OF status ON builds
WHEN NEW.status IN ('error', 'cancelled') AND NEW.finished_at IS NOT NULL
BEGIN
    UPDATE build_exports SET recorded_at = NEW.finished_at WHERE build_id = NEW.id;
END;
