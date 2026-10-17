package routing

import (
	"context"
	"fmt"
	"sync"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/bedrock"
)

// bedrockDiscoverer is the subset of *bedrock.BedrockProvider this source
// depends on: the fixed public surface (New, Name, Region, DiscoverModels,
// BedrockAPIError) documented in the shared design contract. Defined as an
// interface so tests can substitute a fake instead of a real AWS client.
type bedrockDiscoverer interface {
	provider.Provider
	DiscoverModels(ctx context.Context) (map[string]bool, error)
}

// compile-time check: *bedrock.BedrockProvider satisfies bedrockDiscoverer
// without this package (or the bedrock package) needing to know about the
// other.
var _ bedrockDiscoverer = (*bedrock.BedrockProvider)(nil)

// bedrockSourceState holds the lazily-initialised Bedrock client and retry
// state behind a Source's Discover closure.
//
// Unlike a one-shot startup attempt (the previous cmd/main.go behaviour,
// where a failed bedrock.New left Bedrock "unavailable until restart"),
// initialisation is retried on every tick until it succeeds: a transient
// credential or VPC-endpoint problem at boot no longer requires a pod
// restart to recover.
type bedrockSourceState struct {
	region  string
	filters []string
	newFn   func(ctx context.Context, region string) (bedrockDiscoverer, error)
	// onReady is invoked exactly once, synchronously, the first time a
	// Bedrock client is successfully constructed — before discover returns
	// any model ids for it. Callers use this to register the provider in
	// their dispatch registry so a model can never appear routable before
	// its provider is reachable.
	onReady func(provider.Provider)

	mu   sync.Mutex
	prov bedrockDiscoverer
}

// NewBedrockSource builds the Bedrock Source. region and filters come from
// BEDROCK_REGION / BEDROCK_MODEL_FILTER. onReady may be nil.
func NewBedrockSource(region string, filters []string, onReady func(provider.Provider)) Source {
	state := &bedrockSourceState{
		region:  region,
		filters: filters,
		newFn: func(ctx context.Context, region string) (bedrockDiscoverer, error) {
			return bedrock.New(ctx, region)
		},
		onReady: onReady,
	}
	return newBedrockSourceFromState(state)
}

// newBedrockSourceFromState builds the Source wrapper around a state value.
// Split out from NewBedrockSource so tests (same package) can construct a
// state with a fake newFn without touching the real AWS SDK.
func newBedrockSourceFromState(state *bedrockSourceState) Source {
	return Source{
		Name:        "bedrock",
		Kind:        "bedrock",
		Class:       ClassCloudManaged,
		ProviderKey: "bedrock",
		Discover:    state.discover,
	}
}

func (s *bedrockSourceState) discover(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	prov := s.prov
	s.mu.Unlock()

	if prov == nil {
		p, err := s.newFn(ctx, s.region)
		if err != nil {
			return nil, fmt.Errorf("bedrock: provider init failed (will retry next tick): %w", err)
		}

		s.mu.Lock()
		s.prov = p
		s.mu.Unlock()
		prov = p

		if s.onReady != nil {
			s.onReady(p)
		}
	}

	discovered, err := prov.DiscoverModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("bedrock: discovery failed: %w", err)
	}

	ids := make([]string, 0, len(discovered))
	for id := range discovered {
		ids = append(ids, id)
	}

	return ApplyGlobFilter(ids, s.filters, "BEDROCK_MODEL_FILTER")
}
