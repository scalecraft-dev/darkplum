package handler

// Backend class and residency values. These mirror the CHECK constraints in
// migrations/010_model_routing.sql (models.backend_class, routing_policy.residency)
// and are shared between models.go and routing.go.
const (
	backendClassSelfHosted   = "self_hosted"
	backendClassCloudManaged = "cloud_managed"

	residencyAny            = "any"
	residencySelfHostedOnly = "self_hosted_only"
)

// normalizeBackendClass maps a gateway-reported backend_class value onto one
// of the two values the models.backend_class CHECK constraint accepts,
// defaulting to self_hosted when the field is absent or holds anything the
// gateway doesn't currently emit. This matches the contract: an unrecognized
// class must not be treated as cloud_managed (that would be a "unexpected new
// value gets extra restrictions" surprise) — the gateway is the sole
// enforcement point and is expected to report a real value once it knows one.
func normalizeBackendClass(s string) string {
	if s == backendClassCloudManaged {
		return backendClassCloudManaged
	}
	return backendClassSelfHosted
}

// modelDBRow is the subset of the models table needed to build the chat-facing
// model list.
type modelDBRow struct {
	ModelID      string
	Provider     string
	BackendClass string
}

// aliasDBRow is one model_aliases row joined against its (enabled) target
// model, as read for the chat-facing model list.
type aliasDBRow struct {
	Alias         string
	TargetModelID string
	OwnedBy       string // copied from the target model's provider
	BackendClass  string // copied from the target model's backend_class
}

// buildModelsList combines enabled models and aliases-to-enabled-models into
// the OpenAI-compatible /v1/models entry list, honouring the residency
// policy. It is pure so alias-entry construction and residency filtering can
// be unit tested without a database.
//
// Note: this filtering is a UX convenience for the model picker. It is not
// the enforcement point — a request for a filtered-out cloud_managed model
// must still be refused by the gateway (403 model_residency_violation), since
// this endpoint can be bypassed entirely by callers that already know a model
// id.
func buildModelsList(models []modelDBRow, aliases []aliasDBRow, residency string, now int64) []openAIModelEntry {
	entries := make([]openAIModelEntry, 0, len(models)+len(aliases))

	for _, m := range models {
		if residency == residencySelfHostedOnly && m.BackendClass == backendClassCloudManaged {
			continue
		}
		entries = append(entries, openAIModelEntry{
			ID:           m.ModelID,
			Object:       "model",
			Created:      now,
			OwnedBy:      m.Provider,
			BackendClass: m.BackendClass,
		})
	}

	for _, a := range aliases {
		if residency == residencySelfHostedOnly && a.BackendClass == backendClassCloudManaged {
			continue
		}
		target := a.TargetModelID
		entries = append(entries, openAIModelEntry{
			ID:           a.Alias,
			Object:       "model",
			Created:      now,
			OwnedBy:      a.OwnedBy,
			BackendClass: a.BackendClass,
			AliasOf:      &target,
		})
	}

	return entries
}
