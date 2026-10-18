package handler

// Tests for handleProviderError/writeStreamError's scrubbing contract: a
// provider.APIError's structured status/body/retry-after is relayed
// verbatim, and NOTHING else ever echoes a raw error string to the client,
// on either the JSON (non-streaming) or SSE (streaming) response path.

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

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// fakeAPIError implements provider.APIError. Its Error() string deliberately
// carries text that must never reach a client response, so a test can assert
// the scrubbing contract by searching for that text in the response body.
type fakeAPIError struct {
	status     int
	body       []byte
	retryAfter int
}

func (e *fakeAPIError) Error() string {
	return "RAW-UPSTREAM-DETAIL-vendor-internal-host-account-id-9999"
}
func (e *fakeAPIError) HTTPStatus() int        { return e.status }
func (e *fakeAPIError) ResponseBody() []byte   { return e.body }
func (e *fakeAPIError) RetryAfterSeconds() int { return e.retryAfter }

var _ provider.APIError = (*fakeAPIError)(nil)

// fakeTimeoutErr is a plain (non-APIError) error whose dynamic type
// satisfies net.Error with Timeout()==true, exactly like
// context.DeadlineExceeded or an *http.Client dial timeout — the shape
// genericUpstreamError classifies as upstream_timeout.
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout talking to internal-upstream-host:9999" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

// fakeErrorProvider is a provider.Provider whose Complete/Stream return a
// scripted error, optionally after writing streamPreWrite bytes to w first
// (simulating a partial stream committed before the upstream failed).
type fakeErrorProvider struct {
	name           string
	kind           string
	completeErr    error
	streamErr      error
	streamPreWrite string
}

func (p *fakeErrorProvider) Name() string { return p.name }
func (p *fakeErrorProvider) Kind() string { return p.kind }

func (p *fakeErrorProvider) Complete(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, p.completeErr
}

func (p *fakeErrorProvider) Stream(_ context.Context, _ provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	if p.streamPreWrite != "" {
		_, _ = io.WriteString(w, p.streamPreWrite)
	}
	return nil, p.streamErr
}

var _ provider.Provider = (*fakeErrorProvider)(nil)

func buildStreamChatRequest(t *testing.T, model string) *http.Request {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("buildStreamChatRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func doStreamChat(t *testing.T, h *ChatHandler, model string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildStreamChatRequest(t, model)
	h.HandleChatCompletions(c)
	return w
}

// ---------------------------------------------------------------------------
// Non-streaming (JSON) path
// ---------------------------------------------------------------------------

// TestHandleChatCompletions_VendorRateLimitPassesThroughWithRetryAfter is the
// wire contract's 429 passthrough: a provider.APIError reporting a rate
// limit is relayed with its own status, body, and Retry-After header — not
// re-wrapped or re-worded.
func TestHandleChatCompletions_VendorRateLimitPassesThroughWithRetryAfter(t *testing.T) {
	rateLimitBody := []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`)
	prov := &fakeErrorProvider{
		name: "openai-default", kind: "openai",
		completeErr: &fakeAPIError{status: http.StatusTooManyRequests, body: rateLimitBody, retryAfter: 7},
	}
	h := newTestHandler(
		fakeProviders{"openai-default": prov},
		map[string]routing.Route{"gpt-4o": {ProviderKey: "openai-default", Kind: "openai", Class: routing.ClassExternalAPI}},
		policy.Snapshot{Enabled: map[string]bool{"gpt-4o": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "gpt-4o")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want 7", got)
	}
	if !bytes.Equal(w.Body.Bytes(), rateLimitBody) {
		t.Errorf("body = %s, want the provider's own scrubbed body verbatim: %s", w.Body.Bytes(), rateLimitBody)
	}
}

// TestHandleChatCompletions_APIErrorWithNoRetryAfterOmitsHeader asserts a
// zero RetryAfterSeconds (Bedrock's case today) never sets the header at
// all, rather than sending "Retry-After: 0".
func TestHandleChatCompletions_APIErrorWithNoRetryAfterOmitsHeader(t *testing.T) {
	body := []byte(`{"error":{"message":"Access denied","type":"permission_error"}}`)
	prov := &fakeErrorProvider{
		name: "bedrock", kind: "bedrock",
		completeErr: &fakeAPIError{status: http.StatusForbidden, body: body, retryAfter: 0},
	}
	h := newTestHandler(
		fakeProviders{"bedrock": prov},
		map[string]routing.Route{"claude": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}},
		policy.Snapshot{Enabled: map[string]bool{"claude": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "claude")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want no header", got)
	}
}

// TestHandleChatCompletions_GenericProviderErrorNeverLeaksRawText covers a
// plain (non-APIError) error from a backend like openaicompat, which wraps
// dial/connect failures with the upstream URL embedded in the error string
// — that text must never reach the client.
func TestHandleChatCompletions_GenericProviderErrorNeverLeaksRawText(t *testing.T) {
	prov := &fakeErrorProvider{
		name: "vllm-default", kind: "vllm",
		completeErr: fmt.Errorf("openaicompat(vllm-default): upstream request failed: dial tcp internal-hostname-should-not-leak:8000: connect: connection refused"),
	}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "internal-hostname-should-not-leak") {
		t.Errorf("raw upstream error text leaked into the response body: %s", w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "upstream_error" {
		t.Errorf("code = %q, want upstream_error", e.Error.Code)
	}
}

// TestHandleChatCompletions_TimeoutErrorMapsTo504 asserts a net.Error with
// Timeout()==true (what a dial/response timeout actually looks like) maps to
// 504 upstream_timeout, not the generic 502.
func TestHandleChatCompletions_TimeoutErrorMapsTo504(t *testing.T) {
	prov := &fakeErrorProvider{name: "vllm-default", kind: "vllm", completeErr: fakeTimeoutErr{}}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "internal-upstream-host") {
		t.Errorf("raw upstream error text leaked into the response body: %s", w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "upstream_timeout" {
		t.Errorf("code = %q, want upstream_timeout", e.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// Streaming (SSE) path
// ---------------------------------------------------------------------------

// TestHandleChatCompletions_StreamErrorBeforeAnyWriteServesCleanJSON asserts
// that when the provider fails before writing any SSE bytes, the handler
// still serves a clean JSON error (headers were never committed to the
// wire) — and it is scrubbed exactly like the non-streaming path.
func TestHandleChatCompletions_StreamErrorBeforeAnyWriteServesCleanJSON(t *testing.T) {
	prov := &fakeErrorProvider{name: "vllm-default", kind: "vllm", streamErr: fakeTimeoutErr{}}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w := doStreamChat(t, h, "llama-3-8b")
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (clean JSON error, not a corrupted SSE stream)", got)
	}
	if strings.Contains(w.Body.String(), "internal-upstream-host") {
		t.Errorf("raw upstream error text leaked into the response body: %s", w.Body.String())
	}
}

// TestHandleChatCompletions_StreamErrorAfterPartialWriteEmitsScrubbedSSEFrame
// asserts a mid-stream failure (after real SSE bytes were already flushed)
// appends a scrubbed "data: ..." error frame rather than a raw JSON body
// (which would corrupt the stream) or the upstream's own error text.
func TestHandleChatCompletions_StreamErrorAfterPartialWriteEmitsScrubbedSSEFrame(t *testing.T) {
	prov := &fakeErrorProvider{
		name: "vllm-default", kind: "vllm",
		streamPreWrite: "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		streamErr:      fmt.Errorf("openaicompat(vllm-default): reading stream: connection reset by peer at internal-upstream-host:9999"),
	}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w := doStreamChat(t, h, "llama-3-8b")

	body := w.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("expected the partial content already streamed to survive, got body=%s", body)
	}
	if strings.Contains(body, "internal-upstream-host") {
		t.Errorf("raw upstream error text leaked into the SSE stream: %s", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) {
		t.Errorf("expected a scrubbed upstream_error SSE frame, got body=%s", body)
	}
}
