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

// TestRecordPair_EnqueuesBothWhenRoomExists asserts the ordinary case: both
// the main and the routing event land in the buffer, in order.
func TestRecordPair_EnqueuesBothWhenRoomExists(t *testing.T) {
	r := &Recorder{events: make(chan InferenceEvent, 4)}
	main := InferenceEvent{RequestID: "req-1", Purpose: "inference"}
	routing := InferenceEvent{RequestID: "req-1", Purpose: "routing"}

	r.RecordPair(main, routing)

	if len(r.events) != 2 {
		t.Fatalf("expected both events enqueued, got %d", len(r.events))
	}
	if got := <-r.events; got.Purpose != "inference" {
		t.Errorf("expected the main event first, got %+v", got)
	}
	if got := <-r.events; got.Purpose != "routing" {
		t.Errorf("expected the routing event second, got %+v", got)
	}
	if r.DroppedPairs() != 0 {
		t.Errorf("DroppedPairs() = %d, want 0", r.DroppedPairs())
	}
}

// TestRecordPair_DropsBothWhenInsufficientRoom is the core both-or-neither
// guard: a buffer with room for only one of the two events must enqueue
// NEITHER, not one of them, and must count the drop.
func TestRecordPair_DropsBothWhenInsufficientRoom(t *testing.T) {
	r := &Recorder{events: make(chan InferenceEvent, 1)}
	r.events <- InferenceEvent{RequestID: "already-buffered"} // leaves exactly 0 free slots

	r.RecordPair(InferenceEvent{RequestID: "main"}, InferenceEvent{RequestID: "routing"})

	if len(r.events) != 1 {
		t.Fatalf("expected the pair to be dropped entirely, buffer len = %d", len(r.events))
	}
	if got := (<-r.events).RequestID; got != "already-buffered" {
		t.Errorf("expected the pre-existing event untouched, got %q", got)
	}
	if r.DroppedPairs() != 1 {
		t.Errorf("DroppedPairs() = %d, want 1", r.DroppedPairs())
	}
}

// TestRecordPair_NeverBlocksWhenBufferIsFull mirrors
// TestRecord_NeverBlocksWhenBufferIsFull for the pair path.
func TestRecordPair_NeverBlocksWhenBufferIsFull(t *testing.T) {
	r := &Recorder{events: make(chan InferenceEvent, 1)}
	r.events <- InferenceEvent{RequestID: "kept"}

	done := make(chan struct{})
	go func() {
		r.RecordPair(InferenceEvent{RequestID: "main"}, InferenceEvent{RequestID: "routing"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordPair blocked on a full buffer")
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

// A gateway that starts before the api has migrated must not lose audit
// events: it steps down through schemas (routing -> requested_model ->
// legacy), writes each event exactly once, and picks newer schemas back up
// without a restart once their columns exist — one schema level at a time,
// matching how migrations 010/011 and 013 land independently.
func TestWriteBatch_FallsBackThenRecoversAcrossSchemaLevels(t *testing.T) {
	pool := setupRecorderDB(t)
	ctx := context.Background()
	r := &Recorder{pool: pool}

	r.writeBatch([]InferenceEvent{denialEvent("before-migration")})

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM inference_events WHERE request_id = 'before-migration'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("event must be written exactly once via the legacy insert, got %d rows", n)
	}
	if r.schemaLevel != 2 {
		t.Fatalf("expected the recorder to settle on the legacy schema (level 2), got %d", r.schemaLevel)
	}
	if !r.nextSchemaProbe.After(time.Now()) {
		t.Fatal("recorder should schedule a future re-probe of newer schemas")
	}

	// The api's migrations 010 + 011 land (requested_model, denial_code) —
	// but not yet 013 (purpose, route_name, router_reason, router_latency_ms).
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

	// Probe due: the recorder walks forward from the newest schema (routing,
	// still missing its columns) and correctly settles on requested_model
	// (level 1) rather than jumping straight back to legacy.
	r.nextSchemaProbe = time.Now().Add(-time.Second)
	r.writeBatch([]InferenceEvent{denialEvent("after-011")})

	var model string
	var status int
	if err := pool.QueryRow(ctx, `
		SELECT model, requested_model, denial_code, status_code
		FROM inference_events WHERE request_id = 'after-011'
	`).Scan(&model, &requested, &denial, &status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if model != "llama-3-8b" || requested == nil || *requested != "default-chat" ||
		denial == nil || *denial != "model_not_enabled" || status != 403 {
		t.Fatalf("audit row wrong: model=%q requested=%v denial=%v status=%d", model, requested, denial, status)
	}
	if r.schemaLevel != 1 {
		t.Fatalf("expected the recorder to settle on the requested_model schema (level 1), got %d", r.schemaLevel)
	}

	// Migration 013 lands. The next re-probe recovers all the way to the
	// newest (routing) schema.
	if _, err := pool.Exec(ctx, `
		ALTER TABLE inference_events
			ADD COLUMN purpose TEXT NOT NULL DEFAULT 'inference',
			ADD COLUMN route_name TEXT,
			ADD COLUMN router_reason TEXT,
			ADD COLUMN router_latency_ms INT`); err != nil {
		t.Fatalf("alter: %v", err)
	}
	r.nextSchemaProbe = time.Now().Add(-time.Second)

	latency := 42
	r.writeBatch([]InferenceEvent{{
		RequestID: "after-013", Model: "llama-3-8b", StatusCode: 200, LatencyMS: 5,
		Purpose: "routing", RouteName: "general", RouterReason: "rules:default", RouterLatencyMS: &latency,
	}})

	var purpose string
	var routeName, routerReason *string
	var routerLatency *int
	if err := pool.QueryRow(ctx, `
		SELECT purpose, route_name, router_reason, router_latency_ms
		FROM inference_events WHERE request_id = 'after-013'
	`).Scan(&purpose, &routeName, &routerReason, &routerLatency); err != nil {
		t.Fatalf("select: %v", err)
	}
	if purpose != "routing" || routeName == nil || *routeName != "general" ||
		routerReason == nil || *routerReason != "rules:default" || routerLatency == nil || *routerLatency != 42 {
		t.Fatalf("got purpose=%q route_name=%v router_reason=%v router_latency_ms=%v", purpose, routeName, routerReason, routerLatency)
	}
	if r.schemaLevel != 0 {
		t.Fatalf("expected the recorder to recover to the routing schema (level 0), got %d", r.schemaLevel)
	}
}

// TestWriteBatch_PurposeDefaultsToInference asserts an ordinary event (Purpose
// left at its Go zero value) is stored as purpose='inference', not an empty
// string that would violate the column's CHECK constraint.
func TestWriteBatch_PurposeDefaultsToInference(t *testing.T) {
	pool := setupRecorderDB(t)
	ctx := context.Background()
	mustAddRoutingColumns(t, pool)
	r := &Recorder{pool: pool}

	r.writeBatch([]InferenceEvent{{RequestID: "plain", Model: "llama-3-8b", StatusCode: 200}})

	var purpose string
	if err := pool.QueryRow(ctx, `SELECT purpose FROM inference_events WHERE request_id = 'plain'`).Scan(&purpose); err != nil {
		t.Fatalf("select: %v", err)
	}
	if purpose != "inference" {
		t.Fatalf("purpose = %q, want inference", purpose)
	}
}

// mustAddRoutingColumns brings a setupRecorderDB table up to the full
// migration 013 shape (requested_model/denial_code from 010/011, plus
// purpose/route_name/router_reason/router_latency_ms from 013).
func mustAddRoutingColumns(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		ALTER TABLE inference_events
			ADD COLUMN requested_model TEXT,
			ADD COLUMN denial_code TEXT,
			ADD COLUMN purpose TEXT NOT NULL DEFAULT 'inference',
			ADD COLUMN route_name TEXT,
			ADD COLUMN router_reason TEXT,
			ADD COLUMN router_latency_ms INT`); err != nil {
		t.Fatalf("alter: %v", err)
	}
}

// A dispatched request (no alias, no denial) stores NULLs, not empty strings,
// so "WHERE denial_code IS NOT NULL" is a correct refusals query.
func TestWriteBatch_DispatchedRequestStoresNulls(t *testing.T) {
	pool := setupRecorderDB(t)
	ctx := context.Background()
	mustAddRoutingColumns(t, pool)
	r := &Recorder{pool: pool}
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
