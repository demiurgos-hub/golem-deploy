-- The endpoint paths the built game serves its transports on, from the
-- game's golem-deploy.yaml (transport.webtransport_path and
-- transport.websocket_path, defaults "/wt" and "/api/ws"). Servers publish
-- their public and fallback URLs with them. Empty for builds recorded
-- before (the defaults apply).
ALTER TABLE builds ADD COLUMN webtransport_path TEXT NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN websocket_path TEXT NOT NULL DEFAULT '';
