-- Scheduled queries: the placeholder tables from 0001 were never used.
DROP TABLE IF EXISTS schedule_runs;
DROP TABLE IF EXISTS schedules;

CREATE TABLE schedules (
    id            TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    database_name TEXT NOT NULL DEFAULT '',
    schema_name   TEXT NOT NULL DEFAULT '',
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    body          TEXT NOT NULL,
    cron          TEXT NOT NULL,
    timezone      TEXT NOT NULL DEFAULT 'UTC',
    config        TEXT NOT NULL DEFAULT '{}', -- output, alert and delivery (JSON)
    webhook       TEXT NOT NULL DEFAULT '',   -- sealed URL: chat webhooks embed their credentials
    enabled       INTEGER NOT NULL DEFAULT 1,
    paused_reason TEXT NOT NULL DEFAULT '',
    next_run_at   INTEGER NOT NULL DEFAULT 0,
    running_since INTEGER NOT NULL DEFAULT 0,
    last_run_at   INTEGER NOT NULL DEFAULT 0,
    last_status   TEXT NOT NULL DEFAULT '',
    last_hash     TEXT NOT NULL DEFAULT '',   -- result fingerprint, for "when the result changes"
    last_alert    INTEGER NOT NULL DEFAULT 0, -- the condition held on the last run
    failures      INTEGER NOT NULL DEFAULT 0, -- consecutive
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX schedules_due ON schedules (enabled, next_run_at);
CREATE INDEX schedules_owner ON schedules (owner_id);

CREATE TABLE schedule_runs (
    id           TEXT PRIMARY KEY,
    schedule_id  TEXT NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    trigger      TEXT NOT NULL,              -- schedule | manual
    triggered_by TEXT NOT NULL DEFAULT '',   -- user who pressed "Run now"
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL,              -- running | ok | alert | quiet | failed
    row_count    INTEGER NOT NULL DEFAULT 0,
    truncated    INTEGER NOT NULL DEFAULT 0,
    observed     TEXT NOT NULL DEFAULT '',   -- the value the alert looked at
    file_name    TEXT NOT NULL DEFAULT '',
    file_type    TEXT NOT NULL DEFAULT '',
    file_size    INTEGER NOT NULL DEFAULT 0,
    file_key     TEXT NOT NULL DEFAULT '',   -- sealed per-file key; '' once the file is gone
    file_expires INTEGER NOT NULL DEFAULT 0,
    delivery     TEXT NOT NULL DEFAULT '{}', -- who was sent what (JSON)
    error        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX schedule_runs_schedule ON schedule_runs (schedule_id, started_at DESC);
CREATE INDEX schedule_runs_files ON schedule_runs (file_expires) WHERE file_key <> '';
