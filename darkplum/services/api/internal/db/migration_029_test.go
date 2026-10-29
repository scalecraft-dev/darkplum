package db

import (
	"context"
	"testing"
)

// params_mode lives on the org row only, takes only lenient/strict, and the
// whole migration re-applies cleanly.
func TestMigration029_ParamsModeAndDroppedParams(t *testing.T) {
	pool := setupSchemaUpTo(t, "028_model_registered_class.sql")
	ctx := context.Background()
	exec := func(q string, args ...any) error {
		_, err := pool.Exec(ctx, q, args...)
		return err
	}
	if err := exec(`INSERT INTO users (email, name, role) VALUES ('u@example.com', 'U', 'user')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	// The org row (seeded by 015) starts NULL = lenient.
	var mode *string
	if err := pool.QueryRow(ctx, `SELECT params_mode FROM principal_routing_policies WHERE principal_type='org'`).Scan(&mode); err != nil || mode != nil {
		t.Fatalf("org params_mode = %v, err %v; want NULL", mode, err)
	}
	for _, v := range []string{"strict", "lenient"} {
		if err := exec(`UPDATE principal_routing_policies SET params_mode=$1 WHERE principal_type='org'`, v); err != nil {
			t.Errorf("org params_mode %q refused: %v", v, err)
		}
	}
	if err := exec(`UPDATE principal_routing_policies SET params_mode='bogus' WHERE principal_type='org'`); err == nil {
		t.Error("params_mode 'bogus' accepted")
	}
	// Cross-principal denial: a user row cannot carry it.
	if err := exec(`INSERT INTO principal_routing_policies (principal_type, user_id, params_mode)
		SELECT 'user', id, 'strict' FROM users LIMIT 1`); err == nil {
		t.Error("params_mode accepted on a user row")
	}
	if err := exec(`INSERT INTO principal_routing_policies (principal_type, user_id)
		SELECT 'user', id FROM users LIMIT 1`); err != nil {
		t.Errorf("a user row without params_mode refused: %v", err)
	}

	// dropped_params stores entries, NULL by default.
	if err := exec(`INSERT INTO inference_events (request_id, model, status_code, latency_ms, dropped_params)
		VALUES ('r1', 'm', 200, 1, ARRAY['seed','max_tokens=raised']), ('r2', 'm', 200, 1, NULL)`); err != nil {
		t.Fatalf("insert dropped_params: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM inference_events WHERE 'seed' = ANY(dropped_params)`).Scan(&n); err != nil || n != 1 {
		t.Errorf("dropped_params lookup = %d, err %v", n, err)
	}

	// The dedupe table: one row per (kind, id, param).
	id := "00000000-0000-0000-0000-000000000001"
	ins := `INSERT INTO param_drop_notices (principal_kind, principal_id, param) VALUES ('api_key', $1, 'seed') ON CONFLICT DO NOTHING`
	for i := 0; i < 2; i++ {
		if err := exec(ins, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM param_drop_notices`).Scan(&n); err != nil || n != 1 {
		t.Errorf("notices = %d, err %v; want 1", n, err)
	}

	data, err := migrationsFS.ReadFile("migrations/029_dropped_params.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply 029: %v", err)
	}
}
