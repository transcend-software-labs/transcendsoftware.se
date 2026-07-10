-- The machine-readable plan (pages, scope, content needs) drives the customer
-- UI's scope card, page checklist and content slots. And each uploaded asset
-- can fill a named content slot from that plan.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS plan_spec JSONB NOT NULL DEFAULT '{}';
ALTER TABLE assets   ADD COLUMN IF NOT EXISTS slot TEXT NOT NULL DEFAULT '';

-- The customer's UI language at project creation, so their emails (plan ready,
-- preview ready) go out in the same language as the site they're getting.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS locale TEXT NOT NULL DEFAULT 'en';
