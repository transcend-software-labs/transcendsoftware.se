-- Stripe subscription identifiers on a project (one subscription per project).
-- Set when the customer's Checkout completes; the webhook matches subscription
-- lifecycle events back to the project through them. Empty = never subscribed.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS stripe_sub_id      TEXT NOT NULL DEFAULT '';
