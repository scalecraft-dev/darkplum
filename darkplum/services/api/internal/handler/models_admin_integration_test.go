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
)

func newAdminModelsTestRouter(h *ModelHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.PATCH("/api/v1/models/:id", h.ToggleModel)
	return r
}

// TestToggleModel_DisplayNameTriState covers DESIGN4's parity item: PATCH
// /api/v1/models/:id accepts an optional display_name (1-100 chars, null
// clears), independent of the always-required enabled flag.
func TestToggleModel_DisplayNameTriState(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, false)

	var id string
	if err := pool.QueryRow(ctx, `SELECT id FROM models WHERE model_id = 'llama3'`).Scan(&id); err != nil {
		t.Fatalf("fetch model id: %v", err)
	}

	h := NewModelHandler("http://unused.invalid", "unused", pool)
	r := newAdminModelsTestRouter(h)

	// Setting enabled without display_name leaves display_name untouched (nil).
	resp := doJSON(t, r, http.MethodPatch, "/api/v1/models/"+id, map[string]any{"enabled": true})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var got modelRow
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.DisplayName != nil {
		t.Fatalf("expected enabled=true, display_name=nil, got %+v", got)
	}

	// Setting display_name.
	resp = doJSON(t, r, http.MethodPatch, "/api/v1/models/"+id, map[string]any{"enabled": true, "display_name": "Llama 3"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got = modelRow{}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DisplayName == nil || *got.DisplayName != "Llama 3" {
		t.Fatalf("expected display_name=\"Llama 3\", got %v", got.DisplayName)
	}

	// Toggling enabled again without display_name must NOT clear it.
	resp = doJSON(t, r, http.MethodPatch, "/api/v1/models/"+id, map[string]any{"enabled": false})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got = modelRow{}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled || got.DisplayName == nil || *got.DisplayName != "Llama 3" {
		t.Fatalf("expected enabled=false and display_name preserved, got %+v", got)
	}

	// Explicit null clears it.
	resp = doJSON(t, r, http.MethodPatch, "/api/v1/models/"+id, map[string]any{"enabled": false, "display_name": nil})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got = modelRow{}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DisplayName != nil {
		t.Fatalf("expected display_name cleared by explicit null, got %v", got.DisplayName)
	}

	// Invalid (too long) display_name is rejected with 400 and no partial write.
	resp = doJSON(t, r, http.MethodPatch, "/api/v1/models/"+id, map[string]any{"enabled": true, "display_name": string(make([]byte, 101))})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

// TestListAdminModels_DefaultShapeIsBareArrayWithoutExternalAPI covers the
// coordinator's wire-contract ruling: without darkplumClassesHeader, GET
// /api/v1/models keeps returning the bare JSON array it always has (no
// breaking shape change for a stale admin bundle or script that expects an
// array), and that array never includes an external_api row.
func TestListAdminModels_DefaultShapeIsBareArrayWithoutExternalAPI(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "claude-3-opus", backendClassExternalAPI, false)

	h := NewModelHandler("http://unused.invalid", "unused", pool)
	adminRouter := gin.New()
	gin.SetMode(gin.TestMode)
	adminRouter.GET("/api/v1/models", h.ListAdminModels)

	resp := doJSON(t, adminRouter, http.MethodGet, "/api/v1/models", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Must decode as a bare array -- decoding into the wrapper object type
	// would fail if the server sent an object instead, so this alone proves
	// the shape.
	var models []modelRow
	if err := json.Unmarshal(resp.Body.Bytes(), &models); err != nil {
		t.Fatalf("expected a bare JSON array by default, decode failed: %v (body = %s)", err, resp.Body.String())
	}
	foundLlama, foundVendor := false, false
	for _, m := range models {
		switch m.ModelID {
		case "llama3":
			foundLlama = true
		case "claude-3-opus":
			foundVendor = true
		}
	}
	if !foundLlama {
		t.Fatal("expected llama3 (self_hosted) present")
	}
	if foundVendor {
		t.Fatalf("expected external_api model hidden without the classes header, got %+v", models)
	}
}

// TestListAdminModels_ClassesHeaderWrapsWithProvidersAndExternalAPI covers
// the ruling's opt-in half: WITH darkplumClassesHeader, the response becomes
// {"data":[...],"providers":[...]}, external_api rows included, and the
// vendor Providers status block passed through from the last sync.
func TestListAdminModels_ClassesHeaderWrapsWithProvidersAndExternalAPI(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertModel(t, ctx, pool, "llama3", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "claude-3-opus", backendClassExternalAPI, false)

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []openAIModelEntry{},
			"providers": []gatewayProviderStatus{
				{Name: "anthropic", Configured: true, State: "ok", ModelCount: 1},
				{Name: "openai", Configured: false, State: "not_configured", ModelCount: 0},
			},
		})
	}))
	defer gw.Close()

	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx) // populate h.providers

	adminRouter := gin.New()
	gin.SetMode(gin.TestMode)
	adminRouter.GET("/api/v1/models", h.ListAdminModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set(darkplumClassesHeader, backendClassExternalAPI)
	rec := httptest.NewRecorder()
	adminRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got adminModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("expected the {data,providers} wrapper with the classes header, decode failed: %v (body = %s)", err, rec.Body.String())
	}
	foundVendor := false
	for _, m := range got.Data {
		if m.ModelID == "claude-3-opus" {
			foundVendor = true
		}
	}
	if !foundVendor {
		t.Fatalf("expected claude-3-opus visible with the classes header, got %+v", got.Data)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("expected the providers block, got %+v", got.Providers)
	}
}

// TestListAdminModels_ProviderFeaturesAndInstallingUntil covers PLAN-UI's
// three GET /api/v1/models additions: provider_features passed through from
// the gateway's last sync, and each providers[] entry's installing_until
// merged in from vendor_provider_settings -- present while in the future,
// null once absent or past (never a stale timestamp).
func TestListAdminModels_ProviderFeaturesAndInstallingUntil(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	future := time.Now().Add(3 * time.Minute)
	past := time.Now().Add(-time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, installing_until)
		VALUES ('anthropic', '{"claude-*"}', now(), $1), ('openai', '{"gpt-*"}', now(), $2)
	`, future, past); err != nil {
		t.Fatalf("seed vendor_provider_settings: %v", err)
	}

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []openAIModelEntry{},
			"providers": []gatewayProviderStatus{
				{Name: "anthropic", Configured: true, State: "ok", FilterSource: "db", Filter: []string{"claude-*"}},
				{Name: "openai", Configured: false, State: "not_configured"},
			},
			"provider_features": []string{"ui_key_install"},
		})
	}))
	defer gw.Close()

	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	adminRouter := gin.New()
	gin.SetMode(gin.TestMode)
	adminRouter.GET("/api/v1/models", h.ListAdminModels)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set(darkplumClassesHeader, backendClassExternalAPI)
	rec := httptest.NewRecorder()
	adminRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got adminModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.ProviderFeatures) != 1 || got.ProviderFeatures[0] != "ui_key_install" {
		t.Fatalf("provider_features = %v", got.ProviderFeatures)
	}

	byName := map[string]gatewayProviderStatus{}
	for _, p := range got.Providers {
		byName[p.Name] = p
	}
	anthropic, ok := byName["anthropic"]
	if !ok {
		t.Fatal("expected an anthropic provider entry")
	}
	if anthropic.FilterSource != "db" || len(anthropic.Filter) != 1 || anthropic.Filter[0] != "claude-*" {
		t.Fatalf("anthropic filter_source/filter = %q/%v, want db/[claude-*]", anthropic.FilterSource, anthropic.Filter)
	}
	if anthropic.InstallingUntil == nil {
		t.Fatal("expected anthropic installing_until to be present (in the future)")
	}
	if !anthropic.InstallingUntil.Equal(future) && anthropic.InstallingUntil.Sub(future).Abs() > time.Second {
		t.Fatalf("anthropic installing_until = %v, want ~%v", anthropic.InstallingUntil, future)
	}

	openai, ok := byName["openai"]
	if !ok {
		t.Fatal("expected an openai provider entry")
	}
	if openai.InstallingUntil != nil {
		t.Fatalf("expected openai installing_until to be nil (its window is in the past), got %v", openai.InstallingUntil)
	}
}

// TestListAdminModels_RemovingUntilAndKeyInstallTransportOK covers the two
// review additions: each providers[] entry's removing_until (present while
// in the future, explicit null once absent or past -- unlike
// installing_until, never omitted), and the top-level
// key_install_transport_ok computed from secureCookies + the request's Host.
func TestListAdminModels_RemovingUntilAndKeyInstallTransportOK(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	future := time.Now().Add(3 * time.Minute)
	past := time.Now().Add(-time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, removing_until)
		VALUES ('anthropic', '{"claude-*"}', now(), $1), ('openai', '{"gpt-*"}', now(), $2)
	`, future, past); err != nil {
		t.Fatalf("seed vendor_provider_settings: %v", err)
	}

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []openAIModelEntry{},
			"providers": []gatewayProviderStatus{
				{Name: "anthropic", Configured: true, State: "ok"},
				{Name: "openai", Configured: false, State: "not_configured"},
			},
		})
	}))
	defer gw.Close()

	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	adminRouter := gin.New()
	gin.SetMode(gin.TestMode)
	adminRouter.GET("/api/v1/models", h.ListAdminModels)

	// secureCookies=false (default zero value) + a non-loopback Host ->
	// key_install_transport_ok must be false.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set(darkplumClassesHeader, backendClassExternalAPI)
	rec := httptest.NewRecorder()
	adminRouter.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got adminModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KeyInstallTransportOK {
		t.Fatal("expected key_install_transport_ok=false: secureCookies=false and Host is not loopback")
	}

	byName := map[string]gatewayProviderStatus{}
	for _, p := range got.Providers {
		byName[p.Name] = p
	}
	anthropic, ok := byName["anthropic"]
	if !ok {
		t.Fatal("expected an anthropic provider entry")
	}
	if anthropic.RemovingUntil == nil {
		t.Fatal("expected anthropic removing_until to be present (in the future)")
	}
	if anthropic.RemovingUntil.Sub(future).Abs() > time.Second {
		t.Fatalf("anthropic removing_until = %v, want ~%v", anthropic.RemovingUntil, future)
	}

	openai, ok := byName["openai"]
	if !ok {
		t.Fatal("expected an openai provider entry")
	}
	if openai.RemovingUntil != nil {
		t.Fatalf("expected openai removing_until to be nil (its window is in the past), got %v", openai.RemovingUntil)
	}
	if !strings.Contains(rec.Body.String(), `"removing_until"`) {
		t.Fatalf("expected removing_until to be an explicit key (even null), body = %s", rec.Body.String())
	}

	// secureCookies=true -> key_install_transport_ok true regardless of Host.
	h.SetSecureCookies(true)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req2.Header.Set(darkplumClassesHeader, backendClassExternalAPI)
	adminRouter.ServeHTTP(rec2, req2)
	var got2 adminModelsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got2.KeyInstallTransportOK {
		t.Fatal("expected key_install_transport_ok=true once secureCookies is true")
	}
}

// TestModelHandler_AnyInstalling covers cmd/main.go's sync-loop dependency:
// SyncModels refreshes the cached "is any vendor mid-install-or-removal"
// flag every run, driven purely by vendor_provider_settings'
// installing_until/removing_until, not by the gateway's response.
func TestModelHandler_AnyInstalling(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []openAIModelEntry{}})
	}))
	defer gw.Close()

	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)
	if h.AnyInstalling() {
		t.Fatal("expected AnyInstalling() false with no vendor_provider_settings rows")
	}

	future := time.Now().Add(2 * time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, installing_until)
		VALUES ('anthropic', '{"claude-*"}', now(), $1)
	`, future); err != nil {
		t.Fatalf("seed installing row: %v", err)
	}
	h.SyncModels(ctx)
	if !h.AnyInstalling() {
		t.Fatal("expected AnyInstalling() true with a future installing_until")
	}

	if _, err := pool.Exec(ctx, `UPDATE vendor_provider_settings SET installing_until = NULL WHERE vendor = 'anthropic'`); err != nil {
		t.Fatalf("clear installing_until: %v", err)
	}
	h.SyncModels(ctx)
	if h.AnyInstalling() {
		t.Fatal("expected AnyInstalling() false after installing_until is cleared")
	}

	// removing_until must drive the same fast-sync flag as installing_until.
	if _, err := pool.Exec(ctx, `UPDATE vendor_provider_settings SET removing_until = $1 WHERE vendor = 'anthropic'`, future); err != nil {
		t.Fatalf("set removing_until: %v", err)
	}
	h.SyncModels(ctx)
	if !h.AnyInstalling() {
		t.Fatal("expected AnyInstalling() true with a future removing_until")
	}
}
