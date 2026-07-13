// Package pgtest provides a real Postgres for integration tests. Prefer this
// over the in-memory store whenever a test's value depends on the actual SQL
// (bindings, migrations, JSON round-trips) — the memory store silently accepts
// what Postgres would reject (a placeholder/arg mismatch once shipped that way).
//
// Start spins up a throwaway postgres:16 via testcontainers, so `go test` just
// works wherever Docker is available. Set PG_TEST_URL to point at an existing
// database instead (CI provisions one as a service; devs can reuse a local one)
// — then no container is started. With neither Docker nor PG_TEST_URL, the
// calling test is skipped rather than failed.
package pgtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Start returns a DSN for a ready Postgres and registers cleanup on t. It skips
// the test when no database is reachable (no PG_TEST_URL and no Docker).
func Start(t *testing.T) string {
	t.Helper()

	if dsn := os.Getenv("PG_TEST_URL"); dsn != "" {
		return dsn // reuse an existing DB (CI service / local) — no container
	}

	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("forge"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		// No Docker on this machine — skip rather than fail the whole suite.
		t.Skipf("pgtest: no Postgres available (set PG_TEST_URL or run Docker): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = container.Terminate(ctx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("pgtest: connection string: %v", err)
	}
	return dsn
}
