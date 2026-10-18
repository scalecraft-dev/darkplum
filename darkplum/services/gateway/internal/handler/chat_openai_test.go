package handler

// A handler-level test proving an OpenAI outage never affects an unrelated
// local model OR an unrelated Anthropic model dispatched through the same
// ChatHandler — the "vendor provider can NEVER stop the gateway" failure-
// isolation ruling, exercised across all three backend classes at once. This
// uses a REAL *openai.Provider (unconfigured: its credential file does not
// exist) rather than a fake, so the assertion covers the actual error path
// openai.Provider.Complete returns, translated all the way through
// handleProviderError — mirroring chat_anthropic_test.go's identical
// two-provider version.

import (
	"net/http"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

func TestHandleChatCompletions_OpenAIOutageLeavesLocalAndAnthropicAnswering(t *testing.T) {
	creds := credsource.New("/nonexistent/openai-api-key")
	openAIProv := openai.New(creds, openai.Config{})

	localProv := &fakeKindProvider{name: "vllm-default", kind: "vllm"}
	anthropicProv := &fakeKindProvider{name: "anthropic", kind: "anthropic"}

	h := newTestHandler(
		fakeProviders{
			"vllm-default": localProv,
			"anthropic":    anthropicProv,
			"openai":       openAIProv,
		},
		map[string]routing.Route{
			"llama-3-8b":       {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"claude-haiku-4-5": {ProviderKey: "anthropic", Kind: "anthropic", Class: routing.ClassExternalAPI},
			"gpt-5-mini":       {ProviderKey: "openai", Kind: "openai", Class: routing.ClassExternalAPI},
		},
		policy.Snapshot{
			Enabled: map[string]bool{
				"llama-3-8b":       true,
				"claude-haiku-4-5": true,
				"gpt-5-mini":       true,
			},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	// The OpenAI model fails (no credential configured) — must be a scrubbed
	// 502, never a panic or a hang.
	w, _ := doChat(t, h, "gpt-5-mini")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for the unconfigured openai model, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "upstream_error" {
		t.Errorf("expected code upstream_error, got %q", e.Error.Code)
	}

	// Both the local model AND the Anthropic model must still answer
	// normally — the openai outage must not poison any shared state
	// (registry, handler, or otherwise).
	w, _ = doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusOK {
		t.Fatalf("expected local model to still answer after the openai outage, got %d; body=%s", w.Code, w.Body.String())
	}
	if localProv.completeCalls != 1 {
		t.Errorf("expected local provider to be dispatched to exactly once, got %d", localProv.completeCalls)
	}

	w, _ = doChat(t, h, "claude-haiku-4-5")
	if w.Code != http.StatusOK {
		t.Fatalf("expected the anthropic model to still answer after the openai outage, got %d; body=%s", w.Code, w.Body.String())
	}
	if anthropicProv.completeCalls != 1 {
		t.Errorf("expected the anthropic provider to be dispatched to exactly once, got %d", anthropicProv.completeCalls)
	}

	// And once more for good measure — the openai model failing repeatedly
	// still doesn't degrade either of the others.
	w, _ = doChat(t, h, "gpt-5-mini")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 again for the openai model, got %d", w.Code)
	}
	w, _ = doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusOK {
		t.Fatalf("expected local model to still answer, got %d; body=%s", w.Code, w.Body.String())
	}
	w, _ = doChat(t, h, "claude-haiku-4-5")
	if w.Code != http.StatusOK {
		t.Fatalf("expected the anthropic model to still answer, got %d; body=%s", w.Code, w.Body.String())
	}
}
