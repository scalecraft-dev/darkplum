package handler

// Tests for the external_api backend class: residency enforcement across the
// full any/cloud_account_only/self_hosted_only ladder, deny-unknown-class,
// backend_class propagation to context/headers on both dispatch and denial,
// and GET /v1/models' X-Darkplum-Classes header gating (including the
// `providers` array).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/middleware"
	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

func externalAPIRoutes() map[string]routing.Route {
	return map[string]routing.Route{
		"claude-3-5-sonnet": {ProviderKey: "anthropic", Kind: "anthropic", Class: routing.ClassExternalAPI},
	}
}

// ---------------------------------------------------------------------------
// Residency ladder: external_api is refused under cloud_account_only and
// self_hosted_only, and permitted under any.
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_ExternalAPI_RefusedUnderCloudAccountOnly(t *testing.T) {
	prov := &fakeKindProvider{name: "anthropic", kind: "anthropic"}
	h := newTestHandler(
		fakeProviders{"anthropic": prov},
		externalAPIRoutes(),
		policy.Snapshot{
			Enabled:   map[string]bool{"claude-3-5-sonnet": true},
			Residency: policy.ResidencyCloudAccountOnly,
		},
		true,
	)

	w, c := doChat(t, h, "claude-3-5-sonnet")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_residency_violation" {
		t.Errorf("expected code model_residency_violation, got %q", e.Error.Code)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called, got %d calls", prov.completeCalls)
	}
	if bc, _ := c.Get("backend_class"); bc != routing.ClassExternalAPI {
		t.Errorf("backend_class = %v, want %q (denied row must still carry it)", bc, routing.ClassExternalAPI)
	}
	if got := w.Header().Get("X-Darkplum-Backend-Class"); got != routing.ClassExternalAPI {
		t.Errorf("X-Darkplum-Backend-Class header = %q, want %q", got, routing.ClassExternalAPI)
	}
}

func TestHandleChatCompletions_ExternalAPI_RefusedUnderSelfHostedOnly(t *testing.T) {
	prov := &fakeKindProvider{name: "anthropic", kind: "anthropic"}
	h := newTestHandler(
		fakeProviders{"anthropic": prov},
		externalAPIRoutes(),
		policy.Snapshot{
			Enabled:   map[string]bool{"claude-3-5-sonnet": true},
			Residency: policy.ResidencySelfHostedOnly,
		},
		true,
	)

	w, _ := doChat(t, h, "claude-3-5-sonnet")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_residency_violation" {
		t.Errorf("expected code model_residency_violation, got %q", e.Error.Code)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called, got %d calls", prov.completeCalls)
	}
}

func TestHandleChatCompletions_ExternalAPI_AllowedUnderAny(t *testing.T) {
	prov := &fakeKindProvider{name: "anthropic", kind: "anthropic"}
	h := newTestHandler(
		fakeProviders{"anthropic": prov},
		externalAPIRoutes(),
		policy.Snapshot{
			Enabled:   map[string]bool{"claude-3-5-sonnet": true},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	w, c := doChat(t, h, "claude-3-5-sonnet")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if prov.completeCalls != 1 {
		t.Errorf("expected the provider to be called exactly once, got %d", prov.completeCalls)
	}
	if bc, _ := c.Get("backend_class"); bc != routing.ClassExternalAPI {
		t.Errorf("backend_class = %v, want %q", bc, routing.ClassExternalAPI)
	}
	if got := w.Header().Get("X-Darkplum-Backend-Class"); got != routing.ClassExternalAPI {
		t.Errorf("X-Darkplum-Backend-Class header = %q, want %q", got, routing.ClassExternalAPI)
	}
	if got := w.Header().Get("X-Darkplum-Provider"); got != "anthropic" {
		t.Errorf("X-Darkplum-Provider header = %q, want anthropic", got)
	}
}

// TestHandleChatCompletions_CloudManaged_AllowedUnderCloudAccountOnly proves
// the middle rung of the ladder actually loosens something relative to
// self_hosted_only, not just tightens relative to any.
func TestHandleChatCompletions_CloudManaged_AllowedUnderCloudAccountOnly(t *testing.T) {
	prov := &fakeNameOnlyProvider{name: "bedrock"}
	h := newTestHandler(
		fakeProviders{"bedrock": prov},
		map[string]routing.Route{"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}},
		policy.Snapshot{
			Enabled:   map[string]bool{"us.anthropic.claude-3-5-sonnet": true},
			Residency: policy.ResidencyCloudAccountOnly,
		},
		true,
	)

	w, _ := doChat(t, h, "us.anthropic.claude-3-5-sonnet")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if prov.completeCalls != 1 {
		t.Errorf("expected the provider to be called exactly once, got %d", prov.completeCalls)
	}
}

// ---------------------------------------------------------------------------
// Deny-unknown-class-under-ALL-residencies [RULING]
// ---------------------------------------------------------------------------

// TestHandleChatCompletions_UnknownClassRefusedUnderAny asserts a route
// carrying a class this gateway build does not recognise is refused even
// under residency "any" — the ruling that overrides "unknown -> denied under
// anything but any".
func TestHandleChatCompletions_UnknownClassRefusedUnderAny(t *testing.T) {
	prov := &fakeKindProvider{name: "mystery", kind: "mystery"}
	h := newTestHandler(
		fakeProviders{"mystery": prov},
		map[string]routing.Route{"mystery-model": {ProviderKey: "mystery", Kind: "mystery", Class: "some_future_class"}},
		policy.Snapshot{
			Enabled:   map[string]bool{"mystery-model": true},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	w, _ := doChat(t, h, "mystery-model")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unrecognised class even under residency any, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_residency_violation" {
		t.Errorf("expected code model_residency_violation, got %q", e.Error.Code)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called for an unrecognised class, got %d calls", prov.completeCalls)
	}
}

// ---------------------------------------------------------------------------
// GET /v1/models: X-Darkplum-Classes header gating
// ---------------------------------------------------------------------------

func modelsRequestWithHeaders(t *testing.T, keyType string, headers map[string]string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest(http.MethodGet, "/v1/models", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	c.Set(middleware.ContextKeyType, keyType)
	return w, c
}

func mixedClassRoutes() map[string]routing.Route {
	return map[string]routing.Route{
		"llama-3-8b":        {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		"claude-3-5-sonnet": {ProviderKey: "anthropic", Kind: "anthropic", Class: routing.ClassExternalAPI},
	}
}

// TestHandleListModels_UserKey_ExternalAPIOmittedWithoutHeader asserts a
// user-key caller (old or new client, doesn't matter) never sees an
// external_api model unless it sends the classes header — today's webchat/
// api clients never do, so this is the "old caller never even catalogues a
// vendor model" invariant from the caller's side.
func TestHandleListModels_UserKey_ExternalAPIOmittedWithoutHeader(t *testing.T) {
	h := newTestHandler(
		fakeProviders{}, mixedClassRoutes(),
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true, "claude-3-5-sonnet": true}, Residency: policy.ResidencyAny},
		true,
	)
	w, c := modelsRequestWithHeaders(t, middleware.KeyTypeUser, nil)
	c.Set(middleware.ContextUserID, "user-1")
	c.Set(middleware.ContextKeyID, "key-1")
	h.HandleListModels(c)

	ids := decodeModelIDs(t, w.Body.Bytes())
	if len(ids) != 1 || ids[0] != "llama-3-8b" {
		t.Errorf("expected only llama-3-8b without the classes header, got %v", ids)
	}
}

// TestHandleListModels_UserKey_ExternalAPIIncludedWithHeader asserts the
// header works for a user-key caller too (a policy that permits the model
// still must see it in order to select it), matching the api's bearerModels
// invariant referenced in the wire contract.
func TestHandleListModels_UserKey_ExternalAPIIncludedWithHeader(t *testing.T) {
	h := newTestHandler(
		fakeProviders{}, mixedClassRoutes(),
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true, "claude-3-5-sonnet": true}, Residency: policy.ResidencyAny},
		true,
	)
	w, c := modelsRequestWithHeaders(t, middleware.KeyTypeUser, map[string]string{"X-Darkplum-Classes": "external_api"})
	c.Set(middleware.ContextUserID, "user-1")
	c.Set(middleware.ContextKeyID, "key-1")
	h.HandleListModels(c)

	ids := decodeModelIDs(t, w.Body.Bytes())
	byID := map[string]bool{}
	for _, id := range ids {
		byID[id] = true
	}
	if !byID["claude-3-5-sonnet"] || !byID["llama-3-8b"] {
		t.Errorf("expected both models with the classes header, got %v", ids)
	}
}

// TestHandleListModels_UserKey_ResidencyFiltersTheList: a key held to the cloud
// account must not be offered a vendor model it would be refused on, even when
// it sends the classes header — the list and the enforcement point agree.
func TestHandleListModels_UserKey_ResidencyFiltersTheList(t *testing.T) {
	for _, tc := range []struct {
		residency string
		want      map[string]bool
	}{
		{policy.ResidencyAny, map[string]bool{"llama-3-8b": true, "claude-3-5-sonnet": true}},
		{policy.ResidencyCloudAccountOnly, map[string]bool{"llama-3-8b": true}},
		{policy.ResidencySelfHostedOnly, map[string]bool{"llama-3-8b": true}},
	} {
		t.Run(tc.residency, func(t *testing.T) {
			h := newTestHandler(
				fakeProviders{}, mixedClassRoutes(),
				policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true, "claude-3-5-sonnet": true}, Residency: tc.residency},
				true,
			)
			w, c := modelsRequestWithHeaders(t, middleware.KeyTypeUser, map[string]string{"X-Darkplum-Classes": "external_api"})
			c.Set(middleware.ContextUserID, "user-1")
			c.Set(middleware.ContextKeyID, "key-1")
			h.HandleListModels(c)

			got := map[string]bool{}
			for _, id := range decodeModelIDs(t, w.Body.Bytes()) {
				got[id] = true
			}
			for id := range tc.want {
				if !got[id] {
					t.Errorf("residency %s: %q missing from %v", tc.residency, id, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("residency %s: listed %v, want exactly %v", tc.residency, got, tc.want)
			}
		})
	}
}

// TestHandleListModels_ServiceKey_ExternalAPIOmittedWithoutHeader is the
// order-independence invariant from the SERVICE side: an old api's
// SyncModels never sends the header and so can never catalogue a vendor
// model, even though a service key would otherwise see the unfiltered list.
func TestHandleListModels_ServiceKey_ExternalAPIOmittedWithoutHeader(t *testing.T) {
	h := newTestHandler(
		fakeProviders{}, mixedClassRoutes(),
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true, "claude-3-5-sonnet": true}, Residency: policy.ResidencyAny},
		true,
	)
	w, c := modelsRequestWithHeaders(t, middleware.KeyTypeService, nil)
	h.HandleListModels(c)

	ids := decodeModelIDs(t, w.Body.Bytes())
	if len(ids) != 1 || ids[0] != "llama-3-8b" {
		t.Errorf("expected only llama-3-8b without the classes header, got %v", ids)
	}

	var body struct {
		Providers []provider.ProviderStatus `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Providers != nil {
		t.Errorf("expected no providers array without the classes header, got %v", body.Providers)
	}
}

// TestHandleListModels_ServiceKey_WithHeader_IncludesExternalAPIAndProviders
// asserts the SERVICE-key + header case gets both the external_api model AND
// the `providers` array (always both vendors, per the wire contract), while
// a plain user-key + header request does NOT get the providers array (see
// the next test) — providers is service-key-only.
func TestHandleListModels_ServiceKey_WithHeader_IncludesExternalAPIAndProviders(t *testing.T) {
	h := newTestHandler(
		fakeProviders{}, mixedClassRoutes(),
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true, "claude-3-5-sonnet": true}, Residency: policy.ResidencyAny},
		true,
	)
	w, c := modelsRequestWithHeaders(t, middleware.KeyTypeService, map[string]string{"X-Darkplum-Classes": "external_api"})
	h.HandleListModels(c)

	ids := decodeModelIDs(t, w.Body.Bytes())
	if len(ids) != 2 {
		t.Errorf("expected both models with the classes header, got %v", ids)
	}

	var body struct {
		Providers []provider.ProviderStatus `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Providers) != 2 {
		t.Fatalf("expected exactly 2 providers (anthropic, openai) always listed, got %+v", body.Providers)
	}
	names := map[string]bool{}
	for _, p := range body.Providers {
		names[p.Name] = true
		if p.State != provider.ProviderStateNotConfigured {
			t.Errorf("provider %s state = %q, want %q (no vendor wired in yet)", p.Name, p.State, provider.ProviderStateNotConfigured)
		}
	}
	if !names["anthropic"] || !names["openai"] {
		t.Errorf("expected both anthropic and openai listed, got %+v", body.Providers)
	}
}

// TestHandleListModels_UserKey_WithHeader_NoProvidersArray asserts the
// providers array is service-key only: a user key sending the header still
// gets the external_api model in `data`, but never the `providers` block.
func TestHandleListModels_UserKey_WithHeader_NoProvidersArray(t *testing.T) {
	h := newTestHandler(
		fakeProviders{}, mixedClassRoutes(),
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true, "claude-3-5-sonnet": true}, Residency: policy.ResidencyAny},
		true,
	)
	w, c := modelsRequestWithHeaders(t, middleware.KeyTypeUser, map[string]string{"X-Darkplum-Classes": "external_api"})
	c.Set(middleware.ContextUserID, "user-1")
	c.Set(middleware.ContextKeyID, "key-1")
	h.HandleListModels(c)

	var body struct {
		Providers []provider.ProviderStatus `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Providers != nil {
		t.Errorf("expected no providers array for a user key, got %v", body.Providers)
	}
}

// TestClassesHeaderAllowsExternalAPI_TokenMatching guards the header parsing
// itself: comma-separated, trimmed, exact-token match only.
func TestClassesHeaderAllowsExternalAPI_TokenMatching(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"external_api", true},
		{" external_api ", true},
		{"self_hosted,external_api", true},
		{"external_apixyz", false},
		{"self_hosted", false},
	}
	for _, tc := range cases {
		w, c := modelsRequestWithHeaders(t, middleware.KeyTypeService, map[string]string{"X-Darkplum-Classes": tc.header})
		_ = w
		if got := classesHeaderAllowsExternalAPI(c); got != tc.want {
			t.Errorf("classesHeaderAllowsExternalAPI(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
