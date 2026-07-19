-- The original users.email UNIQUE is case-sensitive, but every lookup matches
-- on lower(email). That gap let "Victim@x.com" coexist with "victim@x.com" —
-- a duplicate-account / admin-impersonation vector (isAdmin compared emails).
-- A unique index on lower(email) closes it at the database, independent of any
-- application-side normalisation. Pre-launch the table is effectively empty, so
-- this cannot fail on existing case-variant rows.
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_key ON users (lower(email));
