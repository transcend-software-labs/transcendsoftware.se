-- The design critic's verdict on the deployed preview (a vision model reviews
-- the page screenshots): "SHIP", or "POLISH" + the visual issues it found.
-- Shown in /admin next to the screenshots and findings.
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS critique text NOT NULL DEFAULT '';
