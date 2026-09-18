package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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
