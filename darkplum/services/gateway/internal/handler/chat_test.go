package handler

// Tests are in the same package (handler) so they can exercise unexported
// helpers directly.

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
// completeCalls/streamCalls let tests assert a provider was never dispatched
// to (e.g. for a 404/403 refusal).
type fakeKindProvider struct {
	name          string
	kind          string
	completeCalls int
	streamCalls   int
}

func (p *fakeKindProvider) Name() string { return p.name }
func (p *fakeKindProvider) Kind() string { return p.kind }

func (p *fakeKindProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p.completeCalls++
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
	p.streamCalls++
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
	name          string
	completeCalls int
}

func (p *fakeNameOnlyProvider) Name() string { return p.name }

func (p *fakeNameOnlyProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p.completeCalls++
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
// Fakes: provider registry, route table, policy store
// ---------------------------------------------------------------------------

// fakeProviders is a minimal providerLookup fake — a plain map avoids pulling
// in the concurrency machinery of provider.Registry for tests that don't need it.
type fakeProviders map[string]provider.Provider

func (f fakeProviders) Get(key string) (provider.Provider, bool) {
	p, ok := f[key]
	return p, ok
}

// staticRoutes is a routeSnapshotter fake backed by a fixed Table.
type staticRoutes struct{ table *routing.Table }

func (s staticRoutes) Snapshot() *routing.Table { return s.table }

// fakePolicyStore is a policy.Store fake. loaded=false models "never
// successfully loaded" (fail closed).
type fakePolicyStore struct {
	snap   policy.Snapshot
	loaded bool
}

func (f *fakePolicyStore) Current() (policy.Snapshot, bool) { return f.snap, f.loaded }

var _ policy.Store = (*fakePolicyStore)(nil)

// newTestHandler wires a ChatHandler from plain fakes — no network, no DB.
func newTestHandler(providers fakeProviders, routes map[string]routing.Route, snap policy.Snapshot, loaded bool) *ChatHandler {
	return NewChatHandler(providers, staticRoutes{table: routing.NewTable(routes)}, &fakePolicyStore{snap: snap, loaded: loaded})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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

func doChat(t *testing.T, h *ChatHandler, model string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = buildChatRequest(t, model)
	h.HandleChatCompletions(c)
	return w, c
}

func decodeError(t *testing.T, body []byte) openAIError {
	t.Helper()
	var e openAIError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("failed to decode error body %s: %v", body, err)
	}
	return e
}

// ---------------------------------------------------------------------------
// Request order enforcement: unknown -> disabled -> residency -> dispatch
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_UnknownModel_404AndProviderNotCalled(t *testing.T) {
	prov := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "does-not-exist")

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_found" {
		t.Errorf("expected code model_not_found, got %q", e.Error.Code)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called for an unknown model, got %d calls", prov.completeCalls)
	}
}

func TestHandleChatCompletions_DisabledModel_403AndProviderNotCalled(t *testing.T) {
	prov := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": false}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "llama-3-8b")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_enabled" {
		t.Errorf("expected code model_not_enabled, got %q", e.Error.Code)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called for a disabled model, got %d calls", prov.completeCalls)
	}
}

// TestHandleChatCompletions_DisabledModel_NotSyncedTreatedAsDisabled asserts
// that a model with a route but absent from the Enabled map entirely (not yet
// synced by the api) is refused, not defaulted to enabled.
func TestHandleChatCompletions_DisabledModel_NotSyncedTreatedAsDisabled(t *testing.T) {
	prov := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"brand-new-model": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{}, Residency: policy.ResidencyAny},
		true,
	)

	w, _ := doChat(t, h, "brand-new-model")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unsynced model, got %d", w.Code)
	}
}

func TestHandleChatCompletions_ResidencyBlocksCloudManaged(t *testing.T) {
	brk := &fakeNameOnlyProvider{name: "bedrock"}
	h := newTestHandler(
		fakeProviders{"bedrock": brk},
		map[string]routing.Route{"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}},
		policy.Snapshot{
			Enabled:   map[string]bool{"us.anthropic.claude-3-5-sonnet": true},
			Residency: policy.ResidencySelfHostedOnly,
		},
		true,
	)

	w, _ := doChat(t, h, "us.anthropic.claude-3-5-sonnet")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_residency_violation" {
		t.Errorf("expected code model_residency_violation, got %q", e.Error.Code)
	}
	if brk.completeCalls != 0 {
		t.Errorf("bedrock must not be called when residency policy blocks it, got %d calls", brk.completeCalls)
	}
}

func TestHandleChatCompletions_ResidencyAllowsSelfHosted(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"llama-3-8b": true},
			Residency: policy.ResidencySelfHostedOnly,
		},
		true,
	)

	w, _ := doChat(t, h, "llama-3-8b")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a self-hosted model under self_hosted_only residency, got %d; body=%s", w.Code, w.Body.String())
	}
	if vllm.completeCalls != 1 {
		t.Errorf("expected provider to be called exactly once, got %d", vllm.completeCalls)
	}
}

// ---------------------------------------------------------------------------
// Alias resolution
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_AliasResolvesAndSetsCtxKeys(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"llama-3-8b": true},
			Aliases:   map[string]string{"prod-default": "llama-3-8b"},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	w, c := doChat(t, h, "prod-default")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	resolved, _ := c.Get("resolved_model")
	requested, _ := c.Get("requested_model")
	if resolved != "llama-3-8b" {
		t.Errorf("resolved_model = %v, want llama-3-8b", resolved)
	}
	if requested != "prod-default" {
		t.Errorf("requested_model = %v, want prod-default", requested)
	}
}

// TestHandleChatCompletions_NoAliasLeavesRequestedModelEmpty asserts that
// requested_model is only set (non-empty) when an alias was actually used.
func TestHandleChatCompletions_NoAliasLeavesRequestedModelEmpty(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, c := doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	requested, _ := c.Get("requested_model")
	if requested != "" {
		t.Errorf("requested_model should be empty when no alias was used, got %v", requested)
	}
}

// TestHandleChatCompletions_AliasWinsOverSameNamedRoute asserts that when a
// discovered model id happens to collide with an alias name, the alias always
// wins — admin intent is authoritative.
func TestHandleChatCompletions_AliasWinsOverSameNamedRoute(t *testing.T) {
	vllm := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": vllm},
		map[string]routing.Route{
			"claude":        {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"claude-target": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"claude": true, "claude-target": true},
			Aliases:   map[string]string{"claude": "claude-target"},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	w, c := doChat(t, h, "claude")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	resolved, _ := c.Get("resolved_model")
	if resolved != "claude-target" {
		t.Errorf("expected alias to win over the same-named route, resolved_model = %v, want claude-target", resolved)
	}
}

func TestHandleChatCompletions_AliasToUnknownTargetRefused404(t *testing.T) {
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{}, // ghost-model was never discovered
		policy.Snapshot{
			Enabled:   map[string]bool{},
			Aliases:   map[string]string{"broken-alias": "ghost-model"},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	w, _ := doChat(t, h, "broken-alias")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an alias pointing at an unknown model, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_found" {
		t.Errorf("expected code model_not_found, got %q", e.Error.Code)
	}
}

func TestHandleChatCompletions_AliasToDisabledTargetRefused403(t *testing.T) {
	prov := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"model-x": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{
			Enabled:   map[string]bool{"model-x": false},
			Aliases:   map[string]string{"disabled-alias": "model-x"},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	w, _ := doChat(t, h, "disabled-alias")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an alias pointing at a disabled model, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "model_not_enabled" {
		t.Errorf("expected code model_not_enabled, got %q", e.Error.Code)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called for a disabled alias target, got %d calls", prov.completeCalls)
	}
}

// ---------------------------------------------------------------------------
// Policy fail-closed
// ---------------------------------------------------------------------------

func TestHandleChatCompletions_PolicyNeverLoaded_503(t *testing.T) {
	prov := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	h := newTestHandler(
		fakeProviders{"vllm-default": prov},
		map[string]routing.Route{"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{},
		false, // never loaded
	)

	w, _ := doChat(t, h, "llama-3-8b")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "policy_unavailable" {
		t.Errorf("expected code policy_unavailable, got %q", e.Error.Code)
	}
	if prov.completeCalls != 0 {
		t.Errorf("provider must not be called when policy has never loaded, got %d calls", prov.completeCalls)
	}
}

// ---------------------------------------------------------------------------
// Audit kind dispatch — regression guard (unchanged from the pre-routing
// implementation, adapted to the new NewChatHandler wiring)
// ---------------------------------------------------------------------------

func TestAuditKind_KindProviderUsesKind(t *testing.T) {
	lmsProv := &fakeKindProvider{name: "lmstudio-dev", kind: "lmstudio"}
	h := newTestHandler(
		fakeProviders{"lmstudio-dev": lmsProv},
		map[string]routing.Route{"mistral-7b-instruct": {ProviderKey: "lmstudio-dev", Kind: "lmstudio", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"mistral-7b-instruct": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, c := doChat(t, h, "mistral-7b-instruct")
	if w.Code != http.StatusOK {
		t.Fatalf("HandleChatCompletions returned %d, want 200; body: %s", w.Code, w.Body.String())
	}

	providerName, exists := c.Get("provider_name")
	if !exists {
		t.Fatal("provider_name not set in gin context — handler did not reach Kind() dispatch")
	}
	if providerName != "lmstudio" {
		t.Errorf("provider_name = %q, want %q (the kind, not the instance name)", providerName, "lmstudio")
	}
}

func TestAuditKind_NameOnlyProviderFallsBackToName(t *testing.T) {
	bedrockLike := &fakeNameOnlyProvider{name: "bedrock"}
	h := newTestHandler(
		fakeProviders{"bedrock": bedrockLike},
		map[string]routing.Route{"claude-haiku": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged}},
		policy.Snapshot{Enabled: map[string]bool{"claude-haiku": true}, Residency: policy.ResidencyAny},
		true,
	)

	w, c := doChat(t, h, "claude-haiku")
	if w.Code != http.StatusOK {
		t.Fatalf("HandleChatCompletions returned %d, want 200; body: %s", w.Code, w.Body.String())
	}

	providerName, exists := c.Get("provider_name")
	if !exists {
		t.Fatal("provider_name not set in gin context")
	}
	if providerName != "bedrock" {
		t.Errorf("provider_name = %q, want %q (Name() fallback)", providerName, "bedrock")
	}
}

// ---------------------------------------------------------------------------
// HandleListModels
// ---------------------------------------------------------------------------

func TestHandleListModels_IncludesBackendClassAndOwnedBy(t *testing.T) {
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{
			"llama-3-8b":                     {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"us.anthropic.claude-3-5-sonnet": {ProviderKey: "bedrock", Kind: "bedrock", Class: routing.ClassCloudManaged},
		},
		policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
		true,
	)

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
			ID           string `json:"id"`
			OwnedBy      string `json:"owned_by"`
			BackendClass string `json:"backend_class"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(body.Data))
	}

	byID := make(map[string]struct {
		OwnedBy      string
		BackendClass string
	})
	for _, m := range body.Data {
		byID[m.ID] = struct {
			OwnedBy      string
			BackendClass string
		}{m.OwnedBy, m.BackendClass}
	}

	if got := byID["llama-3-8b"]; got.OwnedBy != "vllm" || got.BackendClass != routing.ClassSelfHosted {
		t.Errorf("llama-3-8b: got %+v", got)
	}
	if got := byID["us.anthropic.claude-3-5-sonnet"]; got.OwnedBy != "bedrock" || got.BackendClass != routing.ClassCloudManaged {
		t.Errorf("us.anthropic.claude-3-5-sonnet: got %+v", got)
	}
}

// TestHandleListModels_IncludesDisabledModels asserts /v1/models returns
// every discovered model regardless of enabled status (the api needs the full
// set for sync).
func TestHandleListModels_IncludesDisabledModels(t *testing.T) {
	h := newTestHandler(
		fakeProviders{},
		map[string]routing.Route{"not-yet-enabled": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{}, Residency: policy.ResidencyAny},
		true,
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/v1/models", nil)
	h.HandleListModels(c)

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Data) != 1 || body.Data[0].ID != "not-yet-enabled" {
		t.Errorf("expected the disabled model to still be listed, got %+v", body.Data)
	}
}
