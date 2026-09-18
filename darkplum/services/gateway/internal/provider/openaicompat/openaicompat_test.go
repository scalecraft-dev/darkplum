package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/openaicompat"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestProvider(t *testing.T, serverURL string) *openaicompat.OpenAICompatProvider {
	t.Helper()
	p, err := openaicompat.New("vllm-default", "vllm", serverURL)
	if err != nil {
		t.Fatalf("openaicompat.New: %v", err)
	}
	return p
}

func testRequest() provider.ChatRequest {
	return provider.ChatRequest{
		Model: "llama-3-8b",
		Messages: []provider.Message{
			{Role: "user", Content: "hello"},
		},
		Stream: false,
	}
}

// buildMockCompletion returns a JSON-encoded OpenAI chat.completion response.
func buildMockCompletion(id, model, content string, promptTok, compTok int) []byte {
	finishReason := "stop"
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": content},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     promptTok,
			"completion_tokens": compTok,
			"total_tokens":      promptTok + compTok,
		},
	}
	b, _ := json.Marshal(resp)
	return b
}

// buildMockStreamingResponse builds a multi-chunk OpenAI SSE response that
// includes a content chunk and a final stop chunk, followed by [DONE].
func buildMockStreamingResponse(id, model, content string, promptTok, compTok int) string {
	var sb strings.Builder

	// Content chunk.
	contentChunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]string{"role": "assistant", "content": content},
				"finish_reason": nil,
			},
		},
	}
	b, _ := json.Marshal(contentChunk)
	fmt.Fprintf(&sb, "data: %s\n\n", b)

	// Final chunk with finish_reason and usage.
	finishReason := "stop"
	finalChunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]string{},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     promptTok,
			"completion_tokens": compTok,
			"total_tokens":      promptTok + compTok,
		},
	}
	b, _ = json.Marshal(finalChunk)
	fmt.Fprintf(&sb, "data: %s\n\n", b)

	fmt.Fprintf(&sb, "data: [DONE]\n\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// New() validation
// ---------------------------------------------------------------------------

func TestNew_InvalidURL(t *testing.T) {
	_, err := openaicompat.New("vllm-default", "vllm", "://bad-url")
	if err == nil {
		t.Fatal("expected error for invalid URL, got nil")
	}
}

func TestNew_MissingScheme(t *testing.T) {
	_, err := openaicompat.New("vllm-default", "vllm", "vllm-inference:8000")
	if err == nil {
		t.Fatal("expected error for URL missing scheme, got nil")
	}
}

func TestNew_ValidURL(t *testing.T) {
	_, err := openaicompat.New("vllm-default", "vllm", "http://vllm-inference.darkplum-inference.svc.cluster.local:8000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNew_EmptyName(t *testing.T) {
	_, err := openaicompat.New("", "vllm", "http://localhost:8000")
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
}

func TestNew_EmptyKind(t *testing.T) {
	_, err := openaicompat.New("vllm-default", "", "http://localhost:8000")
	if err == nil {
		t.Fatal("expected error for empty kind, got nil")
	}
}

// ---------------------------------------------------------------------------
// Name() and Kind()
// ---------------------------------------------------------------------------

func TestName(t *testing.T) {
	p, err := openaicompat.New("vllm-default", "vllm", "http://localhost:9999")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "vllm-default" {
		t.Errorf("Name() = %q, want %q", p.Name(), "vllm-default")
	}
}

func TestKind(t *testing.T) {
	p, err := openaicompat.New("vllm-default", "vllm", "http://localhost:9999")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Kind() != "vllm" {
		t.Errorf("Kind() = %q, want %q", p.Kind(), "vllm")
	}
}

func TestKind_LMStudio(t *testing.T) {
	p, err := openaicompat.New("lmstudio-dev", "lmstudio", "http://localhost:1234")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != "lmstudio-dev" {
		t.Errorf("Name() = %q, want %q", p.Name(), "lmstudio-dev")
	}
	if p.Kind() != "lmstudio" {
		t.Errorf("Kind() = %q, want %q", p.Kind(), "lmstudio")
	}
}

// ---------------------------------------------------------------------------
// Complete() — happy path
// ---------------------------------------------------------------------------

func TestComplete_HappyPath(t *testing.T) {
	const (
		wantID      = "chatcmpl-abc123"
		wantModel   = "llama-3-8b"
		wantContent = "The capital of France is Paris."
		wantPrompt  = 12
		wantComp    = 8
	)

	var capturedAuthHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		capturedAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildMockCompletion(wantID, wantModel, wantContent, wantPrompt, wantComp))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	req := testRequest()

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if resp.ID != wantID {
		t.Errorf("ID = %q, want %q", resp.ID, wantID)
	}
	if resp.Model != wantModel {
		t.Errorf("Model = %q, want %q", resp.Model, wantModel)
	}
	if resp.Content != wantContent {
		t.Errorf("Content = %q, want %q", resp.Content, wantContent)
	}
	if resp.PromptTokens != wantPrompt {
		t.Errorf("PromptTokens = %d, want %d", resp.PromptTokens, wantPrompt)
	}
	if resp.CompletionTokens != wantComp {
		t.Errorf("CompletionTokens = %d, want %d", resp.CompletionTokens, wantComp)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "stop")
	}

	// Authorization must be stripped before reaching the upstream.
	if capturedAuthHeader != "" {
		t.Errorf("Authorization header leaked to upstream: %q", capturedAuthHeader)
	}
}

// ---------------------------------------------------------------------------
// Complete() — upstream sends Authorization (shouldn't happen but check strip)
// ---------------------------------------------------------------------------

func TestComplete_AuthorizationHeaderStripped(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildMockCompletion("id-1", "model", "ok", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	resp, err := p.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_ = resp

	if capturedAuth != "" {
		t.Errorf("Authorization should be stripped, got: %q", capturedAuth)
	}
}

// ---------------------------------------------------------------------------
// Complete() — non-200 response
// ---------------------------------------------------------------------------

func TestComplete_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"message":"model loading","type":"server_error"}}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.Complete(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected error for 503 response, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status code 503, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Complete() — upstream returns well-formed error in 200 body
// ---------------------------------------------------------------------------

func TestComplete_UpstreamErrorIn200Body(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error":{"message":"context length exceeded","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.Complete(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected error for upstream error body, got nil")
	}
	if !strings.Contains(err.Error(), "context length exceeded") {
		t.Errorf("expected upstream error message in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Complete() — empty choices
// ---------------------------------------------------------------------------

func TestComplete_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[]}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.Complete(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected error for empty choices, got nil")
	}
}

// ---------------------------------------------------------------------------
// Stream() — happy path: bytes forwarded, stats populated
// ---------------------------------------------------------------------------

func TestStream_HappyPath(t *testing.T) {
	const (
		wantID     = "chatcmpl-stream-1"
		wantModel  = "llama-3-8b"
		wantText   = "Hello there."
		wantPrompt = 5
		wantComp   = 3
	)

	sseBody := buildMockStreamingResponse(wantID, wantModel, wantText, wantPrompt, wantComp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, sseBody)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Stats from the final chunk.
	if stats.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", stats.FinishReason, "stop")
	}
	if stats.PromptTokens != wantPrompt {
		t.Errorf("PromptTokens = %d, want %d", stats.PromptTokens, wantPrompt)
	}
	if stats.CompletionTokens != wantComp {
		t.Errorf("CompletionTokens = %d, want %d", stats.CompletionTokens, wantComp)
	}
	if stats.Model != wantModel {
		t.Errorf("Model = %q, want %q", stats.Model, wantModel)
	}

	// The raw SSE bytes should flow through to the writer. We check that the
	// content appears somewhere in the written bytes and that [DONE] was forwarded.
	written := buf.String()
	if !strings.Contains(written, wantText) {
		t.Errorf("stream output should contain content %q, got:\n%s", wantText, written)
	}
	if !strings.Contains(written, "[DONE]") {
		t.Errorf("stream output should contain [DONE] terminator, got:\n%s", written)
	}
}

// ---------------------------------------------------------------------------
// Stream() — upstream ignores stream=true and replies with plain JSON
// ---------------------------------------------------------------------------

func TestStream_JSONUpstreamAdaptedToSSE(t *testing.T) {
	const (
		wantID     = "chatcmpl-nonstream-1"
		wantModel  = "llama-3-8b"
		wantText   = "Hello from a non-streaming upstream."
		wantPrompt = 11
		wantComp   = 6
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// LM Studio / some proxies ignore stream:true and answer with a
		// plain chat.completion JSON body instead of SSE.
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildMockCompletion(wantID, wantModel, wantText, wantPrompt, wantComp))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if stats.Model != wantModel {
		t.Errorf("Model = %q, want %q", stats.Model, wantModel)
	}
	if stats.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", stats.FinishReason, "stop")
	}
	if stats.PromptTokens != wantPrompt {
		t.Errorf("PromptTokens = %d, want %d", stats.PromptTokens, wantPrompt)
	}
	if stats.CompletionTokens != wantComp {
		t.Errorf("CompletionTokens = %d, want %d", stats.CompletionTokens, wantComp)
	}

	written := buf.String()
	if !strings.HasPrefix(written, "data: ") {
		t.Errorf("adapted output should be SSE (start with 'data: '), got:\n%s", written)
	}
	if !strings.Contains(written, wantText) {
		t.Errorf("adapted output should contain content %q, got:\n%s", wantText, written)
	}
	if !strings.Contains(written, `"finish_reason":"stop"`) {
		t.Errorf("adapted output should contain finish_reason, got:\n%s", written)
	}
	if !strings.HasSuffix(written, provider.StreamDoneSSE) {
		t.Errorf("adapted output should end with the [DONE] terminator, got:\n%s", written)
	}

	// Every SSE frame must be independently valid JSON.
	for _, line := range strings.Split(written, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			t.Errorf("adapted SSE data line is not valid JSON: %q: %v", line, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Stream() — upstream ignores stream=true and replies with undecodable JSON
// ---------------------------------------------------------------------------

func TestStream_JSONUpstreamGarbage_NothingWritten(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true

	var buf bytes.Buffer
	_, err := p.Stream(context.Background(), req, &buf)
	if err == nil {
		t.Fatal("expected error for undecodable JSON body, got nil")
	}
	if buf.Len() != 0 {
		t.Errorf("expected nothing written to w on decode failure, got:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Stream() — upstream JSON body with no choices produces an error, no writes
// ---------------------------------------------------------------------------

func TestStream_JSONUpstreamNoChoices_NothingWritten(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[]}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true

	var buf bytes.Buffer
	_, err := p.Stream(context.Background(), req, &buf)
	if err == nil {
		t.Fatal("expected error for JSON body with no choices, got nil")
	}
	if buf.Len() != 0 {
		t.Errorf("expected nothing written to w, got:\n%s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Stream() — non-200 response
// ---------------------------------------------------------------------------

func TestStream_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"internal error","type":"server_error"}}`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true

	_, err := p.Stream(context.Background(), req, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code 500, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Stream() — upstream connection failure
// ---------------------------------------------------------------------------

func TestStream_ConnectionFailure(t *testing.T) {
	// Point at a port that has nothing listening.
	p, err := openaicompat.New("vllm-default", "vllm", "http://127.0.0.1:19999")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := testRequest()
	req.Stream = true

	_, err = p.Stream(context.Background(), req, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for connection failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// Complete() — upstream connection failure
// ---------------------------------------------------------------------------

func TestComplete_ConnectionFailure(t *testing.T) {
	p, err := openaicompat.New("vllm-default", "vllm", "http://127.0.0.1:19999")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Complete(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected error for connection failure, got nil")
	}
}

// ---------------------------------------------------------------------------
// Stream() — malformed JSON chunks don't abort the stream
// ---------------------------------------------------------------------------

func TestStream_MalformedChunkSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Mix a malformed chunk with a valid one and [DONE].
		fmt.Fprint(w, "data: {this is not json}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true

	var buf bytes.Buffer
	// Should not return an error — malformed chunks are skipped.
	_, err := p.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("Stream should tolerate malformed chunks, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// provider.BuildCompletionJSON — round-trip check
// ---------------------------------------------------------------------------

func TestBuildCompletionJSON(t *testing.T) {
	resp := &provider.ChatResponse{
		ID:               "chatcmpl-test",
		Model:            "llama-3-8b",
		Content:          "hello",
		PromptTokens:     10,
		CompletionTokens: 5,
		FinishReason:     "stop",
	}

	b, err := provider.BuildCompletionJSON(resp)
	if err != nil {
		t.Fatalf("BuildCompletionJSON: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if parsed["object"] != "chat.completion" {
		t.Errorf("object = %q, want %q", parsed["object"], "chat.completion")
	}

	usage, ok := parsed["usage"].(map[string]any)
	if !ok {
		t.Fatal("usage field missing or wrong type")
	}
	if usage["total_tokens"].(float64) != 15 {
		t.Errorf("total_tokens = %v, want 15", usage["total_tokens"])
	}
}

// ---------------------------------------------------------------------------
// provider.BuildStreamingChunkSSE and BuildFinalChunkSSE
// ---------------------------------------------------------------------------

func TestBuildStreamingChunkSSE(t *testing.T) {
	b, err := provider.BuildStreamingChunkSSE("id-1", "llama-3-8b", "hello")
	if err != nil {
		t.Fatalf("BuildStreamingChunkSSE: %v", err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "data: ") {
		t.Errorf("chunk should start with 'data: ', got: %q", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Errorf("chunk should end with \\n\\n, got: %q", s)
	}
	// Parse the JSON payload.
	payload := strings.TrimPrefix(strings.TrimSuffix(s, "\n\n"), "data: ")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("chunk payload is not valid JSON: %v", err)
	}
	if parsed["object"] != "chat.completion.chunk" {
		t.Errorf("object = %q, want chat.completion.chunk", parsed["object"])
	}
}

func TestBuildFinalChunkSSE(t *testing.T) {
	stats := &provider.StreamStats{
		PromptTokens:     7,
		CompletionTokens: 3,
		FinishReason:     "stop",
	}
	b, err := provider.BuildFinalChunkSSE("id-1", "llama-3-8b", "stop", stats)
	if err != nil {
		t.Fatalf("BuildFinalChunkSSE: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Errorf("final chunk should contain finish_reason, got: %s", s)
	}
}

func TestStreamDoneSSE(t *testing.T) {
	if provider.StreamDoneSSE != "data: [DONE]\n\n" {
		t.Errorf("StreamDoneSSE = %q, want %q", provider.StreamDoneSSE, "data: [DONE]\n\n")
	}
}
