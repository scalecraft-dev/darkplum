package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newSessionModelsTestRouter mirrors newModelsTestRouter (models_integration_test.go)
// but injects a session user_id, exercising ListModels' session-filtered path
// (getSessionEffective) instead of the unfiltered Bearer/no-context path.
func newSessionModelsTestRouter(h *ModelHandler, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	})
	r.GET("/v1/models", h.ListModels)
	return r
}

func TestListModels_SessionUserFilteredByGatewayEffective(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	if _, err := pool.Exec(ctx, `INSERT INTO model_aliases (alias, kind, target_model_id) VALUES ('blocked-alias', 'static', 'm2')`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}

	dm := "m1"
	gw := fakeGatewayEffective(t, gatewayEffectivePolicy{
		AllowedModels: strs("m1"),
		Residency:     residencyAny,
		DefaultModel:  &dm,
	})

	h := NewModelHandler(gw.URL, "testkey", pool)
	r := newSessionModelsTestRouter(h, "some-user-id")

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
	if listResp.DefaultModel == nil || *listResp.DefaultModel != "m1" {
		t.Fatalf("expected top-level default_model=m1, got %v", listResp.DefaultModel)
	}
	ids := map[string]bool{}
	for _, e := range listResp.Data {
		ids[e.ID] = true
	}
	if !ids["m1"] {
		t.Fatalf("expected m1 (allowed) present, got %v", ids)
	}
	if ids["m2"] {
		t.Fatalf("expected m2 (disallowed) omitted, got %v", ids)
	}
	if ids["blocked-alias"] {
		t.Fatalf("expected the alias whose only target is disallowed to be omitted entirely, got %v", ids)
	}
}

func TestListModels_GatewayUnreachableFallsBackUnfiltered(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)

	h := NewModelHandler("http://127.0.0.1:1", "testkey", pool)
	r := newSessionModelsTestRouter(h, "some-user-id")

	logs := captureLogs(t)
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected the list to degrade to unfiltered rather than fail, status = %d body = %s", resp.Code, resp.Body.String())
	}
	var listResp openAIModelsResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range listResp.Data {
		ids[e.ID] = true
	}
	if !ids["m1"] {
		t.Fatalf("expected unfiltered fallback to still include m1, got %v", ids)
	}
	if got := logs.String(); !strings.Contains(got, "gateway effective policy unavailable") {
		t.Fatalf("expected a warning log about the unreachable gateway, got: %s", got)
	}
}

// TestListModels_SessionUserEffectiveResidencyOverridesOrg proves the fix for
// review finding "api session/bearer model list ignores per-principal
// residency": with the ORG residency left at "any" (the loosest rung), a
// session user whose OWN effective residency (as returned by the gateway's
// GET /v1/policy/effective — see getSessionEffective) is narrower must not be
// offered a model their own policy would refuse on the actual chat request.
func TestListModels_SessionUserEffectiveResidencyOverridesOrg(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `UPDATE principal_routing_policies SET residency = NULL WHERE principal_type = 'org'`); err != nil {
		t.Fatalf("set org residency to any: %v", err)
	}

	insertModel(t, ctx, pool, "self-hosted-model", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "cloud-managed-model", backendClassCloudManaged, true)
	insertModel(t, ctx, pool, "vendor-model", backendClassExternalAPI, true)

	cases := []struct {
		name          string
		userResidency string
		wantPresent   []string
		wantAbsent    []string
	}{
		{
			name:          "cloud_account_only omits external_api only",
			userResidency: "cloud_account_only",
			wantPresent:   []string{"self-hosted-model", "cloud-managed-model"},
			wantAbsent:    []string{"vendor-model"},
		},
		{
			name:          "self_hosted_only omits cloud_managed and external_api",
			userResidency: "self_hosted_only",
			wantPresent:   []string{"self-hosted-model"},
			wantAbsent:    []string{"cloud-managed-model", "vendor-model"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := fakeGatewayEffective(t, gatewayEffectivePolicy{
				AllowedModels: nil, // unrestricted allowlist -- residency is the only gate under test
				Residency:     tc.userResidency,
			})
			h := NewModelHandler(gw.URL, "testkey", pool)
			r := newSessionModelsTestRouter(h, "some-user-id")

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set(darkplumClassesHeader, "external_api")
			resp := httptest.NewRecorder()
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
			for _, want := range tc.wantPresent {
				if !ids[want] {
					t.Errorf("expected %q present under user residency %q, got %v", want, tc.userResidency, ids)
				}
			}
			for _, want := range tc.wantAbsent {
				if ids[want] {
					t.Errorf("expected %q ABSENT under user residency %q (org is 'any', so this only holds if the caller's own effective residency is used), got %v", want, tc.userResidency, ids)
				}
			}
		})
	}
}

// TestListModels_BearerCallerNeverFiltered exercises the "no user_id in
// context" path (Bearer/API-key caller, apiOrSession skips sessionMW) —
// filtering must never apply there; the gateway keeps enforcing per-key
// policy on the actual chat request, and this endpoint's job for that caller
// is unfiltered discovery, not a second potentially-stale filter.
func TestListModels_BearerCallerNeverFiltered(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)

	// A gateway that would 403 any effective call — proves it's never called
	// for the Bearer/no-context path.
	gw := fakeGatewayEffective(t, gatewayEffectivePolicy{})
	h := NewModelHandler(gw.URL, "wrong-key", pool) // wrong key -> 403 if ever called
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
	if len(listResp.Data) != 2 {
		t.Fatalf("expected both models present unfiltered, got %+v", listResp.Data)
	}
}
