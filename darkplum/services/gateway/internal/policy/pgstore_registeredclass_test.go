package policy

import (
	"context"
	"testing"
)

// A database without migration 028 loads (no registrations); with the table
// the registered classes round-trip into the snapshot, and only a 42P01 is
// tolerated.
func TestPgStore_RegisteredClasses_MissingTableTolerated_PopulatedRoundTrips(t *testing.T) {
	pool := setupPolicyDB(t)
	ctx := context.Background()
	s := &PgStore{pool: pool}

	if _, err := loadRegisteredClasses(ctx, pool); err == nil || !isUndefinedTableError(err) {
		t.Fatalf("a missing table must surface as 42P01, got %v", err)
	}
	snap, err := s.loadFromDB(ctx)
	if err != nil {
		t.Fatalf("a missing model_registered_classes must be tolerated: %v", err)
	}
	if len(snap.RegisteredClass) != 0 {
		t.Errorf("RegisteredClass = %v, want empty", snap.RegisteredClass)
	}

	if _, err := pool.Exec(ctx, `
		CREATE TABLE model_registered_classes (model_id TEXT PRIMARY KEY, registered_class TEXT NOT NULL);
		INSERT INTO model_registered_classes VALUES ('hf/llama', 'self_hosted'), ('claude-x', 'external_api')`); err != nil {
		t.Fatal(err)
	}
	snap, err = s.loadFromDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.RegisteredClass["hf/llama"] != "self_hosted" || snap.RegisteredClass["claude-x"] != "external_api" || len(snap.RegisteredClass) != 2 {
		t.Errorf("RegisteredClass = %v", snap.RegisteredClass)
	}

	// Any other failure is a load failure, never silently "no registrations".
	if _, err := pool.Exec(ctx, `ALTER TABLE model_registered_classes RENAME COLUMN registered_class TO renamed`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadFromDB(ctx); err == nil {
		t.Fatal("a broken model_registered_classes must fail the load, not be treated as empty")
	}
}
