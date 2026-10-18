package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

func TestDiscoverModels_HappyPath(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.modelsResponse = modelsPage("gpt-5", "gpt-5-mini")
	p := newTestProvider(t, srv, nil, Config{})

	ids, err := p.DiscoverModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ids["gpt-5"] || !ids["gpt-5-mini"] {
		t.Errorf("expected both models discovered, got %v", ids)
	}
	if len(ids) != 2 {
		t.Errorf("expected exactly 2 models, got %d", len(ids))
	}
}

func TestDiscoverModels_AuthFailure(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVendorError(w, http.StatusUnauthorized, "invalid_request_error", "invalid key", "")
	})
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.DiscoverModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsAuthError(err) {
		t.Errorf("expected IsAuthError to classify this failure as auth, got %v", err)
	}
}

func TestDiscoverModels_NetworkFailureIsNotAnAuthError(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.Server.Close()
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.DiscoverModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsAuthError(err) {
		t.Errorf("a transport-level failure must not be classified as an auth error: %v", err)
	}
}

func TestIsAuthError_NonDiscoveryError(t *testing.T) {
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
	srv := newFakeOpenAIServer(t)
	srv.modelsResponse = modelsPage("gpt-5", "gpt-5-mini")
	p := newTestProvider(t, srv, nil, Config{})

	ids, err := p.VerifyKey(context.Background(), "sk-candidate", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %v", ids)
	}
}

// TestVerifyKey_UsesTheSubmittedKeyNotTheInstalledOne asserts the actual
// wire request carries the CANDIDATE key, not the provider's installed
// credential.
func TestVerifyKey_UsesTheSubmittedKeyNotTheInstalledOne(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		modelsPage("gpt-5")(w)
	}))
	t.Cleanup(srv.Close)

	installed := &fakeCreds{key: "sk-installed", version: 1}
	p := New(installed, Config{BaseURL: srv.URL})

	if _, err := p.VerifyKey(context.Background(), "sk-candidate", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer sk-candidate" {
		t.Errorf("expected the request to carry the submitted key, got %q", gotAuth)
	}
}

func TestVerifyKey_AuthFailureClassifiesAsAuthFailed(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVendorError(w, http.StatusUnauthorized, "invalid_request_error", "invalid key", "")
	})
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.VerifyKey(context.Background(), "sk-bad-key", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !provider.IsAuthFailedError(err) {
		t.Errorf("expected provider.IsAuthFailedError to classify this failure as auth_failed, got %v", err)
	}
}

func TestVerifyKey_NetworkFailureIsNotAuthFailed(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.Server.Close()
	p := newTestProvider(t, srv, nil, Config{})

	_, err := p.VerifyKey(context.Background(), "sk-x", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if provider.IsAuthFailedError(err) {
		t.Errorf("a transport-level failure must not classify as auth_failed: %v", err)
	}
}

// TestVerifyKey_NeverTouchesCredsource is the item-4 isolation guarantee:
// VerifyKey must never call creds.Key() (the gateway's installed
// credential).
func TestVerifyKey_NeverTouchesCredsource(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.modelsResponse = modelsPage("gpt-5")
	creds := &callCountingCreds{key: "sk-installed"}
	p := New(creds, Config{BaseURL: srv.Server.URL})

	if _, err := p.VerifyKey(context.Background(), "sk-candidate", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.calls != 0 {
		t.Errorf("expected VerifyKey never to call creds.Key(), got %d calls", creds.calls)
	}
}

// ---------------------------------------------------------------------------
// Chat-model hygiene denylist
// ---------------------------------------------------------------------------

func TestFilterChatModels(t *testing.T) {
	in := []string{
		"gpt-5", "gpt-5-mini", "gpt-4o",
		"text-embedding-3-large", "whisper-1", "tts-1", "dall-e-3",
		"omni-moderation-latest", "gpt-4o-realtime-preview",
		"gpt-4o-audio-preview", "gpt-4o-transcribe",
		"davinci-002", "babbage-002", "sora-1",
	}
	got := FilterChatModels(in)

	want := map[string]bool{"gpt-5": true, "gpt-5-mini": true, "gpt-4o": true}
	if len(got) != len(want) {
		t.Fatalf("FilterChatModels(%v) = %v, want exactly the chat models", in, got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected id survived the denylist: %q", id)
		}
	}
}

func TestFilterChatModels_CaseInsensitive(t *testing.T) {
	got := FilterChatModels([]string{"GPT-Image-1", "gpt-5"})
	if len(got) != 1 || got[0] != "gpt-5" {
		t.Errorf("expected the denylist match to be case-insensitive, got %v", got)
	}
}

// TestFilterChatModels_AppliedAfterGlob is exercised at the routing.Source
// level (internal/routing/openaisource_test.go): a broad "*" filter alone
// must not put a non-chat model in the catalog. This test only proves the
// hygiene function itself is order-independent input-wise (it doesn't care
// whether its input already went through a glob).
func TestFilterChatModels_EmptyInput(t *testing.T) {
	if got := FilterChatModels(nil); len(got) != 0 {
		t.Errorf("expected no ids from empty input, got %v", got)
	}
}
