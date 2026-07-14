-- Domain renewal billing. A purchased domain auto-renews yearly at GleSYS (which
-- charges us); we detect the renewal (the registry expiry advancing) and pass
-- the cost through to the customer's next invoice, same as the initial
-- registration. domain_paid_through is the expiry the customer has paid through:
-- set at activation, advanced each time we bill a renewal. NULL = not tracked
-- (BYOD, or a domain from before this feature — anchored on first observation).
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_paid_through TIMESTAMPTZ;
