// Package testutil provides shared test infrastructure.
//
// The philosophy here is deliberate: integration tests run against a REAL
// Postgres, not a fake. A fake repository proves the fake works. It cannot prove
// the SQL is valid, that a CHECK constraint fires, that a foreign key ordering is
// right, or that FOR UPDATE actually serialises — and every one of those has
// already produced a real bug in this codebase.
//
// Cross-module contracts ARE faked, because those are interfaces we own and the
// point of a checkout test is to test checkout, not catalog.
package testutil

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
)

// DB connects to the test database using the same config loader the app uses.
//
// Skips rather than fails when no database is reachable: `go test ./...` on a
// laptop without Docker running should not be a wall of red. CI always has one,
// so coverage is not silently lost.
func DB(t *testing.T) *database.DB {
	t.Helper()

	loadEnv()
	if os.Getenv("POSTGRES_DSN") == "" {
		t.Skip("POSTGRES_DSN not set; skipping integration test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := database.Connect(ctx, cfg.Postgres)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// Cache returns an in-memory cache.
//
// Real Redis is not needed for most tests: the Memory implementation satisfies
// the same interface, and using it keeps tests fast and independent of another
// container. Tests that specifically exercise Redis semantics should connect
// properly instead.
func Cache(t *testing.T) cache.Cache {
	t.Helper()
	return cache.NewMemory()
}

// loadEnv walks up looking for .env, so a test works regardless of which
// directory `go test` was invoked from.
func loadEnv() {
	for _, path := range []string{
		".env", "../.env", "../../.env", "../../../.env",
		"../../../../.env", "../../../../../.env",
	} {
		if _, err := os.Stat(path); err == nil {
			_ = godotenv.Load(path)
			return
		}
	}
}

// ErrRollback aborts a test transaction so nothing is persisted.
//
// Wrapping each test's work in db.InTx and returning this at the end gives
// automatic cleanup with no truncation, no fixture teardown, and no
// cross-test pollution — and tests can run repeatedly against a database that
// already has seed data in it.
//
// The limitation worth knowing: it cannot be used for tests that need real
// CONCURRENT transactions, since those must genuinely commit to see each other.
// Those tests clean up explicitly instead.
var ErrRollback = rollbackErr{}

type rollbackErr struct{}

func (rollbackErr) Error() string { return "testutil: intentional rollback" }

// InRollbackTx runs fn inside a transaction that is always rolled back.
func InRollbackTx(t *testing.T, db *database.DB, fn func(ctx context.Context) error) {
	t.Helper()

	err := db.InTx(context.Background(), func(ctx context.Context) error {
		if err := fn(ctx); err != nil {
			return err
		}
		return ErrRollback
	})
	if err != nil && !errors.Is(err, ErrRollback) {
		t.Fatalf("transaction failed: %v", err)
	}
}
