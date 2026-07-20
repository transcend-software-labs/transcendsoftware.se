-- Forge accounts authenticate only through magic links and OAuth. Drop the
-- password-era columns from databases that already applied the early schema.
ALTER TABLE users DROP COLUMN IF EXISTS password_hash;
ALTER TABLE users DROP COLUMN IF EXISTS verified;
