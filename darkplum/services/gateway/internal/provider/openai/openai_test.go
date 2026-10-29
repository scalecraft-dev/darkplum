package openai

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
// Fake Chat Completions / Models server
// ---------------------------------------------------------------------------

// capturedRequest records what the fake server saw for a single
// /v1/chat/completions call, for assertions that inspect the actual wire
// request (headers, body).
type capturedRequest struct {
	authHeader string
	body       map[string]any
}

// fakeOpenAIServer is a minimal httptest server standing in for
// api.openai.com's /v1/chat/completions and /v1/models endpoints. handler
// supplies the response for each /v1/chat/completions call in sequence
// order; modelsHandler (if set) supplies the /v1/models response.
type fakeOpenAIServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []capturedRequest

	// completionResponses is consumed in order, one per
	// /v1/chat/completions call. The last entry is reused for any call
	// beyond len(completionResponses).
	completionResponses []func(w http.ResponseWriter, r *http.Request, body map[string]any)
	modelsResponse      func(w http.ResponseWriter)
}

func newFakeOpenAIServer(t *testing.T) *fakeOpenAIServer {
	t.Helper()
	f := &fakeOpenAIServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", f.handleCompletions)
	mux.HandleFunc("/v1/models", f.handleModels)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeOpenAIServer) handleCompletions(w http.ResponseWriter, r *http.Request) {
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
	f.requests = append(f.requests, capturedRequest{authHeader: r.Header.Get("Authorization"), body: body})
	var respond func(w http.ResponseWriter, r *http.Request, body map[string]any)
	switch {
	case idx < len(f.completionResponses):
		respond = f.completionResponses[idx]
	case len(f.completionResponses) > 0:
		respond = f.completionResponses[len(f.completionResponses)-1]
	}
	f.mu.Unlock()

	if respond == nil {
		http.Error(w, "no fake response configured", http.StatusInternalServerError)
		return
	}
	respond(w, r, body)
}

func (f *fakeOpenAIServer) handleModels(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	respond := f.modelsResponse
	f.mu.Unlock()
	if respond == nil {
		http.Error(w, "no fake models response configured", http.StatusInternalServerError)
		return
	}
	respond(w)
}

func (f *fakeOpenAIServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeOpenAIServer) requestAt(i int) capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// ---------------------------------------------------------------------------
// Fake response bodies
// ---------------------------------------------------------------------------

func jsonCompletion(w http.ResponseWriter, statusCode int, id, model, content, refusal, finishReason string, promptTokens, completionTokens int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	body := map[string]any{
		"id":     id,
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
					"refusal": refusal,
				},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

func vendorErrorJSON(errType, message, param string) []byte {
	errBody := map[string]any{"message": message, "type": errType}
	if param != "" {
		errBody["param"] = param
	} else {
		errBody["param"] = nil
	}
	b, _ := json.Marshal(map[string]any{"error": errBody})
	return b
}

func writeVendorError(w http.ResponseWriter, statusCode int, errType, message, param string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(vendorErrorJSON(errType, message, param))
}

// sseCompletion writes a complete streaming response: one delta chunk per
// entry in textDeltas, a finish_reason chunk, a terminal usage-only chunk
// (choices: []), then [DONE].
func sseCompletion(w http.ResponseWriter, id, model string, textDeltas []string, finishReason string, promptTokens, completionTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	write := func(v map[string]any) {
		b, _ := json.Marshal(v)
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	// A role-only chunk first, matching real OpenAI streams — must be
	// skipped (empty content) by Stream.
	write(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	})
	for _, delta := range textDeltas {
		write(map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}},
		})
	}
	write(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
	})
	// Terminal usage-only chunk: choices is [] (not omitted), matching
	// stream_options.include_usage's documented shape.
	write(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens,
		},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func modelsPage(ids ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "openai"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestProvider(t *testing.T, srv *fakeOpenAIServer, creds credentialSource, cfg Config) *Provider {
	t.Helper()
	cfg.BaseURL = srv.Server.URL
	if creds == nil {
		creds = &fakeCreds{key: "sk-test-key", version: 1}
	}
	return New(creds, cfg)
}

func basicRequest(model string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
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
// Name/Kind
// ---------------------------------------------------------------------------

func TestNameKind(t *testing.T) {
	p := New(&fakeCreds{key: "x", version: 1}, Config{})
	if p.Name() != "openai" {
		t.Errorf("Name() = %q, want openai", p.Name())
	}
	if p.Kind() != "openai" {
		t.Errorf("Kind() = %q, want openai", p.Kind())
	}
}

// ---------------------------------------------------------------------------
// Request builder table test
// ---------------------------------------------------------------------------

func TestBuildRequestBody_Table(t *testing.T) {
	temp := 0.7
	topP := 0.9

	cases := []struct {
		name   string
		req    provider.ChatRequest
		stream bool
		drop   droppedParams
		check  func(t *testing.T, body map[string]any)
	}{
		{
			name: "max_completion_tokens sent, never max_tokens, when caller sets MaxTokens",
			req:  provider.ChatRequest{Model: "gpt-5", Messages: []provider.Message{{Role: "user", Content: "hi"}}, MaxTokens: 256},
			check: func(t *testing.T, body map[string]any) {
				if _, has := body["max_tokens"]; has {
					t.Error("max_tokens must never be sent")
				}
				if got, ok := body["max_completion_tokens"].(float64); !ok || int(got) != 256 {
					t.Errorf("max_completion_tokens = %v, want 256", body["max_completion_tokens"])
				}
			},
		},
		{
			name: "max_completion_tokens omitted when caller doesn't set MaxTokens",
			req:  basicRequest("gpt-5"),
			check: func(t *testing.T, body map[string]any) {
				if _, has := body["max_completion_tokens"]; has {
					t.Errorf("expected max_completion_tokens omitted, got %v", body["max_completion_tokens"])
				}
			},
		},
		{
			name: "sampling omitted unless caller sets it",
			req:  basicRequest("gpt-5"),
			check: func(t *testing.T, body map[string]any) {
				for _, k := range []string{"temperature", "top_p", "stop"} {
					if _, has := body[k]; has {
						t.Errorf("expected %s omitted when caller did not set it", k)
					}
				}
			},
		},
		{
			name: "sampling forwarded when caller sets it",
			req: provider.ChatRequest{
				Model: "gpt-5", Messages: []provider.Message{{Role: "user", Content: "hi"}},
				Temperature: &temp, TopP: &topP, Stop: []string{"END"},
			},
			check: func(t *testing.T, body map[string]any) {
				if got, ok := body["temperature"].(float64); !ok || got != temp {
					t.Errorf("temperature = %v, want %v", body["temperature"], temp)
				}
				if got, ok := body["top_p"].(float64); !ok || got != topP {
					t.Errorf("top_p = %v, want %v", body["top_p"], topP)
				}
				stop, _ := body["stop"].([]any)
				if len(stop) != 1 || stop[0] != "END" {
					t.Errorf("stop = %v, want [END]", body["stop"])
				}
			},
		},
		{
			name: "dropped sampling params omitted even though caller set them",
			req: provider.ChatRequest{
				Model: "gpt-5", Messages: []provider.Message{{Role: "user", Content: "hi"}},
				Temperature: &temp, TopP: &topP, Stop: []string{"END"},
			},
			drop: droppedParams{temperature: true, topP: true, stop: true},
			check: func(t *testing.T, body map[string]any) {
				for _, k := range []string{"temperature", "top_p", "stop"} {
					if _, has := body[k]; has {
						t.Errorf("expected %s dropped, got %v", k, body[k])
					}
				}
			},
		},
		{
			name:   "stream_options present only when streaming",
			req:    basicRequest("gpt-5"),
			stream: true,
			check: func(t *testing.T, body map[string]any) {
				if got, ok := body["stream"].(bool); !ok || !got {
					t.Errorf("stream = %v, want true", body["stream"])
				}
				so, ok := body["stream_options"].(map[string]any)
				if !ok {
					t.Fatalf("expected stream_options present, got %v", body["stream_options"])
				}
				if inc, ok := so["include_usage"].(bool); !ok || !inc {
					t.Errorf("stream_options.include_usage = %v, want true", so["include_usage"])
				}
			},
		},
		{
			name: "stream_options absent for a non-streaming request",
			req:  basicRequest("gpt-5"),
			check: func(t *testing.T, body map[string]any) {
				if _, has := body["stream_options"]; has {
					t.Error("expected stream_options omitted for a non-streaming request")
				}
				if got, has := body["stream"]; has && got != false {
					t.Errorf("expected stream omitted/false, got %v", got)
				}
			},
		},
		{
			name: "roles passed through",
			req: provider.ChatRequest{
				Model: "gpt-5",
				Messages: []provider.Message{
					{Role: "system", Content: "be nice"},
					{Role: "user", Content: "hi"},
					{Role: "assistant", Content: "hello"},
				},
			},
			check: func(t *testing.T, body map[string]any) {
				msgs, ok := body["messages"].([]any)
				if !ok || len(msgs) != 3 {
					t.Fatalf("messages = %v", body["messages"])
				}
				wantRoles := []string{"system", "user", "assistant"}
				for i, r := range wantRoles {
					m := msgs[i].(map[string]any)
					if m["role"] != r {
						t.Errorf("messages[%d].role = %v, want %v", i, m["role"], r)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := buildRequestBody(tc.req, tc.stream, tc.drop)
			if err != nil {
				t.Fatalf("buildRequestBody: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			tc.check(t, body)
		})
	}
}

// ---------------------------------------------------------------------------
// Complete: happy path, exact wire shape
// ---------------------------------------------------------------------------

func TestComplete_HappyPath(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "chatcmpl_abc", "gpt-5-mini", "Hello, world!", "", "stop", 10, 5)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	resp, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
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
			TotalTokens int `json:"total_tokens"`
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

	if strings.Contains(string(wire), "chat.completion.chunk") {
		t.Error("non-stream response must not resemble a stream chunk")
	}
}

func TestComplete_UsesBearerAuthHeader(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "chatcmpl_1", "gpt-5-mini", "hi", "", "stop", 1, 1)
		},
	}
	sentinel := "sk-sentinel-do-not-leak-CANARY123"
	p := newTestProvider(t, srv, &fakeCreds{key: sentinel, version: 1}, Config{})

	if _, err := p.Complete(context.Background(), basicRequest("gpt-5-mini")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Bearer " + sentinel; srv.requestAt(0).authHeader != want {
		t.Errorf("Authorization header = %q, want %q", srv.requestAt(0).authHeader, want)
	}
}

// ---------------------------------------------------------------------------
// Complete/Stream: vendor-specific fields never forwarded
// ---------------------------------------------------------------------------

func TestComplete_VendorFieldsNeverForwarded(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"id": "chatcmpl_x", "object": "chat.completion", "model": "gpt-5-mini",
				"system_fingerprint": "fp_leak_me_not", "service_tier": "default",
				"choices": [{"index":0,"message":{"role":"assistant","content":"hi","refusal":""},"finish_reason":"stop"}],
				"usage": {"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	resp, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wire, _ := provider.BuildCompletionJSON(resp)
	if strings.Contains(string(wire), "fp_leak_me_not") || strings.Contains(string(wire), "service_tier") {
		t.Errorf("vendor-specific fields leaked into the rendered response: %s", wire)
	}
}

// ---------------------------------------------------------------------------
// Stream: happy path
// ---------------------------------------------------------------------------

func TestStream_HappyPath(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			sseCompletion(w, "chatcmpl_stream1", "gpt-5-mini", []string{"Hello", ", ", "world!"}, "stop", 8, 3)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), basicRequest("gpt-5-mini"), &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", stats.FinishReason)
	}
	if stats.PromptTokens != 8 || stats.CompletionTokens != 3 {
		t.Errorf("tokens = %d/%d, want 8/3 (from the terminal usage-only chunk)", stats.PromptTokens, stats.CompletionTokens)
	}

	out := buf.String()
	if !strings.HasSuffix(out, provider.StreamDoneSSE) {
		t.Errorf("expected stream to end with the DONE sentinel, got: %q", out)
	}
	if strings.Contains(out, "system_fingerprint") || strings.Contains(out, "service_tier") {
		t.Error("vendor-specific fields must never appear in the rendered stream")
	}

	var content strings.Builder
	sawFinal := false
	chunkCount := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		chunkCount++
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
			t.Fatalf("expected exactly one choice per rendered chunk, got %+v", chunk)
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
	// The role-only chunk and the terminal usage-only (empty choices) chunk
	// must both have been skipped — only the 3 content deltas + 1 final
	// chunk should have been rendered.
	if chunkCount != 4 {
		t.Errorf("rendered %d content chunks, want 4 (role-only and usage-only chunks must be skipped)", chunkCount)
	}
}

// ---------------------------------------------------------------------------
// Refusal
// ---------------------------------------------------------------------------

func TestComplete_Refusal(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "chatcmpl_refusal", "gpt-5-mini", "", "I can't help with that.", "stop", 10, 0)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	resp, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
	if err != nil {
		t.Fatalf("a refusal must still produce a well-formed response, got error: %v", err)
	}
	if resp.FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want content_filter", resp.FinishReason)
	}
	if resp.Content != "I can't help with that." {
		t.Errorf("Content = %q, want the refusal text surfaced as content", resp.Content)
	}
}

func TestStream_Refusal(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			write := func(v map[string]any) {
				b, _ := json.Marshal(v)
				_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			write(map[string]any{
				"id": "chatcmpl_refusal_stream", "model": "gpt-5-mini",
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"refusal": "I can't help with that."}, "finish_reason": nil}},
			})
			write(map[string]any{
				"id": "chatcmpl_refusal_stream", "model": "gpt-5-mini",
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), basicRequest("gpt-5-mini"), &buf)
	if err != nil {
		t.Fatalf("a refusal must still produce a well-formed stream, got error: %v", err)
	}
	if stats.FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want content_filter", stats.FinishReason)
	}
	if !strings.Contains(buf.String(), "I can't help with that.") {
		t.Errorf("expected the refusal text surfaced as content, got: %s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Retry-once / no-loop
// ---------------------------------------------------------------------------

func TestComplete_RetriesOnceWithoutUnsupportedParam(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasTemp := body["temperature"]; !hasTemp {
				t.Fatal("expected the first attempt to include temperature")
			}
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "Unsupported value: 'temperature' is not supported with this model.", "temperature")
		},
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasTemp := body["temperature"]; hasTemp {
				t.Fatal("expected the retry to omit temperature")
			}
			jsonCompletion(w, http.StatusOK, "chatcmpl_retry", "gpt-5-mini", "ok", "", "stop", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	req := basicRequest("gpt-5-mini")
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

func TestComplete_RetryDetectsParamFromMessageWhenParamFieldAbsent(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "top_p is not supported for this model.", "")
		},
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasTopP := body["top_p"]; hasTopP {
				t.Fatal("expected the retry to omit top_p")
			}
			jsonCompletion(w, http.StatusOK, "chatcmpl_retry2", "gpt-5-mini", "ok", "", "stop", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	req := basicRequest("gpt-5-mini")
	topP := 0.5
	req.TopP = &topP

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv.callCount() != 2 {
		t.Errorf("expected exactly 2 calls, got %d", srv.callCount())
	}
}

func TestComplete_RetryDoesNotLoopOnRepeatedFailure(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	unsupportedResponder := func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "temperature is not supported for this model.", "temperature")
	}
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		unsupportedResponder, unsupportedResponder,
	}
	p := newTestProvider(t, srv, nil, Config{})

	req := basicRequest("gpt-5-mini")
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

func TestComplete_400UnrelatedToSamplingParamsIsNotRetried(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "'model' does not exist.", "model")
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.Complete(context.Background(), basicRequest("does-not-exist"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if srv.callCount() != 1 {
		t.Errorf("expected exactly 1 call (no retry for a non-sampling 400), got %d", srv.callCount())
	}
}

func TestStream_RetriesOnceWithoutUnsupportedParam(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasStop := body["stop"]; !hasStop {
				t.Fatal("expected the first attempt to include stop")
			}
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "'stop' is not supported with this model.", "stop")
		},
		func(w http.ResponseWriter, r *http.Request, body map[string]any) {
			if _, hasStop := body["stop"]; hasStop {
				t.Fatal("expected the retry to omit stop")
			}
			sseCompletion(w, "chatcmpl_retry_stream", "gpt-5-mini", []string{"ok"}, "stop", 1, 1)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	req := basicRequest("gpt-5-mini")
	req.Stop = []string{"END"}

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

func TestStream_400PrecedesAnyByteWritten(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "'model' does not exist.", "model")
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	var buf bytes.Buffer
	_, err := p.Stream(context.Background(), basicRequest("does-not-exist"), &buf)
	if err == nil {
		t.Fatal("expected an error")
	}
	if buf.Len() != 0 {
		t.Errorf("expected nothing written to w before a 400 is returned, got: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Error translation
// ---------------------------------------------------------------------------

func TestComplete_RateLimitedWithRetryAfter(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Retry-After", "3")
			writeVendorError(w, http.StatusTooManyRequests, "rate_limit_error", "rate limited", "")
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	if apiErr.HTTPStatus() != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus = %d, want 429", apiErr.HTTPStatus())
	}
	if apiErr.RetryAfterSeconds() != 3 {
		t.Errorf("RetryAfterSeconds = %d, want 3", apiErr.RetryAfterSeconds())
	}
	assertErrorCode(t, apiErr, "provider_rate_limited")
	if srv.callCount() != 1 {
		t.Errorf("expected exactly 1 call (no client-side retry for a plain 429), got %d", srv.callCount())
	}
}

func TestComplete_AuthFailureReportsStatus(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusUnauthorized, "invalid_request_error", "Incorrect API key provided.", "")
		},
	}
	status := &fakeStatus{}
	p := newTestProvider(t, srv, nil, Config{Status: status})

	_, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
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
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusServiceUnavailable, "server_error", "overloaded", "")
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
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
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "bad request for account "+vendorSecret, "")
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
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
	sentinel := "sk-CANARY-DO-NOT-LEAK-abc123"

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "chatcmpl_1", "gpt-5-mini", "hi", "", "stop", 1, 1)
		},
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "bad request", "")
		},
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "temperature is not supported for this model.", "temperature")
		},
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "chatcmpl_2", "gpt-5-mini", "ok", "", "stop", 1, 1)
		},
	}
	p := newTestProvider(t, srv, &fakeCreds{key: sentinel, version: 1}, Config{})

	var bodies [][]byte

	resp, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wire, _ := provider.BuildCompletionJSON(resp)
	bodies = append(bodies, wire)

	_, err = p.Complete(context.Background(), basicRequest("gpt-5-mini"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %v", err)
	}
	bodies = append(bodies, apiErr.ResponseBody())
	if strings.Contains(apiErr.Error(), sentinel) {
		t.Errorf("APIError.Error() leaked the key: %q", apiErr.Error())
	}

	req := basicRequest("gpt-5-mini")
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

// ---------------------------------------------------------------------------
// Key rotation mid-run
// ---------------------------------------------------------------------------

func TestKeyRotation_SecondRequestUsesNewKey(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "chatcmpl_1", "gpt-5-mini", "one", "", "stop", 1, 1)
		},
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			jsonCompletion(w, http.StatusOK, "chatcmpl_2", "gpt-5-mini", "two", "", "stop", 1, 1)
		},
	}
	creds := &fakeCreds{key: "sk-old-key", version: 1}
	p := newTestProvider(t, srv, creds, Config{})

	if _, err := p.Complete(context.Background(), basicRequest("gpt-5-mini")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Bearer sk-old-key"; srv.requestAt(0).authHeader != want {
		t.Fatalf("expected first request to use the old key, got %q", srv.requestAt(0).authHeader)
	}

	creds.set("sk-new-key")

	if _, err := p.Complete(context.Background(), basicRequest("gpt-5-mini")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Bearer sk-new-key"; srv.requestAt(1).authHeader != want {
		t.Fatalf("expected second request to use the new key, got %q", srv.requestAt(1).authHeader)
	}
}

// ---------------------------------------------------------------------------
// Idle timeout
// ---------------------------------------------------------------------------

func TestStream_IdleTimeout(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: "+`{"id":"chatcmpl_hang","model":"gpt-5-mini","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-block
		},
	}
	p := newTestProvider(t, srv, nil, Config{})
	p.streamIdleTimeout = 100 * time.Millisecond

	var buf bytes.Buffer
	start := time.Now()
	_, err := p.Stream(context.Background(), basicRequest("gpt-5-mini"), &buf)
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
	srv := newFakeOpenAIServer(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-block:
			case <-r.Context().Done():
			}
		},
	}
	p := newTestProvider(t, srv, nil, Config{})
	p.streamIdleTimeout = time.Minute // must not be what ends the stream here

	before := countGoroutines()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf bytes.Buffer
		_, _ = p.Stream(ctx, basicRequest("gpt-5-mini"), &buf)
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

func countGoroutines() int {
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)
	runtime.GC()
	return runtime.NumGoroutine()
}

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
// Phase A1 parameter honesty: json_object, max_completion_tokens,
// stream_options.include_usage, and the retry never touching new fields.
// ---------------------------------------------------------------------------

func TestParamCaps_OpenAI(t *testing.T) {
	want := provider.ParamCaps{JSONMode: true, Stop: true, ForcedToolChoice: true}
	if got := New(&fakeCreds{key: "k"}, Config{}).ParamCaps(); got != want {
		t.Errorf("ParamCaps = %+v, want %+v", got, want)
	}
}

func TestBuildRequestBody_JSONModeAndMaxCompletionTokens(t *testing.T) {
	req := basicRequest("gpt-5")
	req.JSONMode = true
	req.MaxTokens = 321
	b, err := buildRequestBody(req, false, droppedParams{})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	if rf, _ := body["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want {type:json_object}", body["response_format"])
	}
	if body["max_completion_tokens"] != float64(321) {
		t.Errorf("max_completion_tokens = %v, want 321", body["max_completion_tokens"])
	}
	if _, has := body["max_tokens"]; has {
		t.Error("max_tokens must never be sent")
	}

	// Absent when not asked for.
	b, _ = buildRequestBody(basicRequest("gpt-5"), false, droppedParams{})
	body = nil
	_ = json.Unmarshal(b, &body)
	if _, has := body["response_format"]; has {
		t.Error("response_format must be omitted unless json_object was requested")
	}
}

// Even with every droppable sampling parameter dropped, the new fields stay.
func TestBuildRequestBody_DropNeverRemovesJSONModeOrMaxTokens(t *testing.T) {
	req := basicRequest("gpt-5")
	req.JSONMode = true
	req.MaxTokens = 50
	b, _ := buildRequestBody(req, true, droppedParams{temperature: true, topP: true, stop: true})
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	if body["response_format"] == nil || body["max_completion_tokens"] == nil {
		t.Errorf("drop removed a non-sampling field: %v", body)
	}
}

// A vendor 400 that names response_format (or max_completion_tokens) is not
// retried by dropping a sampling parameter, even when the message text
// contains a word the retry heuristic matches, and the caller gets a clean
// 400 naming the field, never the vendor's text.
func TestComplete_400OnResponseFormatIsNotRetriedAndNamesTheField(t *testing.T) {
	for _, param := range []string{"response_format", "max_completion_tokens"} {
		t.Run(param, func(t *testing.T) {
			srv := newFakeOpenAIServer(t)
			srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
				func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
					writeVendorError(w, http.StatusBadRequest, "invalid_request_error",
						"'"+param+"' is not supported with this model; stop and use something else. acct org-123", param)
				},
			}
			p := newTestProvider(t, srv, nil, Config{})
			req := basicRequest("gpt-4")
			req.JSONMode = true
			req.Stop = []string{"END"}
			_, err := p.Complete(context.Background(), req)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.HTTPStatus() != http.StatusBadRequest {
				t.Fatalf("err = %v, want a 400 APIError", err)
			}
			if srv.callCount() != 1 {
				t.Errorf("calls = %d, want 1 (no retry)", srv.callCount())
			}
			body := string(apiErr.ResponseBody())
			if !strings.Contains(body, `"param":"`+param+`"`) || !strings.Contains(body, `"code":"invalid_request"`) {
				t.Errorf("body = %s, want invalid_request naming %s", body, param)
			}
			if strings.Contains(body, "org-123") {
				t.Errorf("vendor text leaked: %s", body)
			}
		})
	}
}

func TestStream_400OnResponseFormatNamesTheField(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "nope", "response_format")
		},
	}
	p := newTestProvider(t, srv, nil, Config{})
	req := basicRequest("gpt-4")
	req.JSONMode = true
	var buf bytes.Buffer
	_, err := p.Stream(context.Background(), req, &buf)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(string(apiErr.ResponseBody()), `"param":"response_format"`) {
		t.Fatalf("err = %v, want a 400 naming response_format", err)
	}
	if buf.Len() != 0 || srv.callCount() != 1 {
		t.Errorf("wrote %d bytes, %d calls; want 0 and 1", buf.Len(), srv.callCount())
	}
}

func TestDetectUnsupportedParam_IgnoresOtherFieldsEvenWithMatchingWords(t *testing.T) {
	body := vendorErrorJSON("invalid_request_error", "stop is not supported here", "response_format")
	if _, ok := detectUnsupportedParam(body, droppedParams{}); ok {
		t.Error("a 400 attributed to response_format must not trigger a sampling-parameter retry")
	}
}

// streamFrames returns the decoded data frames of an SSE body (without DONE).
func streamFrames(t *testing.T, out string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, line := range strings.Split(out, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("decode %q: %v", payload, err)
		}
		frames = append(frames, m)
	}
	return frames
}

func TestStream_IncludeUsageShape(t *testing.T) {
	run := func(t *testing.T, includeUsage bool) []map[string]any {
		srv := newFakeOpenAIServer(t)
		srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
			func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
				// The upstream is always asked for usage (metering).
				if so, _ := body["stream_options"].(map[string]any); so["include_usage"] != true {
					t.Errorf("upstream stream_options = %v, want include_usage true", body["stream_options"])
				}
				sseCompletion(w, "chatcmpl_u", "gpt-5-mini", []string{"Hi"}, "stop", 8, 3)
			},
		}
		p := newTestProvider(t, srv, nil, Config{})
		req := basicRequest("gpt-5-mini")
		req.IncludeUsage = includeUsage
		var buf bytes.Buffer
		if _, err := p.Stream(context.Background(), req, &buf); err != nil {
			t.Fatal(err)
		}
		return streamFrames(t, buf.String())
	}

	t.Run("asked", func(t *testing.T) {
		frames := run(t, true)
		// content, finish (usage null), usage-only (choices []).
		if len(frames) != 3 {
			t.Fatalf("frames = %d, want 3: %v", len(frames), frames)
		}
		if frames[1]["usage"] != nil || frames[1]["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
			t.Errorf("finish frame = %v", frames[1])
		}
		last := frames[2]
		if ch, ok := last["choices"].([]any); !ok || len(ch) != 0 {
			t.Errorf("usage frame choices = %v, want []", last["choices"])
		}
		if u := last["usage"].(map[string]any); u["prompt_tokens"] != float64(8) || u["completion_tokens"] != float64(3) || u["total_tokens"] != float64(11) {
			t.Errorf("usage = %v", u)
		}
	})

	t.Run("not asked keeps usage on the finish chunk", func(t *testing.T) {
		frames := run(t, false)
		if len(frames) != 2 {
			t.Fatalf("frames = %d, want 2 (content, finish+usage): %v", len(frames), frames)
		}
		if frames[1]["usage"].(map[string]any)["total_tokens"] != float64(11) {
			t.Errorf("finish frame usage = %v", frames[1]["usage"])
		}
	})
}
