package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

// newTestUserLabeled inserts a users row like newTestUser (see
// routing_integration_test.go), but with a label folded into the email so a
// single test can create more than one user (newTestUser alone collides on
// email since it keys only off t.Name(), which is constant within a test).
func newTestUserLabeled(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, name, role)
		VALUES ($1, 'Test User', 'admin')
		RETURNING id
	`, t.Name()+"-"+label+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert test user %s: %v", label, err)
	}
	return id
}

// newConversationsTestRouter wires ConversationHandler's endpoints behind a
// fake session middleware that injects userID as the authenticated actor,
// mirroring what auth.SessionRequired would set in production (see
// routing_integration_test.go's newRoutingTestRouter for the same pattern).
func newConversationsTestRouter(h *ConversationHandler, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	})
	r.GET("/api/v1/conversations", h.ListConversations)
	r.POST("/api/v1/conversations", h.CreateConversation)
	r.GET("/api/v1/conversations/:id", h.GetConversation)
	r.PATCH("/api/v1/conversations/:id", h.PatchConversation)
	r.POST("/api/v1/conversations/:id/messages", h.AppendMessages)
	r.DELETE("/api/v1/conversations/:id", h.DeleteConversation)
	r.DELETE("/api/v1/conversations/:id/messages", h.TruncateMessages)
	return r
}

// insertInferenceEvent inserts a minimal inference_events row for provenance
// lookups. created_at defaults to now(), landing in whichever monthly
// partition (or the default) is current.
func insertInferenceEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, requestID, userID, model string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO inference_events (request_id, user_id, model, status_code, latency_ms)
		VALUES ($1, $2, $3, 200, 10)
	`, requestID, userID, model)
	if err != nil {
		t.Fatalf("insert inference_events row: %v", err)
	}
}

func TestCreateConversation_DerivesTitleFromFirstUserMessage(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "what's the weather like"},
		},
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", resp.Code, resp.Body.String())
	}
	var full conversationFull
	if err := json.Unmarshal(resp.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if full.Title != "what's the weather like" {
		t.Errorf("title = %q, want derived from first user message", full.Title)
	}
	if full.MessageCount != 1 || len(full.Messages) != 1 {
		t.Fatalf("expected 1 message, got %+v", full)
	}
	if full.Messages[0].Seq != 1 {
		t.Errorf("seq = %d, want 1", full.Messages[0].Seq)
	}
	if full.Messages[0].Model != nil {
		t.Errorf("user message model = %v, want nil", full.Messages[0].Model)
	}
}

func TestCreateConversation_NoMessagesDefaultsTitle(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", resp.Code, resp.Body.String())
	}
	var full conversationFull
	if err := json.Unmarshal(resp.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if full.Title != conversationDefaultTitle {
		t.Errorf("title = %q, want %q", full.Title, conversationDefaultTitle)
	}
	if full.Model != "" {
		t.Errorf("model = %q, want empty default", full.Model)
	}
}

func TestCreateConversation_ExplicitTitleAndModel(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{
		"title": "My Chat",
		"model": "llama3",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", resp.Code, resp.Body.String())
	}
	var full conversationFull
	if err := json.Unmarshal(resp.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if full.Title != "My Chat" || full.Model != "llama3" {
		t.Fatalf("unexpected conversation: %+v", full)
	}
}

func TestCreateConversation_ValidationErrors(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	cases := []map[string]any{
		{"messages": []map[string]any{{"role": "bogus", "content": "x"}}},
		{"messages": []map[string]any{{"role": "user", "content": ""}}},
		{"title": ""},
		{"title": ""},
	}
	for i, body := range cases {
		resp := doJSON(t, r, http.MethodPost, "/api/v1/conversations", body)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400; body = %s", i, resp.Code, resp.Body.String())
		}
	}
}

func TestCreateConversation_ContentOverLimitReturns400(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	huge := make([]byte, conversationContentMaxBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	resp := doJSON(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": string(huge)}},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

func TestCreateConversation_ImportKeyIsIdempotent(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	body := map[string]any{
		"import_key": "local-123",
		"title":      "Imported",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi there"},
		},
	}

	first := doJSON(t, r, http.MethodPost, "/api/v1/conversations", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first import: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}
	var firstFull conversationFull
	if err := json.Unmarshal(first.Body.Bytes(), &firstFull); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	second := doJSON(t, r, http.MethodPost, "/api/v1/conversations", body)
	if second.Code != http.StatusOK {
		t.Fatalf("second import: status = %d, want 200 (idempotent); body = %s", second.Code, second.Body.String())
	}
	var secondFull conversationFull
	if err := json.Unmarshal(second.Body.Bytes(), &secondFull); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if secondFull.ID != firstFull.ID {
		t.Fatalf("expected the same conversation id back, got %q vs %q", secondFull.ID, firstFull.ID)
	}
	if secondFull.MessageCount != 2 {
		t.Fatalf("expected no re-append on idempotent import, got message_count = %d", secondFull.MessageCount)
	}

	var total int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM conversations WHERE user_id = $1`, userID).Scan(&total); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 conversation row after idempotent import, got %d", total)
	}
}

func TestListConversations_ScopedOrderedAndLimited(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)
	rB := newConversationsTestRouter(h, userB)

	// User B's conversation must never show up in user A's list.
	doJSON(t, rB, http.MethodPost, "/api/v1/conversations", map[string]any{"title": "B's chat"})

	var ids []string
	for i := 0; i < 3; i++ {
		resp := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{"title": fmt.Sprintf("chat-%d", i)})
		var full conversationFull
		if err := json.Unmarshal(resp.Body.Bytes(), &full); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids = append(ids, full.ID)
		// Ensure distinct updated_at ordering (created_at/updated_at have
		// second-level jitter risk otherwise on a fast test run).
		if _, err := pool.Exec(ctx, `UPDATE conversations SET updated_at = now() + ($1 * interval '1 second') WHERE id = $2`, i, full.ID); err != nil {
			t.Fatalf("backdate updated_at: %v", err)
		}
	}

	resp := doJSON(t, rA, http.MethodGet, "/api/v1/conversations", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}
	var summaries []conversationSummary
	if err := json.Unmarshal(resp.Body.Bytes(), &summaries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("expected 3 conversations scoped to user A, got %d: %+v", len(summaries), summaries)
	}
	// Most-recently-updated first.
	if summaries[0].ID != ids[2] || summaries[2].ID != ids[0] {
		t.Fatalf("expected updated_at DESC ordering, got ids %v want last-in-first %v", []string{summaries[0].ID, summaries[1].ID, summaries[2].ID}, ids)
	}

	limited := doJSON(t, rA, http.MethodGet, "/api/v1/conversations?limit=1", nil)
	var limitedSummaries []conversationSummary
	if err := json.Unmarshal(limited.Body.Bytes(), &limitedSummaries); err != nil {
		t.Fatalf("decode limited: %v", err)
	}
	if len(limitedSummaries) != 1 {
		t.Fatalf("expected 1 result with limit=1, got %d", len(limitedSummaries))
	}
}

func TestGetConversation_OwnershipIsolationAndExport(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)
	rB := newConversationsTestRouter(h, userB)

	created := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{"title": "A's private chat"})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Owner can read it.
	own := doJSON(t, rA, http.MethodGet, "/api/v1/conversations/"+full.ID, nil)
	if own.Code != http.StatusOK {
		t.Fatalf("owner GET: status = %d, want 200; body = %s", own.Code, own.Body.String())
	}

	// A different user gets 404, never 403.
	other := doJSON(t, rB, http.MethodGet, "/api/v1/conversations/"+full.ID, nil)
	if other.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant GET: status = %d, want 404; body = %s", other.Code, other.Body.String())
	}

	// Nonexistent id also 404, indistinguishable from cross-tenant.
	missing := doJSON(t, rA, http.MethodGet, "/api/v1/conversations/00000000-0000-0000-0000-000000000000", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing id GET: status = %d, want 404", missing.Code)
	}

	exported := doJSON(t, rA, http.MethodGet, "/api/v1/conversations/"+full.ID+"?format=export", nil)
	if exported.Code != http.StatusOK {
		t.Fatalf("export GET: status = %d, want 200", exported.Code)
	}
	want := `attachment; filename="conversation-` + full.ID + `.json"`
	if got := exported.Header().Get("Content-Disposition"); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
}

func TestPatchConversation_UpdatesAndOwnershipIsolation(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)
	rB := newConversationsTestRouter(h, userB)

	created := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{"title": "Original"})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	cross := doJSON(t, rB, http.MethodPatch, "/api/v1/conversations/"+full.ID, map[string]any{"title": "Hijacked"})
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant PATCH: status = %d, want 404; body = %s", cross.Code, cross.Body.String())
	}

	resp := doJSON(t, rA, http.MethodPatch, "/api/v1/conversations/"+full.ID, map[string]any{"title": "Renamed", "model": "mistral"})
	if resp.Code != http.StatusOK {
		t.Fatalf("PATCH: status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}
	var s conversationSummary
	if err := json.Unmarshal(resp.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.Title != "Renamed" || s.Model != "mistral" {
		t.Fatalf("unexpected summary after patch: %+v", s)
	}
	if !s.UpdatedAt.After(full.CreatedAt) {
		t.Errorf("expected updated_at to be bumped, got %v (created_at %v)", s.UpdatedAt, full.CreatedAt)
	}

	badTitle := doJSON(t, rA, http.MethodPatch, "/api/v1/conversations/"+full.ID, map[string]any{"title": ""})
	if badTitle.Code != http.StatusBadRequest {
		t.Fatalf("empty title PATCH: status = %d, want 400", badTitle.Code)
	}
}

func TestDeleteConversation_OwnershipIsolationCascadeAndActivityEvent(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)
	rB := newConversationsTestRouter(h, userB)

	created := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
		},
	})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	cross := doJSON(t, rB, http.MethodDelete, "/api/v1/conversations/"+full.ID, nil)
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant DELETE: status = %d, want 404; body = %s", cross.Code, cross.Body.String())
	}

	resp := doJSON(t, rA, http.MethodDelete, "/api/v1/conversations/"+full.ID, nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("DELETE: status = %d, want 204; body = %s", resp.Code, resp.Body.String())
	}

	var convCount, msgCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM conversations WHERE id = $1`, full.ID).Scan(&convCount); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if convCount != 0 {
		t.Errorf("expected conversation deleted, count = %d", convCount)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE conversation_id = $1`, full.ID).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 0 {
		t.Errorf("expected messages cascade-deleted, count = %d", msgCount)
	}

	var evtCount int
	var detail []byte
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE((array_agg(detail))[1], '{}') FROM activity_events WHERE event_type = $1 AND target_id = $2`,
		string(activity.ConversationDeleted), full.ID,
	).Scan(&evtCount, &detail); err != nil {
		t.Fatalf("query activity_events: %v", err)
	}
	if evtCount != 1 {
		t.Fatalf("expected 1 conversation.deleted event, got %d", evtCount)
	}
	var d map[string]any
	if err := json.Unmarshal(detail, &d); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if mc, ok := d["message_count"].(float64); !ok || mc != 2 {
		t.Errorf("expected detail.message_count = 2, got %v", d["message_count"])
	}

	deleteAgain := doJSON(t, rA, http.MethodDelete, "/api/v1/conversations/"+full.ID, nil)
	if deleteAgain.Code != http.StatusNotFound {
		t.Fatalf("delete already-deleted: status = %d, want 404", deleteAgain.Code)
	}
}

func TestAppendMessages_SeqAssignmentAndOwnershipIsolation(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)
	rB := newConversationsTestRouter(h, userB)

	created := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	cross := doJSON(t, rB, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "sneaky"}},
	})
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant append: status = %d, want 404; body = %s", cross.Code, cross.Body.String())
	}

	first := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("append 1: status = %d, want 201; body = %s", first.Code, first.Body.String())
	}
	var firstResp struct {
		Messages []conversationMessage `json:"messages"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(firstResp.Messages) != 1 || firstResp.Messages[0].Seq != 1 {
		t.Fatalf("expected seq 1, got %+v", firstResp.Messages)
	}

	second := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "how are you"},
			{"role": "assistant", "content": "good, thanks"},
		},
	})
	if second.Code != http.StatusCreated {
		t.Fatalf("append 2: status = %d, want 201; body = %s", second.Code, second.Body.String())
	}
	var secondResp struct {
		Messages []conversationMessage `json:"messages"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(secondResp.Messages) != 2 || secondResp.Messages[0].Seq != 2 || secondResp.Messages[1].Seq != 3 {
		t.Fatalf("expected seq 2 and 3, got %+v", secondResp.Messages)
	}

	// Invalid shapes. (An assistant turn alone is valid — it is how a
	// regenerated reply is stored after TruncateMessages — but two turns in the
	// wrong order are not.)
	badOrder := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "assistant", "content": "x"}, {"role": "user", "content": "y"}},
	})
	if badOrder.Code != http.StatusBadRequest {
		t.Fatalf("assistant-then-user append: status = %d, want 400", badOrder.Code)
	}
	tooMany := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "a"},
			{"role": "assistant", "content": "b"},
			{"role": "user", "content": "c"},
		},
	})
	if tooMany.Code != http.StatusBadRequest {
		t.Fatalf(">2 messages append: status = %d, want 400", tooMany.Code)
	}
}

func TestAppendMessages_ContentOverLimitReturns400(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	created := doJSON(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	huge := make([]byte, conversationContentMaxBytes+1)
	for i := range huge {
		huge[i] = 'y'
	}
	resp := doJSON(t, r, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": string(huge)}},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE conversation_id = $1`, full.ID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 0 {
		t.Errorf("expected the oversized message to be rejected before insert, got %d rows", count)
	}
}

// TestAppendMessages_ConcurrentAppendsYieldGaplessSeq fires 8 concurrent
// single-message appends at the same conversation and asserts the resulting
// seq values are exactly {1..8}, no gaps, no duplicates -- the FOR UPDATE
// lock on the conversation row in AppendMessages is what's under test.
func TestAppendMessages_ConcurrentAppendsYieldGaplessSeq(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	created := doJSON(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := doJSON(t, r, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
				"messages": []map[string]any{{"role": "user", "content": fmt.Sprintf("msg-%d", i)}},
			})
			codes[i] = resp.Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusCreated {
			t.Fatalf("goroutine %d: status = %d, want 201", i, code)
		}
	}

	rows, err := pool.Query(ctx, `SELECT seq FROM conversation_messages WHERE conversation_id = $1 ORDER BY seq`, full.ID)
	if err != nil {
		t.Fatalf("query seqs: %v", err)
	}
	defer rows.Close()
	var seqs []int
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan seq: %v", err)
		}
		seqs = append(seqs, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}

	if len(seqs) != n {
		t.Fatalf("expected %d messages, got %d: %v", n, len(seqs), seqs)
	}
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("expected gapless seq 1..%d, got %v", n, seqs)
		}
	}
}

// TestAppendMessages_ModelProvenanceFromInferenceEvents is the architect
// MUST-FIX: an assistant turn's model/request_id are never taken from client
// input. They're resolved from inference_events scoped to (request_id,
// user_id); a request_id belonging to a different user resolves to nothing,
// and a client-supplied "model" value is always ignored.
func TestAppendMessages_ModelProvenanceFromInferenceEvents(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)

	created := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A real inference_events row belonging to a DIFFERENT user (userB).
	insertInferenceEvent(t, ctx, pool, "req-foreign", userB, "claude-3-sonnet")
	// A real inference_events row belonging to the conversation's owner (userA).
	insertInferenceEvent(t, ctx, pool, "req-owned", userA, "llama3")

	// First: a user turn, required before an assistant turn by validateAppendMessages.
	// Case 1: request_id belongs to another user -> model must be NULL, even
	// though the client also tries to forge a "model" value directly.
	resp1 := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "question one"},
			{"role": "assistant", "content": "answer one", "model": "forged", "request_id": "req-foreign"},
		},
	})
	if resp1.Code != http.StatusCreated {
		t.Fatalf("append 1: status = %d, want 201; body = %s", resp1.Code, resp1.Body.String())
	}
	var out1 struct {
		Messages []conversationMessage `json:"messages"`
	}
	if err := json.Unmarshal(resp1.Body.Bytes(), &out1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assistant1 := out1.Messages[1]
	if assistant1.Model != nil {
		t.Errorf("foreign request_id: model = %v, want nil (not %q)", *assistant1.Model, "forged")
	}
	// The request id itself is kept as a correlation key (it may simply not
	// have been recorded yet); it never yields a model unless it matches one
	// of THIS user's audit rows — checked again on read below.
	if assistant1.RequestID == nil || *assistant1.RequestID != "req-foreign" {
		t.Errorf("foreign request_id: stored request_id = %v, want the correlation key kept", assistant1.RequestID)
	}
	reread := doJSON(t, rA, http.MethodGet, "/api/v1/conversations/"+full.ID, nil)
	var rereadFull conversationFull
	if err := json.Unmarshal(reread.Body.Bytes(), &rereadFull); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rereadFull.Messages[1].Model != nil {
		t.Errorf("foreign request_id must never resolve on read either, got %q", *rereadFull.Messages[1].Model)
	}

	// Case 2: request_id belongs to this user -> model resolves from
	// inference_events, ignoring the client's "model" field entirely.
	resp2 := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "question two"},
			{"role": "assistant", "content": "answer two", "model": "forged-again", "request_id": "req-owned"},
		},
	})
	if resp2.Code != http.StatusCreated {
		t.Fatalf("append 2: status = %d, want 201; body = %s", resp2.Code, resp2.Body.String())
	}
	var out2 struct {
		Messages []conversationMessage `json:"messages"`
	}
	if err := json.Unmarshal(resp2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assistant2 := out2.Messages[1]
	if assistant2.Model == nil || *assistant2.Model != "llama3" {
		t.Fatalf("owned request_id: model = %v, want \"llama3\"", assistant2.Model)
	}
	if assistant2.RequestID == nil || *assistant2.RequestID != "req-owned" {
		t.Fatalf("owned request_id: request_id = %v, want \"req-owned\"", assistant2.RequestID)
	}

	// conversations.model is the user's picker preference (possibly an alias)
	// and must NOT be overwritten by the resolved model: provenance lives on the
	// message, the preference belongs to the user.
	var convModel string
	if err := pool.QueryRow(ctx, `SELECT model FROM conversations WHERE id = $1`, full.ID).Scan(&convModel); err != nil {
		t.Fatalf("fetch conversation model: %v", err)
	}
	if convModel != full.Model {
		t.Errorf("conversations.model = %q, want the user's original preference %q", convModel, full.Model)
	}
}

func TestDeleteUser_CascadesConversationsAndMessages(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(h, userID)

	created := doJSON(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
		},
	})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	var convCount, msgCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM conversations WHERE id = $1`, full.ID).Scan(&convCount); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if convCount != 0 {
		t.Errorf("expected conversation to cascade-delete with its user, count = %d", convCount)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM conversation_messages WHERE conversation_id = $1`, full.ID).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 0 {
		t.Errorf("expected messages to cascade-delete with their conversation, count = %d", msgCount)
	}
}

// Regenerate drops the previous assistant turn before the new one is appended:
// truncation is owner-scoped, removes exactly seq >= from_seq, and the next
// append continues the sequence without a gap.
func TestTruncateMessages_OwnerScopedAndSeqContinues(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)
	rB := newConversationsTestRouter(h, userB)

	created := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "q1"},
			{"role": "assistant", "content": "a1"},
			{"role": "user", "content": "q2"},
			{"role": "assistant", "content": "a2 (to be regenerated)"},
		},
	})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if cross := doJSON(t, rB, http.MethodDelete, "/api/v1/conversations/"+full.ID+"/messages?from_seq=4", nil); cross.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant truncate: status = %d, want 404", cross.Code)
	}
	if bad := doJSON(t, rA, http.MethodDelete, "/api/v1/conversations/"+full.ID+"/messages?from_seq=0", nil); bad.Code != http.StatusBadRequest {
		t.Fatalf("from_seq=0: status = %d, want 400", bad.Code)
	}

	resp := doJSON(t, rA, http.MethodDelete, "/api/v1/conversations/"+full.ID+"/messages?from_seq=4", nil)
	if resp.Code != http.StatusNoContent || resp.Header().Get("X-Removed-Count") != "1" {
		t.Fatalf("truncate: status = %d removed = %q, want 204 / 1; body = %s", resp.Code, resp.Header().Get("X-Removed-Count"), resp.Body.String())
	}

	appended := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "assistant", "content": "a2 (regenerated)"}},
	})
	if appended.Code != http.StatusCreated {
		t.Fatalf("append after truncate: status = %d body = %s", appended.Code, appended.Body.String())
	}

	got := doJSON(t, rA, http.MethodGet, "/api/v1/conversations/"+full.ID, nil)
	var after conversationFull
	if err := json.Unmarshal(got.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(after.Messages) != 4 || after.Messages[3].Seq != 4 || after.Messages[3].Content != "a2 (regenerated)" {
		t.Fatalf("expected 4 messages ending with the regenerated turn at seq 4, got %+v", after.Messages)
	}
}

// The gateway's audit recorder writes in batches, so the webchat's append can
// run before the inference_events row exists. The request id is kept and the
// model is resolved on the next read — still only from this user's audit rows.
func TestGetConversation_BackfillsProvenanceRecordedAfterAppend(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userA := newTestUserLabeled(t, ctx, pool, "a")
	userB := newTestUserLabeled(t, ctx, pool, "b")
	h := NewConversationHandler(pool, activity.NewRecorder(pool))
	rA := newConversationsTestRouter(h, userA)

	created := doJSON(t, rA, http.MethodPost, "/api/v1/conversations", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "q"}},
	})
	var full conversationFull
	if err := json.Unmarshal(created.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Append BEFORE the audit row exists: model must be NULL, request id kept.
	appended := doJSON(t, rA, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "assistant", "content": "a", "request_id": "req-late"}},
	})
	var out struct{ Messages []conversationMessage }
	if err := json.Unmarshal(appended.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Messages[0].Model != nil || out.Messages[0].RequestID == nil {
		t.Fatalf("before audit row: got model=%v request_id=%v", out.Messages[0].Model, out.Messages[0].RequestID)
	}

	// Another user's audit row with the same request id must not resolve it.
	insertInferenceEvent(t, ctx, pool, "req-late", userB, "someone-elses-model")
	got := doJSON(t, rA, http.MethodGet, "/api/v1/conversations/"+full.ID, nil)
	var after conversationFull
	if err := json.Unmarshal(got.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Messages[1].Model != nil {
		t.Fatalf("resolved from another user's audit row: %q", *after.Messages[1].Model)
	}

	// The owner's audit row lands (recorder flushed): the next read resolves
	// and persists the model.
	insertInferenceEvent(t, ctx, pool, "req-late", userA, "llama3")
	got = doJSON(t, rA, http.MethodGet, "/api/v1/conversations/"+full.ID, nil)
	if err := json.Unmarshal(got.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Messages[1].Model == nil || *after.Messages[1].Model != "llama3" {
		t.Fatalf("expected backfilled model llama3, got %v", after.Messages[1].Model)
	}
	var stored *string
	if err := pool.QueryRow(ctx, `SELECT model FROM conversation_messages WHERE conversation_id = $1 AND seq = 2`, full.ID).Scan(&stored); err != nil {
		t.Fatalf("select: %v", err)
	}
	if stored == nil || *stored != "llama3" {
		t.Fatalf("backfill must persist, stored = %v", stored)
	}
}
