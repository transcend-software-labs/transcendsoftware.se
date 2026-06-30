-- Initial schema for Transcend Forge.
-- Apply with: psql "$DATABASE_URL" -f migrations/0001_init.sql

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS projects (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    brief           TEXT NOT NULL,
    status          TEXT NOT NULL,
    questions       TEXT NOT NULL DEFAULT '[]',  -- JSON array of clarifying questions
    answers         TEXT NOT NULL DEFAULT '',
    plan            TEXT NOT NULL DEFAULT '',
    verdict         TEXT NOT NULL DEFAULT '',
    reject_reason   TEXT NOT NULL DEFAULT '',
    preview_url     TEXT NOT NULL DEFAULT '',
    repo_url        TEXT NOT NULL DEFAULT '',
    iterations_used INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS projects_user_id_idx ON projects (user_id);

CREATE TABLE IF NOT EXISTS iterations (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    number      INTEGER NOT NULL,
    prompt      TEXT NOT NULL DEFAULT '',
    preview_url TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    log          TEXT NOT NULL DEFAULT '',
    machine_id   TEXT NOT NULL DEFAULT '',     -- Fly Machine running this build (for reaping)
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS iterations_project_id_idx ON iterations (project_id);
-- Find in-flight builds quickly (active-builds view + startup recovery).
CREATE INDEX IF NOT EXISTS iterations_building_idx ON iterations (status) WHERE status = 'building';

CREATE TABLE IF NOT EXISTS assets (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    object_key   TEXT NOT NULL,
    filename     TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    size         BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS assets_project_id_idx ON assets (project_id);
