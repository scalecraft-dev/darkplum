package handler

// Tests for request-shape honesty: tools/tool_choice/functions/
// function_call/response_format(json_schema)/non-text content blocks are
// refused with 400 unsupported_parameter on EVERY route, decided before
// dispatch; inert unknown fields are ignored; top_p/stop are forwarded.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
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

func TestHandleChatCompletions_ToolsRejected(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather"}}}
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "tools")
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

func TestHandleChatCompletions_ToolChoiceRejectedWhenNotNone(t *testing.T) {
	h, prov := requestShapeTestHandler(t)
	body := baseChatBody("llama-3-8b")
	body["tool_choice"] = "auto"
	w, c := doRawChat(t, h, body)
	assertUnsupportedParameter(t, w, c, prov, "tool_choice")
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
func TestHandleChatCompletions_UnsupportedParameterAppliesToEveryClass(t *testing.T) {
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
			body["tools"] = []map[string]any{{"type": "function"}}
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
