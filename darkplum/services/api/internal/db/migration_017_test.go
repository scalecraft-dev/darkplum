package db

import (
	"context"
	"testing"
	"time"
)

// TestMigration017_CreatesVendorProviderSettings covers the happy path: the
// table exists post-migration, accepts a row for each real vendor, and
// rejects an unknown one -- the whole point of keying by vendor name instead
// of a UUID.
func TestMigration017_CreatesVendorProviderSettings(t *testing.T) {
	pool := setupSchemaUpTo(t, "016_external_api_residency.sql")
	ctx := context.Background()

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run remaining migrations (017+): %v", err)
	}

	for _, vendor := range []string{"anthropic", "openai"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at)
			VALUES ($1, $2, now())
		`, vendor, []string{"claude-*"}); err != nil {
			t.Fatalf("insert settings row for %s: %v", vendor, err)
		}
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at)
		VALUES ('bedrock', '{}', now())
	`); err == nil {
		t.Fatal("expected the vendor CHECK to reject a vendor other than anthropic/openai")
	}
}

// TestMigration017_InstallingUntilAndUpdatedByTriState covers the two
// nullable columns providers.go depends on: installing_until (open while an
// install/rotate hint window is live, NULL once cleared) and updated_by
// (nullable -- the schema seed/CLI path never sets an actor).
func TestMigration017_InstallingUntilAndUpdatedByTriState(t *testing.T) {
	pool := setupSchemaUpTo(t, "016_external_api_residency.sql")
	ctx := context.Background()

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run remaining migrations (017+): %v", err)
	}

	installingUntil := time.Now().Add(5 * time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, installing_until, updated_by)
		VALUES ('anthropic', $1, now(), $2, NULL)
	`, []string{"claude-*"}, installingUntil); err != nil {
		t.Fatalf("insert with installing_until and NULL updated_by: %v", err)
	}

	var gotInstallingUntil *time.Time
	var gotUpdatedBy *string
	if err := pool.QueryRow(ctx,
		`SELECT installing_until, updated_by FROM vendor_provider_settings WHERE vendor = 'anthropic'`,
	).Scan(&gotInstallingUntil, &gotUpdatedBy); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if gotInstallingUntil == nil {
		t.Fatal("expected installing_until to round-trip non-NULL")
	}
	if gotUpdatedBy != nil {
		t.Fatalf("expected updated_by to stay NULL, got %v", gotUpdatedBy)
	}

	// Clearing installing_until (as DELETE key does) must be a plain NULL
	// UPDATE -- no trigger/constraint should reject it.
	if _, err := pool.Exec(ctx,
		`UPDATE vendor_provider_settings SET installing_until = NULL WHERE vendor = 'anthropic'`,
	); err != nil {
		t.Fatalf("clear installing_until: %v", err)
	}
}

// TestMigration017_RemovingUntilTriState covers removing_until (added in
// review alongside installing_until, before this migration shipped): it
// round-trips non-NULL, is independently nullable, and a plain UPDATE can
// clear it (as PutKey does on a reinstall) without touching
// installing_until or any constraint objecting.
func TestMigration017_RemovingUntilTriState(t *testing.T) {
	pool := setupSchemaUpTo(t, "016_external_api_residency.sql")
	ctx := context.Background()

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run remaining migrations (017+): %v", err)
	}

	removingUntil := time.Now().Add(5 * time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, removing_until)
		VALUES ('anthropic', $1, now(), $2)
	`, []string{"claude-*"}, removingUntil); err != nil {
		t.Fatalf("insert with removing_until: %v", err)
	}

	var gotRemovingUntil, gotInstallingUntil *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT removing_until, installing_until FROM vendor_provider_settings WHERE vendor = 'anthropic'`,
	).Scan(&gotRemovingUntil, &gotInstallingUntil); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if gotRemovingUntil == nil {
		t.Fatal("expected removing_until to round-trip non-NULL")
	}
	if gotInstallingUntil != nil {
		t.Fatalf("expected installing_until to stay NULL, got %v", gotInstallingUntil)
	}

	// Clearing removing_until (as PutKey does on a reinstall) must be a plain
	// NULL UPDATE -- no trigger/constraint should reject it.
	if _, err := pool.Exec(ctx,
		`UPDATE vendor_provider_settings SET removing_until = NULL WHERE vendor = 'anthropic'`,
	); err != nil {
		t.Fatalf("clear removing_until: %v", err)
	}
}

// TestMigration017_Idempotent proves the migration file can be re-applied by
// hand (CREATE TABLE IF NOT EXISTS) against a schema that already ran it,
// matching migration 016's DROP CONSTRAINT IF EXISTS re-runnability pattern.
func TestMigration017_Idempotent(t *testing.T) {
	pool := setupSchemaUpTo(t, "017_vendor_provider_settings.sql")
	ctx := context.Background()

	data, err := migrationsFS.ReadFile("migrations/017_vendor_provider_settings.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply migration 017 by hand: %v", err)
	}
}

// TestMigration018_AddsRemovingUntilToAnOlderTable covers the database that ran
// the first revision of 017: the table exists without removing_until, 017 is
// already recorded, and only 018 can add the column.
func TestMigration018_AddsRemovingUntilToAnOlderTable(t *testing.T) {
	pool := setupSchemaUpTo(t, "016_external_api_residency.sql")
	ctx := context.Background()
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run remaining migrations: %v", err)
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE vendor_provider_settings DROP COLUMN removing_until`); err != nil {
		t.Fatalf("simulate the older table: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = '018_vendor_provider_removing_until.sql'`); err != nil {
		t.Fatalf("forget 018: %v", err)
	}
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var exists bool
	err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'vendor_provider_settings' AND column_name = 'removing_until')`).Scan(&exists)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !exists {
		t.Fatal("removing_until is still missing after 018 ran against the older table")
	}
}

// TestMigration019_AddsWorkspaceID covers the nullable workspace_id column:
// existing rows read NULL, a value round-trips, and the file is re-runnable.
func TestMigration019_AddsWorkspaceID(t *testing.T) {
	pool := setupSchemaUpTo(t, "018_vendor_provider_removing_until.sql")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at)
		VALUES ('anthropic', '{claude-*}', now())`); err != nil {
		t.Fatalf("seed pre-019 row: %v", err)
	}
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var ws *string
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&ws); err != nil {
		t.Fatalf("read workspace_id: %v", err)
	}
	if ws != nil {
		t.Fatalf("existing row must read NULL workspace_id, got %q", *ws)
	}
	if _, err := pool.Exec(ctx, `UPDATE vendor_provider_settings SET workspace_id = 'wrkspc_01' WHERE vendor = 'anthropic'`); err != nil {
		t.Fatalf("set workspace_id: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&ws); err != nil || ws == nil || *ws != "wrkspc_01" {
		t.Fatalf("workspace_id did not round-trip: %v %v", ws, err)
	}

	data, err := migrationsFS.ReadFile("migrations/019_vendor_provider_workspace_id.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply migration 019 by hand: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&ws); err != nil || ws == nil || *ws != "wrkspc_01" {
		t.Fatalf("re-apply must keep the value: %v %v", ws, err)
	}
}
