-- Payment state on a project. One canonical flag: a manual admin action sets it
-- today; a Stripe (or other) webhook will set the same flag later. Delivery is
-- gated on it — the preview stays free, the handover is what money unlocks.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS paid     BOOLEAN     NOT NULL DEFAULT false;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS paid_at  TIMESTAMPTZ;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS paid_via TEXT        NOT NULL DEFAULT '';

-- Already-delivered projects predate the payment concept; treat them as settled
-- so historical revenue reporting is consistent and the deliver gate never
-- retroactively flags a completed handover.
UPDATE projects SET paid = true, paid_at = updated_at, paid_via = 'legacy'
WHERE status = 'delivered' AND paid = false;
