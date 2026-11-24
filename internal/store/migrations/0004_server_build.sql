-- The build a server runs (NULL for rows created before builds existed;
-- such a server cannot start until a build is assigned). A build that a
-- server references is never deleted.
ALTER TABLE servers ADD COLUMN build_id INTEGER REFERENCES builds(id);
CREATE INDEX servers_build ON servers(build_id);
