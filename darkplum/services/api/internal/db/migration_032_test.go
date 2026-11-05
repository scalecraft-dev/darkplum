package db

import (
	"context"
	"strings"
	"testing"
)

// The body cap moves from 64 KiB to 4 MiB: the inline check from 026 is gone,
// the named one is in place, and re-applying is a no-op.
func TestMigration032_LibraryBodyCap(t *testing.T) {
	pool := setupSchemaUpTo(t, "031_library_tree.sql")
	ctx := context.Background()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO users (id, email, name, role) VALUES ('00000000-0000-0000-0000-0000000000a1', 'a@x.com', 'A', 'user')`)
	ins := `INSERT INTO library_items (kind, owner_user_id, visibility, slug, name, body, content_sha256) VALUES ('context', '00000000-0000-0000-0000-0000000000a1', 'private', $1, 'n', $2, 'sha')`
	if _, err := pool.Exec(ctx, ins, "before", strings.Repeat("x", 65537)); err == nil {
		t.Fatal("before 032, a body over 64 KiB was accepted")
	}

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	must(ins, "big", strings.Repeat("x", 65537))
	must(ins, "at-cap", strings.Repeat("x", 4194304))
	if _, err := pool.Exec(ctx, ins, "over", strings.Repeat("x", 4194305)); err == nil || !strings.Contains(err.Error(), "library_items_body_max") {
		t.Fatalf("a body over 4 MiB was accepted, or refused by something else: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'library_items'::regclass AND conname IN ('library_items_body_check', 'library_items_body_max')`).Scan(&n); err != nil || n != 1 {
		t.Errorf("constraints on library_items.body = %d (%v), want only library_items_body_max", n, err)
	}
	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}
}
