package routing

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// fakeBedrockProvider implements bedrockDiscoverer without touching AWS.
type fakeBedrockProvider struct {
	name      string
	models    map[string]bool
	discErr   error
	discCalls int
}

func (f *fakeBedrockProvider) Name() string { return f.name }
func (f *fakeBedrockProvider) Complete(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeBedrockProvider) Stream(_ context.Context, _ provider.ChatRequest, _ io.Writer) (*provider.StreamStats, error) {
	return nil, errors.New("not implemented in fake")
}
func (f *fakeBedrockProvider) DiscoverModels(_ context.Context) (map[string]bool, error) {
	f.discCalls++
	if f.discErr != nil {
		return nil, f.discErr
	}
	return f.models, nil
}

func TestBedrockSource_RetriesInitOnLaterTick(t *testing.T) {
	initCalls := 0
	fake := &fakeBedrockProvider{name: "bedrock", models: map[string]bool{"us.anthropic.claude-3-5-sonnet": true}}

	state := &bedrockSourceState{
		region: "us-east-1",
		newFn: func(_ context.Context, region string) (bedrockDiscoverer, error) {
			initCalls++
			if initCalls < 3 {
				return nil, errors.New("expired sso credentials")
			}
			return fake, nil
		},
	}
	src := newBedrockSourceFromState(state)

	// First two ticks: init fails both times, discovery never runs.
	if _, err := src.Discover(context.Background()); err == nil {
		t.Fatal("expected error on first tick (init failing)")
	}
	if _, err := src.Discover(context.Background()); err == nil {
		t.Fatal("expected error on second tick (init still failing)")
	}
	if fake.discCalls != 0 {
		t.Fatalf("DiscoverModels must not be called before init succeeds, got %d calls", fake.discCalls)
	}

	// Third tick: init succeeds and discovery runs.
	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("expected third tick to succeed, got %v", err)
	}
	if len(ids) != 1 || ids[0] != "us.anthropic.claude-3-5-sonnet" {
		t.Errorf("unexpected discovered ids: %v", ids)
	}
	if initCalls != 3 {
		t.Errorf("expected exactly 3 init attempts, got %d", initCalls)
	}

	// Fourth tick: client is already initialised — newFn must not be called again.
	if _, err := src.Discover(context.Background()); err != nil {
		t.Fatalf("unexpected error on fourth tick: %v", err)
	}
	if initCalls != 3 {
		t.Errorf("expected init to not be retried once it has succeeded, got %d calls", initCalls)
	}
	if fake.discCalls != 2 {
		t.Errorf("expected DiscoverModels called twice (tick 3 and 4), got %d", fake.discCalls)
	}
}

func TestBedrockSource_OnReadyCalledOnceOnFirstSuccess(t *testing.T) {
	fake := &fakeBedrockProvider{name: "bedrock", models: map[string]bool{"m1": true}}
	readyCalls := 0
	var readyProvider provider.Provider

	state := &bedrockSourceState{
		region: "us-east-1",
		newFn: func(_ context.Context, region string) (bedrockDiscoverer, error) {
			return fake, nil
		},
		onReady: func(p provider.Provider) {
			readyCalls++
			readyProvider = p
		},
	}
	src := newBedrockSourceFromState(state)

	if _, err := src.Discover(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := src.Discover(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if readyCalls != 1 {
		t.Errorf("expected onReady to be called exactly once, got %d", readyCalls)
	}
	if readyProvider != provider.Provider(fake) {
		t.Error("expected onReady to receive the constructed provider")
	}
}

func TestBedrockSource_DiscoveryFailureAfterInitKeepsRetrying(t *testing.T) {
	fake := &fakeBedrockProvider{name: "bedrock", discErr: errors.New("access denied")}
	state := &bedrockSourceState{
		region: "us-east-1",
		newFn:  func(_ context.Context, region string) (bedrockDiscoverer, error) { return fake, nil },
	}
	src := newBedrockSourceFromState(state)

	if _, err := src.Discover(context.Background()); err == nil {
		t.Fatal("expected discovery error to propagate")
	}
	fake.discErr = nil
	fake.models = map[string]bool{"m1": true}
	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("expected recovery once permissions fixed, got %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("expected 1 model after recovery, got %v", ids)
	}
}

func TestBedrockSource_AppliesModelFilter(t *testing.T) {
	fake := &fakeBedrockProvider{
		name: "bedrock",
		models: map[string]bool{
			"us.anthropic.claude-3-5-sonnet": true,
			"us.meta.llama3-70b":             true,
			"us.mistral.mistral-large":       true,
		},
	}
	state := &bedrockSourceState{
		region:  "us-east-1",
		filters: []string{"us.anthropic.*"},
		newFn:   func(_ context.Context, region string) (bedrockDiscoverer, error) { return fake, nil },
	}
	src := newBedrockSourceFromState(state)

	ids, err := src.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 1 || ids[0] != "us.anthropic.claude-3-5-sonnet" {
		t.Errorf("expected filter to restrict to anthropic models, got %v", ids)
	}
}

// TestManager_BedrockVsVLLMCollision exercises the full Manager + a
// (fake-backed) Bedrock Source together with a self-hosted source, mirroring
// the real cmd/main.go wiring: same model id from vLLM and Bedrock must
// always route to vLLM.
func TestManager_BedrockVsVLLMCollision(t *testing.T) {
	fake := &fakeBedrockProvider{name: "bedrock", models: map[string]bool{"shared-model": true}}
	state := &bedrockSourceState{
		region: "us-east-1",
		newFn:  func(_ context.Context, region string) (bedrockDiscoverer, error) { return fake, nil },
	}
	brk := newBedrockSourceFromState(state)
	vllm := staticSource("vllm-default", "vllm", ClassSelfHosted, "vllm-default", []string{"shared-model"})

	m := NewManager([]Source{brk, vllm}, 0)
	m.Refresh(context.Background())

	route, ok := m.Snapshot().Lookup("shared-model")
	if !ok {
		t.Fatal("expected a route for shared-model")
	}
	if route.ProviderKey != "vllm-default" {
		t.Errorf("expected vllm-default to win over bedrock, got %q", route.ProviderKey)
	}
}

// refreshingBedrock adds the optional data-retention refresh seam.
type refreshingBedrock struct {
	fakeBedrockProvider
	refreshes int
	order     *[]string
}

func (f *refreshingBedrock) RefreshAccountDataRetention(_ context.Context) {
	f.refreshes++
	*f.order = append(*f.order, "refresh")
}

func TestBedrockSource_RefreshesDataRetentionOncePerTickAndNeverFailsDiscovery(t *testing.T) {
	var order []string
	fake := &refreshingBedrock{
		fakeBedrockProvider: fakeBedrockProvider{name: "bedrock", models: map[string]bool{"us.anthropic.claude-fable-5": true}},
		order:               &order,
	}
	state := &bedrockSourceState{
		region: "us-east-1",
		newFn:  func(context.Context, string) (bedrockDiscoverer, error) { return fake, nil },
	}
	src := newBedrockSourceFromState(state)
	for i := 0; i < 3; i++ {
		ids, err := src.Discover(context.Background())
		if err != nil || len(ids) != 1 {
			t.Fatalf("tick %d: ids=%v err=%v", i, ids, err)
		}
	}
	if fake.refreshes != 3 {
		t.Errorf("refreshes = %d, want 3 (once per tick)", fake.refreshes)
	}
}
