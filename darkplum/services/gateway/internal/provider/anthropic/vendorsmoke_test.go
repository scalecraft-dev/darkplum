//go:build vendorsmoke

// Package anthropic vendor smoke tests call the real Anthropic API. They are
// excluded from every ordinary `go test ./...` by the vendorsmoke build tag
// and are only ever run by the repo's manual "Vendor smoke" GitHub Actions
// workflow (.github/workflows/vendor-smoke.yml), never on push or PR.
package anthropic

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

// vendorSmokeModel returns the model id to exercise: VENDOR_SMOKE_ANTHROPIC_MODEL
// if set, else claude-haiku-4-5 (the cheapest current model, matching the
// vendor-smoke workflow's default input).
func vendorSmokeModel() string {
	if m := os.Getenv("VENDOR_SMOKE_ANTHROPIC_MODEL"); m != "" {
		return m
	}
	return "claude-haiku-4-5"
}

func vendorSmokeProvider(t *testing.T) *Provider {
	t.Helper()
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping real-vendor smoke test")
	}
	return New(&fakeCreds{key: apiKey, version: 1}, Config{DefaultMaxTokens: 256})
}

func TestVendorSmoke_DiscoverModels(t *testing.T) {
	p := vendorSmokeProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ids, err := p.DiscoverModels(ctx)
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("expected at least one model from the real Models API")
	}
}

func TestVendorSmoke_Complete(t *testing.T) {
	p := vendorSmokeProvider(t)
	model := vendorSmokeModel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := p.Complete(ctx, basicRequest(model))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Error("expected non-empty completion text")
	}
	if resp.PromptTokens == 0 || resp.CompletionTokens == 0 {
		t.Errorf("expected non-zero usage, got prompt=%d completion=%d", resp.PromptTokens, resp.CompletionTokens)
	}
}

func TestVendorSmoke_VerifyKey(t *testing.T) {
	p := vendorSmokeProvider(t)
	apiKey := os.Getenv("ANTHROPIC_API_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ids, err := p.VerifyKey(ctx, apiKey)
	if err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("expected at least one model from the real Models API")
	}
}

func TestVendorSmoke_Stream(t *testing.T) {
	p := vendorSmokeProvider(t)
	model := vendorSmokeModel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var buf bytes.Buffer
	stats, err := p.Stream(ctx, basicRequest(model), &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected non-empty streamed output")
	}
	if stats.PromptTokens == 0 || stats.CompletionTokens == 0 {
		t.Errorf("expected non-zero usage, got prompt=%d completion=%d", stats.PromptTokens, stats.CompletionTokens)
	}
}
