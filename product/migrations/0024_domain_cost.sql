-- Domain registration cost captured at buy time (GleSYS 1-year price, in öre).
-- The domain billing model changed from a flat recurring Stripe add-on to a
-- one-off pass-through charge: when a purchased domain goes active we add the
-- actual registration cost (clamped to the price cap) as a one-off item on the
-- customer's next invoice. This column holds the amount to bill. 0 = nothing to
-- bill (BYOD, or a legacy domain bought under the old add-on model).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_cost_ore INTEGER NOT NULL DEFAULT 0;
