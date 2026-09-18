package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	r.POST("/api/v1/model-aliases/:alias/test", h.TestAlias)
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
	h := NewRoutingHandler(pool, rec, "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
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

// The alias endpoints must apply the same operator-configured availability
// window as the model endpoints, or target_available and available disagree.
func TestListAliases_TargetAvailableHonoursConfiguredWindow(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	if _, err := pool.Exec(ctx, `UPDATE models SET discovered_at = now() - interval '4 minutes' WHERE model_id = 'llama3'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_aliases (alias, target_model_id) VALUES ('smart', 'llama3')`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	for _, tc := range []struct {
		window time.Duration
		want   bool
	}{
		{2 * time.Minute, false}, // default: unseen for 4 min -> unavailable
		{10 * time.Minute, true}, // operator widened the window
	} {
		h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
		h.SetAvailabilityWindow(tc.window)
		r := gin.New()
		r.GET("/api/v1/model-aliases", h.ListAliases)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/model-aliases", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("window %v: status %d body %s", tc.window, w.Code, w.Body.String())
		}
		var rows []aliasResponse
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(rows) != 1 || rows[0].TargetAvailable != tc.want {
			t.Fatalf("window %v: expected target_available=%v, got %+v", tc.window, tc.want, rows)
		}
	}
}

// routedBody builds a PUT /model-aliases/:alias request body for a routed
// alias with a rules classifier, using routes as (route_name, target_model_id)
// pairs. defaultRoute must name one of routes' route_names for the body to
// be valid.
func routedBody(defaultRoute string, routes ...[2]string) map[string]any {
	rs := make([]map[string]any, len(routes))
	for i, r := range routes {
		rs[i] = map[string]any{
			"route_name":      r[0],
			"description":     "route " + r[0],
			"target_model_id": r[1],
			"keywords":        []string{r[0]},
		}
	}
	return map[string]any{
		"kind":          "routed",
		"default_route": defaultRoute,
		"classifier":    map[string]any{"kind": "rules"},
		"routes":        rs,
	}
}

func TestPutAlias_Routed_CreateListAndDeleteCascades(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "llama3-code", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	body := routedBody("general", [2]string{"general", "llama3"}, [2]string{"code", "llama3-code"})
	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", body)
	if resp.Code != http.StatusOK {
		t.Fatalf("create routed alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var row aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Kind != aliasKindRouted {
		t.Fatalf("kind = %q, want %q", row.Kind, aliasKindRouted)
	}
	if row.DefaultRoute != "general" {
		t.Fatalf("default_route = %q, want general", row.DefaultRoute)
	}
	if row.Classifier == nil || row.Classifier.Kind != "rules" || row.Classifier.ModelID != nil {
		t.Fatalf("classifier = %+v, want rules with no model_id", row.Classifier)
	}
	if len(row.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2 entries", row.Routes)
	}
	if row.Routes[0].RouteName != "general" || row.Routes[0].TargetModelID != "llama3" {
		t.Fatalf("routes[0] = %+v", row.Routes[0])
	}
	if !row.Routes[0].TargetExists || !row.Routes[0].TargetEnabled || !row.Routes[0].TargetAvailable {
		t.Fatalf("routes[0] target_* = %+v, want all true", row.Routes[0])
	}
	if row.Routes[0].TargetBackendClass == nil || *row.Routes[0].TargetBackendClass != backendClassSelfHosted {
		t.Fatalf("routes[0] target_backend_class = %v, want self_hosted", row.Routes[0].TargetBackendClass)
	}
	if row.Routes[1].Keywords[0] != "code" {
		t.Fatalf("routes[1] keywords = %v, want normalized [code]", row.Routes[1].Keywords)
	}

	// ListAliases must return the same shape.
	resp = doJSON(t, r, http.MethodGet, "/api/v1/model-aliases", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("list: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var aliases []aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &aliases); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(aliases) != 1 || aliases[0].Kind != aliasKindRouted || len(aliases[0].Routes) != 2 {
		t.Fatalf("list aliases = %+v", aliases)
	}

	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activity_events WHERE event_type = $1 AND detail->>'kind' = 'routed' AND (detail->>'route_count')::int = 2`,
		string(activity.ModelAliasCreated),
	).Scan(&eventCount); err != nil {
		t.Fatalf("count created events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("expected 1 model_alias.created event with kind/route_count detail, got %d", eventCount)
	}

	// DELETE must cascade the routes (FK ON DELETE CASCADE, migration 013).
	resp = doJSON(t, r, http.MethodDelete, "/api/v1/model-aliases/smart", nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var routeCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_alias_routes WHERE alias = 'smart'`).Scan(&routeCount); err != nil {
		t.Fatalf("count routes after delete: %v", err)
	}
	if routeCount != 0 {
		t.Fatalf("expected routes to cascade-delete, found %d", routeCount)
	}

	var deleteDetail []byte
	if err := pool.QueryRow(ctx,
		`SELECT detail FROM activity_events WHERE event_type = $1`, string(activity.ModelAliasDeleted),
	).Scan(&deleteDetail); err != nil {
		t.Fatalf("fetch delete event detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(deleteDetail, &detail); err != nil {
		t.Fatalf("decode delete detail: %v", err)
	}
	if detail["kind"] != aliasKindRouted || detail["route_count"] != float64(2) {
		t.Fatalf("delete event detail = %+v, want kind=routed route_count=2", detail)
	}
}

func TestPutAlias_Routed_SemanticRouterClassifierEchoedBack(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	body := routedBody("general", [2]string{"general", "llama3"})
	body["classifier"] = map[string]any{"kind": "semantic_router"}

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var row aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Classifier == nil || row.Classifier.Kind != "semantic_router" || row.Classifier.ModelID != nil {
		t.Fatalf("classifier = %+v, want semantic_router with no model_id", row.Classifier)
	}
}

func TestPutAlias_Routed_ValidationErrors(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "claude-3-sonnet", backendClassCloudManaged, true)
	insertModel(t, ctx, pool, "not-self-hosted", backendClassCloudManaged, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	// Alias-of-alias: create a base static alias first, then try to route to it.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/base", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("setup base alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
	}{
		{"no routes", func() map[string]any { b := routedBody("general"); return b }(), http.StatusBadRequest},
		{"default_route not in routes", routedBody("missing", [2]string{"general", "llama3"}), http.StatusBadRequest},
		{"duplicate route names", routedBody("general", [2]string{"general", "llama3"}, [2]string{"general", "claude-3-sonnet"}), http.StatusBadRequest},
		{"target not found", routedBody("general", [2]string{"general", "does-not-exist"}), http.StatusNotFound},
		{"target is itself an alias", routedBody("general", [2]string{"general", "base"}), http.StatusBadRequest},
		{"classifier model not self_hosted", func() map[string]any {
			b := routedBody("general", [2]string{"general", "llama3"})
			b["classifier"] = map[string]any{"kind": "model", "model_id": "not-self-hosted"}
			return b
		}(), http.StatusBadRequest},
		{"classifier model not found", func() map[string]any {
			b := routedBody("general", [2]string{"general", "llama3"})
			b["classifier"] = map[string]any{"kind": "model", "model_id": "does-not-exist"}
			return b
		}(), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", tc.body)
			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
		})
	}
}

func TestPutAlias_Routed_ClassifierModelSelfHostedSucceeds(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "classifier-model", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	body := routedBody("general", [2]string{"general", "llama3"})
	body["classifier"] = map[string]any{"kind": "model", "model_id": "classifier-model"}

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var row aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Classifier == nil || row.Classifier.Kind != "model" || row.Classifier.ModelID == nil || *row.Classifier.ModelID != "classifier-model" {
		t.Fatalf("classifier = %+v", row.Classifier)
	}
}

func TestPutAlias_Routed_ResidencyConflict409(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "claude-3-sonnet", backendClassCloudManaged, true)
	if _, err := pool.Exec(ctx, `UPDATE routing_policy SET residency = 'self_hosted_only' WHERE id = true`); err != nil {
		t.Fatalf("set residency: %v", err)
	}
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	body := routedBody("general", [2]string{"general", "llama3"}, [2]string{"cloud", "claude-3-sonnet"})
	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", body)
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}
	if msg := decodeErrorBody(t, resp); !strings.Contains(msg, "cloud") || !strings.Contains(msg, "claude-3-sonnet") {
		t.Fatalf("error = %q, want it to name the conflicting route and target", msg)
	}

	// No route conflicts with residency -> succeeds even under self_hosted_only.
	body = routedBody("general", [2]string{"general", "llama3"})
	resp = doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart2", body)
	if resp.Code != http.StatusOK {
		t.Fatalf("all-self-hosted routed alias under self_hosted_only: status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestPutAlias_KindSwitch_RoutedToStaticClearsRoutes(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/flip", routedBody("general", [2]string{"general", "llama3"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("create routed: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/flip", map[string]string{"target_model_id": "llama3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("switch to static: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var row aliasResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Kind != aliasKindStatic || row.TargetModelID != "llama3" || len(row.Routes) != 0 {
		t.Fatalf("expected a clean static alias after kind switch, got %+v", row)
	}

	var routeCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_alias_routes WHERE alias = 'flip'`).Scan(&routeCount); err != nil {
		t.Fatalf("count routes: %v", err)
	}
	if routeCount != 0 {
		t.Fatalf("expected routes cleared on kind switch to static, found %d", routeCount)
	}
}

func TestPutRoutingPolicy_WarningsListCloudManagedRoutes(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "claude-3-sonnet", backendClassCloudManaged, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	// Create the routed alias while residency is still "any", so the PUT
	// itself doesn't 409 -- the warning fires later, on the residency flip.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart",
		routedBody("general", [2]string{"general", "llama3"}, [2]string{"cloud", "claude-3-sonnet"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("create routed alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]string{"residency": residencySelfHostedOnly})
	if resp.Code != http.StatusOK {
		t.Fatalf("set residency: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var policy routingPolicyResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &policy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "alias smart: route 'cloud' → claude-3-sonnet will be refused"
	found := false
	for _, w := range policy.Warnings {
		if w == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want to contain %q", policy.Warnings, want)
	}

	// Flipping back to "any" must not carry stale warnings. Decoded into a
	// fresh variable -- json.Unmarshal only overwrites keys present in the
	// input, and "warnings" is omitted (omitempty) on this response, so
	// reusing `policy` here would silently keep the previous response's value.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]string{"residency": residencyAny})
	if resp.Code != http.StatusOK {
		t.Fatalf("reset residency: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var resetPolicy routingPolicyResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &resetPolicy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resetPolicy.Warnings) != 0 {
		t.Fatalf("expected no warnings when residency = any, got %v", resetPolicy.Warnings)
	}
}

func TestTestAlias_AliasNotFound(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPost, "/api/v1/model-aliases/does-not-exist/test", map[string]string{"text": "hello"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", resp.Code, resp.Body.String())
	}
}

func TestTestAlias_TextValidation(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	h := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", routedBody("general", [2]string{"general", "llama3"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("setup routed alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodPost, "/api/v1/model-aliases/smart/test", map[string]string{"text": ""})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("empty text: status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodPost, "/api/v1/model-aliases/smart/test", map[string]string{"text": strings.Repeat("a", aliasTestMaxTextBytes+1)})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("oversized text: status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

func TestTestAlias_ProxiesToGatewayDryrunVerbatim(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)

	var gotPath, gotAuth, gotUserID, gotRequestID string
	var gotBody map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotAuth = req.Header.Get("Authorization")
		gotUserID = req.Header.Get("X-Darkplum-User-Id")
		gotRequestID = req.Header.Get("X-Request-Id")
		_ = json.NewDecoder(req.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"route_name":"general","target_model_id":"llama3","reason":"rules:default","latency_ms":1,"would_be_refused":null}`))
	}))
	defer gw.Close()

	h := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "dp_test_service_key")

	// A router that sets both user_id and request_id in the gin context, like
	// production's session middleware + requestID() (cmd/main.go) together
	// do, so this test can assert TestAlias propagates X-Request-Id too.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Set("request_id", c.GetHeader("X-Request-Id"))
		c.Next()
	})
	r.PUT("/api/v1/model-aliases/:alias", h.PutAlias)
	r.POST("/api/v1/model-aliases/:alias/test", h.TestAlias)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", routedBody("general", [2]string{"general", "llama3"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("setup routed alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/model-aliases/smart/test", bytes.NewReader(mustJSON(t, map[string]string{"text": "hello there"})))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "req-123")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/routing/dryrun" {
		t.Errorf("gateway path = %q, want /v1/routing/dryrun", gotPath)
	}
	if gotAuth != "Bearer dp_test_service_key" {
		t.Errorf("Authorization = %q, want service key bearer token", gotAuth)
	}
	if gotUserID != userID {
		t.Errorf("X-Darkplum-User-Id = %q, want %q", gotUserID, userID)
	}
	if gotRequestID != "req-123" {
		t.Errorf("X-Request-Id = %q, want req-123 passed through", gotRequestID)
	}
	if gotBody["alias"] != "smart" || gotBody["text"] != "hello there" {
		t.Errorf("gateway request body = %+v", gotBody)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["route_name"] != "general" {
		t.Errorf("response body not passed through verbatim: %+v", body)
	}
}

func TestTestAlias_ProxiesGatewayErrorStatusVerbatim(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"alias is not routed","type":"invalid_request_error","code":"alias_not_routed"}}`))
	}))
	defer gw.Close()

	h := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "dp_test_service_key")
	r := newRoutingTestRouter(h, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/model-aliases/smart", routedBody("general", [2]string{"general", "llama3"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("setup routed alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = doJSON(t, r, http.MethodPost, "/api/v1/model-aliases/smart/test", map[string]string{"text": "hello"})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 passed through from the gateway; body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "alias_not_routed") {
		t.Fatalf("body not passed through verbatim: %s", resp.Body.String())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
