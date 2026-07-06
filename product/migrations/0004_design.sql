-- Per-project design: the intake step suggests visual directions (JSON array
-- of {name, description}); the customer picks one or states their own, stored
-- as design_brief and fed to the planner + build agent.
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS design_options text NOT NULL DEFAULT '[]';
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS design_brief text NOT NULL DEFAULT '';
