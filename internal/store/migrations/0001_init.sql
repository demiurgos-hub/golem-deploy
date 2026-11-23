-- Initial schema. Timestamps are TEXT in the fixed-width UTC format
-- 2006-01-02T15:04:05.000Z so they sort lexically.

CREATE TABLE accounts (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin', 'developer')),
    approved      INTEGER NOT NULL DEFAULT 0 CHECK (approved IN (0, 1)),
    token_version INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL
);

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    token_hash   TEXT    NOT NULL UNIQUE,
    prefix       TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,
    last_used_at TEXT,
    expires_at   TEXT
);
CREATE INDEX api_tokens_account ON api_tokens(account_id);

CREATE TABLE projects (
    id             INTEGER PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    repo_url       TEXT NOT NULL,
    branch         TEXT NOT NULL DEFAULT '',
    auth_type      TEXT NOT NULL DEFAULT 'none' CHECK (auth_type IN ('none', 'https', 'ssh')),
    auth_username  TEXT NOT NULL DEFAULT '',
    auth_secret    BLOB, -- AES-GCM ciphertext (internal/secrets) of the password or SSH private key
    last_commit    TEXT NOT NULL DEFAULT '',
    last_pulled_at TEXT,
    status         TEXT NOT NULL DEFAULT 'idle',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE TABLE servers (
    id            INTEGER PRIMARY KEY,
    name          TEXT    NOT NULL UNIQUE,
    project_id    INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    image         TEXT    NOT NULL,
    port          INTEGER UNIQUE,
    env           BLOB, -- AES-GCM ciphertext (internal/secrets) of a JSON object
    desired_state TEXT    NOT NULL DEFAULT 'stopped' CHECK (desired_state IN ('running', 'stopped')),
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);

CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    subject_type TEXT NOT NULL,
    subject_id   TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('queued', 'running', 'success', 'error', 'cancelled')),
    detail       TEXT NOT NULL DEFAULT '',
    created_by   INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
    created_at   TEXT NOT NULL,
    started_at   TEXT,
    finished_at  TEXT
);
CREATE INDEX jobs_subject ON jobs(subject_type, subject_id);
CREATE INDEX jobs_status ON jobs(status);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
