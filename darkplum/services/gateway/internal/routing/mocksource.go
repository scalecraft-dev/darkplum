package routing

import "context"

// NewMockSource builds a Source for the in-process mock provider. Discover
// never fails and the model list never changes — it exists purely so mock
// mode participates in the same discovery/collision machinery as every other
// backend instead of being special-cased in the handler.
func NewMockSource(providerKey string, models []string) Source {
	// Copy so later mutation of the caller's slice cannot affect this source.
	fixed := make([]string, len(models))
	copy(fixed, models)

	return Source{
		Name:        "mock",
		Kind:        "mock",
		Class:       ClassSelfHosted,
		ProviderKey: providerKey,
		Discover: func(_ context.Context) ([]string, error) {
			out := make([]string, len(fixed))
			copy(out, fixed)
			return out, nil
		},
	}
}
