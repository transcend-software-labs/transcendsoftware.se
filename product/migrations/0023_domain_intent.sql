-- Domain chosen at checkout (Phase B). The customer picks a domain on the
-- subscribe page before paying; the choice is captured here and provisioned
-- automatically once the subscription settles (BYOD attach, or buy + register).
-- Cleared once acted on. Empty domain_intent = no domain bundled.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_intent     TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_intent_buy BOOLEAN NOT NULL DEFAULT false;
