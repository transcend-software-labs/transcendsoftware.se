-- Preview screenshots: after each successful build the sandbox captures a
-- screenshot of the deployed site (Playwright/Chromium) and uploads it; the key
-- lets /admin show Rasmus the actual site during his review.
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS screenshot_key text NOT NULL DEFAULT '';
