package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeCreds is an in-memory credentialSource, letting tests drive a key
// rotation deterministically without waiting on credsource.Source's own
// recheck interval (which is exercised separately by its own package tests).
type fakeCreds struct {
	mu      sync.Mutex
	key     string
	version uint64
	err     error
}

func (f *fakeCreds) Key() (string, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key, f.version, f.err
}

func (f *fakeCreds) set(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key = key
	f.version++
}

var _ credentialSource = (*fakeCreds)(nil)

// fakeStatus is an authFailureReporter fake recording every Report call.
type fakeStatus struct {
	mu       sync.Mutex
	reports  []provider.ProviderStatus
	snapshot []provider.ProviderStatus
}

func (f *fakeStatus) Report(name string, st provider.ProviderStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.Name = name
	f.reports = append(f.reports, st)
	f.snapshot = []provider.ProviderStatus{st}
}

func (f *fakeStatus) Snapshot() []provider.ProviderStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]provider.ProviderStatus, len(f.snapshot))
	copy(out, f.snapshot)
	return out
}

func (f *fakeStatus) lastState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) == 0 {
		return ""
	}
	return f.reports[len(f.reports)-1].State
}

// ---------------------------------------------------------------------------
// Fake Messages API server
// ---------------------------------------------------------------------------

// capturedRequest records what the fake server saw for a single request, for
// assertions that inspect what the SDK actually sent on the wire (headers,
// body).
type capturedRequest struct {
	apiKey string
	body   map[string]any
}

// fakeAnthropicServer is a minimal httptest server standing in for
// api.anthropic.com's /v1/messages and /v1/models endpoints. handler
// supplies the response for each /v1/messages call in sequence order;
// modelsHandler (if set) supplies the /v1/models response.
type fakeAnthropicServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []capturedRequest

	// messagesResponses is consumed in order, one per /v1/messages call. The
	// last entry is reused for any call beyond len(messagesResponses).
	messagesResponses []func(w http.ResponseWriter, r *http.Request, body map[string]any)
	modelsResponse    func(w http.ResponseWriter)
}

func newFakeAnthropicServer(t *testing.T) *fakeAnthropicServer {
	t.Helper()
	f := &fakeAnthropicServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", f.handleMessages)
	mux.HandleFunc("/v1/models", f.handleModels)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeAnthropicServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	idx := len(f.requests)
	f.requests = append(f.requests, capturedRequest{apiKey: r.Header.Get("x-api-key"), body: body})
	var respond func(w http.ResponseWriter, r *http.Request, body map[string]any)
	switch {
	case idx < len(f.messagesResponses):
		respond = f.messagesResponses[idx]
	case len(f.messagesResponses) > 0:
		respond = f.messagesResponses[len(f.messagesResponses)-1]
	}
	f.mu.Unlock()

	if respond == nil {
		http.Error(w, "no fake response configured", http.StatusInternalServerError)
		return
	}
	respond(w, r, body)
}

func (f *fakeAnthropicServer) handleModels(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	respond := f.modelsResponse
	f.mu.Unlock()
	if respond == nil {
		http.Error(w, "no fake models response configured", http.StatusInternalServerError)
		return
	}
	respond(w)
}

func (f *fakeAnthropicServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAnthropicServer) requestAt(i int) capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// ---------------------------------------------------------------------------
// Fake response bodies
// ---------------------------------------------------------------------------

func jsonCompletion(w http.ResponseWriter, statusCode int, id, model, text, stopReason string, inputTokens, outputTokens int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	content := []map[string]any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	body := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

func jsonErrorBody(errType, message string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
	return b
}

func writeVendorError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(jsonErrorBody(errType, message))
}

// sseCompletion writes a complete streaming response: message_start,
// content_block deltas for each piece of text, message_delta with the given
// stop_reason and cumulative output tokens, and message_stop.
func sseCompletion(w http.ResponseWriter, id, model string, textDeltas []string, stopReason string, inputTokens, outputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeEvent := func(eventType string, data map[string]any) {
		b, _ := json.Marshal(data)
		_, _ = io.WriteString(w, "event: "+eventType+"\ndata: "+string(b)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                inputTokens,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})
	writeEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	for _, delta := range textDeltas {
		writeEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		})
	}
	writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	})
	writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

func modelsPage(models []map[string]any) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"data":     models,
			"has_more": false,
			"first_id": nil,
			"last_id":  nil,
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

func modelInfo(id string, maxTokens int) map[string]any {
	return map[string]any{
		"id":               id,
		"type":             "model",
		"display_name":     id,
		"created_at":       "2025-01-01T00:00:00Z",
		"max_input_tokens": 200000,
		"max_tokens":       maxTokens,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestProvider(t *testing.T, srv *fakeAnthropicServer, creds credentialSource, cfg Config) *Provider {
	t.Helper()
	cfg.BaseURL = srv.Server.URL
	if creds == nil {
		creds = &fakeCreds{key: "sk-ant-test-key", version: 1}
	}
	return New(creds, cfg)
}

func basicRequest(model string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

// ---------------------------------------------------------------------------
// Complete: happy path, exact wire shape
// ---------------------------------------------------------------------------

func TestComplete_HappyPath(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "msg_abc", "claude-haiku-4-5", "Hello, world!", "end_turn", 10, 5)
		},
	}

	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	resp, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello, world!" {
		t.Errorf("Content = %q", resp.Content)
	}
	if resp.PromptTokens != 10 || resp.CompletionTokens != 5 {
		t.Errorf("tokens = %d/%d, want 10/5", resp.PromptTokens, resp.CompletionTokens)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}

	// Assert the exact OpenAI wire shape via the shared render helper.
	wire, err := provider.BuildCompletionJSON(resp)
	if err != nil {
		t.Fatalf("BuildCompletionJSON: %v", err)
	}
	var decoded struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("decode wire JSON: %v", err)
	}
	if decoded.Object != "chat.completion" {
		t.Errorf("object = %q", decoded.Object)
	}
	if len(decoded.Choices) != 1 || decoded.Choices[0].Message.Role != "assistant" ||
		decoded.Choices[0].Message.Content != "Hello, world!" || decoded.Choices[0].FinishReason != "stop" {
		t.Errorf("unexpected choices: %+v", decoded.Choices)
	}
	if decoded.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", decoded.Usage.TotalTokens)
	}
}

func TestComplete_UsesXAPIKeyHeader(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
		},
	}
	sentinel := "sk-ant-sentinel-do-not-leak-CANARY123"
	p := newTestProvider(t, srv, &fakeCreds{key: sentinel, version: 1}, Config{DefaultMaxTokens: 100})

	if _, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv.requestAt(0).apiKey != sentinel {
		t.Errorf("expected server to see the configured key, got %q", srv.requestAt(0).apiKey)
	}
}

// ---------------------------------------------------------------------------
// Stream: happy path
// ---------------------------------------------------------------------------

func TestStream_HappyPath(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			sseCompletion(w, "msg_stream1", "claude-haiku-4-5", []string{"Hello", ", ", "world!"}, "end_turn", 8, 3)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), basicRequest("claude-haiku-4-5"), &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", stats.FinishReason)
	}
	if stats.PromptTokens != 8 || stats.CompletionTokens != 3 {
		t.Errorf("tokens = %d/%d, want 8/3", stats.PromptTokens, stats.CompletionTokens)
	}

	out := buf.String()
	if !strings.HasSuffix(out, provider.StreamDoneSSE) {
		t.Errorf("expected stream to end with the DONE sentinel, got: %q", out)
	}

	var content strings.Builder
	sawFinal := false
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode SSE chunk %q: %v", payload, err)
		}
		if len(chunk.Choices) != 1 {
			t.Fatalf("expected exactly one choice per chunk, got %+v", chunk)
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			if *chunk.Choices[0].FinishReason != "stop" {
				t.Errorf("finish_reason = %q, want stop", *chunk.Choices[0].FinishReason)
			}
			sawFinal = true
		}
	}
	if content.String() != "Hello, world!" {
		t.Errorf("reassembled content = %q, want %q", content.String(), "Hello, world!")
	}
	if !sawFinal {
		t.Error("expected a final chunk carrying finish_reason")
	}
}

// ---------------------------------------------------------------------------
// Refusal
// ---------------------------------------------------------------------------

func TestComplete_Refusal(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "msg_refusal", "claude-haiku-4-5", "", "refusal", 10, 0)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	resp, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	if err != nil {
		t.Fatalf("a refusal must still produce a well-formed response, got error: %v", err)
	}
	if resp.FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want content_filter", resp.FinishReason)
	}
	if resp.Content != "" {
		t.Errorf("Content = %q, want empty", resp.Content)
	}
}

func TestStream_RefusalWithNoText(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			sseCompletion(w, "msg_refusal_stream", "claude-haiku-4-5", nil, "refusal", 10, 0)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), basicRequest("claude-haiku-4-5"), &buf)
	if err != nil {
		t.Fatalf("a refusal must still produce a well-formed stream, got error: %v", err)
	}
	if stats.FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want content_filter", stats.FinishReason)
	}
	if !strings.Contains(buf.String(), provider.StreamDoneSSE) {
		t.Error("expected the DONE sentinel even for a textless refusal")
	}
}

// ---------------------------------------------------------------------------
// max_tokens default and clamp
// ---------------------------------------------------------------------------

func requestMaxTokens(t *testing.T, body map[string]any) int {
	t.Helper()
	v, ok := body["max_tokens"]
	if !ok {
		t.Fatalf("request body missing max_tokens: %+v", body)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("max_tokens is not a number: %+v", v)
	}
	return int(f)
}

func TestMaxTokens_DefaultWhenCallerOmits(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if got := requestMaxTokens(t, body); got != 4096 {
				t.Errorf("max_tokens = %d, want default 4096", got)
			}
			jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	req := basicRequest("claude-haiku-4-5")
	req.MaxTokens = 0
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMaxTokens_CallerValueUsedWhenUnderModelLimit(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if got := requestMaxTokens(t, body); got != 2000 {
				t.Errorf("max_tokens = %d, want 2000", got)
			}
			jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	req := basicRequest("claude-haiku-4-5")
	req.MaxTokens = 2000
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMaxTokens_ClampedToDiscoveredModelLimit(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.modelsResponse = modelsPage([]map[string]any{modelInfo("claude-haiku-4-5", 1024)})
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if got := requestMaxTokens(t, body); got != 1024 {
				t.Errorf("max_tokens = %d, want clamped to 1024", got)
			}
			jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	if _, err := p.DiscoverModels(context.Background()); err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}

	req := basicRequest("claude-haiku-4-5")
	req.MaxTokens = 100000 // far above the discovered 1024 limit
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Temperature/top_p retry-once
// ---------------------------------------------------------------------------

func TestComplete_RetriesOnceWithoutSamplingParams(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasTemp := body["temperature"]; !hasTemp {
				t.Fatal("expected the first attempt to include temperature")
			}
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error",
				"temperature is not supported for this model")
		},
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasTemp := body["temperature"]; hasTemp {
				t.Fatal("expected the retry to omit temperature")
			}
			jsonCompletion(w, http.StatusOK, "msg_retry", "claude-haiku-4-5", "ok", "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	req := basicRequest("claude-haiku-4-5")
	temp := 0.9
	req.Temperature = &temp

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q", resp.Content)
	}
	if srv.callCount() != 2 {
		t.Errorf("expected exactly 2 calls (original + one retry), got %d", srv.callCount())
	}
}

func TestComplete_SamplingRetryDoesNotLoopOnRepeatedFailure(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	unsupportedResponder := func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		writeVendorError(w, http.StatusBadRequest, "invalid_request_error",
			"temperature is not supported for this model")
	}
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		unsupportedResponder,
		unsupportedResponder,
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	req := basicRequest("claude-haiku-4-5")
	temp := 0.9
	req.Temperature = &temp

	_, err := p.Complete(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error when the retry also fails")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("expected a 400 APIError, got %v", err)
	}
	if srv.callCount() != 2 {
		t.Errorf("expected exactly 2 calls total (no further retries), got %d", srv.callCount())
	}
}

func TestStream_RetriesOnceWithoutSamplingParams(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasTopP := body["top_p"]; !hasTopP {
				t.Fatal("expected the first attempt to include top_p")
			}
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error",
				"top_p is not supported for this model")
		},
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasTopP := body["top_p"]; hasTopP {
				t.Fatal("expected the retry to omit top_p")
			}
			sseCompletion(w, "msg_retry_stream", "claude-haiku-4-5", []string{"ok"}, "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	req := basicRequest("claude-haiku-4-5")
	topP := 0.5
	req.TopP = &topP

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.CompletionTokens != 1 {
		t.Errorf("CompletionTokens = %d", stats.CompletionTokens)
	}
	if srv.callCount() != 2 {
		t.Errorf("expected exactly 2 calls, got %d", srv.callCount())
	}
}

// ---------------------------------------------------------------------------
// Error translation
// ---------------------------------------------------------------------------

func TestComplete_RateLimitedWithRetryAfter(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			// Kept small (1s, not a realistic multi-minute value) because
			// sdkMaxRetries retries a 429 once internally, honouring this
			// same Retry-After value as the real SDK would before giving up
			// and returning the error — a larger value here would make this
			// test itself slow without testing anything more.
			w.Header().Set("Retry-After", "1")
			writeVendorError(w, http.StatusTooManyRequests, "rate_limit_error", "rate limited")
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	_, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	if apiErr.HTTPStatus() != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want 429", apiErr.HTTPStatus())
	}
	if apiErr.RetryAfterSeconds() != 1 {
		t.Errorf("RetryAfterSeconds = %d, want 1", apiErr.RetryAfterSeconds())
	}
	assertErrorCode(t, apiErr, "provider_rate_limited")
	// sdkMaxRetries=1 means exactly one retry (two total calls) rather than
	// the SDK's own default of two retries — kept low so a 429 surfaces
	// quickly instead of being silently absorbed for longer.
	if srv.callCount() != 2 {
		t.Errorf("expected exactly 2 calls (original + one SDK retry), got %d", srv.callCount())
	}
}

func TestComplete_AuthFailureReportsStatus(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusUnauthorized, "authentication_error", "invalid x-api-key")
		},
	}
	status := &fakeStatus{}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100, Status: status})

	_, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	if apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("HTTPStatus = %d, want 502", apiErr.HTTPStatus())
	}
	assertErrorCode(t, apiErr, "provider_auth_failed")
	if status.lastState() != provider.ProviderStateAuthFailed {
		t.Errorf("expected status registry to be reported auth_failed, got %q", status.lastState())
	}
}

func TestComplete_ServerErrorMapsToUpstreamError(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusServiceUnavailable, "overloaded_error", "overloaded")
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	_, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	if apiErr.HTTPStatus() != http.StatusBadGateway {
		t.Errorf("HTTPStatus = %d, want 502", apiErr.HTTPStatus())
	}
	assertErrorCode(t, apiErr, "upstream_error")
}

func TestErrorBody_NeverContainsVendorMessage(t *testing.T) {
	vendorSecret := "internal-account-id-should-never-leak"
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "bad request for account "+vendorSecret)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	_, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	if strings.Contains(string(apiErr.ResponseBody()), vendorSecret) {
		t.Errorf("response body leaked vendor error text: %s", apiErr.ResponseBody())
	}
}

// TestNoKeyLeak exercises success, error, and sampling-retry (which logs at
// info level) paths with a sentinel API key, then greps every response body,
// every error's Error() string, and everything written to the log during the
// run for that sentinel. The key must never appear anywhere.
func TestNoKeyLeak(t *testing.T) {
	sentinel := "sk-ant-CANARY-DO-NOT-LEAK-abc123"

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		// 1: success
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
		},
		// 2: vendor error
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "bad request")
		},
		// 3: sampling-unsupported then retry succeeds (exercises the
		// slog.Info retry log line)
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "temperature is not supported for this model")
		},
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "msg_2", "claude-haiku-4-5", "ok", "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, &fakeCreds{key: sentinel, version: 1}, Config{DefaultMaxTokens: 100})

	var bodies [][]byte

	resp, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wire, _ := provider.BuildCompletionJSON(resp)
	bodies = append(bodies, wire)

	_, err = p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	bodies = append(bodies, apiErr.ResponseBody())
	if strings.Contains(apiErr.Error(), sentinel) {
		t.Errorf("APIError.Error() leaked the key: %q", apiErr.Error())
	}

	req := basicRequest("claude-haiku-4-5")
	temp := 0.5
	req.Temperature = &temp
	resp, err = p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on the sampling-retry path: %v", err)
	}
	wire, _ = provider.BuildCompletionJSON(resp)
	bodies = append(bodies, wire)

	for i, b := range bodies {
		if strings.Contains(string(b), sentinel) {
			t.Errorf("response body %d leaked the key: %s", i, b)
		}
	}
	if strings.Contains(logBuf.String(), sentinel) {
		t.Errorf("log output leaked the key: %s", logBuf.String())
	}
}

func assertErrorCode(t *testing.T, apiErr *APIError, wantCode string) {
	t.Helper()
	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(apiErr.ResponseBody(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if decoded.Error.Code != wantCode {
		t.Errorf("error code = %q, want %q (body=%s)", decoded.Error.Code, wantCode, apiErr.ResponseBody())
	}
}

// ---------------------------------------------------------------------------
// Key rotation mid-run
// ---------------------------------------------------------------------------

func TestKeyRotation_SecondRequestUsesNewKey(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "one", "end_turn", 1, 1)
		},
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "msg_2", "claude-haiku-4-5", "two", "end_turn", 1, 1)
		},
	}
	creds := &fakeCreds{key: "sk-ant-old-key", version: 1}
	p := newTestProvider(t, srv, creds, Config{DefaultMaxTokens: 100})

	if _, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv.requestAt(0).apiKey != "sk-ant-old-key" {
		t.Fatalf("expected first request to use the old key, got %q", srv.requestAt(0).apiKey)
	}

	creds.set("sk-ant-new-key")

	if _, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv.requestAt(1).apiKey != "sk-ant-new-key" {
		t.Fatalf("expected second request to use the new key, got %q", srv.requestAt(1).apiKey)
	}
}

// ---------------------------------------------------------------------------
// Idle timeout
// ---------------------------------------------------------------------------

func TestStream_IdleTimeout(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			// Send message_start, then hang without ever sending
			// content_block_stop/message_stop — the idle watchdog must fire.
			_, _ = io.WriteString(w, "event: message_start\ndata: "+
				`{"type":"message_start","message":{"id":"msg_hang","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+
				"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-block
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
	p.streamIdleTimeout = 100 * time.Millisecond

	var buf bytes.Buffer
	start := time.Now()
	_, err := p.Stream(context.Background(), basicRequest("claude-haiku-4-5"), &buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an idle-timeout error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	if apiErr.HTTPStatus() != http.StatusGatewayTimeout {
		t.Errorf("HTTPStatus = %d, want 504", apiErr.HTTPStatus())
	}
	assertErrorCode(t, apiErr, "upstream_timeout")
	if elapsed > 5*time.Second {
		t.Errorf("idle timeout took too long to fire: %s", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Cancelled context: no goroutine leak
// ---------------------------------------------------------------------------

func TestStream_CancelledContextDoesNotLeakGoroutines(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Exit as soon as the client disconnects (context cancellation
			// propagates to the server's request context), mirroring a real
			// server — otherwise this handler goroutine itself would count
			// as a "leak" unrelated to anything Stream did.
			select {
			case <-block:
			case <-r.Context().Done():
			}
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
	p.streamIdleTimeout = time.Minute // must not be what ends the stream here

	before := countGoroutines()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf bytes.Buffer
		_, _ = p.Stream(ctx, basicRequest("claude-haiku-4-5"), &buf)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after context cancellation")
	}

	after := waitForGoroutineSettle(before)
	if after > before {
		t.Errorf("goroutine count grew from %d to %d after a cancelled stream", before, after)
	}
}

// countGoroutines settles the runtime briefly before sampling, so a
// just-finished goroutine that hasn't been reaped yet doesn't produce a
// falsely high baseline.
func countGoroutines() int {
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
	runtime.GC()
	return runtime.NumGoroutine()
}

// waitForGoroutineSettle polls up to a short bound for the goroutine count to
// come back down to (or below) baseline, absorbing scheduler noise from
// unrelated background goroutines rather than flaking on a race between
// Stream returning and its goroutines actually unwinding.
func waitForGoroutineSettle(baseline int) int {
	last := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		if last <= baseline {
			return last
		}
		time.Sleep(25 * time.Millisecond)
		runtime.GC()
		last = runtime.NumGoroutine()
	}
	return last
}

// ---------------------------------------------------------------------------
// Phase A1 parameter honesty: max_tokens wire field, include_usage shape.
// ---------------------------------------------------------------------------

func TestParamCaps_Anthropic(t *testing.T) {
	want := provider.ParamCaps{Stop: true, ForcedToolChoice: true}
	if got := New(&fakeCreds{key: "k"}, Config{}).ParamCaps(); got != want {
		t.Errorf("ParamCaps = %+v, want %+v", got, want)
	}
}

// max_completion_tokens reaches the Messages API as max_tokens (the handler
// folds both names into ChatRequest.MaxTokens).
func TestMaxTokens_ReachesWireOnStream(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if got := requestMaxTokens(t, body); got != 321 {
				t.Errorf("max_tokens = %d, want 321", got)
			}
			sseCompletion(w, "msg_1", "claude-haiku-4-5", []string{"x"}, "end_turn", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
	req := basicRequest("claude-haiku-4-5")
	req.MaxTokens = 321
	if _, err := p.Stream(context.Background(), req, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestStream_IncludeUsageShape(t *testing.T) {
	run := func(t *testing.T, includeUsage bool) []string {
		srv := newFakeAnthropicServer(t)
		srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
			func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				sseCompletion(w, "msg_u", "claude-haiku-4-5", []string{"Hi"}, "end_turn", 8, 3)
			},
		}
		p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
		req := basicRequest("claude-haiku-4-5")
		req.IncludeUsage = includeUsage
		var buf bytes.Buffer
		if _, err := p.Stream(context.Background(), req, &buf); err != nil {
			t.Fatal(err)
		}
		var frames []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if p, ok := strings.CutPrefix(line, "data: "); ok && p != "[DONE]" {
				frames = append(frames, p)
			}
		}
		return frames
	}

	frames := run(t, true)
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3 (content, finish, usage): %v", len(frames), frames)
	}
	if !strings.Contains(frames[1], `"finish_reason":"stop"`) || !strings.Contains(frames[1], `"usage":null`) {
		t.Errorf("finish frame = %s", frames[1])
	}
	if !strings.Contains(frames[2], `"choices":[]`) || !strings.Contains(frames[2], `"prompt_tokens":8`) ||
		!strings.Contains(frames[2], `"completion_tokens":3`) || !strings.Contains(frames[2], `"total_tokens":11`) {
		t.Errorf("usage frame = %s", frames[2])
	}

	frames = run(t, false)
	if len(frames) != 2 || !strings.Contains(frames[1], `"total_tokens":11`) {
		t.Errorf("without include_usage want 2 frames with usage on the finish chunk, got %v", frames)
	}
}
