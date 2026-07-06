-- Passwordless ("magic link") login tokens. Single-use, stored hashed; the
-- email in the link carries the raw token.
CREATE TABLE IF NOT EXISTS login_tokens (
    token_hash text PRIMARY KEY,
    email      text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS login_tokens_expires_at_idx ON login_tokens (expires_at);
