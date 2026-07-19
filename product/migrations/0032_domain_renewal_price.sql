-- The renewal price, captured at buy time alongside the (often discounted)
-- first-year price. Renewals bill this amount; rows from before this column
-- fall back to domain_cost_ore.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_renewal_ore INTEGER NOT NULL DEFAULT 0;
