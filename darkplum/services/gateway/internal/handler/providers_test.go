package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft/darkplum/gateway/internal/provider/credsource"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeVendorVerifier struct {
	verifyIDs []string
	verifyErr error
	discIDs   map[string]bool
	discErr   error
}

func (f fakeVendorVerifier) VerifyKey(_ context.Context, _ string) ([]string, error) {
	return f.verifyIDs, f.verifyErr
}
func (f fakeVendorVerifier) DiscoverModels(_ context.Context) (map[string]bool, error) {
	return f.discIDs, f.discErr
}

type fakeVendorCreds struct{ err error }

func (f fakeVendorCreds) Key() (string, uint64, error) { return "sk-installed", 1, f.err }

type fakeRecheckSetter struct {
	mu       sync.Mutex
	interval time.Duration
	duration time.Duration
	calls    int
}

func (f *fakeRecheckSetter) SetTemporaryRecheckInterval(interval, duration time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interval, f.duration = interval, duration
	f.calls++
}

type fakeRefreshManager struct {
	mu    sync.Mutex
	calls []string
	ch    chan string
}

func (f *fakeRefreshManager) RefreshSource(_ context.Context, name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if f.ch != nil {
		f.ch <- name
	}
}

func (f *fakeRefreshManager) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newTestProvidersRouter builds a gin.Engine serving both routes, with a
// test-only middleware that sets ContextKeyType from a header — mirroring
// how chat_policy_test.go's doChatAs injects auth context without a real
// APIKey middleware run.
func newTestProvidersRouter(h *ProvidersHandler) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.ContextKeyType, c.GetHeader("X-Test-Key-Type"))
		c.Next()
	})
	r.POST("/v1/providers/:vendor/verify", h.HandleVerify)
	r.POST("/v1/providers/:vendor/refresh", h.HandleRefresh)
	return r
}

func doProvidersRequest(r *gin.Engine, method, path, keyType string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if keyType != "" {
		req.Header.Set("X-Test-Key-Type", keyType)
	}
	r.ServeHTTP(w, req)
	return w
}

func decodeVerifyResponse(t *testing.T, w *httptest.ResponseRecorder) verifyResponse {
	t.Helper()
	var resp verifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return resp
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeErrorEnvelope(t *testing.T, w *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var body errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	return body
}

// ---------------------------------------------------------------------------
// Service-key gate / unknown vendor
// ---------------------------------------------------------------------------

func TestHandleVerify_RefusesNonServiceKey(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeUser, []byte(`{}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
	if body := decodeErrorEnvelope(t, w); body.Error.Code != "service_key_required" {
		t.Errorf("error.code = %q, want service_key_required", body.Error.Code)
	}
}

func TestHandleRefresh_RefusesNonServiceKey(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/refresh", "", []byte(`{}`))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_UnknownVendor404(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/bedrock/verify", middleware.KeyTypeService, []byte(`{}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unsupported vendor, got %d; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleRefresh_UnknownVendor404(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"openai": {Verifier: fakeVendorVerifier{}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/bedrock/refresh", middleware.KeyTypeService, []byte(`{}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unsupported vendor, got %d; body=%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// HandleVerify — submitted key
// ---------------------------------------------------------------------------

func TestHandleVerify_WithKey_HappyPath(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{verifyIDs: []string{"claude-opus-4-5", "claude-haiku-4-5"}}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	body, _ := json.Marshal(verifyRequest{Key: "sk-ant-candidate"})
	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	resp := decodeVerifyResponse(t, w)
	if len(resp.Reachable) != 2 || len(resp.Admitted) != 2 {
		t.Errorf("expected both ids reachable and admitted (empty filter), got %+v", resp)
	}
}

func TestHandleVerify_WithKey_NonEmptyFilterAppliesGlobThenPostFilter(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"openai": {
			Verifier:   fakeVendorVerifier{verifyIDs: []string{"gpt-5", "gpt-5-mini", "text-embedding-3-large"}},
			Creds:      fakeVendorCreds{},
			EnvVar:     "OPENAI_MODEL_FILTER",
			PostFilter: func(ids []string) []string { return dropSubstring(ids, "embedding") },
		},
	})
	r := newTestProvidersRouter(h)

	body, _ := json.Marshal(verifyRequest{Key: "sk-candidate", ModelFilter: []string{"*"}})
	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/openai/verify", middleware.KeyTypeService, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	resp := decodeVerifyResponse(t, w)
	if len(resp.Reachable) != 3 {
		t.Errorf("expected all 3 reachable (no filter applied to Reachable), got %v", resp.Reachable)
	}
	if len(resp.Admitted) != 2 {
		t.Errorf("expected the embeddings model excluded by PostFilter from Admitted, got %v", resp.Admitted)
	}
}

func dropSubstring(ids []string, substr string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !strings.Contains(id, substr) {
			out = append(out, id)
		}
	}
	return out
}

func TestHandleVerify_WithKey_AuthFailedClassification(t *testing.T) {
	authErr := provider.TranslateVendorHTTPStatus("anthropic", http.StatusUnauthorized, 0, nil, errors.New("boom"))
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{verifyErr: authErr}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	body, _ := json.Marshal(verifyRequest{Key: "sk-bad"})
	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := decodeErrorEnvelope(t, w).Error.Code; got != "auth_failed" {
		t.Errorf("error.code = %q, want auth_failed", got)
	}
}

func TestHandleVerify_WithKey_UnreachableClassification(t *testing.T) {
	upstreamErr := provider.TranslateVendorHTTPStatus("openai", http.StatusBadGateway, 0, nil, errors.New("boom"))
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"openai": {Verifier: fakeVendorVerifier{verifyErr: upstreamErr}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	body, _ := json.Marshal(verifyRequest{Key: "sk-x"})
	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/openai/verify", middleware.KeyTypeService, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := decodeErrorEnvelope(t, w).Error.Code; got != "unreachable" {
		t.Errorf("error.code = %q, want unreachable", got)
	}
}

func TestHandleVerify_InvalidModelFilterGlob400(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{verifyIDs: []string{"claude-x"}}, Creds: fakeVendorCreds{}, EnvVar: "ANTHROPIC_MODEL_FILTER"},
	})
	r := newTestProvidersRouter(h)

	body, _ := json.Marshal(verifyRequest{Key: "sk-x", ModelFilter: []string{"[invalid-glob"}})
	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed glob, got %d; body=%s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// HandleVerify — no key submitted (installed-credential path)
// ---------------------------------------------------------------------------

func TestHandleVerify_NoKey_KeyRequiredWhenNoneInstalled(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{}, Creds: fakeVendorCreds{err: credsource.ErrNotConfigured}},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, []byte(`{}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := decodeErrorEnvelope(t, w).Error.Code; got != "key_required" {
		t.Errorf("error.code = %q, want key_required", got)
	}
}

func TestHandleVerify_NoKey_UsesInstalledDiscoveryPath(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {
			Verifier: fakeVendorVerifier{discIDs: map[string]bool{"claude-x": true}},
			Creds:    fakeVendorCreds{},
		},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, []byte(`{}`))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	resp := decodeVerifyResponse(t, w)
	if len(resp.Reachable) != 1 || resp.Reachable[0] != "claude-x" {
		t.Errorf("expected the installed credential's discovery result, got %+v", resp)
	}
}

func TestHandleVerify_NoKey_DiscoverAuthFailedClassification(t *testing.T) {
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {
			Verifier:    fakeVendorVerifier{discErr: errors.New("401")},
			Creds:       fakeVendorCreds{},
			IsAuthError: func(error) bool { return true },
		},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, []byte(`{}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := decodeErrorEnvelope(t, w).Error.Code; got != "auth_failed" {
		t.Errorf("error.code = %q, want auth_failed", got)
	}
}

// ---------------------------------------------------------------------------
// Sentinel leak (item 4/5): the submitted key must never appear in the
// response body or in any log line, including when the fake vendor echoes
// it back.
// ---------------------------------------------------------------------------

func TestHandleVerify_NeverLeaksSubmittedKey(t *testing.T) {
	const sentinelKeyFragment = "sk-ant-SENTINEL-abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + sentinelKeyFragment + `"}}`))
	}))
	t.Cleanup(srv.Close)

	realProv := anthropic.New(fakeSentinelCreds{}, anthropic.Config{BaseURL: srv.URL, DefaultMaxTokens: 512})
	h := NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: realProv, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	body, _ := json.Marshal(verifyRequest{Key: sentinelKeyFragment})
	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, body)

	if strings.Contains(w.Body.String(), sentinelKeyFragment) {
		t.Errorf("response body leaked the submitted key: %s", w.Body.String())
	}
	if strings.Contains(logBuf.String(), sentinelKeyFragment) {
		t.Errorf("log output leaked the submitted key: %s", logBuf.String())
	}
}

type fakeSentinelCreds struct{}

func (fakeSentinelCreds) Key() (string, uint64, error) { return "sk-installed-unused", 1, nil }

// ---------------------------------------------------------------------------
// HandleRefresh — 202, triggers the hint loop, dedup, clean shutdown
// ---------------------------------------------------------------------------

func TestHandleRefresh_Returns202AndTriggersImmediateRefresh(t *testing.T) {
	refresh := &fakeRefreshManager{ch: make(chan string, 4)}
	h := NewProvidersHandler(context.Background(), refresh, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: fakeVendorVerifier{}, Creds: fakeVendorCreds{}},
	})
	r := newTestProvidersRouter(h)

	w := doProvidersRequest(r, http.MethodPost, "/v1/providers/anthropic/refresh", middleware.KeyTypeService, []byte(`{}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d; body=%s", w.Code, w.Body.String())
	}

	select {
	case name := <-refresh.ch:
		if name != "anthropic" {
			t.Errorf("expected RefreshSource(anthropic), got %q", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected an immediate RefreshSource call")
	}
}

func TestHandleRefresh_SetsTemporaryRecheckInterval(t *testing.T) {
	recheck := &fakeRecheckSetter{}
	refresh := &fakeRefreshManager{ch: make(chan string, 4)}
	h := NewProvidersHandler(context.Background(), refresh, map[string]ProviderVendorConfig{
		"openai": {Verifier: fakeVendorVerifier{}, Creds: fakeVendorCreds{}, Recheck: recheck},
	})
	r := newTestProvidersRouter(h)

	doProvidersRequest(r, http.MethodPost, "/v1/providers/openai/refresh", middleware.KeyTypeService, []byte(`{}`))
	<-refresh.ch

	recheck.mu.Lock()
	defer recheck.mu.Unlock()
	if recheck.calls != 1 {
		t.Fatalf("expected exactly 1 SetTemporaryRecheckInterval call, got %d", recheck.calls)
	}
	if recheck.interval != refreshHintCredsRecheckInterval || recheck.duration != refreshHintWindow {
		t.Errorf("interval=%v duration=%v, want %v/%v", recheck.interval, recheck.duration, refreshHintCredsRecheckInterval, refreshHintWindow)
	}
}

// TestTriggerRefreshHint_DedupesConcurrentCalls asserts a second refresh
// call for the SAME vendor while the hint loop is already running extends
// the deadline instead of starting a second loop (which would double up on
// immediate RefreshSource calls).
func TestTriggerRefreshHint_DedupesConcurrentCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	refresh := &fakeRefreshManager{ch: make(chan string, 8)}
	h := NewProvidersHandler(ctx, refresh, nil)

	h.triggerRefreshHint("anthropic")
	h.triggerRefreshHint("anthropic")

	select {
	case <-refresh.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("expected at least one immediate RefreshSource call")
	}

	// Give a second, erroneous loop (if one existed) time to also fire its
	// own immediate call before asserting there was only ever one.
	time.Sleep(100 * time.Millisecond)
	if n := refresh.callCount(); n != 1 {
		t.Errorf("expected exactly 1 immediate call from the deduped hint, got %d", n)
	}

	h.hintMu.Lock()
	running := h.hintRunning["anthropic"]
	h.hintMu.Unlock()
	if !running {
		t.Error("expected the hint loop to still be marked running")
	}
}

// TestRunRefreshHintLoop_StopsCleanlyOnContextCancel is the "no goroutine
// leak" guard: canceling the handler's root context must make the loop
// return promptly, clearing hintRunning, with no lingering goroutine.
func TestRunRefreshHintLoop_StopsCleanlyOnContextCancel(t *testing.T) {
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	refresh := &fakeRefreshManager{ch: make(chan string, 8)}
	h := NewProvidersHandler(ctx, refresh, nil)

	h.triggerRefreshHint("anthropic")
	<-refresh.ch // wait for the immediate call so the loop has actually started

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.hintMu.Lock()
		running := h.hintRunning["anthropic"]
		h.hintMu.Unlock()
		if !running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	h.hintMu.Lock()
	running := h.hintRunning["anthropic"]
	h.hintMu.Unlock()
	if running {
		t.Fatal("expected the hint loop to stop and clear hintRunning after ctx cancellation")
	}

	// Best-effort goroutine-count check: allow generous slack since the test
	// runtime itself can have background goroutines churn independently of
	// this handler.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine count did not settle after cancellation: before=%d after=%d", before, runtime.NumGoroutine())
}
