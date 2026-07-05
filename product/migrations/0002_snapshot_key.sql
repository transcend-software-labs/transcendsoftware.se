-- Workspace snapshots: the object-storage key of the tarred /workspace from a
-- project's last successful build. Reiterations restore it so the agent edits
-- the existing site instead of rebuilding from scratch.
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS snapshot_key text NOT NULL DEFAULT '';
