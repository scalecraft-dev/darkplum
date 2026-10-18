package routing

import (
	"context"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
)

// openaiCredentialChecker is the subset of *credsource.Source this source
// depends on, so tests can substitute a fake without touching the
// filesystem.
type openaiCredentialChecker interface {
	Key() (key string, version uint64, err error)
}

var _ openaiCredentialChecker = (*credsource.Source)(nil)

// openaiDiscoverer is the subset of *openai.Provider this source depends on.
// Defined as an interface (mirroring anthropicDiscoverer) so tests can
// substitute a fake instead of a real HTTP round trip.
type openaiDiscoverer interface {
	provider.Provider
	DiscoverModels(ctx context.Context) (map[string]bool, error)
}

var _ openaiDiscoverer = (*openai.Provider)(nil)

// openaiStatusReporter is the subset of *provider.StatusRegistry this source
// needs — just Report, unlike openai.authFailureReporter (which also reads
// Snapshot to preserve fields across a request-time report).
type openaiStatusReporter interface {
	Report(name string, st provider.ProviderStatus)
}

var _ openaiStatusReporter = (*provider.StatusRegistry)(nil)

// NewOpenAISource builds the OpenAI routing Source: a thin wrapper around
// the six-state vendor-source skeleton shared with NewAnthropicSource — see
// vendorsource.go's vendorSourceState.discover for the state machine
// (not_configured, filter_missing, auth_failed, filter_matched_nothing,
// unreachable, ok). creds and prov should be the SAME instances cmd/main.go
// passed to openai.New, so both share one cached credential read per tick.
// status may be nil (no ProviderStatus reporting, e.g. in tests that don't
// care about it) — Report is simply skipped.
//
// Unlike NewAnthropicSource, the glob-filtered id list is additionally
// passed through openai.FilterChatModels (see vendorsource.go's postFilter
// hook) — OpenAI's Models API lists embeddings/tts/whisper/image/moderation/
// realtime/audio models alongside chat models, and a broad "*"
// OPENAI_MODEL_FILTER alone must not be able to put one of those into the
// chat catalog. filters is evaluated fresh on every discovery tick (see
// NewVendorFilterFunc) rather than fixed at construction, so a filter change
// from the UI or the ConfigMap takes effect without a gateway restart.
func NewOpenAISource(creds openaiCredentialChecker, prov openaiDiscoverer, filters vendorFilterFunc, status openaiStatusReporter) Source {
	return newVendorSource(vendorSourceConfig{
		name:         "openai",
		kind:         "openai",
		envVar:       "OPENAI_MODEL_FILTER",
		creds:        creds,
		prov:         prov,
		filters:      filters,
		status:       status,
		isAuthError:  openai.IsAuthError,
		httpStatusOf: openai.HTTPStatusOf,
		postFilter:   openai.FilterChatModels,
	})
}
