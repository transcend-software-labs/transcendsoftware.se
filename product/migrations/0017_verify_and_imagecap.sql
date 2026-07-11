-- Email verification for password signups, and a per-project counter to cap AI
-- image generations (each is a real paid API call).
ALTER TABLE users    ADD COLUMN IF NOT EXISTS verified BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS image_gen_count INTEGER NOT NULL DEFAULT 0;

-- Existing accounts predate verification; grandfather them in so we don't lock
-- out current users. New signups start unverified.
UPDATE users SET verified = true WHERE verified = false;
