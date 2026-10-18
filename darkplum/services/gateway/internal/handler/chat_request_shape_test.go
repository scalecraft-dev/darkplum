package handler

// Tests for request-shape honesty: functions/function_call/
// response_format(json_schema)/non-text content blocks are refused with 400
// unsupported_parameter on EVERY route, decided before dispatch; tools are
// validated (400 invalid_tools), refused with 400 unsupported_parameter once
// the resolved backend turns out not to support them, and forwarded intact
// to one that does; inert unknown fields are ignored; top_p/stop are
// forwarded.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// buildRawChatRequest builds a POST /v1/chat/completions request from an
// arbitrary JSON body (a map, so a test can add fields chatCompletionRequest
// doesn't otherwise expose a builder for).
func buildRawChatRequest(t *testing.T, body map[string]any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("buildRawChatRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func doRawChat(t *testing.T, h *ChatHandler, body map[string]any) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildRawChatRequest(t, body)
	h.HandleChatCompletions(c)
	return w, c
}

func baseChatBody(model string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}
}

// requestShapeTestHandler builds a handler with one self-hosted route,
// enabled and unrestricted — request-shape honesty applies before any of
// that matters, but every test needs a resolvable model to prove the 400
// fires before dispatch rather than by coincidence of a later denial.
func requestShapeTestHandler(t *testing.T) (*ChatHandler, *fakeKindProvider) {
	t.Helper()
	prov := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)
	return h, prov
}

func assertUnsupportedParameter(t *testing.T, w *httptest.ResponseRecorder, c *gin.Context, prov *fakeKindProvider, wantParam string) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "unsupported_parameter" {
		t.Errorf("code = %q, want unsupported_parameter", e.Error.Code)
	}
	if e.Error.Param != wantParam {
		t.Errorf("param = %q, want %q", e.Error.Param, wantParam)
	}
	if prov.completeCalls != 0 || prov.streamCalls != 0 {
		t.Errorf("provider must never be dispatched to, got complete=%d stream=%d", prov.completeCalls, prov.streamCalls)
	}
	if denialCode, _ := c.Get("denial_code"); denialCode != "unsupported_parameter" {
		t.Errorf("denial_code = %v, want unsupported_parameter", denialCode)
	}
}

// weatherTool is a well-formed tool definition for the tests below.
var weatherTool = map[string]any{"type": "function", "function": map[string]any{
	"name":        "get_weather",
	"description": "Current weather",
	"parameters":  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
}}

// fakeKindProvider does not implement provider.ToolCapable, so a request
// carrying tools is refused after route resolution with the backend named.
func TestHandleChatCompletions_ToolsRefusedWhenBackendLacksSupport(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["tools"] = []map[string]any{weatherTool}
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "tools")
	if resolved := c.GetString("resolved_model"); resolved != "llama-3-8b" {
		t.Errorf("resolved_model = %q: the refusal must come after route resolution", resolved)
	}
}

// assertInvalidTools checks a 400 invalid_tools refusal naming param, with
// no dispatch.
func assertInvalidTools(t *testing.T, w *httptest.ResponseRecorder, prov *fakeKindProvider, wantParam string) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "invalid_tools" {
		t.Errorf("code = %q, want invalid_tools (body=%s)", e.Error.Code, w.Body.String())
	}
	if e.Error.Param != wantParam {
		t.Errorf("param = %q, want %q", e.Error.Param, wantParam)
	}
	if prov.completeCalls != 0 || prov.streamCalls != 0 {
		t.Errorf("provider must never be dispatched to, got complete=%d stream=%d", prov.completeCalls, prov.streamCalls)
	}
}

func TestHandleChatCompletions_ToolsValidated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(body map[string]any)
		wantParam string
	}{
		{"unknown tool type", func(b map[string]any) {
			b["tools"] = []map[string]any{{"type": "retrieval", "function": map[string]any{"name": "x"}}}
		}, "tools"},
		{"bad function name", func(b map[string]any) {
			b["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "get weather"}}}
		}, "tools"},
		{"duplicate function name", func(b map[string]any) {
			b["tools"] = []map[string]any{weatherTool, weatherTool}
		}, "tools"},
		{"parameters not an object", func(b map[string]any) {
			b["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "f", "parameters": []int{1}}}}
		}, "tools"},
		{"tool_choice without tools", func(b map[string]any) { b["tool_choice"] = "auto" }, "tool_choice"},
		{"tool_choice unknown string", func(b map[string]any) {
			b["tools"] = []map[string]any{weatherTool}
			b["tool_choice"] = "always"
		}, "tool_choice"},
		{"tool_choice names a missing function", func(b map[string]any) {
			b["tools"] = []map[string]any{weatherTool}
			b["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "nope"}}
		}, "tool_choice"},
		{"tool turn without tool_call_id", func(b map[string]any) {
			b["tools"] = []map[string]any{weatherTool}
			b["messages"] = []map[string]any{
				{"role": "user", "content": "hi"},
				{"role": "tool", "content": "sunny"},
			}
		}, "messages"},
		{"tool turn answering no call", func(b map[string]any) {
			b["tools"] = []map[string]any{weatherTool}
			b["messages"] = []map[string]any{
				{"role": "user", "content": "hi"},
				{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
			}
		}, "messages"},
		{"assistant tool_calls entry without id", func(b map[string]any) {
			b["tools"] = []map[string]any{weatherTool}
			b["messages"] = []map[string]any{
				{"role": "user", "content": "hi"},
				{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "arguments": "{}"}}}},
			}
		}, "messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, prov := requestShapeTestHandler(t)
			body := baseChatBody("llama-3-8b")
			tc.mutate(body)
			w, _ := doRawChat(t, h, body)
			assertInvalidTools(t, w, prov, tc.wantParam)
		})
	}
}

// toolCapturingProvider is capturingProvider plus provider.ToolCapable.
type toolCapturingProvider struct{ capturingProvider }

func (p *toolCapturingProvider) SupportsTools() bool { return true }

var _ provider.ToolCapable = (*toolCapturingProvider)(nil)

func TestHandleChatCompletions_ToolsForwardedToToolCapableBackend(t *testing.T) {
	prov := &toolCapturingProvider{capturingProvider{name: "vllm-default", kind: "vllm"}}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)
	body := baseChatBody("llama-3-8b")
	body["tools"] = []map[string]any{weatherTool}
	body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
	body["messages"] = []map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Oslo"}`}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
	}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	got := prov.lastReq
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "get_weather" || len(got.Tools[0].Function.Parameters) == 0 {
		t.Errorf("tools not forwarded intact: %+v", got.Tools)
	}
	if kind, name, _ := provider.ToolChoiceKind(got.ToolChoice); kind != "function" || name != "get_weather" {
		t.Errorf("tool_choice not forwarded: kind=%q name=%q raw=%s", kind, name, got.ToolChoice)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(got.Messages))
	}
	if a := got.Messages[1]; a.Content != "" || len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != "call_1" || a.ToolCalls[0].Function.Arguments != `{"city":"Oslo"}` {
		t.Errorf("assistant tool_calls turn mangled: %+v", a)
	}
	if r := got.Messages[2]; r.Role != "tool" || r.ToolCallID != "call_1" || r.Content != "sunny" {
		t.Errorf("tool result turn mangled: %+v", r)
	}
}

func TestHandleChatCompletions_ToolsEmptyArrayIgnored(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["tools"] = []map[string]any{}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for an empty tools array (a no-op SDK default), got %d; body=%s", w.Code, w.Body.String())
	}
	if prov.completeCalls != 1 {
		t.Errorf("expected dispatch to proceed, got %d calls", prov.completeCalls)
	}
}

// A tool_choice other than "none" counts as asking for tools: with a tool
// defined and a backend that cannot take it, the refusal names tools.
func TestHandleChatCompletions_ToolChoiceRefusedWhenBackendLacksSupport(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["tools"] = []map[string]any{weatherTool}
	body["tool_choice"] = "auto"
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "tools")
}

func TestHandleChatCompletions_ToolChoiceNoneIgnored(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["tool_choice"] = "none"
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf(`expected 200 for tool_choice:"none", got %d; body=%s`, w.Code, w.Body.String())
	}
	if prov.completeCalls != 1 {
		t.Errorf("expected dispatch to proceed, got %d calls", prov.completeCalls)
	}
}

func TestHandleChatCompletions_FunctionsRejected(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["functions"] = []map[string]any{{"name": "get_weather"}}
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "functions")
}

func TestHandleChatCompletions_FunctionCallRejectedWhenNotNone(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["function_call"] = map[string]any{"name": "get_weather"}
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "function_call")
}

func TestHandleChatCompletions_ResponseFormatJSONSchemaRejected(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x"}}
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "response_format")
}

// TestHandleChatCompletions_ResponseFormatJSONObjectIgnored asserts plain
// JSON mode (no schema) is left alone — only the structured-output shape is
// refused.
func TestHandleChatCompletions_ResponseFormatJSONObjectIgnored(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["response_format"] = map[string]any{"type": "json_object"}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for response_format:json_object, got %d; body=%s", w.Code, w.Body.String())
	}
	if prov.completeCalls != 1 {
		t.Errorf("expected dispatch to proceed, got %d calls", prov.completeCalls)
	}
}

// TestHandleChatCompletions_NonTextContentBlockRejected asserts an image
// content block (or any non-"text" block) is refused before dispatch, on a
// self_hosted route just as much as any other — request-shape honesty is
// backend-independent.
func TestHandleChatCompletions_NonTextContentBlockRejected(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := map[string]any{
		"model": "llama-3-8b",
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "what is in this image?"},
				{"type": "image_url", "image_url": map[string]string{"url": "https://example.com/x.png"}},
			}},
		},
	}
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "messages[].content")
}

// TestHandleChatCompletions_InertUnknownFieldsIgnored asserts fields the
// gateway has no reason to inspect (user, n, seed, logit_bias, metadata) are
// silently ignored rather than producing any 400 — request-shape honesty is
// a closed list, not a strict-schema rejection of every unrecognised field.
func TestHandleChatCompletions_InertUnknownFieldsIgnored(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["user"] = "end-user-123"
	body["n"] = 1
	body["seed"] = 42
	body["logit_bias"] = map[string]int{"50256": -100}
	body["metadata"] = map[string]string{"trace": "abc"}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, inert fields must be ignored, got %d; body=%s", w.Code, w.Body.String())
	}
	if prov.completeCalls != 1 {
		t.Errorf("expected dispatch to proceed, got %d calls", prov.completeCalls)
	}
}

// TestHandleChatCompletions_UnsupportedParameterAppliesToEveryClass asserts
// the 400 fires identically for a cloud_managed or external_api route, not
// just self_hosted — request-shape honesty is not scoped to one class.
func TestHandleChatCompletions_ToolsRefusedOnEveryClassWithoutSupport(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route routing.Route
	}{
		{"cloud_managed", routing.Route{ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}},
		{"external_api", routing.Route{ProviderKey: "anthropic", Kind: "anthropic", Class: routing.ClassExternalAPI}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeKindProvider{name: tc.route.ProviderKey, kind: tc.route.Kind}
			h := newTestHandler(
				fakeProviders{tc.route.ProviderKey: prov},
				map[string]routing.Route{"vendor-model": tc.route},
				policy.Snapshot{Enabled: map[string]bool{"vendor-model": true}, Residency: policy.ResidencyAny},
				true,
			)
			body := baseChatBody("vendor-model")
			body["tools"] = []map[string]any{weatherTool}
			w, c := doRawChat(t, h, body)
			assertUnsupportedParameter(t, w, c, prov, "tools")
		})
	}
}

// ---------------------------------------------------------------------------
// top_p / stop forwarding
// ---------------------------------------------------------------------------

// capturingProvider records the ChatRequest it was called with, so a test
// can assert exactly what was forwarded.
type capturingProvider struct {
	name, kind string
	lastReq    provider.ChatRequest
}

func (p *capturingProvider) Name() string { return p.name }
func (p *capturingProvider) Kind() string { return p.kind }
func (p *capturingProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p.lastReq = req
	return &provider.ChatResponse{ID: "id", Model: req.Model, Content: "ok", FinishReason: "stop"}, nil
}
func (p *capturingProvider) Stream(_ context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	p.lastReq = req
	return &provider.StreamStats{Model: req.Model, FinishReason: "stop"}, nil
}

var _ provider.Provider = (*capturingProvider)(nil)

func TestHandleChatCompletions_TopPAndStopForwardedToProvider(t *testing.T) {
	prov := &capturingProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)
	body := baseChatBody("llama-3-8b")
	body["top_p"] = 0.9
	body["stop"] = []string{"STOP", "END"}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if prov.lastReq.TopP == nil || *prov.lastReq.TopP != 0.9 {
		t.Errorf("TopP = %v, want 0.9", prov.lastReq.TopP)
	}
	if len(prov.lastReq.Stop) != 2 || prov.lastReq.Stop[0] != "STOP" || prov.lastReq.Stop[1] != "END" {
		t.Errorf("Stop = %v, want [STOP END]", prov.lastReq.Stop)
	}
}

func TestHandleChatCompletions_StopAsSingleStringForwarded(t *testing.T) {
	prov := &capturingProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)
	body := baseChatBody("llama-3-8b")
	body["stop"] = "STOP"
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if len(prov.lastReq.Stop) != 1 || prov.lastReq.Stop[0] != "STOP" {
		t.Errorf("Stop = %v, want [STOP] (single-string shape normalised)", prov.lastReq.Stop)
	}
}

// A history that already carries tool turns needs a tool-capable backend
// even when the request has no tools array: the gate must fire.
func TestHandleChatCompletions_ToolHistoryRefusedWhenBackendLacksSupport(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["messages"] = []map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
	}
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "tools")
}

// tool_choice "none" with tools defined still forwards the definitions: a
// history with tool turns is only valid to a backend alongside them.
func TestHandleChatCompletions_ToolChoiceNoneStillForwardsTools(t *testing.T) {
	prov := &toolCapturingProvider{capturingProvider{name: "vllm-default", kind: "vllm"}}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)
	body := baseChatBody("llama-3-8b")
	body["tools"] = []map[string]any{weatherTool}
	body["tool_choice"] = "none"
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if len(prov.lastReq.Tools) != 1 {
		t.Errorf("tools dropped with tool_choice none: %+v", prov.lastReq.Tools)
	}
	if kind, _, _ := provider.ToolChoiceKind(prov.lastReq.ToolChoice); kind != "none" {
		t.Errorf("tool_choice = %s, want none forwarded", prov.lastReq.ToolChoice)
	}
}
