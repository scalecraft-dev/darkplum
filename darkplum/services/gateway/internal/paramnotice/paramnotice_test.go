package paramnotice

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func setup(t *testing.T, withTables bool) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("paramnotice_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	cfg, _ := pgxpool.ParseConfig(url)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if withTables {
		if _, err := pool.Exec(ctx, `
			CREATE TABLE activity_events (
				id UUID PRIMARY KEY DEFAULT gen_random_uuid(), event_type TEXT NOT NULL, actor_id UUID,
				target_type TEXT, target_id UUID, detail JSONB, ip_address INET, request_id TEXT,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now());
			CREATE TABLE param_drop_notices (
				principal_kind TEXT NOT NULL, principal_id UUID NOT NULL, param TEXT NOT NULL,
				first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				PRIMARY KEY (principal_kind, principal_id, param))`); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// drain waits until the queue is empty and the last write finished.
func drain(n *Notifier) {
	for i := 0; i < 200; i++ {
		if len(n.q) == 0 {
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM activity_events WHERE event_type = 'inference.param_dropped' `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const keyA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const keyB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

func TestFirstDropIsAuditedOncePerKeyAndParameterAcrossRestarts(t *testing.T) {
	pool := setup(t, true)
	n := New(pool)
	for i := 0; i < 5; i++ {
		n.NoteDrops("api_key", keyA, "anthropic", "claude", []string{"seed", "frequency_penalty"})
		drain(n)
	}
	if got := count(t, pool, ""); got != 2 {
		t.Fatalf("events = %d, want 2 (one per parameter, not per request)", got)
	}

	// A restarted gateway (empty in-process set) or a second replica must not repeat it.
	n2 := New(pool)
	n2.NoteDrops("api_key", keyA, "anthropic", "claude", []string{"seed"})
	drain(n2)
	if got := count(t, pool, ""); got != 2 {
		t.Errorf("a second notifier repeated an event: %d", got)
	}

	// Another key, and the same parameter dropped for it, is a new first.
	n2.NoteDrops("api_key", keyB, "bedrock", "claude", []string{"seed"})
	// A user (session traffic) is its own principal.
	n2.NoteDrops("user", keyB, "bedrock", "claude", []string{"seed"})
	drain(n2)
	if got := count(t, pool, ""); got != 4 {
		t.Errorf("events = %d, want 4", got)
	}

	var typ string
	var target string
	var detail string
	if err := pool.QueryRow(context.Background(), `
		SELECT target_type, target_id::text, detail::text FROM activity_events
		WHERE target_id = $1::uuid AND target_type = 'api_key' AND detail->>'param' = 'seed'`, keyA).Scan(&typ, &target, &detail); err != nil {
		t.Fatal(err)
	}
	if typ != "api_key" || target != keyA {
		t.Errorf("event target = %s %s", typ, target)
	}
	for _, v := range []string{`"param": "seed"`, `"action": "dropped"`, `"backend": "anthropic"`} {
		if !contains(detail, v) {
			t.Errorf("detail %s lacks %s", detail, v)
		}
	}
}

func TestFirstDropEventsAreCappedPerPrincipal(t *testing.T) {
	pool := setup(t, true)
	n := New(pool)
	var names []string
	for i := 0; i < maxPerPrincipal+20; i++ {
		names = append(names, fmt.Sprintf("unknown_%03d", i))
	}
	for i := 0; i < len(names); i += 10 {
		n.NoteDrops("api_key", keyA, "openai", "m", names[i:i+10])
		drain(n)
	}
	if got := count(t, pool, ""); got != maxPerPrincipal {
		t.Errorf("events = %d, want the cap %d (caller-chosen names must not grow the audit log without bound)", got, maxPerPrincipal)
	}
}

func TestMissingTablesNeverBreakTheRequestPath(t *testing.T) {
	pool := setup(t, false)
	n := New(pool)
	done := make(chan struct{})
	go func() {
		n.NoteDrops("api_key", keyA, "openai", "m", []string{"seed"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NoteDrops blocked")
	}
	drain(n)
	n.NoteDrops("api_key", keyA, "openai", "m", []string{"seed"}) // backed off; still no panic
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Caller-chosen unknown key names are capped, but they must never suppress the
// audit of a registry parameter dropped later.
func TestJunkKeysDoNotSuppressRegistryParameterAudit(t *testing.T) {
	pool := setup(t, true)
	n := New(pool)
	for i := 0; i < maxPerPrincipal+20; i += 10 {
		var names []string
		for j := i; j < i+10; j++ {
			names = append(names, fmt.Sprintf("junk_%03d", j))
		}
		n.NoteDrops("api_key", keyA, "openai", "m", names)
		drain(n)
	}
	n.NoteDrops("api_key", keyA, "anthropic", "m", []string{"seed"})
	drain(n)
	if got := count(t, pool, "AND detail->>'param' = 'seed'"); got != 1 {
		t.Fatalf("seed audit events = %d, want 1 even after %d junk keys", got, maxPerPrincipal+20)
	}
	if got := count(t, pool, "AND detail->>'param' LIKE 'junk_%'"); got != maxPerPrincipal {
		t.Errorf("junk events = %d, want the cap %d", got, maxPerPrincipal)
	}
}
