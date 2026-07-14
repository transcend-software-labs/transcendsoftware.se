-- Branded preview subdomain label ("bageriet-a1fa81"): previews are served as
-- <preview_host>.<PREVIEW_DOMAIN> through a reverse proxy in the Forge app, so
-- customers never see the internal fly.dev URLs. Assigned once when the first
-- build finishes (slug of the project name + short id), stable thereafter.
-- '' = never assigned (feature off, or no successful build yet).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS preview_host TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS projects_preview_host_idx ON projects (preview_host) WHERE preview_host <> '';
