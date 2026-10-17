package db

import (
	"context"
	"testing"
)

// TestMigration022_VendorBilling covers the three tables: the NULL-safe
// unique indexes (a NULL workspace/api key still collides), the status
// CHECKs and seeded rows, and re-runnability.
func TestMigration022_VendorBilling(t *testing.T) {
	pool := setupSchemaUpTo(t, "021_cost_attribution.sql")
	ctx := context.Background()
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vendor_billing_status WHERE state='not_configured' AND NOT configured`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("seeded status rows = %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE vendor_billing_status SET state='bogus' WHERE vendor='openai'`); err == nil {
		t.Fatal("state CHECK must reject")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vendor_billing_status (vendor) VALUES ('azure')`); err == nil {
		t.Fatal("vendor CHECK must reject")
	}

	u := `INSERT INTO vendor_usage_reports (vendor, day, model, workspace_id, api_key_id) VALUES ('anthropic','2026-10-01','m',$1,$2)`
	if _, err := pool.Exec(ctx, u, nil, nil); err != nil {
		t.Fatalf("usage insert: %v", err)
	}
	if _, err := pool.Exec(ctx, u, nil, nil); err == nil {
		t.Fatal("NULL/NULL usage duplicate must collide")
	}
	if _, err := pool.Exec(ctx, u, "ws1", nil); err != nil {
		t.Fatalf("distinct workspace must insert: %v", err)
	}
	c := `INSERT INTO vendor_cost_reports (vendor, day, description, workspace_id, cost_cents) VALUES ('anthropic','2026-10-01','d',$1,1.5)`
	if _, err := pool.Exec(ctx, c, nil); err != nil {
		t.Fatalf("cost insert: %v", err)
	}
	if _, err := pool.Exec(ctx, c, nil); err == nil {
		t.Fatal("NULL workspace cost duplicate must collide")
	}

	data, err := migrationsFS.ReadFile("migrations/022_vendor_billing_reconciliation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply 022: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vendor_billing_status`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("re-apply duplicated status rows: %d", n)
	}
}
