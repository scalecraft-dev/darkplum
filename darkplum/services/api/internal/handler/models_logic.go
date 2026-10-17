package handler

import (
	"encoding/json"
	"strings"
	"time"
)

// Backend class and residency values. These mirror the CHECK constraints in
// migrations/010_model_routing.sql and migrations/016_external_api_residency.sql
// (models.backend_class, routing_policy.residency, principal_routing_policies.
// residency) and are shared between models.go, routing.go and policy.go.
const (
	backendClassSelfHosted   = "self_hosted"
	backendClassCloudManaged = "cloud_managed"
	backendClassExternalAPI  = "external_api"

	residencyAny              = "any"
	residencyCloudAccountOnly = "cloud_account_only"
	residencySelfHostedOnly   = "self_hosted_only"

	// darkplumClassesHeader is the capability handshake header (PLAN v3.1's
	// "Wire contract"): a comma-separated list of backend_class tokens the
	// CALLER understands and wants included in a model list. Only
	// backendClassExternalAPI is a defined token today. It is honoured by the
	// api's own model-list endpoints (session GET /v1/models, the bearer
	// path, and the admin models list) exactly the way the gateway honours it
	// on its own GET /v1/models: a caller that never sends it (an old cached
	// web bundle, an SDK that predates vendor providers) never learns a
	// vendor model exists, so it can never mislabel one "runs in your
	// infrastructure" or route a stale picker at it. It NEVER affects
	// enforcement on /v1/chat/completions -- see models.go:194's invariant.
	darkplumClassesHeader = "X-Darkplum-Classes"

	// aliasKindStatic and aliasKindRouted mirror model_aliases.kind's CHECK
	// constraint (migration 013_routed_aliases.sql).
	aliasKindStatic = "static"
	aliasKindRouted = "routed"
)

// residencyRank places a residency value on the tighten-only ladder defined
// by PLAN v3.1's wire contract: any(0) < cloud_account_only(1) <
// self_hosted_only(2). An unrecognized value is treated as the strictest
// rung (self_hosted_only) per that contract's "unknown residency -> treat as
// self_hosted_only" rule -- a residency column read back from the database
// should never actually hold anything else (it's CHECK-constrained), but a
// caller-supplied hypothetical (POST .../preview, POST /v1/policy/impact)
// must not silently under-restrict just because it's an unfamiliar string.
func residencyRank(v string) int {
	switch v {
	case residencyAny:
		return 0
	case residencyCloudAccountOnly:
		return 1
	case residencySelfHostedOnly:
		return 2
	default:
		return 2
	}
}

// residencyPermitsClass is the api's mirror of the gateway's
// ResidencyPermits(residency, class) predicate (PLAN v3.1's wire contract):
// any permits every KNOWN class; cloud_account_only permits self_hosted and
// cloud_managed (refuses external_api); self_hosted_only permits only
// self_hosted. An unrecognized class is refused under every residency,
// including "any" -- this is the one asymmetry with residencyRank's
// "unknown -> strictest" rule, because guilfoyle's ruling in PLAN v3 is
// stricter still for classes: an unrecognized class is denied outright, not
// merely gated behind the loosest residency.
//
// This is a UX-filtering predicate only (which entries buildModelsList
// includes), never the enforcement point -- see buildModelsList's own doc
// comment.
func residencyPermitsClass(residency, class string) bool {
	switch class {
	case backendClassSelfHosted:
		return true
	case backendClassCloudManaged:
		return residencyRank(residency) <= residencyRank(residencyCloudAccountOnly)
	case backendClassExternalAPI:
		return residencyRank(residency) <= residencyRank(residencyAny)
	default:
		return false
	}
}

// requestPermitsExternalAPI parses an inbound X-Darkplum-Classes header value
// (comma-separated; only "external_api" is defined) and reports whether it
// names backendClassExternalAPI. Pure so the parsing (whitespace, empty
// entries, absent header) is unit-testable without a database or gin
// context; models.go's callers pass c.GetHeader(darkplumClassesHeader).
func requestPermitsExternalAPI(headerValue string) bool {
	for _, tok := range strings.Split(headerValue, ",") {
		if strings.TrimSpace(tok) == backendClassExternalAPI {
			return true
		}
	}
	return false
}

// modelVisibleInList combines the two independent gates on whether a model
// of the given backend_class belongs in a chat-facing or admin model list:
// residency (an admin policy choice, in force for every caller regardless of
// what their client understands) and the capability header (a rollout-safety
// gate that hides a class the CALLER never asked for, independent of
// policy -- see darkplumClassesHeader's doc comment). Both must pass.
func modelVisibleInList(residency string, permitExternalAPI bool, class string) bool {
	if class == backendClassExternalAPI && !permitExternalAPI {
		return false
	}
	return residencyPermitsClass(residency, class)
}

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
// of the three values the models.backend_class CHECK constraint accepts,
// defaulting to the STRICTEST known class -- external_api -- when the field
// is absent or holds anything the gateway doesn't currently emit.
//
// This is the opposite default from before external_api existed, and
// deliberately so: with only two classes, defaulting unknown to self_hosted
// was a reasonable "don't surprise the admin with extra restrictions" choice
// (the gateway was the sole enforcement point either way, and this endpoint
// is a UX filter -- see buildModelsList's doc comment). With three classes,
// that reasoning is backwards: defaulting an unrecognized value to
// self_hosted would let a model of some future, not-yet-taught class
// masquerade as fully in-cluster -- staying visible under self_hosted_only,
// and (once the web surfaces backend_class as a trust signal) potentially
// labelled "runs in your infrastructure" when it might be calling out to a
// vendor. Defaulting to external_api instead keeps an unrecognized class out
// of every residency-restricted view (residencyPermitsClass refuses it under
// cloud_account_only and self_hosted_only) until this function is taught the
// new value -- fail closed, not fail permissive.
func normalizeBackendClass(s string) string {
	switch s {
	case backendClassSelfHosted, backendClassCloudManaged, backendClassExternalAPI:
		return s
	default:
		return backendClassExternalAPI
	}
}

// modelDBRow is the subset of the models table needed to build the chat-facing
// model list.
type modelDBRow struct {
	ModelID      string
	Provider     string
	BackendClass string
	DiscoveredAt time.Time
	Availability json.RawMessage // models.availability_detail; nil = NULL
}

// aliasDBRow is one model_aliases row joined against its (enabled) target
// model, as read for the chat-facing model list.
type aliasDBRow struct {
	Alias              string
	TargetModelID      string
	OwnedBy            string // copied from the target model's provider
	BackendClass       string // copied from the target model's backend_class
	TargetDiscoveredAt time.Time
	Availability       json.RawMessage // target's availability_detail; nil = NULL
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
	TargetAvailability json.RawMessage
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
//
// permitExternalAPI is the caller's own X-Darkplum-Classes capability (see
// darkplumClassesHeader): false hides every external_api entry regardless of
// residency, so a caller that has never heard of vendor models can't be
// shown one.
func buildModelsList(models []modelDBRow, aliases []aliasDBRow, routedAliases []routedAliasDBRow, residency string, permitExternalAPI bool, now time.Time, availabilityWindow time.Duration, allowed *[]string) []openAIModelEntry {
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
		if !modelVisibleInList(residency, permitExternalAPI, m.BackendClass) {
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
			Availability: m.Availability,
		})
	}

	for _, a := range aliases {
		if !modelVisibleInList(residency, permitExternalAPI, a.BackendClass) {
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
			Availability: a.Availability,
		})
	}

	for _, ra := range routedAliases {
		var viable bool
		var defaultTarget, defaultOwner, defaultBackendClass string
		var defaultAvailability json.RawMessage
		routes := make([]routeSummary, 0, len(ra.Routes))
		for _, rt := range ra.Routes {
			routes = append(routes, routeSummary{RouteName: rt.RouteName, Description: rt.Description, BackendClass: rt.TargetBackendClass})
			if rt.RouteName == ra.DefaultRoute {
				defaultTarget = rt.TargetModelID
				defaultOwner = rt.TargetProvider
				defaultBackendClass = rt.TargetBackendClass
				defaultAvailability = rt.TargetAvailability
			}
			if !rt.TargetExists || !rt.TargetEnabled {
				continue
			}
			if !modelVisibleInList(residency, permitExternalAPI, rt.TargetBackendClass) {
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
			Availability: defaultAvailability,
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
	// AvailabilityState / AvailabilityDetail are the gateway's per-model
	// availability (state, and the whole object as JSON text); both nil when
	// the gateway omitted it, which the upsert writes as NULL.
	AvailabilityState  *string
	AvailabilityDetail *string
}

// normalizeAvailability extracts the state and verbatim JSON text of a
// gateway "availability" value. Absent, null, non-object, or state-less
// values yield (nil, nil) -- stored as NULL rather than guessed at.
func normalizeAvailability(raw json.RawMessage) (state, detail *string) {
	if len(raw) == 0 {
		return nil, nil
	}
	var obj struct {
		State string `json:"state"`
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil || probe == nil {
		return nil, nil
	}
	if json.Unmarshal(raw, &obj) != nil || obj.State == "" {
		return nil, nil
	}
	d := string(raw)
	return &obj.State, &d
}

// normalizeGatewayModels applies field defaulting (empty owned_by -> "vllm",
// unrecognized/absent backend_class -> external_api, the strictest known
// class -- see normalizeBackendClass) and de-duplicates by model id, keeping
// the last occurrence. It is pure so the defaulting and
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
		nm.AvailabilityState, nm.AvailabilityDetail = normalizeAvailability(e.Availability)
		if idx, ok := seen[e.ID]; ok {
			normalized[idx] = nm
			continue
		}
		seen[e.ID] = len(normalized)
		normalized = append(normalized, nm)
	}
	return normalized
}
