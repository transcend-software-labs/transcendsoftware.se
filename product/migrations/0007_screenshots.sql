-- Multiple screenshots per project — one per page of the deployed site — for a
-- full visual review. JSON array of {path, key}. Supersedes the single
-- screenshot_key column (left in place, unused, for compatibility).
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS screenshots text NOT NULL DEFAULT '[]';
