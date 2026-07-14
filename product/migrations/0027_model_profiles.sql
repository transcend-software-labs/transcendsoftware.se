-- Per-build model selection: the operator picks a planner + implementation
-- model per build from /admin to compare quality/cost. planner_profile /
-- impl_profile hold the chosen model-profile keys (empty = the configured
-- default combo). tokens_input records the input-token subset of a build's
-- total token count, so the /admin cost estimate can price input and
-- output+reasoning at their (very different) rates.
ALTER TABLE projects   ADD COLUMN IF NOT EXISTS planner_profile TEXT NOT NULL DEFAULT '';
ALTER TABLE projects   ADD COLUMN IF NOT EXISTS impl_profile    TEXT NOT NULL DEFAULT '';
ALTER TABLE iterations ADD COLUMN IF NOT EXISTS tokens_input    INTEGER NOT NULL DEFAULT 0;
