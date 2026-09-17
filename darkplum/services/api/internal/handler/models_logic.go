package handler

import "time"

// Backend class and residency values. These mirror the CHECK constraints in
// migrations/010_model_routing.sql (models.backend_class, routing_policy.residency)
// and are shared between models.go and routing.go.
const (
	backendClassSelfHosted   = "self_hosted"
	backendClassCloudManaged = "cloud_managed"

	residencyAny            = "any"
	residencySelfHostedOnly = "self_hosted_only"
)

// modelAvailabilityWindow is how recently a model must have been (re-)discovered
// to be considered available. A vLLM backend can take 15-25 minutes to finish
// loading a model after a pod restart, and SyncModels no longer deletes a row
// the instant it isn't re-discovered (see models.go), so "available" is a
// separate, time-based signal layered on top of the durable "enabled" flag.
// The gateway's own discovery interval defaults to 60s (DISCOVERY_INTERVAL_SECONDS),
// so 3 minutes tolerates a couple of missed ticks without flapping a model in
// and out of the chat-facing list while it's mid-restart.
const modelAvailabilityWindow = 3 * time.Minute

// modelIsAvailable reports whether discoveredAt is recent enough, relative to
// now, for a model to be considered routable. It is a pure function, pulled
// out of buildModelsList, so the cutoff boundary is unit-testable without a
// database.
func modelIsAvailable(discoveredAt, now time.Time) bool {
	return discoveredAt.After(now.Add(-modelAvailabilityWindow))
}

// modelRoutingAdvisoryLockKey is a fixed pg_advisory_xact_lock key shared by
// RoutingHandler.PutAlias (routing.go) and ModelHandler.SyncModels (models.go)
// to serialise the model-id/alias conflict check against a concurrent
// discovery upsert of the same id — see the comment on each call site. Any
// int64 constant works as long as every caller agrees on it; nothing else in
// this schema currently takes an advisory lock, so there is no collision risk
// to guard against.
const modelRoutingAdvisoryLockKey int64 = 0x64706d6c // "dpml", arbitrary

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
	DiscoveredAt time.Time
}

// aliasDBRow is one model_aliases row joined against its (enabled) target
// model, as read for the chat-facing model list.
type aliasDBRow struct {
	Alias              string
	TargetModelID      string
	OwnedBy            string // copied from the target model's provider
	BackendClass       string // copied from the target model's backend_class
	TargetDiscoveredAt time.Time
}

// buildModelsList combines enabled+available models and aliases-to-enabled-
// available-models into the OpenAI-compatible /v1/models entry list,
// honouring the residency policy. It is pure so alias-entry construction,
// residency filtering, and the availability cutoff can be unit tested without
// a database.
//
// Note: this filtering is a UX convenience for the model picker. It is not
// the enforcement point — a request for a filtered-out cloud_managed or
// unavailable model must still be refused by the gateway (403
// model_residency_violation, or 404 once the route table drops it), since
// this endpoint can be bypassed entirely by callers that already know a model
// id.
func buildModelsList(models []modelDBRow, aliases []aliasDBRow, residency string, now time.Time) []openAIModelEntry {
	created := now.Unix()
	entries := make([]openAIModelEntry, 0, len(models)+len(aliases))

	for _, m := range models {
		if residency == residencySelfHostedOnly && m.BackendClass == backendClassCloudManaged {
			continue
		}
		if !modelIsAvailable(m.DiscoveredAt, now) {
			continue
		}
		entries = append(entries, openAIModelEntry{
			ID:           m.ModelID,
			Object:       "model",
			Created:      created,
			OwnedBy:      m.Provider,
			BackendClass: m.BackendClass,
		})
	}

	for _, a := range aliases {
		if residency == residencySelfHostedOnly && a.BackendClass == backendClassCloudManaged {
			continue
		}
		if !modelIsAvailable(a.TargetDiscoveredAt, now) {
			continue
		}
		target := a.TargetModelID
		entries = append(entries, openAIModelEntry{
			ID:           a.Alias,
			Object:       "model",
			Created:      created,
			OwnedBy:      a.OwnedBy,
			BackendClass: a.BackendClass,
			AliasOf:      &target,
		})
	}

	return entries
}
