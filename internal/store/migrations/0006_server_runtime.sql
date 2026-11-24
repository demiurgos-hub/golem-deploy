-- Servers run builds: the image a server runs is the image_id of its build
-- (servers.build_id), never an image reference stored on the server. Rows
-- without a build cannot start until one is assigned.
ALTER TABLE servers DROP COLUMN image;
-- A server's project is its build's project. project_id is no longer read
-- or written (it cannot be dropped while it carries a foreign key).
UPDATE servers SET project_id = NULL;
-- Extra browser origins allowed to open WebTransport and WebSocket
-- connections to the server: a JSON array of "scheme://host[:port]".
ALTER TABLE servers ADD COLUMN allowed_origins TEXT NOT NULL DEFAULT '[]';
-- The game's admin endpoint of the recorded container: the loopback host
-- port Docker assigned to it and the per-container bearer token (AES-GCM
-- ciphertext from internal/secrets). Both are cleared with container_id.
ALTER TABLE servers ADD COLUMN admin_port INTEGER;
ALTER TABLE servers ADD COLUMN admin_token BLOB;
