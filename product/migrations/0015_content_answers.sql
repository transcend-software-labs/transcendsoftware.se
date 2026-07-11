-- Text the customer types for text-kind content slots (a contact email, the
-- About copy, opening hours) — the counterpart to uploaded files. slug → value.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS content_answers JSONB NOT NULL DEFAULT '{}';
