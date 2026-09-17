package provider

import (
	"context"
	"io"
	"sync"
	"testing"
)

type fakeProvider struct{ name string }

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) Complete(_ context.Context, _ ChatRequest) (*ChatResponse, error) {
	return nil, nil
}
func (p *fakeProvider) Stream(_ context.Context, _ ChatRequest, _ io.Writer) (*StreamStats, error) {
	return nil, nil
}

func TestRegistry_SetAndGet(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("vllm-default"); ok {
		t.Fatal("expected empty registry to have no entries")
	}

	p := &fakeProvider{name: "vllm-default"}
	r.Set("vllm-default", p)

	got, ok := r.Get("vllm-default")
	if !ok {
		t.Fatal("expected to find registered provider")
	}
	if got.Name() != "vllm-default" {
		t.Errorf("got provider %q, want vllm-default", got.Name())
	}
}

func TestRegistry_SetLateReplacesEntry(t *testing.T) {
	// Models the Bedrock retry-on-tick case: onReady fires after Discover
	// initially had no provider registered.
	r := NewRegistry()
	if _, ok := r.Get("bedrock"); ok {
		t.Fatal("expected no bedrock entry before init succeeds")
	}
	r.Set("bedrock", &fakeProvider{name: "bedrock"})
	if _, ok := r.Get("bedrock"); !ok {
		t.Fatal("expected bedrock entry after late registration")
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.Set("bedrock", &fakeProvider{name: "bedrock"})
		}()
		go func() {
			defer wg.Done()
			_, _ = r.Get("bedrock")
		}()
	}
	wg.Wait()
}
