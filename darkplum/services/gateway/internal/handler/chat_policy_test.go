package handler

// Tests for per-principal routing policy enforcement (model_not_allowed),
// the effective-residency switch, default_model resolution, GET
// /v1/policy/effective, and the policy-aware /v1/models filter. Same
// package as chat_test.go so these tests share its fakes/helpers
// (newTestHandler, newRoutedTestHandler, buildChatRequest, decodeError,
// routedAlias, fakeClassifier).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/router"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

// doChatAs builds a chat completion request exactly like doChat, but also
// sets the auth-middleware context keys (and any extra headers) a real
// APIKey middleware run would have set, so these tests can exercise
// principal-specific enforcement without a database.
func doChatAs(t *testing.T, h *ChatHandler, model, userID, keyID, keyType string, headers map[string]string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildChatRequest(t, model)
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	c.Set(middleware.ContextUserID, userID)
	c.Set(middleware.ContextKeyID, keyID)
	c.Set(middleware.ContextKeyType, keyType)
	h.HandleChatCompletions(c)
	return w, c
}

// ---------------------------------------------------------------------------
// model_not_allowed: basic refusal + denial ordering
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_ModelNotAllowed_403(t *testing.T) {
	prov := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	allowed := []string{"llama-3-8b"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"gpt-4":      {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:     map[string]bool{"llama-3-8b": true, "gpt-4": true},
			Residency:   policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{"key-1": {AllowedModels: &allowed}},
		},
		true,
	)

	w, c := doChatAs(t, h, "gpt-4", "user-1", "key-1", middleware.KeyTypeUser, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_allowed" {
		t.Errorf("expected code model_not_allowed, got %q", e.Error.Code)
	}
	if e.Error.Type != "permission_error" {
		t.Errorf("expected type permission_error, got %q", e.Error.Type)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called for a disallowed model, got %d calls", prov.completeCalls)
	}
	if denialCode, _ := c.Get("denial_code"); denialCode != "model_not_allowed" {
		t.Errorf("denial_code = %v, want model_not_allowed", denialCode)
	}

	// The allowed model on the same key must still succeed.
	w2, _ := doChatAs(t, h, "llama-3-8b", "user-1", "key-1", middleware.KeyTypeUser, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 for the allowed model, got %d; body=%s", w2.Code, w2.Body.String())
	}
}

func TestHandleChatCompletions_DenialOrdering_404BeatsModelNotAllowed(t *testing.T) {
	deny := []string{}
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{}, // the model doesn't even exist in the route table
		policy.Snapshot{
			Enabled:     map[string]bool{},
			Residency:   policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{"key-1": {AllowedModels: &deny}},
		},
		true,
	)

	w, _ := doChatAs(t, h, "does-not-exist", "user-1", "key-1", middleware.KeyTypeUser, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (route lookup precedes model_not_allowed), got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "model_not_found" {
		t.Errorf("expected code model_not_found, got %q", e.Error.Code)
	}
}

func TestHandleChatCompletions_DenialOrdering_EnabledBeatsModelNotAllowed(t *testing.T) {
	deny := []string{}
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:     map[string]bool{"llama-3-8b": false},
			Residency:   policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{"key-1": {AllowedModels: &deny}},
		},
		true,
	)

	w, _ := doChatAs(t, h, "llama-3-8b", "user-1", "key-1", middleware.KeyTypeUser, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "model_not_enabled" {
		t.Errorf("expected code model_not_enabled (enabled check precedes model_not_allowed), got %q", e.Error.Code)
	}
}

func TestHandleChatCompletions_DenialOrdering_ResidencyBeatsModelNotAllowed(t *testing.T) {
	deny := []string{}
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}},
		policy.Snapshot{
			Enabled:   map[string]bool{"us.anthropic.claude-3-5-sonnet": true},
			Residency: policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{
				"key-1": {AllowedModels: &deny, Residency: policy.ResidencySelfHostedOnly},
			},
		},
		true,
	)

	w, _ := doChatAs(t, h, "us.anthropic.claude-3-5-sonnet", "user-1", "key-1", middleware.KeyTypeUser, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "model_residency_violation" {
		t.Errorf("expected code model_residency_violation (residency check precedes model_not_allowed), got %q", e.Error.Code)
	}
}

// TestHandleChatCompletions_RoutedAliasToDisallowedTarget_RefusedNeverSwapped
// is the routed-alias analogue of the design's "never swapped" guarantee:
// the classifier picks a route whose target the caller's effective policy
// forbids — the request is refused, and the audit trail still names the
// picked (refused) target, not silently rerouted to default_route or any
// other route.
func TestHandleChatCompletions_RoutedAliasToDisallowedTarget_RefusedNeverSwapped(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code"}}}
	allowed := []string{"llama-3-8b"} // qwen-coder ("code" route's target) is NOT allowed
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:     map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:     map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency:   policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{"key-1": {AllowedModels: &allowed}},
		},
		classifier,
	)

	w, c := doChatAs(t, h, "smart-chat", "user-1", "key-1", middleware.KeyTypeUser, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "model_not_allowed" {
		t.Errorf("expected code model_not_allowed, got %q", e.Error.Code)
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must not be called — the pick must be refused, never swapped for another route, got %d calls", vllm.completeCalls)
	}
	if resolved, _ := c.Get("resolved_model"); resolved != "qwen-coder" {
		t.Errorf("resolved_model = %v, want qwen-coder (the selected, refused target — never swapped)", resolved)
	}
}

// ---------------------------------------------------------------------------
// default_model resolution for an omitted `model` on a user-key request
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_DefaultModelAppliedWhenModelOmitted(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	def := "llama-3-8b"
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:     map[string]bool{"llama-3-8b": true},
			Residency:   policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{"key-1": {DefaultModel: &def}},
		},
		true,
	)

	w, c := doChatAs(t, h, "", "user-1", "key-1", middleware.KeyTypeUser, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (default_model applied), got %d; body=%s", w.Code, w.Body.String())
	}
	if resolved, _ := c.Get("resolved_model"); resolved != "llama-3-8b" {
		t.Errorf("resolved_model = %v, want llama-3-8b", resolved)
	}
	if vllm.completeCalls != 1 {
		t.Errorf("expected the default model to actually be dispatched, got %d calls", vllm.completeCalls)
	}
}

// TestHandleChatCompletions_DefaultModelOrphanedByTighteningIsRefused covers
// the case DESIGN4.md calls out explicitly: a stored default_model that a
// LATER policy tightening no longer allows must still run the full
// enforcement chain, landing on model_not_allowed rather than silently
// dispatching.
func TestHandleChatCompletions_DefaultModelOrphanedByTighteningIsRefused(t *testing.T) {
	def := "llama-3-8b"
	allowed := []string{"other-model"} // llama-3-8b no longer allowed
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:     map[string]bool{"llama-3-8b": true},
			Residency:   policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{"key-1": {DefaultModel: &def, AllowedModels: &allowed}},
		},
		true,
	)

	w, c := doChatAs(t, h, "", "user-1", "key-1", middleware.KeyTypeUser, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "model_not_allowed" {
		t.Errorf("expected code model_not_allowed, got %q", e.Error.Code)
	}
	if resolved, _ := c.Get("resolved_model"); resolved != "llama-3-8b" {
		t.Errorf("resolved_model = %v, want llama-3-8b (the orphaned default, still recorded)", resolved)
	}
}

// TestHandleChatCompletions_ServiceKeyNeverGetsDefaultModel asserts the
// default-model shortcut is restricted to user keys — a service key omitting
// `model` still gets the plain 400 today, never a resolved default (the
// service key has no single "owner" whose default would even make sense).
func TestHandleChatCompletions_ServiceKeyNeverGetsDefaultModel(t *testing.T) {
	def := "llama-3-8b"
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:      map[string]bool{"llama-3-8b": true},
			Residency:    policy.ResidencyAny,
			UserPolicies: map[string]*policy.EffectivePolicy{"impersonated-user": {DefaultModel: &def}},
		},
		true,
	)

	w, _ := doChatAs(t, h, "", "unused", "service-key", middleware.KeyTypeService,
		map[string]string{"X-Darkplum-User-Id": "impersonated-user"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 model_required for a service key with no model, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "model_required" {
		t.Errorf("expected code model_required, got %q", e.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// Service key principal resolution
// ---------------------------------------------------------------------------

// TestHandleChatCompletions_ServiceKeyIgnoresKeyRowsHonoursHeaderUser asserts
// the service-key principal rule end to end: the service key's OWN row (if
// it somehow had one) is never consulted, and X-Darkplum-User-Id names the
// actual principal whose effective policy applies.
func TestHandleChatCompletions_ServiceKeyIgnoresKeyRowsHonoursHeaderUser(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	keyDenyEverything := []string{}
	userAllowed := []string{"llama-3-8b"}
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:      map[string]bool{"llama-3-8b": true},
			Residency:    policy.ResidencyAny,
			KeyPolicies:  map[string]*policy.EffectivePolicy{"service-key": {AllowedModels: &keyDenyEverything}},
			UserPolicies: map[string]*policy.EffectivePolicy{"impersonated-user": {AllowedModels: &userAllowed}},
		},
		true,
	)

	w, c := doChatAs(t, h, "llama-3-8b", "context-user-id-must-be-ignored", "service-key", middleware.KeyTypeService,
		map[string]string{"X-Darkplum-User-Id": "impersonated-user"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (service key must ignore its own key-level deny-all row), got %d; body=%s", w.Code, w.Body.String())
	}
	if resolved, _ := c.Get("resolved_model"); resolved != "llama-3-8b" {
		t.Errorf("resolved_model = %v, want llama-3-8b", resolved)
	}
}

// ---------------------------------------------------------------------------
// GET /v1/models: filtered for user keys only
// ---------------------------------------------------------------------------

func newListModelsRequest(t *testing.T, ctxType string, headers map[string]string) (*httptest.ResponseRecorder, *gin.Context) {
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
	c.Set(middleware.ContextKeyType, ctxType)
	return w, c
}

func decodeModelIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal /v1/models response: %v; body=%s", err, body)
	}
	ids := make([]string, len(resp.Data))
	for i, m := range resp.Data {
		ids[i] = m.ID
	}
	return ids
}

func TestHandleListModels_FilteredForUserKey(t *testing.T) {
	allowed := []string{"llama-3-8b"}
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"gpt-4":      {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:     map[string]bool{"llama-3-8b": true, "gpt-4": true},
			Residency:   policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{"key-1": {AllowedModels: &allowed}},
		},
		true,
	)

	w, c := newListModelsRequest(t, middleware.KeyTypeUser, nil)
	c.Set(middleware.ContextUserID, "user-1")
	c.Set(middleware.ContextKeyID, "key-1")

	h.HandleListModels(c)

	ids := decodeModelIDs(t, w.Body.Bytes())
	if len(ids) != 1 || ids[0] != "llama-3-8b" {
		t.Errorf("expected only the allowed model to be listed, got %v", ids)
	}
}

func TestHandleListModels_UnfilteredForServiceKey(t *testing.T) {
	empty := []string{}
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"gpt-4":      {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:      map[string]bool{"llama-3-8b": true, "gpt-4": true},
			Residency:    policy.ResidencyAny,
			UserPolicies: map[string]*policy.EffectivePolicy{"impersonated-user": {AllowedModels: &empty}},
		},
		true,
	)

	w, c := newListModelsRequest(t, middleware.KeyTypeService, map[string]string{"X-Darkplum-User-Id": "impersonated-user"})
	h.HandleListModels(c)

	ids := decodeModelIDs(t, w.Body.Bytes())
	if len(ids) != 2 {
		t.Errorf("expected the full unfiltered list for a service key, got %v", ids)
	}
}

// TestHandleListModels_UnfilteredWhenPolicyNotLoaded asserts /v1/models
// fails OPEN (unfiltered) rather than closed when no policy snapshot has
// loaded yet — it is metadata, not an enforcement point; a real attempt to
// USE any of these models still goes through HandleChatCompletions, which
// fails closed on its own (503 policy_unavailable).
func TestHandleListModels_UnfilteredWhenPolicyNotLoaded(t *testing.T) {
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{},
		false, // never loaded
	)

	w, c := newListModelsRequest(t, middleware.KeyTypeUser, nil)
	c.Set(middleware.ContextUserID, "user-1")
	c.Set(middleware.ContextKeyID, "key-1")
	h.HandleListModels(c)

	ids := decodeModelIDs(t, w.Body.Bytes())
	if len(ids) != 1 {
		t.Errorf("expected the unfiltered list when policy hasn't loaded, got %v", ids)
	}
}

// ---------------------------------------------------------------------------
// GET /v1/policy/effective
// ---------------------------------------------------------------------------

func newEffectivePolicyRequest(t *testing.T, query, ctxType string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest(http.MethodGet, "/v1/policy/effective?"+query, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	c.Request = req
	c.Set(middleware.ContextKeyType, ctxType)
	return w, c
}

func TestHandleEffectivePolicy_ShapeAndReachableModels(t *testing.T) {
	allowed := []string{"llama-3-8b", "us.anthropic.claude-3-5-sonnet"}
	def := "llama-3-8b"
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{
			"llama-3-8b":                     {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"llama-3-8b": true, "us.anthropic.claude-3-5-sonnet": true},
			Residency: policy.ResidencyAny,
			KeyPolicies: map[string]*policy.EffectivePolicy{
				"22222222-2222-2222-2222-222222222222": {
					AllowedModels: &allowed,
					Residency:     policy.ResidencySelfHostedOnly,
					DefaultModel:  &def,
					Sources:       policy.PolicySources{AllowedModels: "user", Residency: "org", DefaultModel: "api_key"},
				},
			},
		},
		true,
	)

	w, c := newEffectivePolicyRequest(t,
		"user_id=11111111-1111-1111-1111-111111111111&api_key_id=22222222-2222-2222-2222-222222222222",
		middleware.KeyTypeService)
	h.HandleEffectivePolicy(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}

	var body struct {
		AllowedModels   []string             `json:"allowed_models"`
		Residency       string               `json:"residency"`
		DefaultModel    *string              `json:"default_model"`
		Sources         policy.PolicySources `json:"sources"`
		ReachableModels []string             `json:"reachable_models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if len(body.AllowedModels) != 2 {
		t.Errorf("allowed_models = %v, want 2 entries", body.AllowedModels)
	}
	if body.Residency != policy.ResidencySelfHostedOnly {
		t.Errorf("residency = %q, want %q", body.Residency, policy.ResidencySelfHostedOnly)
	}
	if body.DefaultModel == nil || *body.DefaultModel != "llama-3-8b" {
		t.Errorf("default_model = %v, want llama-3-8b", body.DefaultModel)
	}
	if body.Sources.AllowedModels != "user" || body.Sources.Residency != "org" || body.Sources.DefaultModel != "api_key" {
		t.Errorf("sources = %+v, want {user org api_key}", body.Sources)
	}
	// llama-3-8b: allowed + enabled + self-hosted -> reachable. The Bedrock
	// model: allowed + enabled but cloud_managed under self_hosted_only
	// residency -> excluded from reachable_models even though it's in
	// allowed_models.
	if len(body.ReachableModels) != 1 || body.ReachableModels[0] != "llama-3-8b" {
		t.Errorf("reachable_models = %v, want [llama-3-8b]", body.ReachableModels)
	}
}

func TestHandleEffectivePolicy_403ForNonServiceKey(t *testing.T) {
	h := newTestHandler(fakeProviders{}, map[string]routing.Route{}, policy.Snapshot{Residency: policy.ResidencyAny}, true)

	w, c := newEffectivePolicyRequest(t, "user_id=11111111-1111-1111-1111-111111111111", middleware.KeyTypeUser)
	h.HandleEffectivePolicy(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-service key, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "service_key_required" {
		t.Errorf("expected code service_key_required, got %q", e.Error.Code)
	}
}

func TestHandleEffectivePolicy_400InvalidUserID(t *testing.T) {
	h := newTestHandler(fakeProviders{}, map[string]routing.Route{}, policy.Snapshot{Residency: policy.ResidencyAny}, true)

	w, c := newEffectivePolicyRequest(t, "user_id=not-a-uuid", middleware.KeyTypeService)
	h.HandleEffectivePolicy(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicy_MissingUserID400(t *testing.T) {
	h := newTestHandler(fakeProviders{}, map[string]routing.Route{}, policy.Snapshot{Residency: policy.ResidencyAny}, true)

	w, c := newEffectivePolicyRequest(t, "", middleware.KeyTypeService)
	h.HandleEffectivePolicy(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing user_id, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicy_400InvalidAPIKeyID(t *testing.T) {
	h := newTestHandler(fakeProviders{}, map[string]routing.Route{}, policy.Snapshot{Residency: policy.ResidencyAny}, true)

	w, c := newEffectivePolicyRequest(t,
		"user_id=11111111-1111-1111-1111-111111111111&api_key_id=not-a-uuid",
		middleware.KeyTypeService)
	h.HandleEffectivePolicy(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicy_PolicyUnavailable503(t *testing.T) {
	h := newTestHandler(fakeProviders{}, map[string]routing.Route{}, policy.Snapshot{}, false)

	w, c := newEffectivePolicyRequest(t, "user_id=11111111-1111-1111-1111-111111111111", middleware.KeyTypeService)
	h.HandleEffectivePolicy(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body=%s", w.Code, w.Body.String())
	}
}
