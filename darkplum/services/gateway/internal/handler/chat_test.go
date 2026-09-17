package handler

// Tests are in the same package (handler) so they can exercise unexported
// helpers like resolveProviderKey without exposing them to the public API.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

func init() {
	// Silence gin's debug output in tests.
	gin.SetMode(gin.TestMode)
}

// ---------------------------------------------------------------------------
// Fake providers
// ---------------------------------------------------------------------------

// fakeKindProvider implements provider.Provider and exposes a Kind() method.
// This is the shape of openaicompat.OpenAICompatProvider — it has both Name()
// and Kind(), where Name() is the instance name and Kind() is the backend type.
type fakeKindProvider struct {
	name string
	kind string
}

func (p *fakeKindProvider) Name() string { return p.name }
func (p *fakeKindProvider) Kind() string { return p.kind }

func (p *fakeKindProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{
		ID:               "chatcmpl-fake-1",
		Model:            req.Model,
		Content:          "fake response",
		PromptTokens:     5,
		CompletionTokens: 10,
		FinishReason:     "stop",
	}, nil
}

func (p *fakeKindProvider) Stream(_ context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	chunk, _ := provider.BuildStreamingChunkSSE("chatcmpl-fake-1", req.Model, "fake")
	_, _ = w.Write(chunk)
	stats := &provider.StreamStats{Model: req.Model, FinishReason: "stop"}
	final, _ := provider.BuildFinalChunkSSE("chatcmpl-fake-1", req.Model, "stop", stats)
	_, _ = w.Write(final)
	_, _ = io.WriteString(w, provider.StreamDoneSSE)
	return stats, nil
}

// fakeNameOnlyProvider implements provider.Provider without Kind().
// This covers the fallback branch (bedrock, mock) where Name() already returns
// the kind string and there is no Kind() method.
type fakeNameOnlyProvider struct {
	name string
}

func (p *fakeNameOnlyProvider) Name() string { return p.name }

func (p *fakeNameOnlyProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{
		ID:               "chatcmpl-name-only-1",
		Model:            req.Model,
		Content:          "name-only response",
		PromptTokens:     3,
		CompletionTokens: 7,
		FinishReason:     "stop",
	}, nil
}

func (p *fakeNameOnlyProvider) Stream(_ context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	stats := &provider.StreamStats{Model: req.Model, FinishReason: "stop"}
	return stats, nil
}

// compile-time interface checks.
var _ provider.Provider = (*fakeKindProvider)(nil)
var _ provider.Provider = (*fakeNameOnlyProvider)(nil)

// ---------------------------------------------------------------------------
// resolveProviderKey — routing precedence
// ---------------------------------------------------------------------------

// TestResolveProviderKey_BedrockWins asserts that a model present in the
// bedrockModels set resolves to "bedrock" regardless of any modelRoute entry.
func TestResolveProviderKey_BedrockWins(t *testing.T) {
	defaultProv := &fakeNameOnlyProvider{name: "vllm-default"}
	lmsProv := &fakeKindProvider{name: "lmstudio-dev", kind: "lmstudio"}

	bedrockModels := map[string]bool{"us.anthropic.claude-3-5-sonnet-20241022-v2:0": true}
	modelRoute := map[string]string{"mistral-7b": "lmstudio-dev"}

	h := NewChatHandler(defaultProv, nil, bedrockModels, []provider.Provider{lmsProv}, modelRoute)

	got := h.resolveProviderKey("us.anthropic.claude-3-5-sonnet-20241022-v2:0")
	if got != "bedrock" {
		t.Errorf("resolveProviderKey: bedrock model should resolve to \"bedrock\", got %q", got)
	}
}

// TestResolveProviderKey_ModelRouteWins asserts that a model in modelRoute
// (but not bedrockModels) resolves to its registered instance name.
func TestResolveProviderKey_ModelRouteWins(t *testing.T) {
	defaultProv := &fakeNameOnlyProvider{name: "vllm-default"}
	lmsProv := &fakeKindProvider{name: "lmstudio-dev", kind: "lmstudio"}

	bedrockModels := map[string]bool{"us.anthropic.claude-3-5-sonnet-20241022-v2:0": true}
	modelRoute := map[string]string{"mistral-7b-instruct": "lmstudio-dev"}

	h := NewChatHandler(defaultProv, nil, bedrockModels, []provider.Provider{lmsProv}, modelRoute)

	got := h.resolveProviderKey("mistral-7b-instruct")
	if got != "lmstudio-dev" {
		t.Errorf("resolveProviderKey: lmstudio model should resolve to \"lmstudio-dev\", got %q", got)
	}
}

// TestResolveProviderKey_DefaultFallback asserts that a model with no
// bedrockModels or modelRoute entry falls through to the default provider key.
func TestResolveProviderKey_DefaultFallback(t *testing.T) {
	defaultProv := &fakeNameOnlyProvider{name: "vllm-default"}
	lmsProv := &fakeKindProvider{name: "lmstudio-dev", kind: "lmstudio"}

	bedrockModels := map[string]bool{"us.anthropic.claude-3-5-sonnet-20241022-v2:0": true}
	modelRoute := map[string]string{"mistral-7b-instruct": "lmstudio-dev"}

	h := NewChatHandler(defaultProv, nil, bedrockModels, []provider.Provider{lmsProv}, modelRoute)

	got := h.resolveProviderKey("llama-3-8b-unknown")
	if got != "vllm-default" {
		t.Errorf("resolveProviderKey: unknown model should fall back to \"vllm-default\", got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Audit kind dispatch — THE regression guard
//
// These tests exercise the actual lines in HandleChatCompletions that set
// provider_name in the gin context. They use gin.CreateTestContext so the
// real chat.go code runs, not a copy of it.
// ---------------------------------------------------------------------------

// buildChatRequest serialises a chat request body for use in handler tests.
func buildChatRequest(t *testing.T, model string) *http.Request {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestAuditKind_KindProviderUsesKind is the primary regression guard.
//
// It asserts that when a provider implements Kind(), the value written to the
// gin context key "provider_name" is the KIND ("lmstudio"), not the instance
// name ("lmstudio-dev").
//
// If someone regresses chat.go to `c.Set("provider_name", prov.Name())` this
// test will fail because it would see "lmstudio-dev" instead of "lmstudio".
func TestAuditKind_KindProviderUsesKind(t *testing.T) {
	lmsProv := &fakeKindProvider{name: "lmstudio-dev", kind: "lmstudio"}
	defaultProv := &fakeNameOnlyProvider{name: "vllm-default"}

	modelRoute := map[string]string{"mistral-7b-instruct": "lmstudio-dev"}
	h := NewChatHandler(defaultProv, nil, nil, []provider.Provider{lmsProv}, modelRoute)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildChatRequest(t, "mistral-7b-instruct")

	h.HandleChatCompletions(c)

	// The handler must have completed without an HTTP error.
	if w.Code != http.StatusOK {
		t.Fatalf("HandleChatCompletions returned %d, want 200; body: %s", w.Code, w.Body.String())
	}

	// The critical assertion: provider_name must be the KIND, not the instance name.
	providerName, exists := c.Get("provider_name")
	if !exists {
		t.Fatal("provider_name not set in gin context — handler did not reach Kind() dispatch")
	}
	if providerName != "lmstudio" {
		t.Errorf("provider_name = %q, want %q (the kind, not the instance name)", providerName, "lmstudio")
	}
}

// TestAuditKind_NameOnlyProviderFallsBackToName asserts that a provider without
// Kind() (e.g. the bedrock or mock provider) falls back to Name() for
// provider_name. This documents the intended dual-path behavior.
func TestAuditKind_NameOnlyProviderFallsBackToName(t *testing.T) {
	// bedrockLike is a name-only provider whose Name() already returns its kind.
	bedrockLike := &fakeNameOnlyProvider{name: "bedrock"}

	// Use bedrockLike as the default provider; also add it to the provider map
	// via bedrockProv so it's reachable as "bedrock".
	// To exercise the name-only fallback path, route a model directly to it
	// through modelRoute — this avoids needing a real bedrockModels set.
	modelRoute := map[string]string{"claude-haiku": "bedrock"}
	h := NewChatHandler(bedrockLike, nil, nil, nil, modelRoute)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildChatRequest(t, "claude-haiku")

	h.HandleChatCompletions(c)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleChatCompletions returned %d, want 200; body: %s", w.Code, w.Body.String())
	}

	providerName, exists := c.Get("provider_name")
	if !exists {
		t.Fatal("provider_name not set in gin context")
	}
	// For a name-only provider, Name() is the expected fallback value.
	if providerName != "bedrock" {
		t.Errorf("provider_name = %q, want %q (Name() fallback)", providerName, "bedrock")
	}
}

// ---------------------------------------------------------------------------
// HandleListModels — owned_by kind dispatch
// ---------------------------------------------------------------------------

// TestHandleListModels_OwnedByIsKind asserts that for a provider implementing
// Kind(), the owned_by field in the /v1/models response is the KIND ("lmstudio"),
// not the instance name ("lmstudio-dev").
func TestHandleListModels_OwnedByIsKind(t *testing.T) {
	lmsProv := &fakeKindProvider{name: "lmstudio-dev", kind: "lmstudio"}
	defaultProv := &fakeNameOnlyProvider{name: "vllm-default"}

	modelRoute := map[string]string{
		"mistral-7b-instruct": "lmstudio-dev",
		"codestral-22b":       "lmstudio-dev",
	}
	h := NewChatHandler(defaultProv, nil, nil, []provider.Provider{lmsProv}, modelRoute)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Request = req

	h.HandleListModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleListModels returned %d, want 200", w.Code)
	}

	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(body.Data) == 0 {
		t.Fatal("expected at least one model in response")
	}

	for _, m := range body.Data {
		if m.OwnedBy != "lmstudio" {
			t.Errorf("model %q: owned_by = %q, want %q (kind, not instance name)", m.ID, m.OwnedBy, "lmstudio")
		}
	}
}

// TestHandleListModels_BedrockOwnedByBedrock asserts that Bedrock models in
// the response always report owned_by = "bedrock".
func TestHandleListModels_BedrockOwnedByBedrock(t *testing.T) {
	defaultProv := &fakeNameOnlyProvider{name: "vllm-default"}

	bedrockModels := map[string]bool{
		"us.anthropic.claude-3-5-sonnet-20241022-v2:0": true,
		"us.anthropic.claude-3-haiku-20240307-v1:0":    true,
	}
	h := NewChatHandler(defaultProv, nil, bedrockModels, nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Request = req

	h.HandleListModels(c)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleListModels returned %d, want 200", w.Code)
	}

	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(body.Data) != 2 {
		t.Fatalf("expected 2 bedrock models, got %d", len(body.Data))
	}

	for _, m := range body.Data {
		if m.OwnedBy != "bedrock" {
			t.Errorf("bedrock model %q: owned_by = %q, want \"bedrock\"", m.ID, m.OwnedBy)
		}
	}
}
