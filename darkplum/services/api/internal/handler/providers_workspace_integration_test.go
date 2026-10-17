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

const testAnthropicKey = "sk-ant-abcdefghijklmnopqrstuvwx"

func okVerify(gw *fakeProviderGateway) {
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
}

func TestWorkspaceID_PutKeyPersistsMirrorsAuditsAndVerifies(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	okVerify(gw)
	var verifyBody []byte
	gw.verifyFunc = func(_ string, body []byte) (int, any) {
		verifyBody = body
		return 200, gw.verifyBody
	}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key": testAnthropicKey, "model_filter": []string{"claude-*"}, "workspace_id": "wrkspc_01AbC-d",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(string(verifyBody), `"workspace_id":"wrkspc_01AbC-d"`) {
		t.Errorf("gateway verify did not carry the workspace id: %s", verifyBody)
	}
	var ws *string
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM vendor_provider_settings WHERE vendor='anthropic'`).Scan(&ws); err != nil || ws == nil || *ws != "wrkspc_01AbC-d" {
		t.Fatalf("stored workspace_id = %v err=%v", ws, err)
	}
	if got, ok := fk.config["ANTHROPIC_WORKSPACE_ID"]; !ok || got != "wrkspc_01AbC-d" {
		t.Errorf("configmap ANTHROPIC_WORKSPACE_ID = %q (present=%v)", got, ok)
	}
	if fk.config["ANTHROPIC_MODEL_FILTER"] != "claude-*" || fk.config["ANTHROPIC_MODEL_FILTER_SET_AT"] == "" {
		t.Errorf("filter+stamp must be mirrored with it: %v", fk.config)
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail::text FROM activity_events WHERE event_type=$1`, string(activity.ProviderKeyInstalled)).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(detail), &d)
	if d["workspace_id_before"] != "" || d["workspace_id_after"] != "wrkspc_01AbC-d" {
		t.Errorf("audit detail = %s", detail)
	}
}

func TestWorkspaceID_AbsentKeepsExplicitEmptyClears(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	okVerify(gw)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))
	insertVendorSettings(t, ctx, pool, "anthropic", []string{"claude-*"}, time.Now().Add(-time.Hour), nil)
	if _, err := pool.Exec(ctx, `UPDATE vendor_provider_settings SET workspace_id='wrkspc_keep' WHERE vendor='anthropic'`); err != nil {
		t.Fatal(err)
	}
	read := func() *string {
		var ws *string
		if err := pool.QueryRow(ctx, `SELECT workspace_id FROM vendor_provider_settings WHERE vendor='anthropic'`).Scan(&ws); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	// Absent: kept in the row AND re-mirrored with the filter.
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{"model_filter": []string{"claude-opus-*"}}); resp.Code != 200 {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	if ws := read(); ws == nil || *ws != "wrkspc_keep" {
		t.Fatalf("absent workspace_id must keep the current one, got %v", ws)
	}
	if fk.config["ANTHROPIC_WORKSPACE_ID"] != "wrkspc_keep" {
		t.Errorf("mirror = %q", fk.config["ANTHROPIC_WORKSPACE_ID"])
	}
	// null behaves like absent.
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{"model_filter": []string{"claude-*"}, "workspace_id": nil}); resp.Code != 200 {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	if ws := read(); ws == nil || *ws != "wrkspc_keep" {
		t.Fatalf("null workspace_id must keep, got %v", ws)
	}

	// Explicit "": cleared in the row (NULL) and in the mirror (empty string present).
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{"model_filter": []string{"claude-*"}, "workspace_id": ""}); resp.Code != 200 {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	if ws := read(); ws != nil {
		t.Fatalf("explicit empty must clear to NULL, got %q", *ws)
	}
	if v, ok := fk.config["ANTHROPIC_WORKSPACE_ID"]; !ok || v != "" {
		t.Errorf("clearing must write an empty ANTHROPIC_WORKSPACE_ID, got %q present=%v", v, ok)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_events WHERE event_type=$1 AND detail ? 'workspace_id_before' AND detail->>'workspace_id_before'='wrkspc_keep' AND detail->>'workspace_id_after'=''`, string(activity.ProviderFilterChanged)).Scan(&n); err != nil || n != 1 {
		t.Errorf("expected one filter_changed audit with before=wrkspc_keep after='', got %d err=%v", n, err)
	}
}

func TestWorkspaceID_NoWorkspaceNoAuditKeys(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	okVerify(gw)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{"key": testAnthropicKey, "model_filter": []string{"claude-*"}}); resp.Code != 200 {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_events WHERE event_type=$1 AND (detail ? 'workspace_id_before' OR detail ? 'workspace_id_after')`, string(activity.ProviderKeyInstalled)).Scan(&n); err != nil || n != 0 {
		t.Errorf("workspace audit keys must be omitted when both empty, got %d err=%v", n, err)
	}
}

func TestWorkspaceID_ValidationAndOpenAIRejection(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	okVerify(gw)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	routes := []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPost, "/api/v1/providers/%s/verify", map[string]any{"key": testAnthropicKey}},
		{http.MethodPut, "/api/v1/providers/%s/key", map[string]any{"key": testAnthropicKey, "model_filter": []string{"x-*"}}},
		{http.MethodPut, "/api/v1/providers/%s/filter", map[string]any{"model_filter": []string{"x-*"}}},
	}
	for _, rt := range routes {
		for _, bad := range []string{"has space", "semi;colon", "x\r\nY: 1", strings.Repeat("a", 65)} {
			body := map[string]any{"workspace_id": bad}
			for k, v := range rt.body {
				body[k] = v
			}
			resp := doJSON(t, r, rt.method, strings.Replace(rt.path, "%s", "anthropic", 1), body)
			if resp.Code != 400 || !strings.Contains(resp.Body.String(), `"invalid_workspace_id"`) {
				t.Errorf("%s %q: got %d %s", rt.path, bad, resp.Code, resp.Body.String())
			}
		}
		// openai: any presence (even "") is invalid_request.
		for _, v := range []string{"wrkspc_01", ""} {
			body := map[string]any{"workspace_id": v}
			for k, val := range rt.body {
				body[k] = val
			}
			resp := doJSON(t, r, rt.method, strings.Replace(rt.path, "%s", "openai", 1), body)
			if resp.Code != 400 || !strings.Contains(resp.Body.String(), `"invalid_request"`) {
				t.Errorf("openai %s %q: got %d %s", rt.path, v, resp.Code, resp.Body.String())
			}
		}
	}
	if gw.getVerifyCalls() != 0 || len(fk.secrets) != 0 || fk.setConfigCalls != 0 {
		t.Errorf("rejected requests must change/call nothing: verify=%d secrets=%d config=%d", gw.getVerifyCalls(), len(fk.secrets), fk.setConfigCalls)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vendor_provider_settings`).Scan(&n); err != nil || n != 0 {
		t.Errorf("nothing may be stored, got %d rows", n)
	}
}

func TestWorkspaceID_OpenAIRowNeverGetsWorkspaceOrMirrorKey(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	okVerify(gw)
	var body []byte
	gw.verifyFunc = func(_ string, b []byte) (int, any) { body = b; return 200, gw.verifyBody }
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/openai/key", map[string]any{"key": "sk-abcdefghijklmnopqrstuvwxyz", "model_filter": []string{"gpt-*"}}); resp.Code != 200 {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	if _, ok := fk.config["ANTHROPIC_WORKSPACE_ID"]; ok || strings.Contains(string(body), "workspace_id") {
		t.Errorf("openai install must not touch workspace: config=%v verify=%s", fk.config, body)
	}
}

func TestWorkspaceRequired_PassThroughOnVerifyPutKeyPutFilter(t *testing.T) {
	const sentinel = "SENTINEL-vendor-text-4242"
	routes := []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPost, "/api/v1/providers/anthropic/verify", map[string]any{"key": testAnthropicKey}},
		{http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{"key": testAnthropicKey, "model_filter": []string{"claude-*"}}},
		{http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{"model_filter": []string{"claude-*"}}},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			pool := setupTestDB(t)
			gw := newFakeProviderGateway(t)
			gw.verifyStatus = http.StatusUnprocessableEntity
			gw.verifyBody = map[string]any{"error": map[string]any{"code": "workspace_required", "message": sentinel}}
			ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
			r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

			resp := doJSON(t, r, rt.method, rt.path, rt.body)
			want := `{"error":{"code":"workspace_required","message":"This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace."}}`
			if resp.Code != http.StatusUnprocessableEntity || strings.TrimSpace(resp.Body.String()) != want {
				t.Fatalf("got %d %s", resp.Code, resp.Body.String())
			}
			if len(fk.secrets) != 0 || fk.setConfigCalls != 0 {
				t.Error("nothing may be stored on workspace_required")
			}
		})
	}
}

func TestWorkspaceID_ProvidersFieldOnAdminModels(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[],"providers":[
			{"name":"anthropic","configured":true,"state":"ok","model_count":1,"last_sync_at":null,"filter_source":"db","filter":["claude-*"],"workspace_id":"wrkspc_01"},
			{"name":"openai","configured":false,"state":"not_configured","model_count":0,"last_sync_at":null,"workspace_id":""}]}`))
	}))
	defer gw.Close()
	h := NewModelHandler(gw.URL, "unused", pool)
	h.SyncModels(ctx)

	router := gin.New()
	gin.SetMode(gin.TestMode)
	router.GET("/api/v1/models", h.ListAdminModels)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set(darkplumClassesHeader, backendClassExternalAPI)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var raw struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, p := range raw.Providers {
		v, present := p["workspace_id"]
		if !present {
			t.Fatalf("providers[] entry lacks workspace_id: %v", p)
		}
		got[p["name"].(string)] = v
	}
	if got["anthropic"] != "wrkspc_01" || got["openai"] != nil {
		t.Errorf("workspace_id values = %v (want wrkspc_01 / null)", got)
	}
}

func TestCurrentWorkspaceID_PrefersRowUnlessEnvWins(t *testing.T) {
	ws := func(s string) *string { return &s }
	provs := func(src string, id *string) []gatewayProviderStatus {
		return []gatewayProviderStatus{{Name: "anthropic", FilterSource: src, WorkspaceID: id}}
	}
	cases := []struct {
		name      string
		rowExists bool
		row       *string
		providers []gatewayProviderStatus
		want      string
	}{
		{"row wins over stale cache", true, ws("wrkspc_row"), provs("db", ws("wrkspc_stale")), "wrkspc_row"},
		{"row without workspace", true, nil, provs("db", nil), ""},
		{"env newer than row: gateway effective", true, ws("wrkspc_row"), provs("env", ws("wrkspc_env")), "wrkspc_env"},
		{"no row: gateway effective (env)", false, nil, provs("env", ws("wrkspc_env")), "wrkspc_env"},
		{"no row, no gateway data", false, nil, nil, ""},
	}
	for _, tc := range cases {
		if got := currentWorkspaceID("anthropic", tc.rowExists, tc.row, tc.providers); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	if got := currentWorkspaceID("openai", true, ws("x"), nil); got != "" {
		t.Errorf("openai must never have a workspace, got %q", got)
	}
}
