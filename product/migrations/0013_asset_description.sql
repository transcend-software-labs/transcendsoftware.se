-- The customer tells us what each uploaded file is ("our logo", "photo of the
-- shop front") so the plan and build agents know where it belongs instead of
-- guessing from filenames.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
