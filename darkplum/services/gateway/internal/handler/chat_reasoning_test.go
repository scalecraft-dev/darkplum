package handler

// Phase A3 of #3: reasoning_effort is forwarded (openai), translated to
// extended thinking per model (anthropic, bedrock-served Claude), or dropped
// with a record on the dropped-params seam; forced tool_choice with thinking is
// a contract refusal.

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/bedrock"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// reasoningProvider is a capturing tool-capable provider whose reasoning plan
// comes from plan (a real provider's PlanReasoning, or a stub).
type reasoningProvider struct {
	toolCapturingProvider
	plan func(model, effort string, maxTokens int) provider.ReasoningPlan
}

func (p *reasoningProvider) PlanReasoning(model, effort string, maxTokens int) provider.ReasoningPlan {
	return p.plan(model, effort, maxTokens)
}

const claudeBedrockModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"

func reasoningHandler(t *testing.T, model, kind, class string, caps provider.ParamCaps,
	plan func(string, string, int) provider.ReasoningPlan) (*ChatHandler, *reasoningProvider) {
	t.Helper()
	rp := &reasoningProvider{toolCapturingProvider: toolCapturingProvider{capturingProvider{name: kind + "-default", kind: kind, caps: &caps}}, plan: plan}
	if plan == nil {
		rp.plan = func(string, string, int) provider.ReasoningPlan { return provider.ReasoningPlan{} }
	}
	h := newTestHandler(
		fakeProviders{rp.name: rp},
		map[string]routing.Route{model: {ProviderKey: rp.name, Kind: kind, Class: class}},
		policy.Snapshot{Enabled: map[string]bool{model: true}, Residency: policy.ResidencyAny},
		true,
	)
	return h, rp
}

func bedrockReasoningHandler(t *testing.T, model string) (*ChatHandler, *reasoningProvider) {
	bp := &bedrock.BedrockProvider{}
	return reasoningHandler(t, model, "bedrock", routing.ClassCloudManaged, bp.ParamCaps(), bp.PlanReasoning)
}

// actions is the raw seam; dropped is just the parameters recorded "dropped".
func actions(c interface{ Get(string) (any, bool) }) []paramAction {
	v, ok := c.Get(ctxKeyDroppedParams)
	if !ok {
		return nil
	}
	return v.([]paramAction)
}

func dropped(c interface{ Get(string) (any, bool) }) []string {
	var out []string
	for _, a := range actions(c) {
		if a.Action == actionDropped {
			out = append(out, a.Param)
		}
	}
	return out
}

func TestReasoning_OpenAIForwardsAsSent(t *testing.T) {
	op := &openai.Provider{}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		h, prov := reasoningHandler(t, "m", "openai", routing.ClassExternalAPI, op.ParamCaps(), op.PlanReasoning)
		body := baseChatBody("m")
		body["reasoning_effort"] = effort
		body["temperature"] = 0.4
		w, c := doRawChat(t, h, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", effort, w.Code, w.Body.String())
		}
		r := prov.lastReq
		if r.ReasoningEffort != effort || r.ThinkingBudget != 0 || r.Temperature == nil {
			t.Errorf("%s: req = effort %q budget %d temp %v", effort, r.ReasoningEffort, r.ThinkingBudget, r.Temperature)
		}
		if d := dropped(c); len(d) != 0 {
			t.Errorf("%s: dropped = %v", effort, d)
		}
	}
}

func TestReasoning_AbsentAndNullChangeNothing(t *testing.T) {
	h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
	for _, v := range []any{nil, "__absent__"} {
		body := baseChatBody(claudeBedrockModel)
		if v == nil {
			body["reasoning_effort"] = nil
		}
		w, c := doRawChat(t, h, body)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		if prov.lastReq.ThinkingBudget != 0 || prov.lastReq.ReasoningEffort != "" || len(dropped(c)) != 0 {
			t.Errorf("req = %+v dropped %v", prov.lastReq, dropped(c))
		}
	}
}

func TestReasoning_InvalidValueIs400WithZeroProviderCalls(t *testing.T) {
	h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
	body := baseChatBody(claudeBedrockModel)
	body["reasoning_effort"] = "extreme"
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusBadRequest || prov.calls != 0 {
		t.Fatalf("status %d calls %d: %s", w.Code, prov.calls, w.Body.String())
	}
}

func TestReasoning_BedrockClaudeTranslatesToThinking(t *testing.T) {
	budgets := map[string]int{"minimal": 1024, "low": 1024, "medium": 4096, "high": 16384, "xhigh": 16384}
	for effort, budget := range budgets {
		h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
		body := baseChatBody(claudeBedrockModel)
		body["reasoning_effort"] = effort
		body["max_tokens"] = 2000
		w, _ := doRawChat(t, h, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", effort, w.Code, w.Body.String())
		}
		r := prov.lastReq
		if r.ThinkingBudget != budget || r.MaxTokens <= r.ThinkingBudget || r.ReasoningEffort != "" {
			t.Errorf("%s: budget %d max_tokens %d effort %q", effort, r.ThinkingBudget, r.MaxTokens, r.ReasoningEffort)
		}
	}
}

func TestReasoning_ThinkingNoneIsNotADrop(t *testing.T) {
	h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
	body := baseChatBody(claudeBedrockModel)
	body["reasoning_effort"] = "none"
	body["temperature"] = 0.2
	body["tool_choice"] = "required"
	body["tools"] = []map[string]any{weatherTool}
	w, c := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if prov.lastReq.ThinkingBudget != 0 || prov.lastReq.Temperature == nil || len(dropped(c)) != 0 {
		t.Errorf("none changed the request: %+v dropped %v", prov.lastReq, dropped(c))
	}
}

func TestReasoning_ThinkingDropsSamplingParamsAndRecordsThem(t *testing.T) {
	cases := []struct {
		name          string
		temp, topP    any
		topK          any
		wantDropped   []string
		wantTopPNil   bool
		wantTopPValue float64
	}{
		{"temperature", 0.5, nil, nil, []string{"temperature"}, false, 0},
		{"top_p below 0.95", nil, 0.9, nil, []string{"top_p"}, true, 0},
		{"top_p at 0.95 kept", nil, 0.95, nil, nil, false, 0.95},
		{"top_p 1 kept", nil, 1.0, nil, nil, false, 1},
		{"top_k", nil, nil, 40, []string{"top_k"}, false, 0},
		{"all three", 0.5, 0.5, 40, []string{"temperature", "top_p", "top_k"}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
			body := baseChatBody(claudeBedrockModel)
			body["reasoning_effort"] = "medium"
			if tc.temp != nil {
				body["temperature"] = tc.temp
			}
			if tc.topP != nil {
				body["top_p"] = tc.topP
			}
			if tc.topK != nil {
				body["top_k"] = tc.topK
			}
			w, c := doRawChat(t, h, body)
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			r := prov.lastReq
			if r.ThinkingBudget != 4096 {
				t.Fatalf("budget = %d", r.ThinkingBudget)
			}
			if r.Temperature != nil {
				t.Errorf("temperature reached the provider with thinking on: %v", *r.Temperature)
			}
			if tc.wantTopPNil && r.TopP != nil {
				t.Errorf("top_p forwarded: %v", *r.TopP)
			}
			if !tc.wantTopPNil && tc.topP != nil && (r.TopP == nil || *r.TopP != tc.wantTopPValue) {
				t.Errorf("top_p = %v, want %v", r.TopP, tc.wantTopPValue)
			}
			if got := dropped(c); !reflect.DeepEqual(got, tc.wantDropped) {
				t.Errorf("dropped = %v, want %v", got, tc.wantDropped)
			}
		})
	}
}

func TestReasoning_UnsupportedModelDropsWithRecord(t *testing.T) {
	// Bedrock model that does not take thinking; the sampling params must
	// pass through untouched because thinking is not on.
	const model = "us.anthropic.claude-3-5-sonnet-20241022-v2:0"
	h, prov := bedrockReasoningHandler(t, model)
	body := baseChatBody(model)
	body["reasoning_effort"] = "high"
	body["temperature"] = 0.3
	body["tool_choice"] = "required" // would be refused if thinking were on
	body["tools"] = []map[string]any{weatherTool}
	w, c := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	r := prov.lastReq
	if r.ThinkingBudget != 0 || r.ReasoningEffort != "" || r.Temperature == nil {
		t.Errorf("req = %+v", r)
	}
	if got := dropped(c); !reflect.DeepEqual(got, []string{"reasoning_effort"}) {
		t.Errorf("dropped = %v", got)
	}
}

func TestReasoning_BackendWithoutSupportDropsWithRecord(t *testing.T) {
	// vLLM / mock / any provider that does not implement ReasoningCapable.
	for _, kind := range []string{"vllm", "mock"} {
		h, prov := reasoningHandler(t, "m", kind, routing.ClassSelfHosted, vllmCaps, nil)
		prov.plan = func(string, string, int) provider.ReasoningPlan { return provider.ReasoningPlan{} }
		body := baseChatBody("m")
		body["reasoning_effort"] = "low"
		w, c := doRawChat(t, h, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", kind, w.Code, w.Body.String())
		}
		if prov.lastReq.ReasoningEffort != "" || prov.lastReq.ThinkingBudget != 0 {
			t.Errorf("%s: effort leaked: %+v", kind, prov.lastReq)
		}
		if got := dropped(c); !reflect.DeepEqual(got, []string{"reasoning_effort"}) {
			t.Errorf("%s: dropped = %v", kind, got)
		}
	}
	// Also a provider with no PlanReasoning method at all.
	h, prov := paramsHandler(t, allBackends()[1])
	body := baseChatBody("m")
	body["reasoning_effort"] = "low"
	w, c := doRawChat(t, h, body)
	if w.Code != http.StatusOK || prov.lastReq.ReasoningEffort != "" || !reflect.DeepEqual(dropped(c), []string{"reasoning_effort"}) {
		t.Errorf("status %d req %+v dropped %v", w.Code, prov.lastReq, dropped(c))
	}
}

func TestReasoning_ForcedToolChoiceWithThinkingIsRefused(t *testing.T) {
	choices := map[string]any{
		"required": "required",
		"named":    map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
	}
	for name, choice := range choices {
		h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
		body := baseChatBody(claudeBedrockModel)
		body["reasoning_effort"] = "high"
		body["tools"] = []map[string]any{weatherTool}
		body["tool_choice"] = choice
		w, _ := doRawChat(t, h, body)
		t.Run(name, func(t *testing.T) {
			assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, "reasoning_effort")
		})
	}
	// auto and none are compatible with thinking.
	for _, choice := range []string{"auto", "none"} {
		h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
		body := baseChatBody(claudeBedrockModel)
		body["reasoning_effort"] = "high"
		body["tools"] = []map[string]any{weatherTool}
		body["tool_choice"] = choice
		if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || prov.lastReq.ThinkingBudget != 16384 {
			t.Errorf("tool_choice %s: %d budget %d", choice, w.Code, prov.lastReq.ThinkingBudget)
		}
	}
}

func TestReasoning_ForcedToolChoiceStillRefusedByA1OnItsOwnBackends(t *testing.T) {
	// The A1 refusal keeps precedence: a backend that cannot force a tool
	// call at all names tool_choice, not reasoning_effort.
	caps := provider.ParamCaps{}
	h, prov := reasoningHandler(t, "m", "x", routing.ClassSelfHosted, caps,
		func(string, string, int) provider.ReasoningPlan {
			return provider.ReasoningPlan{Mode: provider.ReasoningThinking, Budget: 1024, MaxTokens: 5000}
		})
	body := baseChatBody("m")
	body["reasoning_effort"] = "low"
	body["tools"] = []map[string]any{weatherTool}
	body["tool_choice"] = "required"
	w, _ := doRawChat(t, h, body)
	assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, "tool_choice")
}

func TestReasoning_ToolTurnHistoryTurnsThinkingOff(t *testing.T) {
	history := []map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": `{"city":"x"}`},
		}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
	}
	h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
	body := map[string]any{"model": claudeBedrockModel, "messages": history,
		"reasoning_effort": "high", "temperature": 0.3, "tools": []map[string]any{weatherTool}}
	w, c := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	r := prov.lastReq
	if r.ThinkingBudget != 0 || r.Temperature == nil {
		t.Errorf("thinking on after a tool turn: %+v", r)
	}
	if got := dropped(c); !reflect.DeepEqual(got, []string{"reasoning_effort"}) {
		t.Errorf("dropped = %v", got)
	}

	// Thinking is off, so a forced tool_choice is no longer in conflict.
	h, prov = bedrockReasoningHandler(t, claudeBedrockModel)
	body["tool_choice"] = "required"
	if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || prov.lastReq.ThinkingBudget != 0 {
		t.Errorf("forced choice on a tool-turn history: %d", w.Code)
	}

	// The same request without the tool turns thinks.
	h, prov = bedrockReasoningHandler(t, claudeBedrockModel)
	body = map[string]any{"model": claudeBedrockModel, "messages": history[:1], "reasoning_effort": "high"}
	if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || prov.lastReq.ThinkingBudget != 16384 {
		t.Errorf("first turn: %d budget %d", w.Code, prov.lastReq.ThinkingBudget)
	}
}

func TestReasoning_AnthropicPlanSeen(t *testing.T) {
	// The anthropic provider with no discovery has described no model, so
	// every model is unsupported: dropped, never sent.
	ap := anthropic.New(nil, anthropic.Config{DefaultMaxTokens: 4096})
	h, prov := reasoningHandler(t, "claude-sonnet-4-5", "anthropic", routing.ClassExternalAPI, ap.ParamCaps(), ap.PlanReasoning)
	body := baseChatBody("claude-sonnet-4-5")
	body["reasoning_effort"] = "high"
	w, c := doRawChat(t, h, body)
	if w.Code != http.StatusOK || prov.lastReq.ThinkingBudget != 0 || !reflect.DeepEqual(dropped(c), []string{"reasoning_effort"}) {
		t.Errorf("status %d req %+v dropped %v", w.Code, prov.lastReq, dropped(c))
	}
}

func TestReasoning_ToolTurnHistoryWithForcedToolChoiceDropsReasoningNo400(t *testing.T) {
	history := []map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": `{}`},
		}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
	}
	h, prov := bedrockReasoningHandler(t, claudeBedrockModel)
	body := map[string]any{"model": claudeBedrockModel, "messages": history, "reasoning_effort": "high",
		"tools": []map[string]any{weatherTool}, "tool_choice": "required"}
	w, c := doRawChat(t, h, body)
	if w.Code != http.StatusOK || prov.calls != 1 || prov.lastReq.ThinkingBudget != 0 {
		t.Fatalf("status %d calls %d budget %d: %s", w.Code, prov.calls, prov.lastReq.ThinkingBudget, w.Body.String())
	}
	if got := dropped(c); !reflect.DeepEqual(got, []string{"reasoning_effort"}) {
		t.Errorf("dropped = %v", got)
	}
}

func TestReasoning_SeamIsAppendOnlyAndRecordsTheRaise(t *testing.T) {
	h, _ := bedrockReasoningHandler(t, claudeBedrockModel)
	body := baseChatBody(claudeBedrockModel)
	body["reasoning_effort"] = "medium"
	body["temperature"] = 0.5
	body["top_p"] = 0.5
	body["top_k"] = 5
	_, c := doRawChat(t, h, body)
	want := []paramAction{{"max_tokens", actionRaised}, {"temperature", actionDropped}, {"top_p", actionDropped}, {"top_k", actionDropped}}
	if got := actions(c); !reflect.DeepEqual(got, want) {
		t.Errorf("actions = %v, want %v", got, want)
	}
	// Appending never overwrites an earlier entry.
	recordParamActions(c, []paramAction{{"x", actionDropped}})
	if got := actions(c); len(got) != 5 || got[0] != want[0] || got[4].Param != "x" {
		t.Errorf("after append = %v", got)
	}
}
