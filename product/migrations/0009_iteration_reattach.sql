-- Re-attach to a running build after an orchestrator restart instead of killing
-- it. The agent runs server-side in the sandbox (opencode async session), so it
-- survives the orchestrator process dying — the orchestrator just needs the
-- session id + sandbox address, alongside the already-persisted machine_id, to
-- reconnect its event stream and finish the build.
ALTER TABLE iterations
    ADD COLUMN IF NOT EXISTS session_id   TEXT NOT NULL DEFAULT '';
ALTER TABLE iterations
    ADD COLUMN IF NOT EXISTS sandbox_addr TEXT NOT NULL DEFAULT '';
