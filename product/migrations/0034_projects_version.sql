-- Whole-row project updates must not silently overwrite a concurrent writer.
-- Every update compares the revision it read and increments it atomically.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;
