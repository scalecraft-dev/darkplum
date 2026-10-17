package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/handler"
	"github.com/scalecraft/darkplum/gateway/internal/health"
	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/router"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ---------------------------------------------------------------------------
// Fakes — mirrors internal/handler's own test fakes, duplicated here because
// ChatHandler's constructor parameters are unexported interfaces (still
// structurally satisfiable from another package).
// ---------------------------------------------------------------------------

type fakeProviders map[string]provider.Provider

func (f fakeProviders) Get(key string) (provider.Provider, bool) {
	p, ok := f[key]
	return p, ok
}

type staticRoutes struct{ table *routing.Table }

func (s staticRoutes) Snapshot() *routing.Table { return s.table }

type fakePolicyStore struct {
	snap   policy.Snapshot
	loaded bool
}

func (f fakePolicyStore) Current() (policy.Snapshot, bool) { return f.snap, f.loaded }

type fakeChatProvider struct{}

func (fakeChatProvider) Name() string { return "vllm" }
func (fakeChatProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{ID: "chatcmpl-1", Model: req.Model, Content: "hi", FinishReason: "stop"}, nil
}
func (fakeChatProvider) Stream(_ context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	return &provider.StreamStats{Model: req.Model, FinishReason: "stop"}, nil
}

// noopMiddleware lets tests exercise the real route table without a database
// — auth/audit behaviour has its own tests in internal/middleware.
func noopMiddleware(c *gin.Context) { c.Next() }

func stubHealthHandler(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }

func testChatHandler() *handler.ChatHandler {
	rules := router.NewRulesClassifier()
	return handler.NewChatHandler(
		fakeProviders{"vllm-default": fakeChatProvider{}},
		staticRoutes{table: routing.NewTable(map[string]routing.Route{
			"llama-3-8b": {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
		})},
		fakePolicyStore{
			snap:   policy.Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: policy.ResidencyAny},
			loaded: true,
		},
		rules, rules,
		provider.NewStatusRegistry("anthropic", "openai"),
	)
}

// fakeVerifier is a vendorKeyVerifier-shaped fake (structurally, since
// vendorKeyVerifier is unexported to internal/handler) letting buildRouter's
// route-table tests exercise POST /v1/providers/:vendor/verify|refresh
// without a real vendor client.
type fakeVerifier struct {
	verifyIDs []string
	verifyErr error
	discIDs   map[string]bool
	discErr   error
}

func (f fakeVerifier) VerifyKey(_ context.Context, _, _ string) ([]string, error) {
	return f.verifyIDs, f.verifyErr
}
func (f fakeVerifier) DiscoverModels(_ context.Context) (map[string]bool, error) {
	return f.discIDs, f.discErr
}

type fakeVendorCreds struct{ err error }

func (f fakeVendorCreds) Key() (string, uint64, error) { return "sk-x", 1, f.err }

func testProvidersHandler() *handler.ProvidersHandler {
	verifier := fakeVerifier{verifyIDs: []string{"claude-x"}, discIDs: map[string]bool{"claude-x": true}}
	return handler.NewProvidersHandler(context.Background(), routing.NewManager(nil, 0), map[string]handler.ProviderVendorConfig{
		"anthropic": {Verifier: verifier, Creds: fakeVendorCreds{}},
		"openai":    {Verifier: verifier, Creds: fakeVendorCreds{}},
	})
}

// ---------------------------------------------------------------------------
// The two live routes
// ---------------------------------------------------------------------------

func TestBuildRouter_ListModels(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestBuildRouter_ChatCompletions(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	body, _ := json.Marshal(map[string]any{
		"model":    "llama-3-8b",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions: expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestBuildRouter_RoutingDryrun asserts POST /v1/routing/dryrun is
// registered (authenticated, not proxied) rather than falling through to the
// unknown-endpoint 404 — it must 400 for an alias that isn't routed, not 404
// for being an unrecognised path.
func TestBuildRouter_RoutingDryrun(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	body, _ := json.Marshal(map[string]any{"alias": "llama-3-8b", "text": "hello"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/routing/dryrun", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// llama-3-8b is a plain model id, not a routed alias — HandleRoutingDryrun
	// itself returns 400, but the important assertion here is that the route
	// exists at all (a 404 would mean it fell through to the NoRoute handler).
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (not a routed alias), got %d; body=%s", w.Code, w.Body.String())
	}
}

// TestBuildRouter_PolicyEffective asserts GET /v1/policy/effective is
// registered (authenticated, not proxied, not in the recorded group) rather
// than falling through to the unknown-endpoint 404. testChatHandler's fake
// policy store has no principal-policy data at all, so a service-key-shaped
// request (noopMiddleware never sets ContextKeyType, leaving it "" — not
// KeyTypeService) exercises the 403 branch here; the 200 shape itself is
// covered by internal/handler's own tests.
func TestBuildRouter_PolicyEffective(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/policy/effective?user_id=11111111-1111-1111-1111-111111111111", nil)
	router.ServeHTTP(w, req)

	// noopMiddleware never authenticates, so ContextKeyType is "" here — not
	// the service key — which HandleEffectivePolicy correctly refuses. The
	// important assertion is that the route exists at all: a 404 would mean
	// it fell through to the NoRoute handler instead of reaching the handler.
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (unauthenticated in this test harness, but the route must exist), got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestBuildRouter_Health(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /health: expected 200, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Item 5: no reverse-proxy passthrough — every other /v1/* path and method
// gets a clean OpenAI-envelope 404, never a proxied request to any backend.
// ---------------------------------------------------------------------------

type openAIErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func TestBuildRouter_UnknownV1Endpoints_404WithOpenAIEnvelope(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"GET embeddings (never registered)", http.MethodGet, "/v1/embeddings"},
		{"POST embeddings (previously gin bare 404)", http.MethodPost, "/v1/embeddings"},
		{"POST completions (previously gin bare 404)", http.MethodPost, "/v1/completions"},
		{"GET models by id (previous proxy leaked this)", http.MethodGet, "/v1/models/gpt-4"},
		{"DELETE models (previously proxied with no policy check)", http.MethodDelete, "/v1/models"},
		{"PUT models (previously proxied with no policy check)", http.MethodPut, "/v1/models"},
		{"PATCH models (previously proxied with no policy check)", http.MethodPatch, "/v1/models"},
		{"GET chat completions (wrong method)", http.MethodGet, "/v1/chat/completions"},
		{"DELETE chat completions (previously proxied)", http.MethodDelete, "/v1/chat/completions"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			router.ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Fatalf("%s %s: expected 404, got %d; body=%s", tc.method, tc.path, w.Code, w.Body.String())
			}
			var body openAIErrorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s %s: response body is not the OpenAI error envelope: %s", tc.method, tc.path, w.Body.String())
			}
			if body.Error.Code != "unknown_endpoint" {
				t.Errorf("%s %s: error.code = %q, want unknown_endpoint", tc.method, tc.path, body.Error.Code)
			}
			if body.Error.Type != "invalid_request_error" {
				t.Errorf("%s %s: error.type = %q, want invalid_request_error", tc.method, tc.path, body.Error.Type)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Item 8: /health surfaces routing and policy state (model counts only, no
// model ids or secrets).
// ---------------------------------------------------------------------------

func TestNewHealthHandler_IncludesRoutingAndPolicyStatus(t *testing.T) {
	src := routing.Source{
		Name: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted, ProviderKey: "vllm-default",
		Discover: func(_ context.Context) ([]string, error) {
			return []string{"llama-3-8b", "mistral-7b"}, nil
		},
	}
	// A failing source whose error — like a real *url.Error from a dial
	// failure — embeds a scheme and hostname, so this test can assert that
	// text never reaches the unauthenticated /health response.
	failingSrc := routing.Source{
		Name: "lmstudio-dev", Kind: "lmstudio", Class: routing.ClassSelfHosted, ProviderKey: "lmstudio-dev",
		Discover: func(_ context.Context) ([]string, error) {
			return nil, &url.Error{
				Op:  "Get",
				URL: "http://lmstudio.internal.example.com:1234/v1/models",
				Err: errors.New("dial tcp 10.0.0.5:1234: connect: connection refused"),
			}
		},
	}
	routeMgr := routing.NewManager([]routing.Source{src, failingSrc}, 0)
	routeMgr.Refresh(context.Background())

	// A PgStore that has never loaded (no DB call is ever made — Status()
	// only reads in-memory state) exercises the policy.loaded=false branch.
	polStore := policy.NewPgStore(nil, 900*time.Second)

	upstreamChecker := health.NewChecker(nil)
	semanticClassifier := router.NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second)
	h := newHealthHandler(upstreamChecker, routeMgr, polStore, semanticClassifier, nil, nil)

	router := gin.New()
	router.GET("/health", h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected /health to always return 200, got %d", w.Code)
	}

	var body struct {
		Status  string `json:"status"`
		Routing struct {
			Models  int `json:"models"`
			Sources map[string]struct {
				Models                int     `json:"models"`
				LastSuccessSecondsAgo *int64  `json:"last_success_seconds_ago"`
				LastErrorKind         *string `json:"last_error_kind"`
			} `json:"sources"`
			Classifier struct {
				SemanticRouter struct {
					Status              string `json:"status"`
					BreakerOpen         bool   `json:"breaker_open"`
					ConsecutiveFailures int    `json:"consecutive_failures"`
				} `json:"semantic_router"`
			} `json:"classifier"`
		} `json:"routing"`
		Policy struct {
			Loaded     bool   `json:"loaded"`
			AgeSeconds *int64 `json:"age_seconds"`
			Stale      bool   `json:"stale"`
			Principals struct {
				Users   int `json:"users"`
				APIKeys int `json:"api_keys"`
			} `json:"principals"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode /health body: %v; body=%s", err, w.Body.String())
	}

	if body.Routing.Models != 2 {
		t.Errorf("routing.models = %d, want 2", body.Routing.Models)
	}
	src1, ok := body.Routing.Sources["vllm-default"]
	if !ok {
		t.Fatal("expected vllm-default under routing.sources")
	}
	if src1.Models != 2 {
		t.Errorf("routing.sources.vllm-default.models = %d, want 2 (model COUNT, not the ids)", src1.Models)
	}
	if src1.LastSuccessSecondsAgo == nil {
		t.Error("expected routing.sources.vllm-default.last_success_seconds_ago to be set after a successful refresh")
	}
	if src1.LastErrorKind != nil {
		t.Errorf("routing.sources.vllm-default.last_error_kind = %v, want null", src1.LastErrorKind)
	}

	failingStatus, ok := body.Routing.Sources["lmstudio-dev"]
	if !ok {
		t.Fatal("expected lmstudio-dev under routing.sources")
	}
	if failingStatus.LastErrorKind == nil || *failingStatus.LastErrorKind != "unreachable" {
		t.Errorf("routing.sources.lmstudio-dev.last_error_kind = %v, want \"unreachable\"", failingStatus.LastErrorKind)
	}

	if body.Routing.Classifier.SemanticRouter.Status != "unconfigured" {
		t.Errorf("routing.classifier.semantic_router.status = %q, want unconfigured (SEMANTIC_ROUTER_URL unset)", body.Routing.Classifier.SemanticRouter.Status)
	}
	if body.Routing.Classifier.SemanticRouter.BreakerOpen {
		t.Error("expected routing.classifier.semantic_router.breaker_open=false for a fresh classifier")
	}
	if body.Routing.Classifier.SemanticRouter.ConsecutiveFailures != 0 {
		t.Errorf("routing.classifier.semantic_router.consecutive_failures = %d, want 0", body.Routing.Classifier.SemanticRouter.ConsecutiveFailures)
	}

	// The raw response must never contain the model ids themselves outside of
	// what's already exposed on the authenticated /v1/models surface.
	if bytes.Contains(w.Body.Bytes(), []byte("llama-3-8b")) || bytes.Contains(w.Body.Bytes(), []byte("mistral-7b")) {
		t.Error("unauthenticated /health must not expose model ids, only counts")
	}

	// The removed in-house model classifier must have no trace in
	// routing.classifier: only semantic_router remains.
	if bytes.Contains(w.Body.Bytes(), []byte(`"model"`)) {
		t.Errorf("routing.classifier must not contain a \"model\" entry anymore, got: %s", w.Body.String())
	}

	// Item 2 regression guard: /health must never leak upstream hostnames or
	// raw error text — only the classified last_error_kind. lmstudio-dev's
	// underlying discovery error above embeds a scheme and a hostname; none
	// of that may survive into the response body.
	for _, needle := range []string{"http://", "https://", "lmstudio.internal.example.com", "10.0.0.5", "connection refused"} {
		if bytes.Contains(w.Body.Bytes(), []byte(needle)) {
			t.Errorf("unauthenticated /health body must not contain %q, got: %s", needle, w.Body.String())
		}
	}

	if body.Policy.Loaded {
		t.Error("expected policy.loaded=false for a PgStore that has never loaded")
	}
	if body.Policy.AgeSeconds != nil {
		t.Errorf("policy.age_seconds = %v, want null when never loaded", body.Policy.AgeSeconds)
	}
	if body.Policy.Stale {
		t.Error("expected policy.stale=false when never loaded (distinct from stale)")
	}
	if body.Policy.Principals.Users != 0 || body.Policy.Principals.APIKeys != 0 {
		t.Errorf("expected policy.principals={0,0} for a PgStore that has never loaded, got %+v", body.Policy.Principals)
	}
}

// fakePolicyHealthStatus is a policyHealthStatus fake letting this test set
// an arbitrary loaded Snapshot (including PrincipalCounts) without a real
// database or the policy package's own unexported test seams.
type fakePolicyHealthStatus struct {
	status policy.Status
	snap   policy.Snapshot
	loaded bool
}

func (f fakePolicyHealthStatus) Status() policy.Status            { return f.status }
func (f fakePolicyHealthStatus) Current() (policy.Snapshot, bool) { return f.snap, f.loaded }

// TestNewHealthHandler_ReportsLoadedPrincipalCounts is the loaded-snapshot
// counterpart of the never-loaded assertion above: once a Snapshot with
// explicit user/api_key policy rows is current, /health must report their
// counts rather than always zero.
func TestNewHealthHandler_ReportsLoadedPrincipalCounts(t *testing.T) {
	fake := fakePolicyHealthStatus{
		status: policy.Status{Loaded: true},
		snap: policy.Snapshot{
			Residency:       policy.ResidencyAny,
			PrincipalCounts: policy.PrincipalCounts{Users: 3, APIKeys: 7},
		},
		loaded: true,
	}

	routeMgr := routing.NewManager(nil, 0)
	upstreamChecker := health.NewChecker(nil)
	semanticClassifier := router.NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second)
	h := newHealthHandler(upstreamChecker, routeMgr, fake, semanticClassifier, nil, nil)

	router := gin.New()
	router.GET("/health", h)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body struct {
		Policy struct {
			Principals struct {
				Users   int `json:"users"`
				APIKeys int `json:"api_keys"`
			} `json:"principals"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if body.Policy.Principals.Users != 3 || body.Policy.Principals.APIKeys != 7 {
		t.Errorf("policy.principals = %+v, want {3 7}", body.Policy.Principals)
	}
}

// TestNewHealthHandler_NilProviderStatusOmitsNothingButEmptyArray covers the
// nil providerStatus case (mirrors how every other newHealthHandler test in
// this file passes nil) — /health must still return a well-formed (empty,
// never null) providers array rather than panicking or omitting the key.
func TestNewHealthHandler_NilProviderStatusOmitsNothingButEmptyArray(t *testing.T) {
	routeMgr := routing.NewManager(nil, 0)
	upstreamChecker := health.NewChecker(nil)
	polStore := policy.NewPgStore(nil, 900*time.Second)
	semanticClassifier := router.NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second)
	h := newHealthHandler(upstreamChecker, routeMgr, polStore, semanticClassifier, nil, nil)

	router := gin.New()
	router.GET("/health", h)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Providers []gin.H `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if body.Providers == nil {
		t.Error("expected providers to be an empty array (`[]`), not null/absent, when providerStatus is nil")
	}
	if len(body.Providers) != 0 {
		t.Errorf("expected no providers, got %+v", body.Providers)
	}
}

// TestNewHealthHandler_IncludesProviders is item 4 (guilfoyle H2)'s
// regression test: /health's `providers` array must surface the SAME
// closed-vocabulary vendor status GET /v1/models already exposes to a
// service key — name, configured, state, model_count, last_sync_at, and
// NOTHING else (no model ids, no filter patterns) — and it must never affect
// the response status even when a vendor is in a failure state.
func TestNewHealthHandler_IncludesProviders(t *testing.T) {
	reg := provider.NewStatusRegistry("anthropic", "openai")
	reg.Report("anthropic", provider.ProviderStatus{Configured: true, State: provider.ProviderStateOK, ModelCount: 3})
	reg.Report("openai", provider.ProviderStatus{Configured: true, State: provider.ProviderStateAuthFailed})

	routeMgr := routing.NewManager(nil, 0)
	upstreamChecker := health.NewChecker(nil)
	polStore := policy.NewPgStore(nil, 900*time.Second)
	semanticClassifier := router.NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second)
	h := newHealthHandler(upstreamChecker, routeMgr, polStore, semanticClassifier, reg, nil)

	router := gin.New()
	router.GET("/health", h)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected /health to always return 200 regardless of vendor state, got %d", w.Code)
	}

	var body struct {
		Providers []struct {
			Name       string  `json:"name"`
			Configured bool    `json:"configured"`
			State      string  `json:"state"`
			ModelCount int     `json:"model_count"`
			LastSyncAt *string `json:"last_sync_at"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(body.Providers) != 2 {
		t.Fatalf("expected both vendors listed, got %+v", body.Providers)
	}
	byName := make(map[string]bool)
	for _, p := range body.Providers {
		byName[p.Name] = true
		switch p.Name {
		case "anthropic":
			if p.State != "ok" || p.ModelCount != 3 || !p.Configured {
				t.Errorf("anthropic entry = %+v, want state=ok model_count=3 configured=true", p)
			}
		case "openai":
			if p.State != "auth_failed" || p.ModelCount != 0 {
				t.Errorf("openai entry = %+v, want state=auth_failed model_count=0", p)
			}
		}
	}
	if !byName["anthropic"] || !byName["openai"] {
		t.Fatalf("expected both anthropic and openai listed, got %+v", body.Providers)
	}

	// Privacy rule: /health is unauthenticated, so — exactly like routing
	// source status — providers must never carry a model id or a filter
	// pattern, only counts/state.
	raw := w.Body.String()
	for _, needle := range []string{"llama", "gpt-", "claude-", "ANTHROPIC_MODEL_FILTER", "OPENAI_MODEL_FILTER"} {
		if strings.Contains(raw, needle) {
			t.Errorf("unauthenticated /health providers array must not contain %q, got: %s", needle, raw)
		}
	}
}

// TestNewHealthHandler_ProvidersOmitsFilterFields is the v2.1 UI-key-install
// regression guard: GET /v1/models' `providers` array carries
// filter/filter_source/env_filter_set_at (service key + classes header
// only), but /health is unauthenticated and MUST switch to its own reduced
// DTO — this asserts a vendor status carrying all three of those fields
// never lets any of them reach the /health response body, by field name OR
// by value.
func TestNewHealthHandler_ProvidersOmitsFilterFields(t *testing.T) {
	envSetAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := provider.NewStatusRegistry("anthropic", "openai")
	reg.Report("anthropic", provider.ProviderStatus{
		Configured:     true,
		State:          provider.ProviderStateOK,
		ModelCount:     2,
		FilterSource:   "db",
		Filter:         []string{"claude-opus-secret-codename-*"},
		EnvFilterSetAt: &envSetAt,
	})

	routeMgr := routing.NewManager(nil, 0)
	upstreamChecker := health.NewChecker(nil)
	polStore := policy.NewPgStore(nil, 900*time.Second)
	semanticClassifier := router.NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second)
	h := newHealthHandler(upstreamChecker, routeMgr, polStore, semanticClassifier, reg, nil)

	router := gin.New()
	router.GET("/health", h)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	raw := w.Body.String()
	for _, needle := range []string{
		"filter_source", "env_filter_set_at", `"filter"`,
		"claude-opus-secret-codename", "\"db\"",
	} {
		if strings.Contains(raw, needle) {
			t.Errorf("/health must never expose %q, got: %s", needle, raw)
		}
	}

	var body struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
	if len(body.Providers) != 2 {
		t.Fatalf("expected both vendors listed, got %+v", body.Providers)
	}
	for _, p := range body.Providers {
		for _, key := range []string{"filter", "filter_source", "env_filter_set_at"} {
			if _, present := p[key]; present {
				t.Errorf("/health provider entry must not have key %q, got %+v", key, p)
			}
		}
	}
}

func TestBuildRouter_NonV1UnknownPath_GenericJSON404(t *testing.T) {
	router := buildRouter(testChatHandler(), testProvidersHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/totally-unrelated", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestHealthHandler_BedrockDataRetention: the routing block carries the closed
// bedrock_data_retention field, "unknown"/null until the mode has been read.
func TestHealthHandler_BedrockDataRetention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checkedAt := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		fn   func() (string, *time.Time)
		want string
	}{
		{"nil source", nil, `{"checked_at":null,"mode":"unknown"}`},
		{"known", func() (string, *time.Time) { return "aws_review", &checkedAt }, `{"checked_at":"2026-10-17T12:00:00Z","mode":"aws_review"}`},
	} {
		h := newHealthHandler(health.NewChecker(nil), routing.NewManager(nil, 0), policy.NewPgStore(nil, 900*time.Second),
			router.NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second), nil, tc.fn)
		r := gin.New()
		r.GET("/health", h)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
		var body struct {
			Routing struct {
				DR json.RawMessage `json:"bedrock_data_retention"`
			} `json:"routing"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if string(body.Routing.DR) != tc.want {
			t.Errorf("%s: bedrock_data_retention = %s, want %s", tc.name, body.Routing.DR, tc.want)
		}
	}
}
