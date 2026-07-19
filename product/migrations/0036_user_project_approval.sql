-- New customers may submit one first brief, but no AI work starts until the
-- operator approves their account. Existing accounts are grandfathered so a
-- deploy cannot unexpectedly lock out current customers or the operator.
ALTER TABLE users ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;

UPDATE users
SET approved_at = created_at
WHERE approved_at IS NULL;

-- Keep the approval queue bounded even if concurrent requests reach different
-- app instances. A rejected/deleted project frees the account to submit again.
CREATE UNIQUE INDEX IF NOT EXISTS projects_one_pending_access_per_user_idx
    ON projects (user_id)
    WHERE status = 'pending_access_approval';
