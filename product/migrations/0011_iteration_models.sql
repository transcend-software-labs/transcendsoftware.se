-- Record which models produced each build (set at build start), so model
-- experiments (make model-grok / model-minimax / ...) can be analyzed per
-- build afterwards: time, tokens and quality per model combination.
ALTER TABLE iterations
    ADD COLUMN IF NOT EXISTS impl_model text NOT NULL DEFAULT '';
ALTER TABLE iterations
    ADD COLUMN IF NOT EXISTS planner_model text NOT NULL DEFAULT '';
