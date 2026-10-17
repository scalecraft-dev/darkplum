package policy

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setupPolicyDB gives the test its own schema holding the minimal set of
// tables loadFromDB needs (models, model_alias_routes, model_aliases,
// routing_policy, api_keys) — the migration-017 table
// (vendor_provider_settings) is deliberately NOT created here; tests that
// need it call withVendorProviderSettingsTable. Gated on TEST_DATABASE_URL,
// mirroring internal/recorder's setupRecorderDB.
func setupPolicyDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	schema := fmt.Sprintf("policy_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with schema: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `
		CREATE TABLE models (
			model_id TEXT PRIMARY KEY,
			enabled  BOOLEAN NOT NULL DEFAULT true
		);
		CREATE TABLE model_alias_routes (
			alias           TEXT,
			route_name      TEXT,
			description     TEXT,
			target_model_id TEXT,
			keywords        TEXT[],
			position        INT
		);
		CREATE TABLE model_aliases (
			alias           TEXT PRIMARY KEY,
			kind            TEXT,
			target_model_id TEXT,
			default_route   TEXT,
			classifier_kind TEXT
		);
		CREATE TABLE routing_policy (
			id        BOOLEAN PRIMARY KEY DEFAULT true,
			residency TEXT NOT NULL DEFAULT 'any'
		);
		INSERT INTO routing_policy (id, residency) VALUES (true, 'any');
		CREATE TABLE api_keys (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id    UUID,
			revoked_at TIMESTAMPTZ
		);
	`); err != nil {
		t.Fatalf("create fixture tables: %v", err)
	}
	return pool
}

// withVendorProviderSettingsTable brings a setupPolicyDB pool up to the
// migration-017 shape.
func withVendorProviderSettingsTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE vendor_provider_settings (
			vendor           TEXT PRIMARY KEY CHECK (vendor IN ('anthropic', 'openai')),
			model_filter     TEXT[] NOT NULL,
			filter_set_at    TIMESTAMPTZ NOT NULL,
			installing_until TIMESTAMPTZ,
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_by       TEXT
		)
	`); err != nil {
		t.Fatalf("create vendor_provider_settings: %v", err)
	}
}

// TestPgStore_LoadFromDB_VendorProviderSettingsMissingTableToleratesEmpty is
// the 42P01-tolerance regression guard: a customer database that hasn't run
// migration 017 yet must load successfully, with VendorFilters simply empty
// — never a failed load, and never a change to any other field's behaviour.
func TestPgStore_LoadFromDB_VendorProviderSettingsMissingTableToleratesEmpty(t *testing.T) {
	pool := setupPolicyDB(t)
	s := &PgStore{pool: pool}

	snap, err := s.loadFromDB(context.Background())
	if err != nil {
		t.Fatalf("expected a missing vendor_provider_settings table to be tolerated, got error: %v", err)
	}
	if len(snap.VendorFilters) != 0 {
		t.Errorf("expected empty VendorFilters, got %+v", snap.VendorFilters)
	}
	if snap.Residency != ResidencyAny {
		t.Errorf("expected the rest of the snapshot to load normally (residency=any), got %q", snap.Residency)
	}
}

// TestPgStore_LoadVendorFilters_MissingTableReturnsUndefinedTableError
// asserts the raw error loadFromDB classifies is actually a 42P01, not some
// other failure being silently swallowed.
func TestPgStore_LoadVendorFilters_MissingTableReturnsUndefinedTableError(t *testing.T) {
	pool := setupPolicyDB(t)
	s := &PgStore{pool: pool}

	_, err := s.loadVendorFilters(context.Background())
	if err == nil {
		t.Fatal("expected an error querying a table that does not exist")
	}
	if !isUndefinedTableError(err) {
		t.Errorf("expected a 42P01 undefined_table error, got: %v", err)
	}
}

// TestPgStore_LoadFromDB_VendorProviderSettingsPopulatesSnapshot is the
// happy path: once migration 017 has landed and an admin has installed a
// vendor key from the UI, the row round-trips into Snapshot.VendorFilters
// exactly (filter patterns, in order, and the exact filter_set_at stamp).
func TestPgStore_LoadFromDB_VendorProviderSettingsPopulatesSnapshot(t *testing.T) {
	pool := setupPolicyDB(t)
	withVendorProviderSettingsTable(t, pool)

	anthropicSetAt := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	openaiSetAt := time.Date(2026, 2, 1, 9, 30, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, updated_by)
		VALUES ('anthropic', ARRAY['claude-opus-*', 'claude-sonnet-*'], $1, 'admin@example.com'),
		       ('openai', ARRAY['gpt-5*'], $2, 'admin@example.com')
	`, anthropicSetAt, openaiSetAt); err != nil {
		t.Fatalf("seed vendor_provider_settings: %v", err)
	}

	s := &PgStore{pool: pool}
	snap, err := s.loadFromDB(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snap.VendorFilters) != 2 {
		t.Fatalf("expected 2 vendor filter rows, got %+v", snap.VendorFilters)
	}

	anthropic, ok := snap.VendorFilters["anthropic"]
	if !ok {
		t.Fatal("expected an anthropic row")
	}
	if len(anthropic.Filter) != 2 || anthropic.Filter[0] != "claude-opus-*" || anthropic.Filter[1] != "claude-sonnet-*" {
		t.Errorf("anthropic Filter = %v, want [claude-opus-* claude-sonnet-*]", anthropic.Filter)
	}
	if !anthropic.SetAt.Equal(anthropicSetAt) {
		t.Errorf("anthropic SetAt = %v, want %v", anthropic.SetAt, anthropicSetAt)
	}

	openai, ok := snap.VendorFilters["openai"]
	if !ok {
		t.Fatal("expected an openai row")
	}
	if len(openai.Filter) != 1 || openai.Filter[0] != "gpt-5*" {
		t.Errorf("openai Filter = %v, want [gpt-5*]", openai.Filter)
	}
	if !openai.SetAt.Equal(openaiSetAt) {
		t.Errorf("openai SetAt = %v, want %v", openai.SetAt, openaiSetAt)
	}
}
