-- Marks a project whose customer added or changed content (logo, photos, copy,
-- team) after a build already ran, so the UI can offer a rebuild that applies
-- it. Cleared when the next build starts.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS content_pending BOOLEAN NOT NULL DEFAULT false;
