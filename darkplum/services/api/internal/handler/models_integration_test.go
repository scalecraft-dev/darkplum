package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
