package db

import (
	"context"
	"testing"
)

// The backfill registers the strictest of a model's current class and every
// class the usage log recorded for it, so an id a vendor had already taken
// over at upgrade time is registered at its earlier, stricter class.
func TestMigration028_RegisteredClassBackfill(t *testing.T) {
	pool := setupSchemaUpTo(t, "027_alias_on_route_unavailable.sql")
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// taken-over: models says external_api now, history says it ran self_hosted.
	mustExec(`INSERT INTO models (model_id, provider, backend_class) VALUES
		('taken-over', 'openai', 'external_api'), ('vendor-only', 'openai', 'external_api'),
		('local', 'vllm', 'self_hosted'), ('no-history', 'bedrock', 'cloud_managed'),
		('weird', 'x', 'external_api')`)
	ev := `INSERT INTO inference_events (request_id, model, status_code, latency_ms, backend_class) VALUES ($1, $2, 200, 1, $3)`
	mustExec(ev, "r1", "taken-over", "self_hosted")
	mustExec(ev, "r2", "taken-over", "external_api")
	mustExec(ev, "r3", "vendor-only", "external_api")
	mustExec(ev, "r4", "local", "external_api") // history looser than now: the strictest wins
	mustExec(ev, "r5", "not-a-model-row", "self_hosted")
	mustExec(`INSERT INTO inference_events (request_id, model, status_code, latency_ms, backend_class) VALUES ('r6', 'weird', 200, 1, NULL)`)

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	want := map[string]string{
		"taken-over": "self_hosted", "vendor-only": "external_api", "local": "self_hosted",
		"no-history": "cloud_managed", "weird": "external_api",
	}
	check := func() {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT model_id, registered_class FROM model_registered_classes`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var id, c string
			if err := rows.Scan(&id, &c); err != nil {
				t.Fatal(err)
			}
			got[id] = c
		}
		if len(got) != len(want) {
			t.Fatalf("registered = %v, want %v", got, want)
		}
		for id, c := range want {
			if got[id] != c {
				t.Errorf("%s registered %q, want %q", id, got[id], c)
			}
		}
	}
	check()

	// Re-applying changes nothing, even after an admin edit.
	mustExec(`UPDATE model_registered_classes SET registered_class = 'external_api' WHERE model_id = 'local'`)
	want["local"] = "external_api"
	data, err := migrationsFS.ReadFile("migrations/028_model_registered_class.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply 028: %v", err)
	}
	check()
}
