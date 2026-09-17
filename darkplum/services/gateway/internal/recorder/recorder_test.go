package recorder

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNullableStr(t *testing.T) {
	if got := nullableStr(""); got != nil {
		t.Fatalf("empty string must become NULL, got %q", *got)
	}
	if got := nullableStr("x"); got == nil || *got != "x" {
		t.Fatalf("non-empty string must round-trip, got %v", got)
	}
}

// Record sits on the request path: a full buffer must drop the event, never block.
func TestRecord_NeverBlocksWhenBufferIsFull(t *testing.T) {
	r := &Recorder{events: make(chan InferenceEvent, 1)} // no flush goroutine draining it
	r.Record(InferenceEvent{RequestID: "kept"})

	done := make(chan struct{})
	go func() {
		r.Record(InferenceEvent{RequestID: "dropped"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full buffer")
	}
	if got := (<-r.events).RequestID; got != "kept" {
		t.Fatalf("expected the first event to be retained, got %q", got)
	}
}

// setupRecorderDB gives the test its own schema holding an inference_events
// table in the PRE-routing shape (api migrations 005+007, before 010/011 add
// requested_model and denial_code). Gated on TEST_DATABASE_URL like the api's
// integration tests.
func setupRecorderDB(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("recorder_test_%d", time.Now().UnixNano())
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
		CREATE TABLE inference_events (
			id                UUID NOT NULL DEFAULT gen_random_uuid(),
			request_id        TEXT NOT NULL,
			api_key_id        UUID,
			user_id           UUID,
			model             TEXT NOT NULL,
			status_code       INT NOT NULL,
			latency_ms        INT NOT NULL,
			prompt_tokens     INT,
			completion_tokens INT,
			stream            BOOLEAN NOT NULL DEFAULT false,
			ip_address        INET,
			provider          TEXT,
			region            TEXT,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return pool
}

func denialEvent(requestID string) InferenceEvent {
	return InferenceEvent{
		RequestID:      requestID,
		Model:          "llama-3-8b",
		RequestedModel: "default-chat",
		DenialCode:     "model_not_enabled",
		StatusCode:     403,
		LatencyMS:      3,
		IP:             "10.0.0.7",
	}
}

// A gateway that starts before the api has migrated must not lose audit events:
// it falls back to the legacy column set, writes each event exactly once, and
// picks the routing columns back up without a restart once they exist.
func TestWriteBatch_FallsBackThenRecoversWhenRoutingColumnsAppear(t *testing.T) {
	pool := setupRecorderDB(t)
	ctx := context.Background()
	r := &Recorder{pool: pool, requestedModelSupported: true}

	r.writeBatch([]InferenceEvent{denialEvent("before-migration")})

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM inference_events WHERE request_id = 'before-migration'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("event must be written exactly once via the legacy insert, got %d rows", n)
	}
	if r.requestedModelSupported {
		t.Fatal("recorder should have downgraded to the legacy schema")
	}
	if !r.nextSchemaProbe.After(time.Now()) {
		t.Fatal("recorder should schedule a future re-probe of the new schema")
	}

	// The api's migrations 010 + 011 land.
	if _, err := pool.Exec(ctx, `ALTER TABLE inference_events ADD COLUMN requested_model TEXT, ADD COLUMN denial_code TEXT`); err != nil {
		t.Fatalf("alter: %v", err)
	}

	// Before the probe is due the recorder stays on the legacy insert.
	r.writeBatch([]InferenceEvent{denialEvent("probe-not-due")})
	var requested, denial *string
	if err := pool.QueryRow(ctx, `SELECT requested_model, denial_code FROM inference_events WHERE request_id = 'probe-not-due'`).Scan(&requested, &denial); err != nil {
		t.Fatalf("select: %v", err)
	}
	if requested != nil || denial != nil {
		t.Fatalf("expected legacy insert before the probe is due, got requested=%v denial=%v", requested, denial)
	}

	// Probe due: the routing columns are used again, no restart needed.
	r.nextSchemaProbe = time.Now().Add(-time.Second)
	r.writeBatch([]InferenceEvent{denialEvent("after-migration")})

	var model string
	var status int
	if err := pool.QueryRow(ctx, `
		SELECT model, requested_model, denial_code, status_code
		FROM inference_events WHERE request_id = 'after-migration'
	`).Scan(&model, &requested, &denial, &status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if model != "llama-3-8b" || requested == nil || *requested != "default-chat" ||
		denial == nil || *denial != "model_not_enabled" || status != 403 {
		t.Fatalf("audit row wrong: model=%q requested=%v denial=%v status=%d", model, requested, denial, status)
	}
	if !r.requestedModelSupported {
		t.Fatal("recorder should have recovered to the new schema")
	}
}

// A dispatched request (no alias, no denial) stores NULLs, not empty strings,
// so "WHERE denial_code IS NOT NULL" is a correct refusals query.
func TestWriteBatch_DispatchedRequestStoresNulls(t *testing.T) {
	pool := setupRecorderDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE inference_events ADD COLUMN requested_model TEXT, ADD COLUMN denial_code TEXT`); err != nil {
		t.Fatalf("alter: %v", err)
	}
	r := &Recorder{pool: pool, requestedModelSupported: true}
	pt, ct := 10, 15

	r.writeBatch([]InferenceEvent{{
		RequestID: "ok", Model: "mock-model", StatusCode: 200, LatencyMS: 12,
		PromptTokens: &pt, CompletionTokens: &ct, Stream: true, Provider: "mock",
	}})

	var requested, denial, region, ip *string
	var provider string
	var prompt, completion int
	if err := pool.QueryRow(ctx, `
		SELECT requested_model, denial_code, region, host(ip_address), provider, prompt_tokens, completion_tokens
		FROM inference_events WHERE request_id = 'ok'
	`).Scan(&requested, &denial, &region, &ip, &provider, &prompt, &completion); err != nil {
		t.Fatalf("select: %v", err)
	}
	if requested != nil || denial != nil || region != nil || ip != nil {
		t.Fatalf("expected NULLs, got requested=%v denial=%v region=%v ip=%v", requested, denial, region, ip)
	}
	if provider != "mock" || prompt != 10 || completion != 15 {
		t.Fatalf("got provider=%q prompt=%d completion=%d", provider, prompt, completion)
	}
}

// End to end through the public API: Record -> background flush -> Drain.
func TestRecordAndDrain_FlushesBufferedEvents(t *testing.T) {
	pool := setupRecorderDB(t)
	ctx := context.Background()
	r := New(pool)
	for i := 0; i < 5; i++ {
		r.Record(InferenceEvent{RequestID: fmt.Sprintf("evt-%d", i), Model: "m", StatusCode: 200})
	}
	r.Drain()

	deadline := time.Now().Add(5 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM inference_events`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 5 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected 5 flushed events after Drain, got %d", n)
}
