package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/db"
)

// captureLogs swaps the global slog default logger for one that writes to a
// buffer, restoring the previous default via t.Cleanup, so tests can assert
// on a specific WARNING/ERROR being logged (e.g. SyncModels' alias-shadow
// warning) without a dedicated logger being threaded through the handler.
// Not safe to use from tests that run in parallel with each other (none in
// this package do — see testdb_test.go's setupTestDB doc comment, which only
// claims parallel-safety with respect to the database).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

var nonSchemaChar = regexp.MustCompile(`[^a-z0-9_]+`)

// setupTestDB skips the calling test unless TEST_DATABASE_URL is set. When
// set, it creates a fresh, uniquely-named Postgres schema, points a pool's
// search_path at it, and runs every migration into that schema. The schema is
// dropped and the pool closed via t.Cleanup, so tests are isolated from each
// other and from any pre-existing data, and are safe to run with t.Parallel()
// and against a shared database.
func setupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB integration test")
	}

	schema := testSchemaName(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA "%s"`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	// Four connections, the pool's floor and what a two-core CI runner gets:
	// a handler that holds a connection while asking the pool for another
	// deadlocks here exactly as it would there, instead of being hidden by
	// a developer machine's larger pool.
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool scoped to schema %s: %v", schema, err)
	}

	if err := db.RunMigrations(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("run migrations into schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := pool.Exec(dropCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, schema)); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
		pool.Close()
	})

	return pool
}

// testSchemaName derives a Postgres-identifier-safe, unique-per-invocation
// schema name from the test name plus random bytes (uniqueness across
// parallel runs and reruns; the test name alone is not enough since
// t.Cleanup happens after, not before, a would-be collision).
func testSchemaName(t *testing.T) string {
	t.Helper()

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	suffixHex := hex.EncodeToString(suffix)

	name := nonSchemaChar.ReplaceAllString(strings.ToLower(t.Name()), "_")
	const prefix = "test_"
	// Postgres identifiers are limited to 63 bytes; reserve room for the
	// prefix and the random suffix so the result is always unique even after
	// truncating a long test name.
	maxNameLen := 63 - len(prefix) - len(suffixHex) - 1
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	return prefix + name + "_" + suffixHex
}
