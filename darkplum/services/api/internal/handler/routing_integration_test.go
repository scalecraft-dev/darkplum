package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

// newTestUser inserts a real users row (satisfying activity_events.actor_id's
// FK) and returns its id, so activity recording can be asserted on rather
// than silently swallowed.
func newTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, name, role)
		VALUES ($1, 'Test Admin', 'admin')
		RETURNING id
	`, t.Name()+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return id
}

// newRoutingTestRouter wires RoutingHandler's endpoints behind a fake session
// middleware that injects userID as the authenticated actor, mirroring what
// auth.SessionRequired + auth.AdminRequired would set in production.
func newRoutingTestRouter(h *RoutingHandler, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	})
	r.GET("/api/v1/model-aliases", h.ListAliases)
	r.PUT("/api/v1/model-aliases/:alias", h.PutAlias)
	r.DELETE("/api/v1/model-aliases/:alias", h.DeleteAlias)
	r.GET("/api/v1/routing-policy", h.GetRoutingPolicy)
	r.PUT("/api/v1/routing-policy", h.PutRoutingPolicy)
	return r
}

func insertModel(t *testing.T, ctx context.Context, pool *pgxpool.Pool, modelID, backendClass string, enabled bool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO models (model_id, provider, backend_class, enabled)
		VALUES ($1, 'vllm', $2, $3)
	`, modelID, backendClass, enabled)
	if err != nil {
		t.Fatalf("insert model %s: %v", modelID, err)
	}
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	msg, _ := body["error"].(string)
	return msg
}

func TestPutAlias_CreateAndRetarget(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "llama3-v2", backendClassSelfHosted, false)

	rec := activity.NewRecorder(pool)
	h := NewRoutingHandler(pool, rec)
	r := newRoutingTestRouter(h, userID)

	// Create.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/default", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("create alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var row aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode alias response: %v", err)
	}
	if row.Alias != "default" || row.TargetModelID != "llama3" || !row.TargetExists || !row.TargetEnabled {
		t.Fatalf("unexpected alias row after create: %+v", row)
	}
	if row.TargetBackendClass == nil || *row.TargetBackendClass != backendClassSelfHosted {
		t.Fatalf("expected target_backend_class = self_hosted, got %+v", row.TargetBackendClass)
	}

	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activity_events WHERE event_type = $1 AND detail->>'alias' = 'default'`,
		string(activity.ModelAliasCreated),
	).Scan(&eventCount); err != nil {
		t.Fatalf("count created events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("expected 1 model_alias.created event, got %d", eventCount)
	}

	// Retarget (update).
	resp = doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/default", map[string]string{"target_model_id": "llama3-v2"})
	if resp.Code != http.StatusOK {
		t.Fatalf("retarget alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode retarget response: %v", err)
	}
	if row.TargetModelID != "llama3-v2" || row.TargetEnabled {
		t.Fatalf("expected alias retargeted to disabled llama3-v2, got %+v", row)
	}

	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activity_events WHERE event_type = $1 AND detail->>'alias' = 'default'`,
		string(activity.ModelAliasUpdated),
	).Scan(&eventCount); err != nil {
		t.Fatalf("count updated events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("expected 1 model_alias.updated event, got %d", eventCount)
	}

	// Only one alias row should exist (update, not a second insert).
	var total int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_aliases`).Scan(&total); err != nil {
		t.Fatalf("count model_aliases: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 alias row, got %d", total)
	}
}

func TestPutAlias_InvalidFormat(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/Not%20Valid", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

func TestPutAlias_MissingTarget(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/default", map[string]string{})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

func TestPutAlias_TargetIsItselfAnAlias(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/base", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("setup alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/chained", map[string]string{"target_model_id": "base"})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
	if msg := decodeErrorBody(t, resp); msg != "target is itself an alias" {
		t.Fatalf("error = %q, want %q", msg, "target is itself an alias")
	}
}

func TestPutAlias_TargetNotFound(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/default", map[string]string{"target_model_id": "does-not-exist"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", resp.Code, resp.Body.String())
	}
	if msg := decodeErrorBody(t, resp); msg != "target model not found" {
		t.Fatalf("error = %q, want %q", msg, "target model not found")
	}
}

func TestPutAlias_ConflictsWithExistingModelID(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "mistral", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/mistral", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}
	if msg := decodeErrorBody(t, resp); msg != "alias conflicts with an existing model id" {
		t.Fatalf("error = %q, want %q", msg, "alias conflicts with an existing model id")
	}
}

func TestDeleteAlias(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/default", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("setup alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodDelete, "/api/v1/model-aliases/default", nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204; body = %s", resp.Code, resp.Body.String())
	}

	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activity_events WHERE event_type = $1`, string(activity.ModelAliasDeleted),
	).Scan(&eventCount); err != nil {
		t.Fatalf("count deleted events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("expected 1 model_alias.deleted event, got %d", eventCount)
	}

	resp = doJSON(t, r, http.MethodDelete, "/api/v1/model-aliases/default", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status = %d, want 404; body = %s", resp.Code, resp.Body.String())
	}
}

// TestPutAlias_TargetAvailableReflectsDiscoveredAt covers the new
// target_available field: it must be true for a freshly-discovered enabled
// target, and false once that target's discovered_at falls outside
// modelAvailabilityWindow, even though target_exists and target_enabled stay
// true (the row survives a cold start; only availability changes).
func TestPutAlias_TargetAvailableReflectsDiscoveredAt(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/default", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("create alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var row aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode alias response: %v", err)
	}
	if !row.TargetAvailable {
		t.Fatalf("expected target_available = true for a freshly discovered target, got %+v", row)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE models SET discovered_at = now() - interval '10 minutes' WHERE model_id = 'llama3'`,
	); err != nil {
		t.Fatalf("backdate discovered_at: %v", err)
	}

	resp = doJSON(t, r, http.MethodGet, "/api/v1/model-aliases", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("list aliases: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var aliases []aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &aliases); err != nil {
		t.Fatalf("decode aliases: %v", err)
	}
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d: %+v", len(aliases), aliases)
	}
	if !aliases[0].TargetExists || !aliases[0].TargetEnabled {
		t.Fatalf("expected target_exists and target_enabled to remain true after the cutoff, got %+v", aliases[0])
	}
	if aliases[0].TargetAvailable {
		t.Fatalf("expected target_available = false once discovered_at is stale, got %+v", aliases[0])
	}
}

func TestListAliases_ReflectsTargetState(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/default", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("setup alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Simulate a resync that dropped the target model (survives, target_exists=false).
	if _, err := pool.Exec(ctx, `DELETE FROM models WHERE model_id = 'llama3'`); err != nil {
		t.Fatalf("delete target model: %v", err)
	}

	resp = doJSON(t, r, http.MethodGet, "/api/v1/model-aliases", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("list aliases: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var aliases []aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &aliases); err != nil {
		t.Fatalf("decode aliases: %v", err)
	}
	if len(aliases) != 1 {
		t.Fatalf("expected 1 alias, got %d: %+v", len(aliases), aliases)
	}
	if aliases[0].TargetExists {
		t.Fatalf("expected target_exists = false after target model deletion, got %+v", aliases[0])
	}
	if aliases[0].TargetEnabled {
		t.Fatalf("expected target_enabled = false when target is gone, got %+v", aliases[0])
	}
	if aliases[0].TargetBackendClass != nil {
		t.Fatalf("expected target_backend_class = nil when target is gone, got %v", aliases[0].TargetBackendClass)
	}
	if aliases[0].TargetAvailable {
		t.Fatalf("expected target_available = false when target is gone, got %+v", aliases[0])
	}
}

func TestRoutingPolicy_GetPutAndInvalidValue(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool))
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodGet, "/api/v1/routing-policy", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("get policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var policy routingPolicyResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &policy); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if policy.Residency != residencyAny {
		t.Fatalf("expected default residency %q, got %q", residencyAny, policy.Residency)
	}

	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]string{"residency": "bogus"})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid residency: status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]string{"residency": residencySelfHostedOnly})
	if resp.Code != http.StatusOK {
		t.Fatalf("set policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &policy); err != nil {
		t.Fatalf("decode updated policy: %v", err)
	}
	if policy.Residency != residencySelfHostedOnly {
		t.Fatalf("expected residency %q after update, got %q", residencySelfHostedOnly, policy.Residency)
	}

	var updatedBy *string
	if err := pool.QueryRow(ctx, `SELECT updated_by FROM routing_policy WHERE id = true`).Scan(&updatedBy); err != nil {
		t.Fatalf("fetch updated_by: %v", err)
	}
	if updatedBy == nil || *updatedBy != userID {
		t.Fatalf("expected updated_by = %q, got %v", userID, updatedBy)
	}

	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activity_events WHERE event_type = $1`, string(activity.RoutingPolicyChanged),
	).Scan(&eventCount); err != nil {
		t.Fatalf("count policy-changed events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("expected 1 routing_policy.changed event, got %d", eventCount)
	}
}
