-- Pre-launch cleanup: drop columns whose features were removed.
-- repo_url: GitHub source mirroring (deleted in eac644a) — always "".
-- domain_sub_item_id: the flat-monthly domain add-on model, replaced by the
-- one-off next-invoice item (2026-07-14); no live subscriptions carry it.
ALTER TABLE projects DROP COLUMN IF EXISTS repo_url;
ALTER TABLE projects DROP COLUMN IF EXISTS domain_sub_item_id;
