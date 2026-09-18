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
		rules, rules, rules,
		nil,
	)
}

// ---------------------------------------------------------------------------
// The two live routes
// ---------------------------------------------------------------------------

func TestBuildRouter_ListModels(t *testing.T) {
	router := buildRouter(testChatHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestBuildRouter_ChatCompletions(t *testing.T) {
	router := buildRouter(testChatHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

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
	router := buildRouter(testChatHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

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

func TestBuildRouter_Health(t *testing.T) {
	router := buildRouter(testChatHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

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
	router := buildRouter(testChatHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

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
	modelClassifier := router.NewModelClassifier(nil, staticRoutes{table: routing.NewTable(nil)}, fakePolicyStore{}, time.Second, 5, 30*time.Second)
	semanticClassifier := router.NewSemanticRouterClassifier("", 0.6, time.Second, 5, 30*time.Second)
	h := newHealthHandler(upstreamChecker, routeMgr, polStore, modelClassifier, semanticClassifier)

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
				Model struct {
					BreakerOpen         bool `json:"breaker_open"`
					ConsecutiveFailures int  `json:"consecutive_failures"`
				} `json:"model"`
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

	if body.Routing.Classifier.Model.BreakerOpen {
		t.Error("expected routing.classifier.model.breaker_open=false for a fresh classifier")
	}
	if body.Routing.Classifier.Model.ConsecutiveFailures != 0 {
		t.Errorf("routing.classifier.model.consecutive_failures = %d, want 0", body.Routing.Classifier.Model.ConsecutiveFailures)
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
}

func TestBuildRouter_NonV1UnknownPath_GenericJSON404(t *testing.T) {
	router := buildRouter(testChatHandler(), noopMiddleware, noopMiddleware, stubHealthHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/totally-unrelated", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
