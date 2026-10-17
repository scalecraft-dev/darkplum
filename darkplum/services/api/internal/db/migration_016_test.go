package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var migration016TestNameRe = regexp.MustCompile(`[^a-z0-9_]+`)

// setupSchemaUpTo creates a fresh, uniquely-named schema and applies every
// embedded migration up to and including upToVersion (e.g. "015_principal_
// policies.sql"), using the same schema_migrations bookkeeping RunMigrations
// itself writes -- so a later, unconstrained RunMigrations call against the
// same pool picks up exactly where this left off (016 onward), exercising
// the real upgrade path from a populated pre-016 database rather than a
// hand-rolled approximation of one.
func setupSchemaUpTo(t *testing.T, upToVersion string) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB integration test")
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	name := migration016TestNameRe.ReplaceAllString(strings.ToLower(t.Name()), "_")
	if len(name) > 40 {
		name = name[:40]
	}
	schema := "test_" + name + "_" + hex.EncodeToString(suffix)

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

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool scoped to schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := pool.Exec(dropCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, schema)); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
		pool.Close()
	})

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		t.Fatalf("bootstrap schema_migrations: %v", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version := entry.Name()

		data, err := migrationsFS.ReadFile("migrations/" + version)
		if err != nil {
			t.Fatalf("read migration file %s: %v", version, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin transaction for %s: %v", version, err)
		}
		if _, err := tx.Exec(ctx, string(data)); err != nil {
			t.Fatalf("exec migration %s: %v", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			t.Fatalf("record migration %s: %v", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit migration %s: %v", version, err)
		}

		if version == upToVersion {
			break
		}
	}

	return pool
}

// TestMigration016_ExistingAnyOrgSeededToCloudAccountOnly covers the common
// upgrade case: an org that never touched routing policy (residency NULL =
// "any", from migration 015's own seed) must come out of 016 at
// cloud_account_only, WITHOUT its updated_at moving -- this is a schema
// default change, not an admin action, and must not appear as one.
func TestMigration016_ExistingAnyOrgSeededToCloudAccountOnly(t *testing.T) {
	pool := setupSchemaUpTo(t, "015_principal_policies.sql")
	ctx := context.Background()

	var residencyBefore *string
	var updatedAtBefore time.Time
	if err := pool.QueryRow(ctx,
		`SELECT residency, updated_at FROM principal_routing_policies WHERE principal_type = 'org'`,
	).Scan(&residencyBefore, &updatedAtBefore); err != nil {
		t.Fatalf("read pre-016 org row: %v", err)
	}
	if residencyBefore != nil {
		t.Fatalf("expected a fresh 015 install's org residency to be NULL (any), got %v", residencyBefore)
	}

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run remaining migrations (016+): %v", err)
	}

	var residencyAfter *string
	var updatedAtAfter time.Time
	if err := pool.QueryRow(ctx,
		`SELECT residency, updated_at FROM principal_routing_policies WHERE principal_type = 'org'`,
	).Scan(&residencyAfter, &updatedAtAfter); err != nil {
		t.Fatalf("read post-016 org row: %v", err)
	}
	if residencyAfter == nil || *residencyAfter != "cloud_account_only" {
		t.Fatalf("expected org residency = cloud_account_only after 016, got %v", residencyAfter)
	}
	if !updatedAtAfter.Equal(updatedAtBefore) {
		t.Fatalf("expected updated_at to stay %v (schema seed, not an admin change), got %v", updatedAtBefore, updatedAtAfter)
	}

	// The legacy mirror must show 'any' for cloud_account_only -- an old
	// gateway reading routing_policy directly during a rolling deploy has no
	// concept of the third rung and must see the loosest value it does
	// understand, not an opaque string its own residency check would
	// misinterpret.
	var mirrored string
	if err := pool.QueryRow(ctx, `SELECT residency FROM routing_policy WHERE id = true`).Scan(&mirrored); err != nil {
		t.Fatalf("read legacy mirror: %v", err)
	}
	if mirrored != "any" {
		t.Fatalf("expected legacy routing_policy.residency = any for a cloud_account_only org, got %q", mirrored)
	}
}

// TestMigration016_ExistingSelfHostedOnlyOrgUntouched covers the other half
// of the seed's guard: an org already at the strictest rung must not be
// loosened to cloud_account_only by the 016 seed.
func TestMigration016_ExistingSelfHostedOnlyOrgUntouched(t *testing.T) {
	pool := setupSchemaUpTo(t, "015_principal_policies.sql")
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`UPDATE principal_routing_policies SET residency = 'self_hosted_only', updated_at = '2020-01-01T00:00:00Z' WHERE principal_type = 'org'`,
	); err != nil {
		t.Fatalf("set pre-016 org residency to self_hosted_only: %v", err)
	}

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run remaining migrations (016+): %v", err)
	}

	var residencyAfter *string
	var updatedAtAfter time.Time
	if err := pool.QueryRow(ctx,
		`SELECT residency, updated_at FROM principal_routing_policies WHERE principal_type = 'org'`,
	).Scan(&residencyAfter, &updatedAtAfter); err != nil {
		t.Fatalf("read post-016 org row: %v", err)
	}
	if residencyAfter == nil || *residencyAfter != "self_hosted_only" {
		t.Fatalf("expected self_hosted_only org to stay self_hosted_only, got %v", residencyAfter)
	}
	wantUpdatedAt, _ := time.Parse(time.RFC3339, "2020-01-01T00:00:00Z")
	if !updatedAtAfter.Equal(wantUpdatedAt) {
		t.Fatalf("expected the seed to leave a self_hosted_only org's updated_at untouched, got %v want %v", updatedAtAfter, wantUpdatedAt)
	}

	var mirrored string
	if err := pool.QueryRow(ctx, `SELECT residency FROM routing_policy WHERE id = true`).Scan(&mirrored); err != nil {
		t.Fatalf("read legacy mirror: %v", err)
	}
	if mirrored != "self_hosted_only" {
		t.Fatalf("expected legacy mirror to keep showing self_hosted_only, got %q", mirrored)
	}
}

// TestMigration016_OrgPutOfEveryLadderValueSucceeds proves the widened CHECK
// (and the rewritten trigger) actually accept every ladder value at the org
// row after 016 -- before this migration, a write of 'cloud_account_only'
// would have been rejected outright by the old two-value CHECK.
func TestMigration016_OrgPutOfEveryLadderValueSucceeds(t *testing.T) {
	pool := setupSchemaUpTo(t, "015_principal_policies.sql")
	ctx := context.Background()

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run remaining migrations (016+): %v", err)
	}

	for _, residency := range []*string{nil, strPtr016("cloud_account_only"), strPtr016("self_hosted_only")} {
		if _, err := pool.Exec(ctx,
			`UPDATE principal_routing_policies SET residency = $1 WHERE principal_type = 'org'`, residency,
		); err != nil {
			t.Fatalf("update org residency to %v: %v (trigger/CHECK should accept every ladder value post-016)", residency, err)
		}
	}
}

func strPtr016(s string) *string { return &s }
