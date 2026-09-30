-- Database migrations between connections, and what each run did.
CREATE TABLE migrations (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source_id    TEXT REFERENCES connections (id) ON DELETE SET NULL,
    source_label TEXT NOT NULL,
    target_id    TEXT REFERENCES connections (id) ON DELETE SET NULL,
    target_label TEXT NOT NULL,
    status       TEXT NOT NULL,             -- running, done, failed, cancelled, interrupted
    tables       INTEGER NOT NULL DEFAULT 0,
    rows_copied  INTEGER NOT NULL DEFAULT 0,
    report       TEXT NOT NULL DEFAULT '{}', -- final progress (JSON)
    plan         TEXT NOT NULL DEFAULT '{}', -- the plan as it ran (JSON)
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX migrations_user ON migrations (user_id, started_at DESC);
