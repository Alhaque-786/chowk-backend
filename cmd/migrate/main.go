// Command migrate applies SQL migrations.
//
// This is hand-rolled rather than golang-migrate on purpose: it is ~120 lines,
// has no dependencies, and its behaviour is fully visible. The three properties
// that actually matter for a migration runner are all here:
//
//  1. Each migration runs inside a transaction, so a failure leaves no partial
//     schema.
//  2. An advisory lock serialises concurrent runners, so two pods booting at once
//     cannot apply the same migration twice.
//  3. Applied files are checksummed, so editing an already-applied migration is
//     caught instead of silently diverging between environments.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

// lockID is an arbitrary but fixed key for the advisory lock. Any concurrent
// migrator uses the same number and therefore queues behind the first.
const lockID int64 = 8_472_119_003

func main() {
	_ = godotenv.Load()

	dir := flag.String("dir", "migrations", "directory containing .sql migrations")
	status := flag.Bool("status", false, "print applied/pending migrations and exit")
	flag.Parse()

	if err := run(*dir, *status); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(dir string, statusOnly bool) error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return errors.New("POSTGRES_DSN is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     TEXT PRIMARY KEY,
			checksum    TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			duration_ms BIGINT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("scan %s: %w", dir, err)
	}
	// Lexical sort, which is why files are named with a zero-padded numeric
	// prefix: 0002 must run before 0010, and "2" before "10" would not.
	sort.Strings(files)

	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return err
	}

	if statusOnly {
		return printStatus(files, applied)
	}

	// Serialise runners. Every replica calling this blocks here until the first
	// one finishes, so a rolling deploy applies each migration exactly once.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)
	}()

	// Re-read after locking: another runner may have applied migrations while we
	// were waiting for the lock.
	if applied, err = loadApplied(ctx, conn); err != nil {
		return err
	}

	pending := 0
	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".sql")

		body, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])

		if prev, ok := applied[version]; ok {
			// A changed checksum means an applied migration was edited. Refusing
			// here is the point: the edit is already live elsewhere, so the two
			// environments have silently diverged.
			if prev != checksum {
				return fmt.Errorf(
					"migration %s was modified after being applied (recorded %s, file %s)\n"+
						"  applied migrations are immutable -- add a new migration instead",
					version, prev[:12], checksum[:12])
			}
			continue
		}

		fmt.Printf("applying %s ... ", version)
		start := time.Now()

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", version, err)
		}

		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			fmt.Println("FAILED")
			return fmt.Errorf("apply %s: %w", version, err)
		}

		elapsed := time.Since(start)
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, checksum, duration_ms) VALUES ($1, $2, $3)`,
			version, checksum, elapsed.Milliseconds()); err != nil {
			_ = tx.Rollback(ctx)
			fmt.Println("FAILED")
			return fmt.Errorf("record %s: %w", version, err)
		}

		if err := tx.Commit(ctx); err != nil {
			fmt.Println("FAILED")
			return fmt.Errorf("commit %s: %w", version, err)
		}

		fmt.Printf("ok (%dms)\n", elapsed.Milliseconds())
		pending++
	}

	if pending == 0 {
		fmt.Println("schema is up to date")
	} else {
		fmt.Printf("applied %d migration(s)\n", pending)
	}
	return nil
}

func loadApplied(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("load applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]string)
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, err
		}
		applied[version] = checksum
	}
	return applied, rows.Err()
}

func printStatus(files []string, applied map[string]string) error {
	fmt.Printf("%-40s %s\n", "MIGRATION", "STATUS")
	for _, file := range files {
		version := strings.TrimSuffix(filepath.Base(file), ".sql")
		state := "pending"
		if _, ok := applied[version]; ok {
			state = "applied"
		}
		fmt.Printf("%-40s %s\n", version, state)
	}
	return nil
}
