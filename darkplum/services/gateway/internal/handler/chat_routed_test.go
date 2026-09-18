package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/recorder"
	"github.com/scalecraft/darkplum/gateway/internal/router"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

// routedAlias is a routed alias with two candidate routes and a default,
// used across the tests below; the classifier's Decision determines which
// one gets exercised.
var routedAlias = policy.AliasDef{
	Kind:         policy.AliasKindRouted,
	DefaultRoute: "general",
	Routes: []policy.RouteDef{
		{Name: "code", Description: "code questions", Target: "qwen-coder"},
		{Name: "cloud-only", Description: "residency-blocked route", Target: "us.anthropic.claude-3-5-sonnet"},
		{Name: "general", Description: "everything else", Target: "llama-3-8b"},
	},
}

// ---------------------------------------------------------------------------
// Happy path: dispatch, ctx keys, headers (non-stream)
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_RoutedAlias_DispatchesToClassifiedRoute(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, c := doChat(t, h, "smart-chat")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if vllm.completeCalls != 1 {
		t.Fatalf("expected exactly one dispatch, got %d", vllm.completeCalls)
	}

	if resolved, _ := c.Get("resolved_model"); resolved != "qwen-coder" {
		t.Errorf("resolved_model = %v, want qwen-coder", resolved)
	}
	if requested, _ := c.Get("requested_model"); requested != "smart-chat" {
		t.Errorf("requested_model = %v, want smart-chat", requested)
	}
	if rn, _ := c.Get("route_name"); rn != "code" {
		t.Errorf("route_name = %v, want code", rn)
	}
	if rr, _ := c.Get("router_reason"); rr != "rules:code:debug" {
		t.Errorf("router_reason = %v, want rules:code:debug", rr)
	}
	if _, ok := c.Get("router_latency_ms"); !ok {
		t.Error("expected router_latency_ms to be set")
	}

	if got := w.Header().Get("X-Darkplum-Model"); got != "qwen-coder" {
		t.Errorf("X-Darkplum-Model = %q, want qwen-coder", got)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "code" {
		t.Errorf("X-Darkplum-Route = %q, want code", got)
	}
	if got := w.Header().Get("X-Darkplum-Router-Reason"); got != "rules:code:debug" {
		t.Errorf("X-Darkplum-Router-Reason = %q, want rules:code:debug", got)
	}

	if len(classifier.calls) != 1 {
		t.Fatalf("expected the classifier to be called exactly once, got %d", len(classifier.calls))
	}
	if classifier.calls[0].Text != "hello" {
		t.Errorf("expected the classifier to see the last user turn, got %q", classifier.calls[0].Text)
	}
}

// TestHandleChatCompletions_PlainModel_HeaderSetWithoutRoute asserts
// X-Darkplum-Model is set on every dispatched chat response, and the two
// routed-only headers are absent when no routed alias was used.
func TestHandleChatCompletions_PlainModel_HeaderSetWithoutRoute(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Darkplum-Model"); got != "llama-3-8b" {
		t.Errorf("X-Darkplum-Model = %q, want llama-3-8b", got)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "" {
		t.Errorf("X-Darkplum-Route should be absent for a non-routed model, got %q", got)
	}
	if got := w.Header().Get("X-Darkplum-Router-Reason"); got != "" {
		t.Errorf("X-Darkplum-Router-Reason should be absent for a non-routed model, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Streaming path: headers set before any body byte
// ---------------------------------------------------------------------------

func buildStreamingChatRequest(t *testing.T, model string) *http.Request {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestHandleChatCompletions_RoutedAlias_HeadersPresentOnStream(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildStreamingChatRequest(t, "smart-chat")
	h.HandleChatCompletions(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Darkplum-Model"); got != "qwen-coder" {
		t.Errorf("X-Darkplum-Model = %q, want qwen-coder", got)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "code" {
		t.Errorf("X-Darkplum-Route = %q, want code", got)
	}
	if got := w.Header().Get("X-Darkplum-Router-Reason"); got != "rules:code:debug" {
		t.Errorf("X-Darkplum-Router-Reason = %q, want rules:code:debug", got)
	}
	if vllm.streamCalls != 1 {
		t.Errorf("expected exactly one stream dispatch, got %d", vllm.streamCalls)
	}
}

// ---------------------------------------------------------------------------
// Forbidden pick: never swapped for default_route or any other route
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_RoutedAlias_ForbiddenPick_NotEnabled(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			// "code"'s target is NOT enabled; "general"'s target (the
			// default_route) IS enabled — the pick must still be refused,
			// never silently swapped to the allowed default.
			Enabled:   map[string]bool{"qwen-coder": false, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, _ := doChat(t, h, "smart-chat")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_enabled" {
		t.Errorf("expected code model_not_enabled, got %q", e.Error.Code)
	}
	wantMsg := `route "code" of alias "smart-chat" selects model "qwen-coder", which is not enabled/allowed by residency`
	if e.Error.Message != wantMsg {
		t.Errorf("message = %q, want %q", e.Error.Message, wantMsg)
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must never be called for a forbidden pick, got %d calls", vllm.completeCalls)
	}
}

// TestHandleChatCompletions_RoutedAlias_ForbiddenPick_Residency is the
// residency-on-selected-target case from the contract's test list.
func TestHandleChatCompletions_RoutedAlias_ForbiddenPick_Residency(t *testing.T) {
	brk := &fakeNameOnlyProvider{name: "bedrock"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "cloud-only", Reason: "rules:cloud-only:cloud"}}}
	h := newRoutedTestHandler(
		fakeProviders{"bedrock": brk},
		map[string]routing.Route{
			"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged},
			"llama-3-8b":                     {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"us.anthropic.claude-3-5-sonnet": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencySelfHostedOnly,
		},
		classifier,
	)

	w, _ := doChat(t, h, "smart-chat")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_residency_violation" {
		t.Errorf("expected code model_residency_violation, got %q", e.Error.Code)
	}
	wantMsg := `route "cloud-only" of alias "smart-chat" selects model "us.anthropic.claude-3-5-sonnet", which is not enabled/allowed by residency`
	if e.Error.Message != wantMsg {
		t.Errorf("message = %q, want %q", e.Error.Message, wantMsg)
	}
	if brk.completeCalls != 0 {
		t.Errorf("provider must never be called for a residency-forbidden pick, got %d calls", brk.completeCalls)
	}
	if got := w.Header().Get("X-Darkplum-Route"); got != "cloud-only" {
		t.Errorf("expected X-Darkplum-Route to still be set on a 4xx refusal where a route was chosen, got %q", got)
	}
}

func TestHandleChatCompletions_RoutedAlias_PickedTargetNotFound(t *testing.T) {
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{},
		map[string]routing.Route{}, // "code"'s target was never discovered
		policy.Snapshot{
			Enabled:   map[string]bool{},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, _ := doChat(t, h, "smart-chat")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_found" {
		t.Errorf("expected code model_not_found, got %q", e.Error.Code)
	}
}

// TestHandleChatCompletions_RoutedAlias_BogusRouteName_StillAudited covers
// the defensive fail-closed path: a classifier (here standing in for
// ModelClassifier) names a route that isn't in alias.Routes and isn't
// alias.DefaultRoute — normally unreachable per policy.buildAliases's
// invariants, but a real ModelClassifier could still hand back an
// out-of-band string if that invariant were ever violated. The request must
// still be refused with 404 model_not_found, but if the classifier actually
// called an LLM (ModelCall != nil) that call's purpose="routing" audit event
// must still be recorded — the model saw the prompt regardless of how the
// route name it returned was handled afterwards.
func TestHandleChatCompletions_RoutedAlias_BogusRouteName_StillAudited(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	pt, ctok := 9, 3
	classifier := &fakeClassifier{decisions: []router.Decision{{
		RouteName: "does-not-exist",
		Reason:    "model:does-not-exist",
		ModelCall: &router.ModelCallInfo{
			Model: "router-model", Provider: "vllm", LatencyMS: 5, StatusCode: 200,
			PromptTokens: &pt, CompletionTokens: &ctok,
		},
	}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, c := doChat(t, h, "smart-chat")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_found" {
		t.Errorf("expected code model_not_found, got %q", e.Error.Code)
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must never be dispatched to for an unresolved route, got %d calls", vllm.completeCalls)
	}

	v, ok := c.Get("routing_audit_event")
	if !ok {
		t.Fatal("expected a routing_audit_event to still be set even though the route name didn't resolve — the classifier model did see the prompt")
	}
	evt, ok := v.(recorder.InferenceEvent)
	if !ok {
		t.Fatalf("routing_audit_event has unexpected type %T", v)
	}
	if evt.Purpose != "routing" || evt.Model != "router-model" || evt.LatencyMS != 5 {
		t.Errorf("got %+v", evt)
	}
}

// ---------------------------------------------------------------------------
// Aborted classification: never dispatch
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_RoutedAlias_ClassifierAborted_NeverDispatches(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{errs: []error{router.ErrAborted}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	w, c := doChat(t, h, "smart-chat")
	if w.Code != 499 {
		t.Fatalf("expected 499, got %d; body=%s", w.Code, w.Body.String())
	}
	if vllm.completeCalls != 0 {
		t.Errorf("provider must never be called when classification is aborted, got %d calls", vllm.completeCalls)
	}
	if code, _ := c.Get("denial_code"); code != "request_canceled" {
		t.Errorf("denial_code = %v, want request_canceled", code)
	}
}

// ---------------------------------------------------------------------------
// Second audit row: only for a model classifier call
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_RoutedAlias_ModelClassifier_SetsRoutingAuditEvent(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	pt, ctok := 12, 2
	classifier := &fakeClassifier{decisions: []router.Decision{{
		RouteName: "code",
		Reason:    "model:code",
		ModelCall: &router.ModelCallInfo{
			Model: "router-model", Provider: "vllm", LatencyMS: 7, StatusCode: 200,
			PromptTokens: &pt, CompletionTokens: &ctok,
		},
	}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": true, "llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	_, c := doChat(t, h, "smart-chat")

	v, ok := c.Get("routing_audit_event")
	if !ok {
		t.Fatal("expected a routing_audit_event to be set on ctx for a model classifier call")
	}
	evt, ok := v.(recorder.InferenceEvent)
	if !ok {
		t.Fatalf("routing_audit_event has unexpected type %T", v)
	}
	if evt.Purpose != "routing" || evt.Model != "router-model" || evt.LatencyMS != 7 {
		t.Errorf("got %+v", evt)
	}
	if evt.PromptTokens == nil || *evt.PromptTokens != 12 || evt.CompletionTokens == nil || *evt.CompletionTokens != 2 {
		t.Errorf("token counts not carried through: %+v", evt)
	}
}

// TestHandleChatCompletions_RulesClassifier_NoRoutingAuditEvent asserts the
// rules classifier (no LLM ever saw the prompt) never sets a second-row ctx
// key — the rules-classifier Decision from the fake below has ModelCall ==
// nil, matching a real RulesClassifier's contract.
func TestHandleChatCompletions_RulesClassifier_NoRoutingAuditEvent(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "general", Reason: "rules:default"}}}
	h := newRoutedTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	_, c := doChat(t, h, "smart-chat")
	if _, ok := c.Get("routing_audit_event"); ok {
		t.Error("expected no routing_audit_event for a rules classifier decision (no model call)")
	}
}
