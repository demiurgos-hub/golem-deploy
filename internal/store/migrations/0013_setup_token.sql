-- The one-time setup token of a new installation ("golem-deploy admin
-- setup-link"). At most one row: a new link replaces the previous one. Only
-- the SHA-256 of the token is kept, as hex. The row is deleted when the
-- setup completes and when an approved admin account appears another way.
CREATE TABLE setup_token (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    token_hash TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
