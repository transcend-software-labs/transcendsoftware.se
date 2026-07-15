-- Bundle-a-domain upfront charge: when a customer buys a domain during the
-- subscription checkout, its 1-year cost is charged on the FIRST invoice (a
-- one-time Checkout line item), not added to the next invoice on activation.
-- domain_prepaid marks such domains so activation skips that (now duplicate)
-- invoice item. The yearly renewal still bills normally.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS domain_prepaid BOOLEAN NOT NULL DEFAULT false;
