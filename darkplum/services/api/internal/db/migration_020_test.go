package db

import (
	"context"
	"testing"
)

// TestMigration020_AddsModelAvailability covers the two nullable columns:
// pre-existing rows read NULL, state/detail round-trip (detail as JSONB), and
// the file is re-runnable by hand.
func TestMigration020_AddsModelAvailability(t *testing.T) {
	pool := setupSchemaUpTo(t, "019_vendor_provider_workspace_id.sql")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO models (model_id, provider, backend_class) VALUES ('pre', 'bedrock', 'cloud_managed')`); err != nil {
		t.Fatalf("seed pre-020 row: %v", err)
	}
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	var st, det *string
	if err := pool.QueryRow(ctx, `SELECT availability_state, availability_detail::text FROM models WHERE model_id = 'pre'`).Scan(&st, &det); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if st != nil || det != nil {
		t.Fatalf("existing row must read NULL/NULL, got %v/%v", st, det)
	}

	if _, err := pool.Exec(ctx, `UPDATE models SET availability_state = 'needs_aws_opt_in',
		availability_detail = '{"state":"needs_aws_opt_in","reason":"data_retention"}' WHERE model_id = 'pre'`); err != nil {
		t.Fatalf("set availability: %v", err)
	}

	data, err := migrationsFS.ReadFile("migrations/020_model_availability.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply migration 020 by hand: %v", err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT availability_state, availability_detail->>'reason' FROM models WHERE model_id = 'pre'`).Scan(&st, &reason); err != nil || st == nil || *st != "needs_aws_opt_in" || reason != "data_retention" {
		t.Fatalf("re-apply must keep values: %v %q %v", st, reason, err)
	}
}
