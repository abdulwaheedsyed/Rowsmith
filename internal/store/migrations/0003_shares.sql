-- Shared query links, their discussion, and notifications.
CREATE TABLE query_shares (
    id             TEXT PRIMARY KEY,
    author_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id  TEXT REFERENCES connections (id) ON DELETE SET NULL,
    connection     TEXT NOT NULL DEFAULT '',  -- name at share time, kept if the connection goes
    driver         TEXT NOT NULL DEFAULT '',
    database_name  TEXT NOT NULL DEFAULT '',
    schema_name    TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL,              -- frozen: line comments point into it
    audience       TEXT NOT NULL DEFAULT 'team' CHECK (audience IN ('team', 'people')),
    result         TEXT NOT NULL DEFAULT '',   -- sealed snapshot of the result, '' for none
    result_rows    INTEGER NOT NULL DEFAULT 0,
    result_at      INTEGER NOT NULL DEFAULT 0,
    expires_at     INTEGER NOT NULL DEFAULT 0, -- 0 = never
    last_activity  INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE INDEX query_shares_author ON query_shares (author_id, last_activity DESC);
CREATE INDEX query_shares_activity ON query_shares (last_activity DESC);

CREATE TABLE query_share_people (
    share_id TEXT NOT NULL REFERENCES query_shares (id) ON DELETE CASCADE,
    user_id  TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (share_id, user_id)
);
CREATE INDEX query_share_people_user ON query_share_people (user_id);

CREATE TABLE share_comments (
    id          TEXT PRIMARY KEY,
    share_id    TEXT NOT NULL REFERENCES query_shares (id) ON DELETE CASCADE,
    author_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id   TEXT NOT NULL DEFAULT '',     -- '' for a thread's first comment
    line        INTEGER NOT NULL DEFAULT 0,   -- SQL line the thread is about; 0 = the whole query
    body        TEXT NOT NULL,
    resolved_at INTEGER NOT NULL DEFAULT 0,
    resolved_by TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    edited_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX share_comments_share ON share_comments (share_id, created_at);

CREATE TABLE notifications (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,                -- shared | comment | reply | mention
    actor_id   TEXT NOT NULL DEFAULT '',
    share_id   TEXT NOT NULL DEFAULT '',
    comment_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    read_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX notifications_user ON notifications (user_id, created_at DESC);
