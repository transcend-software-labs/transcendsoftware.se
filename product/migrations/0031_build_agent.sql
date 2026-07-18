-- Per-project build-agent choice: which coding agent executes sandbox builds.
-- "" = opencode (the default); "grok" = xAI Grok Build CLI, headless.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS build_agent TEXT NOT NULL DEFAULT '';
