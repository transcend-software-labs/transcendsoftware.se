// Package db opens the SQLite database and applies embedded migrations.
//
// SQLite is deliberate: these apps run as one binary on one machine (deploy
// with --ha=false), so a file database on the machine — later a Fly volume at
// /data — is the simplest correct persistence. Two machines would mean two
// diverging databases.
package db

import (
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"sort"

	"os"

	_ "modernc.org/sqlite" // pure-Go driver; CGO_ENABLED=0 builds work
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens (creating if needed) the app database inside dataDir.
func Open(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dataDir, "app.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite has a single writer; one connection avoids SQLITE_BUSY entirely.
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

// Migrate applies embedded migrations in filename order, tracking applied
// versions in schema_migrations. Add a feature by dropping a new numbered
// .sql file into migrations/ — never edit an applied one.
func Migrate(database *sql.DB) error {
	if _, err := database.Exec(
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		if err := database.QueryRow(
			`SELECT count(*) FROM schema_migrations WHERE version = ?`, name).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := database.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
