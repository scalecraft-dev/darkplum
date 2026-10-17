package routing

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft/darkplum/gateway/internal/provider/openai"
)

// sentinelKeyFragment / sentinelWorkspaceID are planted in a fake vendor's
// 401 response body/headers below, mirroring two real vendor behaviours this
// package's discovery path must never surface: OpenAI's real 401 body can
// echo back part of the submitted key, and the Anthropic SDK's *sdk.Error
// (github.com/anthropics/anthropic-sdk-go/internal/apierror.Error.Error())
// renders the raw response JSON plus the request-id/anthropic-workspace-id
// response headers verbatim. See vendorsource.go's discover and
// vendorDiscoveryError — the fix under test.
const (
	sentinelKeyFragment = "sk-SENTINEL-abc123"
	sentinelWorkspaceID = "wksp_SENTINEL_9f8e7d6c"
)

// sentinelCreds is a vendorCredentialChecker/credentialSource fake whose key
// IS the sentinel fragment, so a real vendor "echoed part of your key back"
// 401 body is simulated faithfully by the fake servers below.
type sentinelCreds struct{ key string }

func (c sentinelCreds) Key() (string, uint64, error) { return c.key, 1, nil }

// newSentinelAnthropicServer answers every request (Models list AND
// Messages) with a 401 whose body names sentinelKeyFragment and whose
// anthropic-workspace-id header is sentinelWorkspaceID — both of which
// *sdk.Error.Error() would render verbatim if ever formatted with %v/%w.
func newSentinelAnthropicServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req_should_not_matter")
		w.Header().Set("anthropic-workspace-id", sentinelWorkspaceID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key ` + sentinelKeyFragment + `"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSentinelOpenAIServer answers every request (Models list AND Chat
// Completions) with a 401 whose body names sentinelKeyFragment, mirroring a
// real OpenAI 401 body that echoes back part of the submitted key.
func newSentinelOpenAIServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: ` + sentinelKeyFragment + `","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func basicSentinelChatRequest(model string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

func assertNoSentinel(t *testing.T, label, haystack string) {
	t.Helper()
	for _, needle := range []string{sentinelKeyFragment, sentinelWorkspaceID} {
		if strings.Contains(haystack, needle) {
			t.Errorf("%s leaked sentinel %q: %s", label, needle, haystack)
		}
	}
}

// TestVendorDiscoveryAndChatRequest_NeverLeakVendorErrorText is the
// regression test for the BLOCKER finding: discovery errors leaking vendor
// text into logs. It drives BOTH a discovery tick (routing.Manager.Refresh,
// exercising vendorsource.go's discover -> vendorDiscoveryError) and a
// request-time chat call (Provider.Complete, exercising
// translateError/translateVendorError) against real HTTP fakes for both
// direct vendor providers, capturing the std logger AND slog output across
// both, and asserts the planted sentinels never appear anywhere: not in the
// captured logs, not in a returned error's Error() string, and not in a
// translated APIError's ResponseBody().
func TestVendorDiscoveryAndChatRequest_NeverLeakVendorErrorText(t *testing.T) {
	anthropicSrv := newSentinelAnthropicServer(t)
	openaiSrv := newSentinelOpenAIServer(t)

	creds := sentinelCreds{key: sentinelKeyFragment}
	anthropicProv := anthropic.New(creds, anthropic.Config{BaseURL: anthropicSrv.URL, DefaultMaxTokens: 512})
	openaiProv := openai.New(creds, openai.Config{BaseURL: openaiSrv.URL})

	anthropicSrc := NewAnthropicSource(creds, anthropicProv, staticVendorFilter([]string{"*"}), nil)
	openaiSrc := NewOpenAISource(creds, openaiProv, staticVendorFilter([]string{"*"}), nil)

	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	origLogger := slog.Default()
	var slogBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&slogBuf, nil)))
	defer slog.SetDefault(origLogger)

	// --- Discovery tick ---
	m := NewManager([]Source{anthropicSrc, openaiSrc}, 0)
	m.Refresh(context.Background())

	// --- Chat request ---
	_, err := anthropicProv.Complete(context.Background(), basicSentinelChatRequest("claude-x"))
	if err == nil {
		t.Fatal("expected an error from the sentinel anthropic server")
	}
	assertNoSentinel(t, "anthropic Complete error string", err.Error())
	var apiErr provider.APIError
	if errors.As(err, &apiErr) {
		assertNoSentinel(t, "anthropic Complete response body", string(apiErr.ResponseBody()))
	} else {
		t.Error("expected anthropic Complete's error to be a provider.APIError")
	}

	_, err = openaiProv.Complete(context.Background(), basicSentinelChatRequest("gpt-x"))
	if err == nil {
		t.Fatal("expected an error from the sentinel openai server")
	}
	assertNoSentinel(t, "openai Complete error string", err.Error())
	if errors.As(err, &apiErr) {
		assertNoSentinel(t, "openai Complete response body", string(apiErr.ResponseBody()))
	} else {
		t.Error("expected openai Complete's error to be a provider.APIError")
	}

	// --- VerifyKey (item 4: the SUBMITTED key is itself the sentinel) ---
	// VerifyKey is called with the sentinel key directly as the CANDIDATE
	// being checked (never through creds — see anthropicProv/openaiProv's
	// installed credential, which is a DIFFERENT value here specifically to
	// prove VerifyKey never reads it). The fake servers above echo the
	// sentinel back in their 401 body/headers exactly as a real vendor
	// would.
	installedCreds := sentinelCreds{key: "sk-installed-should-never-be-used"}
	anthropicForVerify := anthropic.New(installedCreds, anthropic.Config{BaseURL: anthropicSrv.URL, DefaultMaxTokens: 512})
	openaiForVerify := openai.New(installedCreds, openai.Config{BaseURL: openaiSrv.URL})

	_, err = anthropicForVerify.VerifyKey(context.Background(), sentinelKeyFragment)
	if err == nil {
		t.Fatal("expected an error from the sentinel anthropic server")
	}
	assertNoSentinel(t, "anthropic VerifyKey error string", err.Error())
	if errors.As(err, &apiErr) {
		assertNoSentinel(t, "anthropic VerifyKey response body", string(apiErr.ResponseBody()))
	} else {
		t.Error("expected anthropic VerifyKey's error to be a provider.APIError")
	}

	_, err = openaiForVerify.VerifyKey(context.Background(), sentinelKeyFragment)
	if err == nil {
		t.Fatal("expected an error from the sentinel openai server")
	}
	assertNoSentinel(t, "openai VerifyKey error string", err.Error())
	if errors.As(err, &apiErr) {
		assertNoSentinel(t, "openai VerifyKey response body", string(apiErr.ResponseBody()))
	} else {
		t.Error("expected openai VerifyKey's error to be a provider.APIError")
	}

	assertNoSentinel(t, "std logger output (discovery tick + VerifyKey)", logBuf.String())
	assertNoSentinel(t, "slog output (discovery tick + chat request + VerifyKey)", slogBuf.String())
}
