-- Richer content collection: structured people (roster-kind slots), and the
-- state for AI image generation (candidate sets awaiting the customer's pick,
-- and a flag marking assets the AI generated vs the customer uploaded).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS content_rosters JSONB NOT NULL DEFAULT '{}';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS pending_images  JSONB NOT NULL DEFAULT '{}';
ALTER TABLE assets   ADD COLUMN IF NOT EXISTS generated BOOLEAN NOT NULL DEFAULT false;
