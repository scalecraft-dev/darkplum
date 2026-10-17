package handler

// A handler-level test proving a vendor provider's failure never affects an
// unrelated local provider dispatched through the same ChatHandler — the
// "vendor provider can NEVER stop the gateway" failure-isolation ruling. This
// uses a REAL *anthropic.Provider (unconfigured: its credential file does not
// exist) rather than a fake, so the assertion covers the actual error path
// anthropic.Provider.Complete returns, translated all the way through
// handleProviderError.

import (
	"net/http"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft/darkplum/gateway/internal/provider/credsource"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

func TestHandleChatCompletions_VendorOutageDoesNotAffectLocalModel(t *testing.T) {
	creds := credsource.New("/nonexistent/anthropic-api-key")
	anthropicProv := anthropic.New(creds, anthropic.Config{DefaultMaxTokens: 8192})

	localProv := &fakeKindProvider{name: "vllm-default", kind: "vllm"}

	h := newTestHandler(
		fakeProviders{
			"vllm-default": localProv,
			"anthropic":    anthropicProv,
		},
		map[string]routing.Route{
			"llama-3-8b":       {ProviderKey: "vllm-default", Kind: "vllm", Class: routing.ClassSelfHosted},
			"claude-haiku-4-5": {ProviderKey: "anthropic", Kind: "anthropic", Class: routing.ClassExternalAPI},
		},
		policy.Snapshot{
			Enabled:   map[string]bool{"llama-3-8b": true, "claude-haiku-4-5": true},
			Residency: policy.ResidencyAny,
		},
		true,
	)

	// The vendor model fails (no credential configured) — must be a scrubbed
	// 502, never a panic or a hang.
	w, _ := doChat(t, h, "claude-haiku-4-5")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for the unconfigured vendor model, got %d; body=%s", w.Code, w.Body.String())
	}
	e := decodeError(t, w.Body.Bytes())
	if e.Error.Code != "upstream_error" {
		t.Errorf("expected code upstream_error, got %q", e.Error.Code)
	}

	// The local model must still answer normally — before AND after the
	// vendor failure, proving the vendor outage did not poison any shared
	// state (registry, handler, or otherwise).
	w, _ = doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusOK {
		t.Fatalf("expected local model to still answer after a vendor outage, got %d; body=%s", w.Code, w.Body.String())
	}
	if localProv.completeCalls != 1 {
		t.Errorf("expected local provider to be dispatched to exactly once, got %d", localProv.completeCalls)
	}

	// And once more for good measure — the vendor model failing repeatedly
	// still doesn't degrade the local model.
	w, _ = doChat(t, h, "claude-haiku-4-5")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 again for the vendor model, got %d", w.Code)
	}
	w, _ = doChat(t, h, "llama-3-8b")
	if w.Code != http.StatusOK {
		t.Fatalf("expected local model to still answer, got %d; body=%s", w.Code, w.Body.String())
	}
}
