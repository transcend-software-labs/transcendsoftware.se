-- Custom domain attached to a project: either the customer's own domain (BYOD,
-- kind='byod') or one bought in-app via Cloudflare (kind='purchased'). Empty
-- domain_status = no domain. domain_records caches the DNS records we show the
-- customer (from the Fly cert requirements) so rendering the page needs no live
-- API call. The partial index feeds the reconcile poller: only in-flight domains.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_name        TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_status      TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_kind        TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_zone_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_ipv6        TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_sub_item_id TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_records     JSONB NOT NULL DEFAULT '[]';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_created_at  TIMESTAMPTZ;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_verified_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS projects_domain_status_idx ON projects (domain_status)
  WHERE domain_status IN ('registering', 'pending_dns', 'verifying');
