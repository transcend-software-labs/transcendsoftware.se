-- Per-build model token usage, for cost visibility in /admin.
ALTER TABLE iterations
    ADD COLUMN IF NOT EXISTS tokens integer NOT NULL DEFAULT 0;
