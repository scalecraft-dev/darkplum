package db

import (
	"context"
	"testing"
)

// TestMigration021_CostAttribution covers the two nullable cache columns on
// the partitioned parent (pre-existing rows read NULL), the model_prices
// table and its CHECKs, and re-runnability.
func TestMigration021_CostAttribution(t *testing.T) {
	pool := setupSchemaUpTo(t, "020_model_availability.sql")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO inference_events (request_id, model, status_code, latency_ms) VALUES ('r1','m',200,5)`); err != nil {
		t.Fatalf("seed pre-021 row: %v", err)
	}
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	var cr, cc *int
	if err := pool.QueryRow(ctx, `SELECT cache_read_tokens, cache_creation_tokens FROM inference_events WHERE request_id='r1'`).Scan(&cr, &cc); err != nil || cr != nil || cc != nil {
		t.Fatalf("pre-existing row must read NULL/NULL: %v %v %v", cr, cc, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO inference_events (request_id, model, status_code, latency_ms, cache_read_tokens, cache_creation_tokens) VALUES ('r2','m',200,5,0,7)`); err != nil {
		t.Fatalf("insert with cache columns: %v", err)
	}

	ins := `INSERT INTO model_prices (provider, model_pattern, input_per_mtok, output_per_mtok, source) VALUES ($1,'x',1,2,$2)`
	if _, err := pool.Exec(ctx, ins, "anthropic", "seed"); err != nil {
		t.Fatalf("valid price: %v", err)
	}
	if _, err := pool.Exec(ctx, ins, "bogus", "seed"); err == nil {
		t.Fatal("provider CHECK must reject")
	}
	if _, err := pool.Exec(ctx, ins, "openai", "other"); err == nil {
		t.Fatal("source CHECK must reject")
	}
	var id string
	var from *string
	if err := pool.QueryRow(ctx, `SELECT id::text, effective_from::text FROM model_prices`).Scan(&id, &from); err != nil || id == "" || from == nil {
		t.Fatalf("defaults: %v", err)
	}

	data, err := migrationsFS.ReadFile("migrations/021_cost_attribution.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply 021: %v", err)
	}
}
