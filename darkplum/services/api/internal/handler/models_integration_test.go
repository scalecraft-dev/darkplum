package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

func newModelsTestRouter(h *ModelHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v1/models", h.ListModels)
	return r
}

func TestListModels_SelfHostedOnlyFiltersCloudManaged(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "claude-3-sonnet", backendClassCloudManaged, true)
	insertModel(t, ctx, pool, "disabled-model", backendClassSelfHosted, false)

	// alias -> enabled cloud_managed model (should be omitted under self_hosted_only)
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_aliases (alias, target_model_id) VALUES ('smart', 'claude-3-sonnet')
	`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}
	// alias -> enabled self_hosted model (should always be present)
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_aliases (alias, target_model_id) VALUES ('default', 'llama3')
	`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	h := NewModelHandler("http://unused.invalid", "unused", pool)
	r := newModelsTestRouter(h)

	// residency = any (default): everything enabled shows up.
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var listResp openAIModelsResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range listResp.Data {
		ids[e.ID] = true
	}
	for _, want := range []string{"llama3", "claude-3-sonnet", "smart", "default"} {
		if !ids[want] {
			t.Errorf("residency=any: expected %q in model list, got %v", want, ids)
		}
	}
	if ids["disabled-model"] {
		t.Errorf("residency=any: disabled model leaked into list")
	}

	// residency = self_hosted_only: cloud_managed model and its alias vanish.
	if _, err := pool.Exec(ctx, `UPDATE routing_policy SET residency = 'self_hosted_only' WHERE id = true`); err != nil {
		t.Fatalf("set residency: %v", err)
	}
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids = map[string]bool{}
	for _, e := range listResp.Data {
		ids[e.ID] = true
	}
	if ids["claude-3-sonnet"] || ids["smart"] {
		t.Fatalf("self_hosted_only: cloud_managed model/alias leaked into list: %v", ids)
	}
	if !ids["llama3"] || !ids["default"] {
		t.Fatalf("self_hosted_only: expected self-hosted model+alias still present, got %v", ids)
	}
}

func TestSyncModels_PreservesEnabledAndDisplayNameUpdatesBackendClass(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	if _, err := pool.Exec(ctx, `UPDATE models SET display_name = 'My Llama' WHERE model_id = 'llama3'`); err != nil {
		t.Fatalf("set display_name: %v", err)
	}

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayModelsResponse{
			Object: "list",
			Data: []openAIModelEntry{
				// Re-discovered with a changed provider/backend_class — provider
				// and backend_class should update; enabled/display_name must not.
				{ID: "llama3", Object: "model", Created: 1, OwnedBy: "bedrock", BackendClass: backendClassCloudManaged},
				// A brand new model with no backend_class field at all — must
				// default to self_hosted, not be dropped or misclassified.
				{ID: "new-model", Object: "model", Created: 1, OwnedBy: "vllm"},
			},
		})
	}))
	defer gw.Close()

	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	var provider, backendClass string
	var enabled bool
	var displayName *string
	if err := pool.QueryRow(ctx,
		`SELECT provider, backend_class, enabled, display_name FROM models WHERE model_id = 'llama3'`,
	).Scan(&provider, &backendClass, &enabled, &displayName); err != nil {
		t.Fatalf("fetch llama3: %v", err)
	}
	if provider != "bedrock" {
		t.Errorf("provider = %q, want bedrock (should refresh on conflict)", provider)
	}
	if backendClass != backendClassCloudManaged {
		t.Errorf("backend_class = %q, want cloud_managed (should refresh on conflict)", backendClass)
	}
	if !enabled {
		t.Errorf("enabled = false, want true (must never be touched by sync)")
	}
	if displayName == nil || *displayName != "My Llama" {
		t.Errorf("display_name = %v, want \"My Llama\" (must never be touched by sync)", displayName)
	}

	var newEnabled bool
	var newBackendClass string
	if err := pool.QueryRow(ctx,
		`SELECT enabled, backend_class FROM models WHERE model_id = 'new-model'`,
	).Scan(&newEnabled, &newBackendClass); err != nil {
		t.Fatalf("fetch new-model: %v", err)
	}
	if newEnabled {
		t.Errorf("new-model enabled = true, want false (schema default for newly discovered models)")
	}
	if newBackendClass != backendClassSelfHosted {
		t.Errorf("new-model backend_class = %q, want self_hosted (default when gateway omits the field)", newBackendClass)
	}
}

// emptySyncServer returns a gateway stub whose /v1/models reports zero
// models, simulating a gateway that is up but hasn't (re-)discovered
// anything this tick — e.g. because a vLLM backend is still cold-starting.
func emptySyncServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayModelsResponse{Object: "list", Data: nil})
	}))
}

// TestSyncModels_NotRediscoveredSurvivesAndBecomesUnavailable is the
// regression test for the cold-start bug: a model that isn't re-discovered
// within the old 1-minute window must NOT be deleted (which used to drop the
// admin's enabled flag and dangle any alias pointing at it), but it must
// still stop showing up in the chat-facing list once it's stale, via
// available=false rather than deletion.
func TestSyncModels_NotRediscoveredSurvivesAndBecomesUnavailable(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)

	gw := emptySyncServer(t)
	defer gw.Close()
	h := NewModelHandler(gw.URL, "unused", pool)

	// A sync that rediscovers nothing must not touch the row at all.
	h.SyncModels(ctx)

	var enabled bool
	var discoveredAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT enabled, discovered_at FROM models WHERE model_id = 'llama3'`,
	).Scan(&enabled, &discoveredAt); err != nil {
		t.Fatalf("expected llama3 row to survive an empty sync, got: %v", err)
	}
	if !enabled {
		t.Fatalf("expected enabled to remain true after an empty sync, got false")
	}

	// Push discovered_at back past the 2-minute availability cutoff (but well
	// under the 7-day purge window) to simulate time passing without a
	// rediscovery, without waiting in the test.
	if _, err := pool.Exec(ctx,
		`UPDATE models SET discovered_at = now() - interval '10 minutes' WHERE model_id = 'llama3'`,
	); err != nil {
		t.Fatalf("backdate discovered_at: %v", err)
	}

	// The row must still exist and still be enabled...
	if err := pool.QueryRow(ctx,
		`SELECT enabled FROM models WHERE model_id = 'llama3'`,
	).Scan(&enabled); err != nil {
		t.Fatalf("expected llama3 row to still exist after backdating, got: %v", err)
	}
	if !enabled {
		t.Fatalf("expected enabled to still be true after backdating discovered_at")
	}

	// ...but must be hidden from the chat-facing list (available = false).
	r := newModelsTestRouter(h)
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var listResp openAIModelsResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range listResp.Data {
		if e.ID == "llama3" {
			t.Fatalf("expected stale-but-enabled llama3 to be hidden from /v1/models, got it in the list")
		}
	}

	// Admin view must still show it, with available = false and enabled true.
	adminRouter := gin.New()
	gin.SetMode(gin.TestMode)
	adminRouter.GET("/api/v1/models", h.ListAdminModels)
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	adminRouter.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("admin list status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var admin []modelRow
	if err := json.Unmarshal(resp.Body.Bytes(), &admin); err != nil {
		t.Fatalf("decode admin list: %v", err)
	}
	found := false
	for _, m := range admin {
		if m.ModelID == "llama3" {
			found = true
			if !m.Enabled {
				t.Fatalf("expected admin row to still show enabled=true, got %+v", m)
			}
			if m.Available {
				t.Fatalf("expected admin row to show available=false after the cutoff, got %+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("expected llama3 row to still be present in the admin list")
	}

	// Re-discovery (backend finished cold-starting) must flip available back
	// to true without touching enabled.
	gw2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayModelsResponse{
			Object: "list",
			Data:   []openAIModelEntry{{ID: "llama3", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted}},
		})
	}))
	defer gw2.Close()
	h2 := NewModelHandler(gw2.URL, "unused", pool)
	h2.SyncModels(ctx)

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(resp, req)
	if err := json.Unmarshal(resp.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	reappeared := false
	for _, e := range listResp.Data {
		if e.ID == "llama3" {
			reappeared = true
		}
	}
	if !reappeared {
		t.Fatalf("expected llama3 to reappear in /v1/models after re-discovery")
	}
	if err := pool.QueryRow(ctx, `SELECT enabled FROM models WHERE model_id = 'llama3'`).Scan(&enabled); err != nil {
		t.Fatalf("fetch llama3 after re-discovery: %v", err)
	}
	if !enabled {
		t.Fatalf("expected enabled to still be true after re-discovery")
	}
}

// TestSyncModels_PurgesOnlyAfterSevenDays verifies the purge window: a row
// unseen for just over the old 1-minute cutoff must survive, and only a row
// unseen for more than 7 days is actually deleted.
func TestSyncModels_PurgesOnlyAfterSevenDays(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	insertModel(t, ctx, pool, "recently-stale", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "long-gone", backendClassSelfHosted, true)

	if _, err := pool.Exec(ctx,
		`UPDATE models SET discovered_at = now() - interval '10 minutes' WHERE model_id = 'recently-stale'`,
	); err != nil {
		t.Fatalf("backdate recently-stale: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE models SET discovered_at = now() - interval '8 days' WHERE model_id = 'long-gone'`,
	); err != nil {
		t.Fatalf("backdate long-gone: %v", err)
	}

	gw := emptySyncServer(t)
	defer gw.Close()
	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM models WHERE model_id = 'recently-stale'`).Scan(&count); err != nil {
		t.Fatalf("count recently-stale: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected recently-stale (10 min unseen) to survive, got count=%d", count)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM models WHERE model_id = 'long-gone'`).Scan(&count); err != nil {
		t.Fatalf("count long-gone: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected long-gone (8 days unseen) to be purged, got count=%d", count)
	}
}

// TestSyncModels_WarnsWhenDiscoveredModelShadowsAlias covers item 4: a
// discovered model id that equals an existing alias name is not a sync
// failure (the gateway lets the alias win by design), but it must be logged
// as a WARNING naming the shadowed alias so the admin notices.
func TestSyncModels_WarnsWhenDiscoveredModelShadowsAlias(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_aliases (alias, target_model_id) VALUES ('smart', 'llama3')`,
	); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayModelsResponse{
			Object: "list",
			// "smart" collides with the existing alias name.
			Data: []openAIModelEntry{{ID: "smart", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted}},
		})
	}))
	defer gw.Close()

	logs := captureLogs(t)
	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	// The sync must not fail: the shadowing model row is still inserted.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM models WHERE model_id = 'smart'`).Scan(&count); err != nil {
		t.Fatalf("count smart model: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the shadowing model row to be inserted despite the alias collision, got count=%d", count)
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, "level=WARN") {
		t.Fatalf("expected a WARNING to be logged, got logs: %s", logOutput)
	}
	if !strings.Contains(logOutput, "smart") {
		t.Fatalf("expected the warning to name the shadowed alias %q, got logs: %s", "smart", logOutput)
	}
}

// One row the database rejects must not abort the whole sync transaction: the
// other discovered models still land. A NUL byte is valid JSON but Postgres
// refuses it in a TEXT column, which makes that one upsert fail — and since
// the batch upsert is a single statement over the whole discovered set, the
// bad row fails the entire batch, which must trigger the per-row fallback
// path (asserted below via the log line) rather than losing the good rows.
func TestSyncModels_OneBadRowDoesNotAbortTheSync(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayModelsResponse{
			Object: "list",
			Data: []openAIModelEntry{
				{ID: "good-before", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
				{ID: "bad\x00model", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
				{ID: "good-after", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
			},
		})
	}))
	defer gw.Close()

	logs := captureLogs(t)
	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM models WHERE model_id IN ('good-before', 'good-after')`,
	).Scan(&count); err != nil {
		t.Fatalf("count models: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected both valid models to be synced despite the rejected row, got %d", count)
	}

	// The bad row must have failed the batch statement as a whole and forced
	// the per-row fallback — prove the fallback path actually ran rather than
	// the batch silently succeeding some other way.
	logOutput := logs.String()
	if !strings.Contains(logOutput, "falling back to per-row upserts") {
		t.Fatalf("expected a log line proving the per-row fallback ran, got logs: %s", logOutput)
	}
}

// TestSyncModels_HappyPathUsesBatchUpsert is the counterpart to
// TestSyncModels_OneBadRowDoesNotAbortTheSync: a sync where every row is
// valid must go through the single set-based batch upsert, not the per-row
// fallback, and must produce correct inserted/updated counts for a mix of
// brand-new and already-known models.
func TestSyncModels_HappyPathUsesBatchUpsert(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	// Pre-existing rows: re-discovering these should count as "updated".
	insertModel(t, ctx, pool, "existing-1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "existing-2", backendClassSelfHosted, false)

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayModelsResponse{
			Object: "list",
			Data: []openAIModelEntry{
				{ID: "existing-1", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
				{ID: "existing-2", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
				{ID: "brand-new-1", Object: "model", Created: 1, OwnedBy: "vllm", BackendClass: backendClassSelfHosted},
				{ID: "brand-new-2", Object: "model", Created: 1, OwnedBy: "bedrock", BackendClass: backendClassCloudManaged},
			},
		})
	}))
	defer gw.Close()

	logs := captureLogs(t)
	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	logOutput := logs.String()
	if !strings.Contains(logOutput, "batch upsert succeeded") {
		t.Fatalf("expected the happy-path sync to log that the batch upsert succeeded, got logs: %s", logOutput)
	}
	if strings.Contains(logOutput, "falling back to per-row upserts") {
		t.Fatalf("expected the happy-path sync to NOT fall back to per-row upserts, got logs: %s", logOutput)
	}
	if !strings.Contains(logOutput, "inserted=2") {
		t.Fatalf("expected the batch success log to report inserted=2, got logs: %s", logOutput)
	}
	if !strings.Contains(logOutput, "updated=2") {
		t.Fatalf("expected the batch success log to report updated=2, got logs: %s", logOutput)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM models WHERE model_id IN ('existing-1', 'existing-2', 'brand-new-1', 'brand-new-2')`,
	).Scan(&count); err != nil {
		t.Fatalf("count models: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected all 4 models present after sync, got %d", count)
	}

	// Pre-existing settings (enabled) must survive the batch resync just like
	// the per-row path guarantees.
	var enabled1, enabled2 bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM models WHERE model_id = 'existing-1'`).Scan(&enabled1); err != nil {
		t.Fatalf("fetch existing-1: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT enabled FROM models WHERE model_id = 'existing-2'`).Scan(&enabled2); err != nil {
		t.Fatalf("fetch existing-2: %v", err)
	}
	if !enabled1 {
		t.Errorf("existing-1 enabled = false, want true (must survive batch resync)")
	}
	if enabled2 {
		t.Errorf("existing-2 enabled = true, want false (must survive batch resync)")
	}

	// New rows default to enabled = false regardless of backend_class.
	var newEnabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM models WHERE model_id = 'brand-new-1'`).Scan(&newEnabled); err != nil {
		t.Fatalf("fetch brand-new-1: %v", err)
	}
	if newEnabled {
		t.Errorf("brand-new-1 enabled = true, want false (schema default)")
	}
}

// TestListModels_RoutedAlias_EndToEnd exercises the real SQL grouping in
// ModelHandler.ListModels (not just the pure buildModelsList logic): a routed
// alias created through RoutingHandler.PutAlias must show up in the
// chat-facing /v1/models list with alias_kind, alias_of, and a routes[]
// summary, and disappear once every route's target is disabled.
func TestListModels_RoutedAlias_EndToEnd(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "llama3-code", backendClassSelfHosted, true)

	routingH := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	routingRouter := newRoutingTestRouter(routingH, userID)
	resp := doJSON(t, routingRouter, http.MethodPut, "/api/v1/model-aliases/smart",
		routedBody("general", [2]string{"general", "llama3"}, [2]string{"code", "llama3-code"}))
	if resp.Code != http.StatusOK {
		t.Fatalf("create routed alias: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	h := NewModelHandler("http://unused.invalid", "unused", pool)
	r := newModelsTestRouter(h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var listResp openAIModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var entry *openAIModelEntry
	for i := range listResp.Data {
		if listResp.Data[i].ID == "smart" {
			entry = &listResp.Data[i]
		}
	}
	if entry == nil {
		t.Fatalf("routed alias 'smart' missing from /v1/models: %+v", listResp.Data)
	}
	if entry.AliasKind != aliasKindRouted {
		t.Fatalf("alias_kind = %q, want routed", entry.AliasKind)
	}
	if entry.AliasOf == nil || *entry.AliasOf != "llama3" {
		t.Fatalf("alias_of = %v, want llama3 (the default route's target)", entry.AliasOf)
	}
	if len(entry.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2", entry.Routes)
	}

	// Disable both route targets: the alias must disappear entirely.
	if _, err := pool.Exec(ctx, `UPDATE models SET enabled = false WHERE model_id IN ('llama3', 'llama3-code')`); err != nil {
		t.Fatalf("disable targets: %v", err)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range listResp.Data {
		if e.ID == "smart" {
			t.Fatalf("expected routed alias to disappear once all targets are disabled, got %+v", e)
		}
	}
}
