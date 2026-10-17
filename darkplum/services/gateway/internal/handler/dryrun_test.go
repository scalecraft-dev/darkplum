package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/router"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

func doDryrun(t *testing.T, h *ChatHandler, alias, text string) (*httptest.ResponseRecorder, routingDryrunResponse) {
	t.Helper()
	body, _ := json.Marshal(routingDryrunRequest{Alias: alias, Text: text})
	req, err := http.NewRequest(http.MethodPost, "/v1/routing/dryrun", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	h.HandleRoutingDryrun(c)

	var resp routingDryrunResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return w, resp
}

// TestHandleRoutingDryrun_NeverCallsTarget is the core contract: even for a
// route whose target IS enabled/allowed, the target provider must never be
// dispatched to.
func TestHandleRoutingDryrun_NeverCallsTarget(t *testing.T) {
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

	w, resp := doDryrun(t, h, "smart-chat", "please fix this bug")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if resp.RouteName != "code" || resp.TargetModelID != "qwen-coder" || resp.Reason != "rules:code:debug" {
		t.Errorf("got %+v", resp)
	}
	if resp.WouldBeRefused != nil {
		t.Errorf("expected no refusal for an enabled/allowed target, got %+v", resp.WouldBeRefused)
	}
	if vllm.completeCalls != 0 || vllm.streamCalls != 0 {
		t.Errorf("dryrun must never call the target model, got complete=%d stream=%d", vllm.completeCalls, vllm.streamCalls)
	}
}

// TestHandleRoutingDryrun_ReportsWouldBeRefused_NotEnabled asserts the
// refusal-detection path (without ever calling the target).
func TestHandleRoutingDryrun_ReportsWouldBeRefused_NotEnabled(t *testing.T) {
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:debug"}}}
	h := newRoutedTestHandler(
		fakeProviders{},
		map[string]routing.Route{"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"qwen-coder": false},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencyAny,
		},
		classifier,
	)

	_, resp := doDryrun(t, h, "smart-chat", "please fix this bug")
	if resp.WouldBeRefused == nil || resp.WouldBeRefused.Code != "model_not_enabled" {
		t.Errorf("got %+v", resp.WouldBeRefused)
	}
}

// TestHandleRoutingDryrun_ReportsWouldBeRefused_Residency covers the
// residency-refusal branch specifically.
func TestHandleRoutingDryrun_ReportsWouldBeRefused_Residency(t *testing.T) {
	classifier := &fakeClassifier{decisions: []router.Decision{{RouteName: "cloud-only", Reason: "rules:cloud-only:cloud"}}}
	h := newRoutedTestHandler(
		fakeProviders{},
		map[string]routing.Route{"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}},
		policy.Snapshot{
			Enabled:   map[string]bool{"us.anthropic.claude-3-5-sonnet": true},
			Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
			Residency: policy.ResidencySelfHostedOnly,
		},
		classifier,
	)

	_, resp := doDryrun(t, h, "smart-chat", "please use claude")
	if resp.WouldBeRefused == nil || resp.WouldBeRefused.Code != "model_residency_violation" {
		t.Errorf("got %+v", resp.WouldBeRefused)
	}
}

// TestHandleRoutingDryrun_RejectsStaticAlias asserts the 400 for an alias
// that exists but is not routed.
func TestHandleRoutingDryrun_RejectsStaticAlias(t *testing.T) {
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"llama-3-8b": true},
			Aliases:   map[string]policy.AliasDef{"prod-default": {Kind: policy.AliasKindStatic, Target: "llama-3-8b"}},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	w, _ := doDryrun(t, h, "prod-default", "hello")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a static alias, got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleRoutingDryrun_RejectsUnknownAlias asserts the 400 for an alias
// name that doesn't exist at all.
func TestHandleRoutingDryrun_RejectsUnknownAlias(t *testing.T) {
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{},
		policy.Snapshot{Enabled: map[string]bool{}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doDryrun(t, h, "does-not-exist", "hello")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown alias, got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleRoutingDryrun_MissingFields asserts the 400 for a body missing
// alias or text.
func TestHandleRoutingDryrun_MissingFields(t *testing.T) {
	h := newTestHandler(fakeProviders{}, map[string]routing.Route{}, policy.Snapshot{}, true)

	w, _ := doDryrun(t, h, "", "hello")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing alias, got %d", w.Code)
	}
	w2, _ := doDryrun(t, h, "smart-chat", "")
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing text, got %d", w2.Code)
	}
}

// TestHandleRoutingDryrun_ReportsFallbackDecision: the dryrun reports the
// same decision a real completion would take for a disabled/unreachable pick.
func TestHandleRoutingDryrun_ReportsFallbackDecision(t *testing.T) {
	cases := []struct {
		name    string
		table   map[string]routing.Route
		enabled map[string]bool
		reason  string
	}{
		{"disabled", map[string]routing.Route{
			"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		}, map[string]bool{"qwen-coder": false, "llama-3-8b": true}, "fallback:route_target_disabled:code"},
		{"unreachable", map[string]routing.Route{
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		}, map[string]bool{"llama-3-8b": true}, "fallback:route_target_unreachable:code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRoutedTestHandler(fakeProviders{}, tc.table, policy.Snapshot{
				Enabled:   tc.enabled,
				Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
				Residency: policy.ResidencyAny,
			}, &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:x"}}})
			w, resp := doDryrun(t, h, "smart-chat", "fix this")
			if w.Code != http.StatusOK {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if resp.RouteName != "general" || resp.TargetModelID != "llama-3-8b" || resp.Reason != tc.reason || resp.WouldBeRefused != nil {
				t.Errorf("got %+v", resp)
			}
		})
	}
}

func TestHandleRoutingDryrun_BothDisabled_ReportsTwoModelRefusal(t *testing.T) {
	h := newRoutedTestHandler(fakeProviders{}, map[string]routing.Route{
		"qwen-coder": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
	}, policy.Snapshot{
		Enabled:   map[string]bool{},
		Aliases:   map[string]policy.AliasDef{"smart-chat": routedAlias},
		Residency: policy.ResidencyAny,
	}, &fakeClassifier{decisions: []router.Decision{{RouteName: "code", Reason: "rules:code:x"}}})
	_, resp := doDryrun(t, h, "smart-chat", "fix this")
	if resp.RouteName != "code" || resp.Reason != "rules:code:x" || resp.WouldBeRefused == nil || resp.WouldBeRefused.Code != "model_not_enabled" {
		t.Fatalf("got %+v", resp)
	}
	if !strings.Contains(resp.WouldBeRefused.Message, `default route "general" selects "llama-3-8b", which is also disabled`) {
		t.Errorf("message = %q", resp.WouldBeRefused.Message)
	}
}
