-- The id of the container the daemon created for a server (NULL when it has
-- none). A server's container is resolved by this id only, never by labels:
-- containers inherit labels from their image. Rows that existed before this
-- migration have no container recorded; an old container left from them
-- blocks the next start by its name until an operator removes it.
ALTER TABLE servers ADD COLUMN container_id TEXT;
