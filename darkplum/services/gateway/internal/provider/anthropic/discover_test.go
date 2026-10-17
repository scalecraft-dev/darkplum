package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

func TestDiscoverModels_HappyPath(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.modelsResponse = modelsPage([]map[string]any{
		modelInfo("claude-haiku-4-5", 8192),
		modelInfo("claude-opus-4-5", 32000),
	})
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	ids, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ids["claude-haiku-4-5"] || !ids["claude-opus-4-5"] {
		t.Errorf("expected both models discovered, got %v", ids)
	}
	if len(ids) != 2 {
		t.Errorf("expected exactly 2 models, got %d", len(ids))
	}
}

func TestDiscoverModels_AuthFailure(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVendorError(w, http.StatusUnauthorized, "authentication_error", "invalid key")
	})
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	_, err := p.DiscoverModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsAuthError(err) {
		t.Errorf("expected IsAuthError to classify this failure as auth, got %v", err)
	}
}

func TestDiscoverModels_NetworkFailureIsNotAnAuthError(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	// Close the server immediately so requests fail at the transport level.
	srv.Server.Close()
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	_, err := p.DiscoverModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsAuthError(err) {
		t.Errorf("a transport-level failure must not be classified as an auth error: %v", err)
	}
}

func TestDiscoverModels_RecordsMaxOutputTokensForClamp(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.modelsResponse = modelsPage([]map[string]any{modelInfo("claude-haiku-4-5", 512)})
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	if _, err := p.DiscoverModels(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := p.resolveMaxTokens("claude-haiku-4-5", 0); got != 512 {
		t.Errorf("resolveMaxTokens with no caller value = %d, want the model's own limit 512 (clamped below the 4096 default)", got)
	}
	if got := p.resolveMaxTokens("some-other-model", 0); got != 4096 {
		t.Errorf("resolveMaxTokens for an undiscovered model should fall back to the default, got %d", got)
	}
}

func TestIsAuthError_NonSDKError(t *testing.T) {
	if IsAuthError(errors.New("plain error")) {
		t.Error("a plain error must not be classified as an auth error")
	}
	if IsAuthError(nil) {
		t.Error("nil must not be classified as an auth error")
	}
}

// ---------------------------------------------------------------------------
// VerifyKey
// ---------------------------------------------------------------------------

// callCountingCreds is a credentialSource that records every Key() call, so
// VerifyKey's "never touches credsource" contract can be asserted directly.
type callCountingCreds struct {
	key   string
	calls int
}

func (c *callCountingCreds) Key() (string, uint64, error) {
	c.calls++
	return c.key, 1, nil
}

func TestVerifyKey_HappyPath(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.modelsResponse = modelsPage([]map[string]any{
		modelInfo("claude-haiku-4-5", 8192),
		modelInfo("claude-opus-4-5", 32000),
	})
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	ids, err := p.VerifyKey(context.Background(), "sk-ant-candidate-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %v", ids)
	}
}

// TestVerifyKey_UsesTheSubmittedKeyNotTheInstalledOne asserts the actual
// wire request carries the CANDIDATE key, not the provider's installed
// credential, using a standalone server that captures the x-api-key header
// (newFakeAnthropicServer's handleModels does not).
func TestVerifyKey_UsesTheSubmittedKeyNotTheInstalledOne(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		modelsPage([]map[string]any{modelInfo("claude-haiku-4-5", 8192)})(w)
	}))
	t.Cleanup(srv.Close)

	installed := &fakeCreds{key: "sk-ant-installed", version: 1}
	p := New(installed, Config{DefaultMaxTokens: 4096, BaseURL: srv.URL})

	if _, err := p.VerifyKey(context.Background(), "sk-ant-candidate"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotKey != "sk-ant-candidate" {
		t.Errorf("expected the request to carry the submitted key, got %q", gotKey)
	}
}

func TestVerifyKey_AuthFailureClassifiesAsAuthFailed(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVendorError(w, http.StatusUnauthorized, "authentication_error", "invalid key")
	})
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	_, err := p.VerifyKey(context.Background(), "sk-ant-bad-key")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !provider.IsAuthFailedError(err) {
		t.Errorf("expected provider.IsAuthFailedError to classify this failure as auth_failed, got %v", err)
	}
}

func TestVerifyKey_NetworkFailureIsNotAuthFailed(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.Server.Close()
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})

	_, err := p.VerifyKey(context.Background(), "sk-ant-x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if provider.IsAuthFailedError(err) {
		t.Errorf("a transport-level failure must not classify as auth_failed: %v", err)
	}
}

// TestVerifyKey_NeverTouchesCredsourceOrCachedClient is the item-4 isolation
// guarantee: VerifyKey must never call creds.Key() (the gateway's installed
// credential) and must never populate the version-cached sdkClient/
// sdkVersion fields sdkClientFor maintains for real request traffic.
func TestVerifyKey_NeverTouchesCredsourceOrCachedClient(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.modelsResponse = modelsPage([]map[string]any{modelInfo("claude-haiku-4-5", 8192)})
	creds := &callCountingCreds{key: "sk-ant-installed"}
	p := New(creds, Config{DefaultMaxTokens: 4096, BaseURL: srv.Server.URL})

	if _, err := p.VerifyKey(context.Background(), "sk-ant-candidate"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if creds.calls != 0 {
		t.Errorf("expected VerifyKey never to call creds.Key(), got %d calls", creds.calls)
	}
	p.mu.Lock()
	cachedClient, cachedVersion := p.sdkClient, p.sdkVersion
	p.mu.Unlock()
	if cachedClient != nil || cachedVersion != 0 {
		t.Errorf("expected VerifyKey never to populate the version-cached sdk client, got client=%v version=%d", cachedClient, cachedVersion)
	}
}
