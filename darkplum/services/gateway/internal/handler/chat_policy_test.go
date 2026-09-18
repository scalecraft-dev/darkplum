package handler

// Tests for per-principal routing policy enforcement (model_not_allowed),
// the effective-residency switch, default_model resolution, GET
// /v1/policy/effective, and the policy-aware /v1/models filter. Same
// package as chat_test.go so these tests share its fakes/helpers
// (newTestHandler, newRoutedTestHandler, buildChatRequest, decodeError,
// routedAlias, fakeClassifier).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// ---------------------------------------------------------------------------
// POST /v1/policy/effective — the override preview path
// ---------------------------------------------------------------------------

const (
	previewUserID = "11111111-1111-1111-1111-111111111111"
	previewKeyID  = "22222222-2222-2222-2222-222222222222"
	previewKeyID2 = "33333333-3333-3333-3333-333333333333"
)

func newEffectivePolicyOverrideRequest(t *testing.T, ctxType string, body string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest(http.MethodPost, "/v1/policy/effective", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	c.Request = req
	c.Set(middleware.ContextKeyType, ctxType)
	return w, c
}

type overridePreviewBody struct {
	AllowedModels   []string                         `json:"allowed_models"`
	Residency       string                           `json:"residency"`
	DefaultModel    *string                          `json:"default_model"`
	Sources         policy.PolicySources             `json:"sources"`
	ReachableModels []string                         `json:"reachable_models"`
	Keys            []overridePreviewKeyEntryForTest `json:"keys"`
}

type overridePreviewKeyEntryForTest struct {
	APIKeyID  string              `json:"api_key_id"`
	Effective overridePreviewBody `json:"effective"`
}

func decodeOverridePreview(t *testing.T, body []byte) overridePreviewBody {
	t.Helper()
	var out overridePreviewBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal preview response: %v; body=%s", err, body)
	}
	return out
}

// baseOverrideSnapshot builds a Snapshot with an org allowlist of three
// models and no residency/default set anywhere, plus routing/enabled data so
// reachable_models resolves. previewUserID owns two keys (previewKeyID,
// previewKeyID2), neither with a row of its own.
func baseOverrideSnapshot() policy.Snapshot {
	orgRow := policy.PolicyRow{Level: policy.PrincipalTypeOrg, AllowedModels: &[]string{"m1", "m2", "m3"}}
	orgEff := policy.Effective([]policy.PolicyRow{orgRow})
	return policy.Snapshot{
		Enabled:   map[string]bool{"m1": true, "m2": true, "m3": true},
		Residency: policy.ResidencyAny,
		OrgRow:    orgRow,
		// OrgPolicy is the precomputed counterpart of OrgRow, kept in sync
		// exactly like buildPrincipalPolicies does for a real snapshot load --
		// GET /v1/policy/effective (EffectiveFor) reads OrgPolicy, while POST's
		// override path (ChainFor) reads OrgRow/UserRows/KeyRows directly; a
		// test that compares the two (see NoOverrideMatchesGET) needs both.
		OrgPolicy: &orgEff,
		UserKeys: map[string][]string{
			previewUserID: {previewKeyID, previewKeyID2},
		},
	}
}

func baseOverrideRoutes() map[string]routing.Route {
	return map[string]routing.Route{
		"m1": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		"m2": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		"m3": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
	}
}

func TestHandleEffectivePolicyOverride_403ForNonServiceKey(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), baseOverrideSnapshot(), true)

	body := fmt.Sprintf(`{"user_id":%q}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeUser, body)
	h.HandleEffectivePolicyOverride(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-service key, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "service_key_required" {
		t.Errorf("expected code service_key_required, got %q", e.Error.Code)
	}
}

func TestHandleEffectivePolicyOverride_400InvalidBody(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), baseOverrideSnapshot(), true)

	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, `not json`)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicyOverride_400MissingUserID(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), baseOverrideSnapshot(), true)

	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, `{}`)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicyOverride_400UnknownOverrideLevel(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), baseOverrideSnapshot(), true)

	body := fmt.Sprintf(`{"user_id":%q,"override":{"level":"workspace","policy":null}}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown override.level, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicyOverride_400APIKeyLevelWithoutAPIKeyID(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), baseOverrideSnapshot(), true)

	body := fmt.Sprintf(`{"user_id":%q,"override":{"level":"api_key","policy":null}}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override.level=api_key with no api_key_id, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicyOverride_400BadResidency(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), baseOverrideSnapshot(), true)

	body := fmt.Sprintf(`{"user_id":%q,"override":{"level":"org","policy":{"residency":"anywhere"}}}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid residency value, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicyOverride_PolicyUnavailable503(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), policy.Snapshot{}, false)

	body := fmt.Sprintf(`{"user_id":%q}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleEffectivePolicyOverride_NoOverrideMatchesGET asserts a POST with
// no override object at all reproduces exactly what GET /v1/policy/effective
// would report for the same user_id/api_key_id — the "before" half of the
// api's preview diff can use either.
func TestHandleEffectivePolicyOverride_NoOverrideMatchesGET(t *testing.T) {
	snap := baseOverrideSnapshot()
	userRow := policy.PolicyRow{Level: policy.PrincipalTypeUser, AllowedModels: &[]string{"m1", "m2"}}
	snap.UserRows = map[string]policy.PolicyRow{previewUserID: userRow}
	// UserPolicies is GET's precomputed counterpart of UserRows -- kept in
	// sync exactly like buildPrincipalPolicies does for a real snapshot load.
	userEff := policy.Effective([]policy.PolicyRow{snap.OrgRow, userRow})
	snap.UserPolicies = map[string]*policy.EffectivePolicy{previewUserID: &userEff}
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), snap, true)

	postBody := fmt.Sprintf(`{"user_id":%q}`, previewUserID)
	wPost, cPost := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, postBody)
	h.HandleEffectivePolicyOverride(cPost)
	if wPost.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", wPost.Code, wPost.Body.String())
	}
	post := decodeOverridePreview(t, wPost.Body.Bytes())

	wGet, cGet := newEffectivePolicyRequest(t, "user_id="+previewUserID, middleware.KeyTypeService)
	h.HandleEffectivePolicy(cGet)
	var getBody overridePreviewBody
	if err := json.Unmarshal(wGet.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}

	if len(post.AllowedModels) != len(getBody.AllowedModels) {
		t.Errorf("POST (no override) allowed_models = %v, want to match GET %v", post.AllowedModels, getBody.AllowedModels)
	}
	if post.Residency != getBody.Residency {
		t.Errorf("POST residency = %q, want %q (match GET)", post.Residency, getBody.Residency)
	}
}

// TestHandleEffectivePolicyOverride_OrgLevel asserts an override.level="org"
// substitution narrows the user's effective allowlist exactly like a real
// org policy tightening would.
func TestHandleEffectivePolicyOverride_OrgLevel(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), baseOverrideSnapshot(), true)

	body := fmt.Sprintf(`{"user_id":%q,"override":{"level":"org","policy":{"allowed_models":["m1"]}}}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeOverridePreview(t, w.Body.Bytes())
	if len(got.AllowedModels) != 1 || got.AllowedModels[0] != "m1" {
		t.Errorf("allowed_models = %v, want [m1]", got.AllowedModels)
	}
	if got.Sources.AllowedModels != policy.PrincipalTypeOrg {
		t.Errorf("sources.allowed_models = %q, want %q", got.Sources.AllowedModels, policy.PrincipalTypeOrg)
	}
}

// TestHandleEffectivePolicyOverride_UserLevel asserts an override.level="user"
// substitution is used instead of whatever (if anything) is actually stored
// for that user.
func TestHandleEffectivePolicyOverride_UserLevel(t *testing.T) {
	snap := baseOverrideSnapshot()
	snap.UserRows = map[string]policy.PolicyRow{previewUserID: {AllowedModels: &[]string{"m1", "m2", "m3"}}}
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), snap, true)

	body := fmt.Sprintf(`{"user_id":%q,"override":{"level":"user","policy":{"allowed_models":["m2"]}}}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeOverridePreview(t, w.Body.Bytes())
	if len(got.AllowedModels) != 1 || got.AllowedModels[0] != "m2" {
		t.Errorf("allowed_models = %v, want [m2] (the override, not the stored row)", got.AllowedModels)
	}
}

// TestHandleEffectivePolicyOverride_APIKeyLevel asserts an override.level=
// "api_key" substitution only affects the named key's own chain, and the
// top-level response is that key's effective, not the owning user's.
func TestHandleEffectivePolicyOverride_APIKeyLevel(t *testing.T) {
	snap := baseOverrideSnapshot()
	snap.UserRows = map[string]policy.PolicyRow{previewUserID: {AllowedModels: &[]string{"m1", "m2", "m3"}}}
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), snap, true)

	body := fmt.Sprintf(`{"user_id":%q,"api_key_id":%q,"override":{"level":"api_key","policy":{"allowed_models":["m1"]}}}`,
		previewUserID, previewKeyID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeOverridePreview(t, w.Body.Bytes())
	if len(got.AllowedModels) != 1 || got.AllowedModels[0] != "m1" {
		t.Errorf("allowed_models = %v, want [m1]", got.AllowedModels)
	}
	if got.Sources.AllowedModels != policy.PrincipalTypeAPIKey {
		t.Errorf("sources.allowed_models = %q, want %q", got.Sources.AllowedModels, policy.PrincipalTypeAPIKey)
	}
	// api_key-level override with an api_key_id must not also return a keys
	// list — the top-level response already IS that key's view.
	if len(got.Keys) != 0 {
		t.Errorf("expected no keys list for an api_key-level override, got %v", got.Keys)
	}
}

// TestHandleEffectivePolicyOverride_InheritRemovesTheRow covers policy=null
// ("inherit"): the level's existing row is removed entirely rather than kept.
func TestHandleEffectivePolicyOverride_InheritRemovesTheRow(t *testing.T) {
	snap := baseOverrideSnapshot()
	snap.UserRows = map[string]policy.PolicyRow{previewUserID: {AllowedModels: &[]string{"m1"}}}
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), snap, true)

	body := fmt.Sprintf(`{"user_id":%q,"override":{"level":"user","policy":null}}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeOverridePreview(t, w.Body.Bytes())
	// With the user's row inherited away, only the org's 3-model allowlist
	// remains — NOT the stored [m1].
	if len(got.AllowedModels) != 3 {
		t.Errorf("allowed_models = %v, want all 3 org models (user row inherited away)", got.AllowedModels)
	}
	if got.Sources.AllowedModels != policy.PrincipalTypeOrg {
		t.Errorf("sources.allowed_models = %q, want %q", got.Sources.AllowedModels, policy.PrincipalTypeOrg)
	}
}

// TestHandleEffectivePolicyOverride_KeysListForUserWithTwoKeys is the design's
// explicit "N keys affected" case: a user-level override must report BOTH of
// the user's keys' resulting effective policies, keyed by id only (no name —
// the gateway doesn't have one).
func TestHandleEffectivePolicyOverride_KeysListForUserWithTwoKeys(t *testing.T) {
	snap := baseOverrideSnapshot()
	// key2 has its own tighter row; key1 has none (inherits the user).
	snap.KeyRows = map[string]policy.PolicyRow{
		previewKeyID2: {AllowedModels: &[]string{"m1"}},
	}
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), snap, true)

	body := fmt.Sprintf(`{"user_id":%q,"override":{"level":"user","policy":{"allowed_models":["m1","m2"]}}}`, previewUserID)
	w, c := newEffectivePolicyOverrideRequest(t, middleware.KeyTypeService, body)
	h.HandleEffectivePolicyOverride(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodeOverridePreview(t, w.Body.Bytes())
	if len(got.Keys) != 2 {
		t.Fatalf("expected 2 keys in the keys list, got %d: %+v", len(got.Keys), got.Keys)
	}
	byID := map[string]overridePreviewKeyEntryForTest{}
	for _, k := range got.Keys {
		byID[k.APIKeyID] = k
	}
	k1, ok := byID[previewKeyID]
	if !ok {
		t.Fatalf("expected %s in keys list", previewKeyID)
	}
	if len(k1.Effective.AllowedModels) != 2 {
		t.Errorf("key1 (no row of its own) effective allowed_models = %v, want the user override's [m1 m2]", k1.Effective.AllowedModels)
	}
	k2, ok := byID[previewKeyID2]
	if !ok {
		t.Fatalf("expected %s in keys list", previewKeyID2)
	}
	if len(k2.Effective.AllowedModels) != 1 || k2.Effective.AllowedModels[0] != "m1" {
		t.Errorf("key2 (own row [m1]) effective allowed_models = %v, want [m1] (intersected with the user override)", k2.Effective.AllowedModels)
	}
	// api_key_id fields must be present on every entry, and no "name" field
	// leaks through (the gateway has none to give).
	if strings.Contains(string(w.Body.Bytes()), `"name"`) {
		t.Errorf("expected no \"name\" field in the gateway's keys list, got body=%s", w.Body.String())
	}
}

// --- POST /v1/policy/impact -------------------------------------------------

func newPolicyImpactRequest(t *testing.T, ctxType string, body string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, err := http.NewRequest(http.MethodPost, "/v1/policy/impact", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	c.Request = req
	c.Set(middleware.ContextKeyType, ctxType)
	return w, c
}

type impactPrincipalForTest struct {
	UserID   string `json:"user_id"`
	APIKeyID string `json:"api_key_id"`
}

type policyImpactBodyForTest struct {
	OrgLockedOut bool                     `json:"org_locked_out"`
	LockedOut    []impactPrincipalForTest `json:"locked_out"`
	OrgNarrowed  bool                     `json:"org_narrowed"`
	Narrowed     []impactPrincipalForTest `json:"narrowed"`
	Evaluated    int                      `json:"evaluated"`
}

func impactSet(ps []impactPrincipalForTest) map[string]bool {
	out := map[string]bool{}
	for _, p := range ps {
		out[p.UserID+"/"+p.APIKeyID] = true
	}
	return out
}

func decodePolicyImpact(t *testing.T, body []byte) policyImpactBodyForTest {
	t.Helper()
	var out policyImpactBodyForTest
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal impact response: %v; body=%s", err, body)
	}
	return out
}

// impactSnapshot: org allows m1,m2,m3. previewUserID has a user row allowing
// only m1 and owns previewKeyID (no row) and previewKeyID2 (row allowing m2 —
// already empty against its owner, which the api's 409 would normally have
// prevented, kept here to prove the walk reports the key's own chain).
// impactUser2 has no user row but owns impactKey3, which has a row allowing
// m3 only. impactUser3 inherits everything and owns nothing with a row.
const (
	impactUser2 = "44444444-4444-4444-4444-444444444444"
	impactUser3 = "55555555-5555-5555-5555-555555555555"
	impactKey3  = "66666666-6666-6666-6666-666666666666"
	impactKey4  = "77777777-7777-7777-7777-777777777777"
)

func impactSnapshot() policy.Snapshot {
	snap := baseOverrideSnapshot()
	snap.UserRows = map[string]policy.PolicyRow{
		previewUserID: {Level: policy.PrincipalTypeUser, AllowedModels: &[]string{"m1"}},
	}
	snap.KeyRows = map[string]policy.PolicyRow{
		previewKeyID2: {Level: policy.PrincipalTypeAPIKey, AllowedModels: &[]string{"m2"}},
		impactKey3:    {Level: policy.PrincipalTypeAPIKey, AllowedModels: &[]string{"m3"}},
	}
	snap.KeyOwner = map[string]string{
		previewKeyID:  previewUserID,
		previewKeyID2: previewUserID,
		impactKey3:    impactUser2,
		impactKey4:    impactUser3,
	}
	snap.UserKeys = map[string][]string{
		previewUserID: {previewKeyID, previewKeyID2},
		impactUser2:   {impactKey3},
		impactUser3:   {impactKey4},
	}
	return snap
}

func TestHandleEffectivePolicyImpact_403ForNonServiceKey(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), impactSnapshot(), true)
	w, c := newPolicyImpactRequest(t, middleware.KeyTypeUser, `{"override":{"level":"org","policy":{"allowed_models":[]}}}`)
	h.HandleEffectivePolicyImpact(c)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-service key, got %d; body=%s", w.Code, w.Body.String())
	}
	if e := decodeError(t, w.Body.Bytes()); e.Error.Code != "service_key_required" {
		t.Errorf("expected code service_key_required, got %q", e.Error.Code)
	}
}

func TestHandleEffectivePolicyImpact_400NonOrgLevel(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), impactSnapshot(), true)
	for _, body := range []string{`{}`, `{"override":{"level":"user","policy":null}}`, `not json`} {
		w, c := newPolicyImpactRequest(t, middleware.KeyTypeService, body)
		h.HandleEffectivePolicyImpact(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: expected 400, got %d; body=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestHandleEffectivePolicyImpact_503WhenPolicyNotLoaded(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), impactSnapshot(), false)
	w, c := newPolicyImpactRequest(t, middleware.KeyTypeService, `{"override":{"level":"org","policy":{"allowed_models":["m1"]}}}`)
	h.HandleEffectivePolicyImpact(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no policy snapshot, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleEffectivePolicyImpact_EmptyOrgAllowlistIsOrgLockedOut(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), impactSnapshot(), true)
	w, c := newPolicyImpactRequest(t, middleware.KeyTypeService, `{"override":{"level":"org","policy":{"allowed_models":[]}}}`)
	h.HandleEffectivePolicyImpact(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodePolicyImpact(t, w.Body.Bytes())
	if !got.OrgLockedOut {
		t.Errorf("expected org_locked_out=true for an empty org allowlist, got %+v", got)
	}
	if got.LockedOut == nil || got.Narrowed == nil {
		t.Errorf("locked_out and narrowed must be [] not null when org_locked_out; body=%s", w.Body.String())
	}
	if !got.OrgNarrowed {
		t.Errorf("org_narrowed must be true: the org reached m1..m3 and would reach nothing")
	}
}

func TestHandleEffectivePolicyImpact_NarrowingReportsExactlyTheDisjointPrincipals(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), impactSnapshot(), true)
	// Narrow the org to m2,m3: previewUserID (m1 only) and its inheriting key
	// previewKeyID lose everything; previewKeyID2 (m2 ∩ owner m1 = ∅) was
	// already empty and stays so; impactKey3 (m3) keeps m3; impactUser3
	// inherits and is never evaluated.
	w, c := newPolicyImpactRequest(t, middleware.KeyTypeService, `{"override":{"level":"org","policy":{"allowed_models":["m2","m3"]}}}`)
	h.HandleEffectivePolicyImpact(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodePolicyImpact(t, w.Body.Bytes())
	if got.OrgLockedOut {
		t.Fatalf("org_locked_out must be false for a non-empty org allowlist; body=%s", w.Body.String())
	}
	want := map[string]bool{
		previewUserID + "/":                 true,
		previewUserID + "/" + previewKeyID:  true,
		previewUserID + "/" + previewKeyID2: true,
	}
	gotSet := map[string]bool{}
	for _, p := range got.LockedOut {
		gotSet[p.UserID+"/"+p.APIKeyID] = true
	}
	for k := range want {
		if !gotSet[k] {
			t.Errorf("expected %s in locked_out, got %+v", k, got.LockedOut)
		}
	}
	for k := range gotSet {
		if !want[k] {
			t.Errorf("unexpected %s in locked_out", k)
		}
	}
	// previewUserID + 2 keys, impactUser2 + 1 key = 5. impactUser3 has no row
	// and no key with a row, so it is not walked.
	if got.Evaluated != 5 {
		t.Errorf("evaluated = %d, want 5", got.Evaluated)
	}
	// Reaches-less: the org itself (m1,m2,m3 -> m2,m3) so every inheriting
	// principal; previewUserID and previewKeyID (m1 -> nothing);
	// previewKeyID2 already reached nothing so it is NOT narrowed;
	// impactUser2 (inherits org: m1,m2,m3 -> m2,m3) and impactKey3 (m3 -> m3,
	// unchanged) — so impactUser2 yes, impactKey3 no.
	if !got.OrgNarrowed {
		t.Errorf("org_narrowed must be true when the org list shrinks")
	}
	wantNarrowed := map[string]bool{
		previewUserID + "/":                true,
		previewUserID + "/" + previewKeyID: true,
		impactUser2 + "/":                  true,
	}
	gotNarrowed := impactSet(got.Narrowed)
	for k := range wantNarrowed {
		if !gotNarrowed[k] {
			t.Errorf("expected %s in narrowed, got %+v", k, got.Narrowed)
		}
	}
	for k := range gotNarrowed {
		if !wantNarrowed[k] {
			t.Errorf("unexpected %s in narrowed", k)
		}
	}
}

func TestHandleEffectivePolicyImpact_WideningLocksNobodyOut(t *testing.T) {
	h := newTestHandler(fakeProviders{}, baseOverrideRoutes(), impactSnapshot(), true)
	// null = inherit-all at org level; the only remaining empty set is
	// previewKeyID2 (m2 ∩ owner m1), which is pre-existing, not caused here —
	// the endpoint reports state under the override, so it is still listed.
	w, c := newPolicyImpactRequest(t, middleware.KeyTypeService, `{"override":{"level":"org","policy":null}}`)
	h.HandleEffectivePolicyImpact(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodePolicyImpact(t, w.Body.Bytes())
	if len(got.LockedOut) != 1 || got.LockedOut[0].APIKeyID != previewKeyID2 {
		t.Errorf("expected only the pre-existing empty key %s, got %+v", previewKeyID2, got.LockedOut)
	}
	if got.OrgNarrowed || len(got.Narrowed) != 0 {
		t.Errorf("a widening must narrow nobody: org_narrowed=%v narrowed=%+v", got.OrgNarrowed, got.Narrowed)
	}
}

func TestHandleEffectivePolicyImpact_ResidencyTighteningNarrowsViaReachability(t *testing.T) {
	// m3 becomes cloud-managed; tightening the org to self_hosted_only drops
	// it from everyone's reachable set even though no allowlist changes —
	// the org is narrowed, and so is impactKey3 (m3 only -> nothing
	// reachable, but NOT locked out: its allowlist is still [m3]).
	routes := baseOverrideRoutes()
	routes["m3"] = routing.Route{ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}
	h := newTestHandler(fakeProviders{}, routes, impactSnapshot(), true)
	w, c := newPolicyImpactRequest(t, middleware.KeyTypeService, `{"override":{"level":"org","policy":{"residency":"self_hosted_only"}}}`)
	h.HandleEffectivePolicyImpact(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := decodePolicyImpact(t, w.Body.Bytes())
	if !got.OrgNarrowed {
		t.Errorf("org_narrowed must be true when residency removes a reachable cloud model")
	}
	narrowed := impactSet(got.Narrowed)
	if !narrowed[impactUser2+"/"+impactKey3] {
		t.Errorf("impactKey3 (m3 only) must be narrowed, got %+v", got.Narrowed)
	}
	if narrowed[previewUserID+"/"] {
		t.Errorf("previewUserID (m1 only, self-hosted) must not be narrowed")
	}
	if len(got.LockedOut) != 1 || got.LockedOut[0].APIKeyID != previewKeyID2 {
		t.Errorf("residency never locks anyone out; only the pre-existing empty key expected, got %+v", got.LockedOut)
	}
}
