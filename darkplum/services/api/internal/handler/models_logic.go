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

	// aliasKindStatic and aliasKindRouted mirror model_aliases.kind's CHECK
	// constraint (migration 013_routed_aliases.sql).
	aliasKindStatic = "static"
	aliasKindRouted = "routed"
)

// modelAvailabilityWindow is the default value for how recently a model must
// have been (re-)discovered to be considered available, used whenever a
// caller doesn't have an operator-configured override in hand.
// MODEL_AVAILABILITY_WINDOW_SECONDS (cmd/main.go) overrides it on both
// ModelHandler and RoutingHandler via SetAvailabilityWindow, so a model's
// `available` and an alias's `target_available` always flip together.
// A vLLM backend can take
// 15-25 minutes to finish loading a model after a pod restart, and
// SyncModels no longer deletes a row the instant it isn't re-discovered (see
// models.go), so "available" is a separate, time-based signal layered on top
// of the durable "enabled" flag. The gateway's own discovery interval
// defaults to 60s (DISCOVERY_INTERVAL_SECONDS), and its own last-good TTL is
// 120s, so 2 minutes tolerates one missed tick without flapping a model in
// and out of the chat-facing list while it's mid-restart, and keeps a
// genuinely dead backend out of the picker within about 3 minutes total
// (sync interval + this window) rather than 5.
const modelAvailabilityWindow = 2 * time.Minute

// modelIsAvailable reports whether discoveredAt is recent enough, relative to
// now, for a model to be considered routable, using the default
// modelAvailabilityWindow. It is a pure function, pulled out of
// buildModelsList, so the cutoff boundary is unit-testable without a
// database.
func modelIsAvailable(discoveredAt, now time.Time) bool {
	return modelIsAvailableWithin(discoveredAt, now, modelAvailabilityWindow)
}

// modelIsAvailableWithin is modelIsAvailable with an explicit window,
// letting ModelHandler apply an operator-configured
// MODEL_AVAILABILITY_WINDOW_SECONDS override instead of the default.
func modelIsAvailableWithin(discoveredAt, now time.Time, window time.Duration) bool {
	return discoveredAt.After(now.Add(-window))
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

// routedAliasRouteDBRow is one model_alias_routes row, left-joined against
// its target model, as read for the chat-facing model list. TargetExists is
// false when the target model row has been purged (see models.go's
// modelPurgeAfter) — a route in that state can never be viable.
type routedAliasRouteDBRow struct {
	RouteName          string
	Description        string
	TargetModelID      string
	TargetExists       bool
	TargetEnabled      bool
	TargetProvider     string
	TargetBackendClass string
	TargetDiscoveredAt time.Time
}

// routedAliasDBRow is one routed model_aliases row plus its ordered routes,
// as read for the chat-facing model list.
type routedAliasDBRow struct {
	Alias        string
	DefaultRoute string
	Routes       []routedAliasRouteDBRow
}

// buildModelsList combines enabled+available models, static aliases-to-
// enabled-available-models, and routed aliases with at least one viable
// route into the OpenAI-compatible /v1/models entry list, honouring the
// residency policy. It is pure so alias-entry construction, residency
// filtering, and the availability cutoff can be unit tested without a
// database.
//
// Note: this filtering is a UX convenience for the model picker. It is not
// the enforcement point — a request for a filtered-out cloud_managed or
// unavailable model must still be refused by the gateway (403
// model_residency_violation, or 404 once the route table drops it), since
// this endpoint can be bypassed entirely by callers that already know a model
// id.
//
// availabilityWindow is caller-supplied (rather than the modelAvailabilityWindow
// default baked in) so ModelHandler can apply an operator-configured
// MODEL_AVAILABILITY_WINDOW_SECONDS override; see cmd/main.go.
//
// allowed is the caller's effective per-principal allowlist (see policy.go's
// gatewayEffectiveClient and ListModels): nil means unrestricted (every
// model that survives the residency/availability checks below is kept); a
// non-nil (possibly empty) slice restricts entries to that concrete set of
// model ids, mirroring the gateway's own allowed_models intersection so the
// picker's list agrees with what the gateway would actually accept. Applied
// against the model/alias TARGET id, never the alias name itself, matching
// the contract that allowlists are concrete model ids only.
func buildModelsList(models []modelDBRow, aliases []aliasDBRow, routedAliases []routedAliasDBRow, residency string, now time.Time, availabilityWindow time.Duration, allowed *[]string) []openAIModelEntry {
	var allowedSet map[string]bool
	if allowed != nil {
		allowedSet = make(map[string]bool, len(*allowed))
		for _, id := range *allowed {
			allowedSet[id] = true
		}
	}

	created := now.Unix()
	entries := make([]openAIModelEntry, 0, len(models)+len(aliases)+len(routedAliases))

	for _, m := range models {
		if residency == residencySelfHostedOnly && m.BackendClass == backendClassCloudManaged {
			continue
		}
		if allowedSet != nil && !allowedSet[m.ModelID] {
			continue
		}
		if !modelIsAvailableWithin(m.DiscoveredAt, now, availabilityWindow) {
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
		if allowedSet != nil && !allowedSet[a.TargetModelID] {
			continue
		}
		if !modelIsAvailableWithin(a.TargetDiscoveredAt, now, availabilityWindow) {
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
			AliasKind:    aliasKindStatic,
		})
	}

	for _, ra := range routedAliases {
		var viable bool
		var defaultTarget, defaultOwner, defaultBackendClass string
		routes := make([]routeSummary, 0, len(ra.Routes))
		for _, rt := range ra.Routes {
			routes = append(routes, routeSummary{RouteName: rt.RouteName, Description: rt.Description, BackendClass: rt.TargetBackendClass})
			if rt.RouteName == ra.DefaultRoute {
				defaultTarget = rt.TargetModelID
				defaultOwner = rt.TargetProvider
				defaultBackendClass = rt.TargetBackendClass
			}
			if !rt.TargetExists || !rt.TargetEnabled {
				continue
			}
			if residency == residencySelfHostedOnly && rt.TargetBackendClass == backendClassCloudManaged {
				continue
			}
			if allowedSet != nil && !allowedSet[rt.TargetModelID] {
				continue
			}
			if !modelIsAvailableWithin(rt.TargetDiscoveredAt, now, availabilityWindow) {
				continue
			}
			viable = true
		}
		// A routed alias is omitted entirely when none of its routes'
		// targets is currently enabled+available+residency-permitted — even
		// though the default route's target (below) is always what alias_of
		// names, regardless of whether that specific route is the viable one.
		if !viable {
			continue
		}
		target := defaultTarget
		entries = append(entries, openAIModelEntry{
			ID:           ra.Alias,
			Object:       "model",
			Created:      created,
			OwnedBy:      defaultOwner,
			BackendClass: defaultBackendClass,
			AliasOf:      &target,
			AliasKind:    aliasKindRouted,
			Routes:       routes,
		})
	}

	return entries
}

// normalizedModel is one gateway-reported model after applying the same
// provider/backend_class defaulting SyncModels has always done, ready to be
// fed into either the batch upsert or the per-row fallback.
type normalizedModel struct {
	ID           string
	Provider     string
	BackendClass string
}

// normalizeGatewayModels applies field defaulting (empty owned_by -> "vllm",
// unrecognized/absent backend_class -> self_hosted) and de-duplicates by
// model id, keeping the last occurrence. It is pure so the defaulting and
// de-dup behaviour is unit-testable without a database or an HTTP stub.
//
// The gateway should never report the same model id twice in one response,
// but SyncModels' batch upsert issues a single
// INSERT ... ON CONFLICT DO UPDATE statement over the whole discovered set,
// and Postgres rejects a statement that would affect the same row twice
// ("ON CONFLICT DO UPDATE command cannot affect row a second time") — so
// de-duplicating client-side is required for correctness, not just hygiene.
func normalizeGatewayModels(entries []openAIModelEntry) []normalizedModel {
	seen := make(map[string]int, len(entries))
	normalized := make([]normalizedModel, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		providerName := e.OwnedBy
		if providerName == "" {
			providerName = "vllm"
		}
		nm := normalizedModel{
			ID:           e.ID,
			Provider:     providerName,
			BackendClass: normalizeBackendClass(e.BackendClass),
		}
		if idx, ok := seen[e.ID]; ok {
			normalized[idx] = nm
			continue
		}
		seen[e.ID] = len(normalized)
		normalized = append(normalized, nm)
	}
	return normalized
}
