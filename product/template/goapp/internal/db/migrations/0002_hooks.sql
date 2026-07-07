-- Data hooks: notify the owner (email, later Slack/webhook) when rows land in
-- any table. Capture is generic and lossless — enabling a hook creates an
-- AFTER INSERT trigger on the target table that enqueues the new row's rowid
-- into _outbox; a background dispatcher polls _outbox and fires the hooks.
-- Both tables are `_`-prefixed, so the site admin hides them from the data grid.

CREATE TABLE IF NOT EXISTS _hooks (
    id          TEXT PRIMARY KEY,
    table_name  TEXT NOT NULL,
    type        TEXT NOT NULL,            -- 'email' (v1); 'slack'/'webhook' later
    target      TEXT NOT NULL,            -- email address / webhook URL
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_status TEXT NOT NULL DEFAULT '', -- 'ok' or 'error: …' from the last dispatch
    last_at     INTEGER,
    created_at  INTEGER NOT NULL,
    UNIQUE (table_name, type, target)
);

CREATE TABLE IF NOT EXISTS _outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    table_name   TEXT NOT NULL,
    row_id       INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    processed_at INTEGER
);

CREATE INDEX IF NOT EXISTS _outbox_unprocessed ON _outbox (id) WHERE processed_at IS NULL;
