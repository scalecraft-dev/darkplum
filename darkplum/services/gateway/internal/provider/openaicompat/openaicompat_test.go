package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openaicompat"
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

// TestComplete_TopPAndStopForwarded asserts top_p and stop are round-tripped
// onto the upstream request body when the caller set them, and omitted
// entirely when not set — request-shape honesty applies both ways.
func TestComplete_TopPAndStopForwarded(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode upstream request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildMockCompletion("id-1", "model", "ok", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	topP := 0.9
	req := testRequest()
	req.TopP = &topP
	req.Stop = []string{"STOP"}

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, ok := captured["top_p"].(float64); !ok || got != 0.9 {
		t.Errorf("top_p = %v, want 0.9", captured["top_p"])
	}
	stopVal, ok := captured["stop"].([]any)
	if !ok || len(stopVal) != 1 || stopVal[0] != "STOP" {
		t.Errorf("stop = %v, want [STOP]", captured["stop"])
	}
}

func TestComplete_TopPAndStopOmittedWhenUnset(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode upstream request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(buildMockCompletion("id-1", "model", "ok", 1, 1))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	if _, err := p.Complete(context.Background(), testRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := captured["top_p"]; ok {
		t.Errorf("expected no top_p field when unset, got %v", captured["top_p"])
	}
	if _, ok := captured["stop"]; ok {
		t.Errorf("expected no stop field when unset, got %v", captured["stop"])
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
// Stream() — idle-read watchdog (real transport, not a fake io.Reader)
// ---------------------------------------------------------------------------

// TestStream_IdleTimeoutAbortsBlockedRead is the regression test for the
// review finding that the idle watchdog never actually aborted a blocked
// read: Stream built the upstream *http.Request with the caller's ctx
// instead of idleCtx, so cancelling idleCtx (via IdleTimeoutReader's
// cancelCause) did nothing to the in-flight request — net/http only aborts a
// request by cancelling the exact context.Context it was built with. This
// server sends headers plus one SSE chunk, then blocks forever (until the
// test ends) without closing the connection, simulating a wedged upstream
// (e.g. an OOM-killed model process that leaves the TCP connection open).
// With a shortened StreamIdleTimeout, Stream must return an idle-timeout
// error promptly, AND the server handler's own request context must
// actually be cancelled (proving the request — not just IdleTimeoutReader's
// local bookkeeping — was aborted).
func TestStream_IdleTimeoutAbortsBlockedRead(t *testing.T) {
	block := make(chan struct{})
	serverCtxDone := make(chan struct{})
	t.Cleanup(func() { close(block) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+`{"id":"chatcmpl_hang","object":"chat.completion.chunk","model":"llama-3-8b","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Block until either the test cleans up (fallback, so the handler
		// goroutine doesn't leak past the test) or the client's request
		// context is cancelled — which is the actual assertion: it proves
		// Stream's idle watchdog aborted the REQUEST, not just its own local
		// read loop.
		select {
		case <-block:
		case <-r.Context().Done():
			close(serverCtxDone)
		}
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	p.StreamIdleTimeout = 100 * time.Millisecond
	req := testRequest()
	req.Stream = true

	start := time.Now()
	_, err := p.Stream(context.Background(), req, &bytes.Buffer{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an idle-timeout error")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Errorf("expected an idle-timeout error, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("idle timeout took too long to fire: %s", elapsed)
	}

	select {
	case <-serverCtxDone:
		// Good: the server observed the request context being cancelled.
	case <-time.After(5 * time.Second):
		t.Fatal("server handler's request context was never cancelled — the idle watchdog aborted only the local read loop, not the actual upstream request")
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

// ---------------------------------------------------------------------------
// Phase A1 parameter honesty: json_object, max_tokens, include_usage.
// ---------------------------------------------------------------------------

func TestParamCaps_OpenAICompat(t *testing.T) {
	want := provider.ParamCaps{JSONMode: true, Stop: true, ForcedToolChoice: true}
	if got := newTestProvider(t, "http://127.0.0.1:1").ParamCaps(); got != want {
		t.Errorf("ParamCaps = %+v, want %+v", got, want)
	}
}

// captureUpstream serves respond for every request and records the decoded
// request bodies.
func captureUpstream(t *testing.T, respond func(w http.ResponseWriter)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		respond(w)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestComplete_JSONModeAndMaxTokensOnTheWire(t *testing.T) {
	srv, bodies := captureUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildMockCompletion("id", "llama-3-8b", "{}", 1, 1))
	})
	p := newTestProvider(t, srv.URL)

	req := testRequest()
	req.JSONMode = true
	req.MaxTokens = 123
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b := (*bodies)[0]
	if rf, _ := b["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want json_object", b["response_format"])
	}
	if b["max_tokens"] != float64(123) {
		t.Errorf("max_tokens = %v, want 123 (vLLM's wire field)", b["max_tokens"])
	}
	if _, has := b["max_completion_tokens"]; has {
		t.Error("vLLM takes max_tokens; max_completion_tokens must not be sent")
	}

	if _, err := p.Complete(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if _, has := (*bodies)[1]["response_format"]; has {
		t.Error("response_format must be omitted unless json_object was requested")
	}
}

func sseFrames(t *testing.T, out string) []map[string]any {
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

// vLLM answers include_usage itself: a usage-only chunk with choices [] right
// before [DONE]. It reaches the caller once, as sent.
func vllmStreamWithUsageChunk() string {
	return `data: {"id":"c1","object":"chat.completion.chunk","model":"llama-3-8b","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}],"usage":null}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"llama-3-8b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"llama-3-8b","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func TestStream_IncludeUsageAskedOfUpstreamOnlyWhenRequested(t *testing.T) {
	srv, bodies := captureUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, vllmStreamWithUsageChunk())
	})
	p := newTestProvider(t, srv.URL)

	req := testRequest()
	req.Stream = true
	req.IncludeUsage = true
	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if so, _ := (*bodies)[0]["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("upstream stream_options = %v, want include_usage true", (*bodies)[0]["stream_options"])
	}
	if stats.PromptTokens != 5 || stats.CompletionTokens != 2 {
		t.Errorf("stats = %+v, want 5/2", stats)
	}
	frames := sseFrames(t, buf.String())
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3 (the upstream's own usage chunk, not duplicated): %v", len(frames), frames)
	}
	if ch, ok := frames[2]["choices"].([]any); !ok || len(ch) != 0 || frames[2]["usage"] == nil {
		t.Errorf("last frame = %v, want choices [] with usage", frames[2])
	}

	req.IncludeUsage = false
	if _, err := p.Stream(context.Background(), req, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, has := (*bodies)[1]["stream_options"]; has {
		t.Error("stream_options must not be sent when the caller did not ask for include_usage")
	}
}

// An upstream that ignores stream_options but reports usage on the finish
// chunk still yields the usage-only chunk the caller asked for.
func TestStream_IncludeUsageSynthesizedWhenUpstreamIgnoresIt(t *testing.T) {
	srv, _ := captureUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, buildMockStreamingResponse("c2", "llama-3-8b", "Hi", 5, 2))
	})
	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true
	req.IncludeUsage = true
	var buf bytes.Buffer
	if _, err := p.Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	frames := sseFrames(t, buf.String())
	last := frames[len(frames)-1]
	if ch, ok := last["choices"].([]any); !ok || len(ch) != 0 {
		t.Fatalf("last frame = %v, want a usage-only chunk (choices [])", last)
	}
	if u := last["usage"].(map[string]any); u["prompt_tokens"] != float64(5) || u["completion_tokens"] != float64(2) || u["total_tokens"] != float64(7) {
		t.Errorf("usage = %v", u)
	}
	if last["id"] != "c2" {
		t.Errorf("usage chunk id = %v, want c2", last["id"])
	}
	if !strings.HasSuffix(buf.String(), "data: [DONE]\n\n") {
		t.Errorf("stream must still end with [DONE], got %q", buf.String())
	}
}

// Without include_usage the passthrough is untouched: no chunk is added.
func TestStream_NoIncludeUsageAddsNothing(t *testing.T) {
	srv, _ := captureUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, buildMockStreamingResponse("c3", "llama-3-8b", "Hi", 5, 2))
	})
	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true
	var buf bytes.Buffer
	if _, err := p.Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	if frames := sseFrames(t, buf.String()); len(frames) != 2 {
		t.Errorf("frames = %d, want 2 (content, finish+usage) as before", len(frames))
	}
}

// A JSON-answering upstream (stream flag ignored) is adapted to the same
// two-chunk ending when include_usage was asked.
func TestStream_JSONUpstreamAdaptedWithIncludeUsage(t *testing.T) {
	srv, _ := captureUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildMockCompletion("c4", "llama-3-8b", "Hello", 11, 6))
	})
	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true
	req.IncludeUsage = true
	var buf bytes.Buffer
	if _, err := p.Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	frames := sseFrames(t, buf.String())
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3 (content, finish, usage): %v", len(frames), frames)
	}
	if frames[1]["usage"] != nil {
		t.Errorf("finish frame usage = %v, want null", frames[1]["usage"])
	}
	if ch := frames[2]["choices"].([]any); len(ch) != 0 || frames[2]["usage"].(map[string]any)["total_tokens"] != float64(17) {
		t.Errorf("usage frame = %v", frames[2])
	}
}

// include_usage is never silently unanswered: an upstream that reports no
// usage and no id still gets a usage chunk (zero counts, generated id).
func TestStream_IncludeUsageAnsweredWithZeroCountsAndGeneratedID(t *testing.T) {
	srv, _ := captureUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`+"\n\n"+
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+"data: [DONE]\n\n")
	})
	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.Stream = true
	req.IncludeUsage = true
	var buf bytes.Buffer
	if _, err := p.Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	frames := sseFrames(t, buf.String())
	last := frames[len(frames)-1]
	if ch, ok := last["choices"].([]any); !ok || len(ch) != 0 {
		t.Fatalf("last frame = %v, want usage-only chunk", last)
	}
	if u := last["usage"].(map[string]any); u["total_tokens"] != float64(0) || u["prompt_tokens"] != float64(0) {
		t.Errorf("usage = %v, want zero counts", u)
	}
	if id, _ := last["id"].(string); !strings.HasPrefix(id, "chatcmpl-") {
		t.Errorf("id = %q, want a generated chatcmpl- id", last["id"])
	}
}

// Wire: MaxTokens (the handler's folded max_completion_tokens) is the
// upstream max_tokens, and stream_options is absent unless asked for.
func TestWire_MaxTokensAndStreamOptions(t *testing.T) {
	srv, bodies := captureUpstream(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, buildMockStreamingResponse("c5", "llama-3-8b", "Hi", 1, 1))
	})
	p := newTestProvider(t, srv.URL)
	for _, ask := range []bool{true, false} {
		req := testRequest()
		req.Stream = true
		req.MaxTokens = 99
		req.IncludeUsage = ask
		if _, err := p.Stream(context.Background(), req, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	if (*bodies)[0]["max_tokens"] != float64(99) {
		t.Errorf("max_tokens = %v, want 99", (*bodies)[0]["max_tokens"])
	}
	if so, _ := (*bodies)[0]["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("asked: stream_options = %v", (*bodies)[0]["stream_options"])
	}
	if _, has := (*bodies)[1]["stream_options"]; has {
		t.Error("not asked: stream_options must be absent")
	}
}
