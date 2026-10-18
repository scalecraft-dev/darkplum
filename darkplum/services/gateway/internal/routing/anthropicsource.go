package routing

import (
	"context"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
)

// anthropicCredentialChecker is the subset of *credsource.Source this source
// depends on, so tests can substitute a fake without touching the
// filesystem.
type anthropicCredentialChecker interface {
	Key() (key string, version uint64, err error)
}

var _ anthropicCredentialChecker = (*credsource.Source)(nil)

// anthropicDiscoverer is the subset of *anthropic.Provider this source
// depends on. Defined as an interface (mirroring bedrockDiscoverer) so tests
// can substitute a fake instead of a real HTTP round trip.
type anthropicDiscoverer interface {
	provider.Provider
	DiscoverModels(ctx context.Context) (map[string]bool, error)
}

var _ anthropicDiscoverer = (*anthropic.Provider)(nil)

// anthropicStatusReporter is the subset of *provider.StatusRegistry this
// source needs — just Report, unlike anthropic.authFailureReporter (which
// also reads Snapshot to preserve fields across a request-time report).
type anthropicStatusReporter interface {
	Report(name string, st provider.ProviderStatus)
}

var _ anthropicStatusReporter = (*provider.StatusRegistry)(nil)

// NewAnthropicSource builds the Anthropic routing Source: a thin wrapper
// around the six-state vendor-source skeleton shared with
// NewOpenAISource — see vendorsource.go's vendorSourceState.discover for the
// state machine (not_configured, filter_missing, auth_failed,
// filter_matched_nothing, unreachable, ok). creds and prov should be the
// SAME instances cmd/main.go passed to anthropic.New, so both share one
// cached credential read per tick. status may be nil (no ProviderStatus
// reporting, e.g. in tests that don't care about it) — Report is simply
// skipped. filters is evaluated fresh on every discovery tick (see
// NewVendorFilterFunc) rather than fixed at construction, so a filter change
// from the UI or the ConfigMap takes effect without a gateway restart.
func NewAnthropicSource(creds anthropicCredentialChecker, prov anthropicDiscoverer, filters vendorFilterFunc, status anthropicStatusReporter) Source {
	return newVendorSource(vendorSourceConfig{
		name:         "anthropic",
		kind:         "anthropic",
		envVar:       "ANTHROPIC_MODEL_FILTER",
		creds:        creds,
		prov:         prov,
		filters:      filters,
		status:       status,
		isAuthError:  anthropic.IsAuthError,
		httpStatusOf: anthropic.HTTPStatusOf,
	})
}
