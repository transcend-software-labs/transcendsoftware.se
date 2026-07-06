// Package migrations embeds the SQL schema migrations so the server applies
// them itself at startup (store.NewPostgres) — no manual psql step, no
// deploy-ordering footguns.
//
// Rules for migration files:
//   - Numbered, never edited after they have shipped; add a new file instead.
//   - Idempotent (IF NOT EXISTS etc.). The runner tracks applied versions in
//     schema_migrations, but idempotency also makes backfilling safe on
//     databases that were originally migrated manually via psql.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
