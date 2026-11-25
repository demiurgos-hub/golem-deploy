-- The server's Inspector switch: when it is on and the installation's
-- Inspector is turned on, the game's Inspector agent is pointed at the
-- daemon's Inspector listener from the next (re)start. Off by default.
-- The installation's own Inspector setting and its sealed secrets are rows
-- of the settings table (inspector_enabled, inspector_secrets), written on
-- first use.
ALTER TABLE servers ADD COLUMN inspector INTEGER NOT NULL DEFAULT 0 CHECK (inspector IN (0, 1));
