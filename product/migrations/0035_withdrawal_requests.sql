-- Durable record for the statutory online withdrawal function. Kept separate
-- from users/projects so the legal audit record survives account erasure.
CREATE TABLE IF NOT EXISTS withdrawal_requests (
    id         TEXT PRIMARY KEY,
    email      TEXT NOT NULL,
    project_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS withdrawal_requests_created_at_idx
    ON withdrawal_requests (created_at DESC);
