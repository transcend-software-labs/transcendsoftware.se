-- Forge Pro change model: a paying subscriber's monthly change allowance.
-- changes_this_period counts changes used in the window starting at
-- change_period_start (advances in whole months); overage past the configured
-- allowance is billed as a flat Stripe invoice item. delivered_at is set on the
-- first delivery and stays set — so a later self-serve change goes live when the
-- customer accepts, without routing back through operator review.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS changes_this_period INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS change_period_start  TIMESTAMPTZ;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS delivered_at         TIMESTAMPTZ;
