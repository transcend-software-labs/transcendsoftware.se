-- One-shot post-payment code review + its selectable model.
-- review_profile: model-profile key for the review step ("" = Forge default,
-- same registry as the planner/impl pickers). code_review: the review report
-- (first line SHIP or FIX, then the findings). code_review_at: when it ran —
-- the one-shot guard (NULL/zero = not yet).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS review_profile TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS code_review TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS code_review_at TIMESTAMPTZ;
