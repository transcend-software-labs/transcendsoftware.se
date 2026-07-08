-- Impeccable design-audit findings from the last build — a JSON array of
-- {antipattern, name, description, severity, file, line, snippet} — surfaced in
-- /admin next to the screenshots as a design-review checklist for Rasmus.
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS findings text NOT NULL DEFAULT '[]';
