-- The server's Profiling switch: when it is on, the game's admin listener
-- serves Go profiles (GOLEM_ADMIN_PROFILING=1) from the next (re)start, and
-- GET /api/v1/servers/{id}/profile relays them. On by default, also for
-- servers that existed before this migration.
ALTER TABLE servers ADD COLUMN profiling INTEGER NOT NULL DEFAULT 1 CHECK (profiling IN (0, 1));
