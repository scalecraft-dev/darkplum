package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Phase A3 of #3: reasoning_effort becomes extended thinking, decided per
// model from discovery, with max_tokens kept above the budget, and thinking
// output never leaving the provider.

func thinkingModelInfo(id string, maxTokens int, enabled bool) map[string]any {
	m := modelInfo(id, maxTokens)
	m["capabilities"] = map[string]any{
		"thinking": map[string]any{
			"supported": enabled,
			"types": map[string]any{
				"enabled":  map[string]any{"supported": enabled},
				"adaptive": map[string]any{"supported": false},
			},
		},
	}
	return m
}

func discoveredProvider(t *testing.T, srv *fakeAnthropicServer, cfg Config, models ...map[string]any) *Provider {
	t.Helper()
	srv.modelsResponse = modelsPage(models)
	p := newTestProvider(t, srv, nil, cfg)
	if _, err := p.DiscoverModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanReasoning_DecidedPerModel(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	p := discoveredProvider(t, srv, Config{DefaultMaxTokens: 4096},
		thinkingModelInfo("claude-sonnet-4-5", 64000, true),
		thinkingModelInfo("claude-3-5-haiku", 8192, false),
		modelInfo("claude-no-caps", 8192), // discovery reported nothing
	)
	cases := []struct {
		model, effort string
		maxTokens     int
		wantMode      provider.ReasoningMode
		wantBudget    int
		wantMax       int
	}{
		{"claude-sonnet-4-5", "low", 0, provider.ReasoningThinking, 1024, 5120}, // default visible 4096 + budget
		{"claude-sonnet-4-5", "minimal", 0, provider.ReasoningThinking, 1024, 5120},
		{"claude-sonnet-4-5", "medium", 4096, provider.ReasoningThinking, 4096, 8192},
		{"claude-sonnet-4-5", "high", 0, provider.ReasoningThinking, 16384, 4096 + 16384},
		{"claude-sonnet-4-5", "xhigh", 1000, provider.ReasoningThinking, 16384, 17384},
		{"claude-sonnet-4-5", "high", 30000, provider.ReasoningThinking, 16384, 46384}, // always visible+budget
		{"claude-sonnet-4-5", "high", 10000, provider.ReasoningThinking, 16384, 26384}, // between budget and 2x
		{"claude-3-5-haiku", "high", 0, provider.ReasoningNone, 0, 0},
		{"claude-no-caps", "high", 0, provider.ReasoningNone, 0, 0},
		{"not-discovered", "low", 0, provider.ReasoningNone, 0, 0},
	}
	for _, tc := range cases {
		got := p.PlanReasoning(tc.model, tc.effort, tc.maxTokens)
		if got.Mode != tc.wantMode || got.Budget != tc.wantBudget || got.MaxTokens != tc.wantMax {
			t.Errorf("%s/%s/%d: plan = %+v, want mode %v budget %d max %d", tc.model, tc.effort, tc.maxTokens, got, tc.wantMode, tc.wantBudget, tc.wantMax)
		}
	}
	// "none" is not a drop: thinking mode with no budget.
	if got := p.PlanReasoning("claude-sonnet-4-5", "none", 0); got.Mode != provider.ReasoningThinking || got.Budget != 0 {
		t.Errorf("none = %+v", got)
	}
}

func TestPlanReasoning_ClampedToTheModelsOutputLimit(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	p := discoveredProvider(t, srv, Config{DefaultMaxTokens: 4096},
		thinkingModelInfo("small", 20000, true),
		thinkingModelInfo("tiny", 1500, true),
	)
	// 4096 + 16384 = 20480 > 20000: clamped, budget still fits below it.
	if got := p.PlanReasoning("small", "high", 0); got.MaxTokens != 20000 || got.Budget != 16384 {
		t.Errorf("small = %+v", got)
	}
	// Ceiling 20000: 19000+16384 is capped, budget still fits below it.
	if got := p.PlanReasoning("small", "high", 19000); got.MaxTokens != 20000 || got.Budget != 16384 {
		t.Errorf("small/19000 = %+v", got)
	}
	// Ceiling 1500: budget shrinks to fit (>= 1024) rather than exceeding max_tokens.
	if got := p.PlanReasoning("tiny", "medium", 0); got.MaxTokens != 1500 || got.Budget != 1499 {
		t.Errorf("tiny = %+v", got)
	}
	// Limit leaves no room for a 1024 budget: treated as unsupported.
	srv2 := newFakeAnthropicServer(t)
	p2 := discoveredProvider(t, srv2, Config{DefaultMaxTokens: 4096}, thinkingModelInfo("nano", 1024, true))
	if got := p2.PlanReasoning("nano", "low", 0); got.Mode != provider.ReasoningNone {
		t.Errorf("nano = %+v, want none", got)
	}
}

func thinkingWire(t *testing.T, req provider.ChatRequest, maxTokens int64) map[string]any {
	t.Helper()
	norm, err := normalizeMessages(req.Messages)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(buildParams(req, norm, maxTokens, true))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return body
}

func TestBuildParams_ThinkingWire(t *testing.T) {
	temp := 0.7
	req := basicRequest("claude-sonnet-4-5")
	req.ThinkingBudget = 4096
	req.Temperature = &temp // the handler unsets it; the provider must not send it regardless
	body := thinkingWire(t, req, 8192)
	th, _ := body["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(4096) {
		t.Errorf("thinking = %v", body["thinking"])
	}
	if _, has := body["temperature"]; has {
		t.Errorf("temperature sent with thinking: %v", body["temperature"])
	}
	if body["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}

	// A budget that does not fit below max_tokens is not sent.
	if _, has := thinkingWire(t, req, 4096)["thinking"]; has {
		t.Error("thinking sent with budget >= max_tokens")
	}
	// No thinking: temperature flows as before, no thinking field.
	req.ThinkingBudget = 0
	body = thinkingWire(t, req, 8192)
	if _, has := body["thinking"]; has {
		t.Error("thinking sent without a budget")
	}
	if body["temperature"] != 0.7 {
		t.Errorf("temperature = %v", body["temperature"])
	}
}

// thinking blocks as Anthropic returns them, in front of the answer.
func jsonThinkingCompletion(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_t", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5",
		"content": []map[string]any{
			{"type": "thinking", "thinking": "SECRET-REASONING", "signature": "SIG-OPAQUE"},
			{"type": "redacted_thinking", "data": "REDACTED-OPAQUE"},
			{"type": "text", "text": "the answer"},
		},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 900, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	})
}

func sseThinkingCompletion(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	ev := func(typ string, data map[string]any) {
		b, _ := json.Marshal(data)
		_, _ = io.WriteString(w, "event: "+typ+"\ndata: "+string(b)+"\n\n")
	}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_t", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	}})
	ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
	ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "SECRET-REASONING"}})
	ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "SIG-OPAQUE"}})
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	ev("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "redacted_thinking", "data": "REDACTED-OPAQUE"}})
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
	ev("content_block_start", map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "text", "text": ""}})
	ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "text_delta", "text": "the answer"}})
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2})
	ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 900}})
	ev("message_stop", map[string]any{"type": "message_stop"})
}

func assertNoThinking(t *testing.T, where, out string) {
	t.Helper()
	for _, secret := range []string{"SECRET-REASONING", "SIG-OPAQUE", "REDACTED-OPAQUE", "thinking", "signature"} {
		if strings.Contains(out, secret) {
			t.Errorf("%s: %q reached the caller: %s", where, secret, out)
		}
	}
}

func TestThinkingOutput_NeverReachesTheCaller(t *testing.T) {
	// Non-streaming, SDK non-stream path.
	srv := newFakeAnthropicServer(t)
	var sent map[string]any
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
			sent = body
			if body["stream"] == true {
				sseThinkingCompletion(w)
				return
			}
			jsonThinkingCompletion(w)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	req := basicRequest("claude-sonnet-4-5")
	req.ThinkingBudget = 4096
	req.MaxTokens = 8192
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if sent["stream"] != true {
		t.Error("a thinking request must be streamed internally (60s response-header timeout)")
	}
	if resp.Content != "the answer" {
		t.Errorf("Content = %q", resp.Content)
	}
	b, _ := json.Marshal(resp)
	assertNoThinking(t, "complete", string(b))
	// completion_tokens is what the vendor billed, thinking included.
	if resp.CompletionTokens != 900 {
		t.Errorf("CompletionTokens = %d, want 900 (thinking included)", resp.CompletionTokens)
	}

	// Streaming.
	srv = newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { sseThinkingCompletion(w) },
	}
	p = newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatal(err)
	}
	assertNoThinking(t, "stream", buf.String())
	if !strings.Contains(buf.String(), "the answer") {
		t.Errorf("answer missing from stream: %s", buf.String())
	}
	if stats.CompletionTokens != 900 {
		t.Errorf("stream CompletionTokens = %d", stats.CompletionTokens)
	}
}

// The SDK refuses a non-streaming request whose max_tokens implies more than
// ten minutes of generation. Thinking pushes max_tokens past that, so Complete
// streams internally and assembles one response.
func TestComplete_LargeMaxTokensStreamsInternally(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	var sent map[string]any
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
			sent = body
			if body["stream"] != true {
				writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "streaming required")
				return
			}
			sseThinkingCompletion(w)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	req := basicRequest("claude-sonnet-4-5")
	req.ThinkingBudget = 16384
	req.MaxTokens = 16384 + 8000 // 24384 > 21333: past the SDK's non-stream guard
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete failed with large max_tokens: %v", err)
	}
	if sent["stream"] != true {
		t.Error("expected the request to be streamed internally")
	}
	if th, _ := sent["thinking"].(map[string]any); th["budget_tokens"] != float64(16384) {
		t.Errorf("thinking = %v", sent["thinking"])
	}
	if resp.Content != "the answer" || resp.FinishReason != "stop" || resp.PromptTokens != 10 || resp.CompletionTokens != 900 {
		t.Errorf("assembled response = %+v", resp)
	}
	b, _ := json.Marshal(resp)
	assertNoThinking(t, "internal stream", string(b))
}

func TestComplete_LargeMaxTokensErrorsStillTranslated(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusTooManyRequests, "rate_limit_error", "slow down")
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	req := basicRequest("claude-sonnet-4-5")
	req.MaxTokens = 40000
	_, err := p.Complete(context.Background(), req)
	if err == nil {
		t.Fatal("want error")
	}
	var ae *APIError
	if !asAPIError(err, &ae) || ae.HTTPStatus() != http.StatusTooManyRequests {
		t.Errorf("err = %v, want translated 429", err)
	}
}

// Without thinking and under the SDK guard, Complete is a plain non-stream call.
func TestComplete_PlainRequestStaysNonStreaming(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	var sent map[string]any
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
			sent = body
			jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	if _, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5")); err != nil {
		t.Fatal(err)
	}
	if sent["stream"] == true {
		t.Error("plain request streamed")
	}
}

func rawSSE(events ...string) func(w http.ResponseWriter, r *http.Request, body map[string]any) {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			var m struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(e), &m)
			_, _ = io.WriteString(w, "event: "+m.Type+"\ndata: "+e+"\n\n")
		}
	}
}

const (
	rsStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":30,"cache_read_input_tokens":60}}}`
	rsStop  = `{"type":"message_stop"}`
)

func thinkingReqFor() provider.ChatRequest {
	req := basicRequest("claude-sonnet-4-5")
	req.ThinkingBudget = 1024
	req.MaxTokens = 5000
	return req
}

func TestInternalStream_TruncatedIsAnError(t *testing.T) {
	cases := map[string][]string{
		"cut after message_start":      {rsStart},
		"cut mid text":                 {rsStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`},
		"message_stop without a delta": {rsStart, rsStop},
	}
	for name, events := range cases {
		srv := newFakeAnthropicServer(t)
		srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){rawSSE(events...)}
		p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
		resp, err := p.Complete(context.Background(), thinkingReqFor())
		var ae *APIError
		if err == nil || !asAPIError(err, &ae) || ae.HTTPStatus() != http.StatusBadGateway {
			t.Errorf("%s: resp=%v err=%v, want 502 upstream_error", name, resp, err)
		}
		if err != nil && strings.Contains(string(ae.ResponseBody()), "partial") {
			t.Errorf("%s: partial content in the error", name)
		}
	}
}

func TestStream_TruncatedIsAnError(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){rawSSE(rsStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`)}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	if _, err := p.Stream(context.Background(), basicRequest("claude-sonnet-4-5"), &bytes.Buffer{}); err == nil {
		t.Fatal("truncated stream returned success")
	}
}

func TestInternalStream_ToolUseAndStopReasons(t *testing.T) {
	toolEvents := []string{rsStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}`, rsStop}
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){rawSSE(toolEvents...)}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	resp, err := p.Complete(context.Background(), thinkingReqFor())
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "tool_calls" || len(resp.ToolCalls) != 1 ||
		resp.ToolCalls[0].Function.Name != "get_weather" || resp.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("resp = %+v", resp)
	}

	// stop_reason max_tokens -> finish_reason length, content kept.
	lenEvents := []string{rsStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut off"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":5000}}`, rsStop}
	srv = newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){rawSSE(lenEvents...)}
	p = newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	resp, err = p.Complete(context.Background(), thinkingReqFor())
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "length" || resp.Content != "cut off" || resp.CompletionTokens != 5000 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestInternalStream_CacheUsage(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){rawSSE(rsStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`, rsStop)}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	resp, err := p.Complete(context.Background(), thinkingReqFor())
	if err != nil {
		t.Fatal(err)
	}
	// input 10 + cache_creation 30 + cache_read 60 = 100 prompt tokens.
	if resp.PromptTokens != 100 || resp.CacheReadTokens != 60 || resp.CacheCreationTokens != 30 || resp.CompletionTokens != 4 {
		t.Errorf("usage = %+v", resp)
	}
}

// A non-streaming response that carries thinking blocks (a plain request
// whose model thinks anyway) still yields only the answer.
func TestToChatResponse_IgnoresThinkingBlocks(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { jsonThinkingCompletion(w) },
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	resp, err := p.Complete(context.Background(), basicRequest("claude-sonnet-4-5"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(resp)
	assertNoThinking(t, "plain non-stream", string(b))
	if resp.Content != "the answer" {
		t.Errorf("Content = %q", resp.Content)
	}
}
