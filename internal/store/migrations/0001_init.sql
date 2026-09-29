-- Rowsmith metadata store, schema v1.
-- Timestamps are Unix milliseconds (UTC). Columns holding secrets contain
-- vault envelopes (see internal/vault), never plaintext.

CREATE TABLE users (
    id                   TEXT PRIMARY KEY,
    email                TEXT NOT NULL UNIQUE COLLATE NOCASE,
    name                 TEXT NOT NULL,
    password_hash        TEXT NOT NULL,
    role                 TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    mfa_secret           TEXT,
    mfa_enabled          INTEGER NOT NULL DEFAULT 0,
    mfa_last_step        INTEGER NOT NULL DEFAULT 0,
    failed_logins        INTEGER NOT NULL DEFAULT 0,
    locked_until         INTEGER NOT NULL DEFAULT 0,
    disabled             INTEGER NOT NULL DEFAULT 0,
    must_change_password INTEGER NOT NULL DEFAULT 0,
    prefs                TEXT NOT NULL DEFAULT '{}',
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    last_login_at        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE recovery_codes (
    user_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL,
    used_at   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, code_hash)
);

CREATE TABLE sessions (
    id_hash      TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf         TEXT NOT NULL,
    stage        TEXT NOT NULL CHECK (stage IN ('mfa', 'enroll', 'full')),
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE connections (
    id           TEXT PRIMARY KEY,
    owner_id     TEXT NOT NULL REFERENCES users (id),
    name         TEXT NOT NULL,
    driver       TEXT NOT NULL,
    color        TEXT NOT NULL DEFAULT '',
    environment  TEXT NOT NULL DEFAULT 'development'
                 CHECK (environment IN ('production', 'staging', 'development', 'local')),
    folder       TEXT NOT NULL DEFAULT '',
    params       TEXT NOT NULL DEFAULT '{}',   -- non-secret driver parameters (JSON)
    ssh          TEXT NOT NULL DEFAULT '{}',   -- non-secret SSH tunnel settings (JSON)
    secrets      TEXT NOT NULL DEFAULT '',     -- vault envelope of {field: value}
    read_only    INTEGER NOT NULL DEFAULT 0,
    team_access  TEXT NOT NULL DEFAULT ''
                 CHECK (team_access IN ('', 'read', 'write', 'manage')),
    notes        TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX connections_owner ON connections (owner_id);

CREATE TABLE connection_shares (
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    access        TEXT NOT NULL CHECK (access IN ('read', 'write', 'manage')),
    PRIMARY KEY (connection_id, user_id)
);

CREATE TABLE ssh_known_hosts (
    host        TEXT NOT NULL,
    port        INTEGER NOT NULL,
    key_type    TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    added_by    TEXT NOT NULL DEFAULT '',
    added_at    INTEGER NOT NULL,
    PRIMARY KEY (host, port, key_type)
);

CREATE TABLE saved_queries (
    id            TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id TEXT REFERENCES connections (id) ON DELETE SET NULL,
    database_name TEXT NOT NULL DEFAULT '',
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    body          TEXT NOT NULL,
    tags          TEXT NOT NULL DEFAULT '[]',
    visibility    TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'team')),
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX saved_queries_owner ON saved_queries (owner_id);

CREATE TABLE query_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL,
    database_name TEXT NOT NULL DEFAULT '',
    body          TEXT NOT NULL,
    started_at    INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    row_count     INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL,
    error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX query_history_user ON query_history (user_id, started_at DESC);
CREATE INDEX query_history_conn ON query_history (connection_id, started_at DESC);

CREATE TABLE object_notes (
    id            TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    object_path   TEXT NOT NULL,  -- database/schema/name, '' for the connection itself
    author_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    body          TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX object_notes_path ON object_notes (connection_id, object_path);

CREATE TABLE schedules (
    id              TEXT PRIMARY KEY,
    owner_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id   TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    database_name   TEXT NOT NULL DEFAULT '',
    name            TEXT NOT NULL,
    body            TEXT NOT NULL,
    cron            TEXT NOT NULL,
    timezone        TEXT NOT NULL DEFAULT 'UTC',
    format          TEXT NOT NULL DEFAULT 'csv' CHECK (format IN ('csv', 'xlsx', 'json')),
    recipients      TEXT NOT NULL DEFAULT '[]',
    alert           TEXT NOT NULL DEFAULT '{}',
    enabled         INTEGER NOT NULL DEFAULT 1,
    last_run_at     INTEGER NOT NULL DEFAULT 0,
    next_run_at     INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE schedule_runs (
    id          TEXT PRIMARY KEY,
    schedule_id TEXT NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL,
    row_count   INTEGER NOT NULL DEFAULT 0,
    alerted     INTEGER NOT NULL DEFAULT 0,
    file_name   TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX schedule_runs_schedule ON schedule_runs (schedule_id, started_at DESC);

CREATE TABLE audit_log (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      INTEGER NOT NULL,
    user_id TEXT NOT NULL DEFAULT '',
    ip      TEXT NOT NULL DEFAULT '',
    action  TEXT NOT NULL,
    target  TEXT NOT NULL DEFAULT '',
    detail  TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_at ON audit_log (at DESC);

CREATE TABLE settings (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL,
    secret INTEGER NOT NULL DEFAULT 0
);
