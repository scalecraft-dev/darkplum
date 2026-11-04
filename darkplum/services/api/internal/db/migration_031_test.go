package db

import (
	"context"
	"strings"
	"testing"
)

// The tree migration: person slugs derived from the email with a collision
// suffix (oldest keeps the bare slug), shared-slug collisions deduped with a
// deterministic -<id8>, every existing item queued, and the new constraints
// holding. Re-applying the file is a no-op.
func TestMigration031_LibraryTree(t *testing.T) {
	pool := setupSchemaUpTo(t, "030_conversation_attachments.sql")
	ctx := context.Background()
	exec := func(q string, args ...any) error {
		_, err := pool.Exec(ctx, q, args...)
		return err
	}
	must := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// a.b@x.com and a-b@x.com both slug to a-b-x-com; the older keeps it.
	must(`INSERT INTO users (id, email, name, role, created_at) VALUES
		('00000000-0000-0000-0000-0000000000a1', 'a.b@x.com', 'A', 'user', now() - interval '2 days'),
		('00000000-0000-0000-0000-0000000000a2', 'a-b@x.com', 'B', 'user', now() - interval '1 day'),
		('00000000-0000-0000-0000-0000000000a3', 'CON', 'C', 'user', now())`)
	// Two people shared a skill with the same slug; a private one is not touched.
	ins := `INSERT INTO library_items (id, kind, owner_user_id, visibility, slug, name, body, content_sha256, created_at)
		VALUES ($1, $2, $3, $4, $5, 'n', 'b', 'sha', now() - $6::interval)`
	must(ins, "10000000-0000-0000-0000-000000000001", "skill", "00000000-0000-0000-0000-0000000000a1", "org", "review", "2 hours")
	must(ins, "10000000-0000-0000-0000-000000000002", "skill", "00000000-0000-0000-0000-0000000000a2", "org", "review", "1 hour")
	must(ins, "10000000-0000-0000-0000-000000000003", "skill", "00000000-0000-0000-0000-0000000000a3", "private", "review", "1 hour")
	must(ins, "10000000-0000-0000-0000-000000000004", "context", "00000000-0000-0000-0000-0000000000a1", "private", "nul", "1 hour")

	if err := RunMigrations(ctx, pool); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	slug := func(id string) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT library_slug FROM users WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := slug("00000000-0000-0000-0000-0000000000a1"); s != "a-b-x-com" {
		t.Errorf("oldest slug = %q", s)
	}
	s2 := slug("00000000-0000-0000-0000-0000000000a2")
	if !strings.HasPrefix(s2, "a-b-x-com-") || len(s2) != len("a-b-x-com-")+4 {
		t.Errorf("collision slug = %q", s2)
	}
	if s := slug("00000000-0000-0000-0000-0000000000a3"); s != "user" {
		t.Errorf("reserved-stem email slug = %q, want user", s)
	}

	itemSlug := func(id string) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT slug FROM library_items WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := itemSlug("10000000-0000-0000-0000-000000000001"); s != "review" {
		t.Errorf("older shared slug = %q", s)
	}
	if s := itemSlug("10000000-0000-0000-0000-000000000002"); s != "review-10000000" {
		t.Errorf("duplicate shared slug = %q", s)
	}
	if s := itemSlug("10000000-0000-0000-0000-000000000003"); s != "review" {
		t.Errorf("private slug changed: %q", s)
	}
	if s := itemSlug("10000000-0000-0000-0000-000000000004"); s != "nul-10000000" {
		t.Errorf("reserved slug = %q", s)
	}

	var queued, items int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM library_sync WHERE item_id IS NOT NULL`).Scan(&queued)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM library_items`).Scan(&items)
	if queued != 4 || items != 4 {
		t.Errorf("queued %d of %d items", queued, items)
	}

	// New uniqueness: same kind+slug in the same place is refused, a different
	// kind is fine, and shared items collide regardless of owner.
	if err := exec(ins, "10000000-0000-0000-0000-000000000005", "skill", "00000000-0000-0000-0000-0000000000a3", "org", "review", "0 hours"); err == nil {
		t.Error("two org skills named review in the same folder accepted")
	}
	if err := exec(ins, "10000000-0000-0000-0000-000000000006", "context", "00000000-0000-0000-0000-0000000000a3", "org", "review", "0 hours"); err != nil {
		t.Errorf("different kind refused: %v", err)
	}
	if err := exec(ins, "10000000-0000-0000-0000-000000000007", "skill", "00000000-0000-0000-0000-0000000000a2", "private", "review", "0 hours"); err != nil {
		t.Errorf("private item with an org item's name refused: %v", err)
	}

	// Folder constraints: charset, depth, and scope/owner agreement.
	f := `INSERT INTO library_folders (scope, owner_user_id, slug, display_name, path) VALUES ($1, $2, $3, 'x', $4)`
	if err := exec(f, "org", nil, "a", "a"); err != nil {
		t.Fatalf("folder insert: %v", err)
	}
	if err := exec(f, "org", nil, "a", "a"); err == nil {
		t.Error("duplicate org folder path accepted (NULL owner must still collide)")
	}
	if err := exec(f, "org", nil, "f", "a/b/c/d/e/f"); err == nil {
		t.Error("six-deep path accepted")
	}
	if err := exec(f, "org", nil, "A", "A"); err == nil {
		t.Error("uppercase folder accepted")
	}
	if err := exec(f, "user", nil, "z", "z"); err == nil {
		t.Error("user folder without an owner accepted")
	}
	if err := exec(f, "org", "00000000-0000-0000-0000-0000000000a1", "z", "z"); err == nil {
		t.Error("org folder with an owner accepted")
	}
	// A queue row targets exactly one thing.
	if err := exec(`INSERT INTO library_sync (item_id, folder_id) VALUES (gen_random_uuid(), gen_random_uuid())`); err == nil {
		t.Error("queue row with both item and folder accepted")
	}

	// A user inserted after the migration gets a slug from the trigger.
	must(`INSERT INTO users (email, name, role) VALUES ('new@x.com', 'N', 'user')`)
	var ns string
	if err := pool.QueryRow(ctx, `SELECT library_slug FROM users WHERE email='new@x.com'`).Scan(&ns); err != nil || ns != "new-x-com" {
		t.Errorf("new user slug = %q, %v", ns, err)
	}
	// ...and an email change leaves it alone.
	must(`UPDATE users SET email='other@x.com' WHERE email='new@x.com'`)
	if err := pool.QueryRow(ctx, `SELECT library_slug FROM users WHERE email='other@x.com'`).Scan(&ns); err != nil || ns != "new-x-com" {
		t.Errorf("slug after email change = %q, %v", ns, err)
	}

	data, err := migrationsFS.ReadFile("migrations/031_library_tree.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(data)); err != nil {
		t.Fatalf("re-apply 031: %v", err)
	}
	if s := itemSlug("10000000-0000-0000-0000-000000000002"); s != "review-10000000" {
		t.Errorf("re-apply changed a slug: %q", s)
	}
}
