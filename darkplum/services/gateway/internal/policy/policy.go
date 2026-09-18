// Package policy loads and caches the gateway's routing-enforcement inputs —
// which models are enabled, aliases, and the org-wide residency policy — from
// Postgres. The gateway never queries these tables per-request: Store.Current
// returns the last successfully loaded Snapshot from an in-memory cache kept
// fresh by a background refresher (PgStore.Run).
//
// Fail-closed semantics: until the first successful load, Current reports
// "not loaded" and the handler must refuse every request with 503
// policy_unavailable. After a first success, a later refresh failure keeps
// serving the last-good snapshot (logged loudly) rather than failing closed —
// a transient DB blip must not take down inference traffic that policy
// already approved.
package policy

// Residency values for routing_policy.residency.
const (
	ResidencyAny            = "any"
	ResidencySelfHostedOnly = "self_hosted_only"
)

// Alias kinds for model_aliases.kind (migration 013).
const (
	AliasKindStatic = "static"
	AliasKindRouted = "routed"
)

// Classifier kinds for model_aliases.classifier_kind (migration 013,
// narrowed by migration 014: the "model" kind — an in-house classifier
// prompt protocol — has been removed; only these two remain).
// ClassifierKindSemanticRouter (see internal/router's
// SemanticRouterClassifier) calls an in-cluster vLLM Semantic Router
// classification API rather than an LLM.
const (
	ClassifierKindRules          = "rules"
	ClassifierKindSemanticRouter = "semantic_router"
)

// RouteDef is one candidate route of a routed alias, loaded from
// model_alias_routes and ordered by position (see AliasDef.Routes).
type RouteDef struct {
	// Name is model_alias_routes.route_name.
	Name string
	// Description is shown to admins and, for a model classifier, sent as
	// part of the classification prompt.
	Description string
	// Target is model_alias_routes.target_model_id — always a concrete model
	// id, never another alias (enforced by the api at PUT time).
	Target string
	// Keywords are case-folded literal substrings (or reserved builtin:*
	// tokens) a rules classifier matches against, in list order.
	Keywords []string
}

// AliasDef describes one row of model_aliases. A static alias only ever
// populates Kind and Target (StaticTarget mirrors the pre-routed-alias
// map[string]string shape for callers that only care about that case). A
// routed alias additionally populates DefaultRoute, ClassifierKind, and
// Routes (ordered by model_alias_routes.position).
type AliasDef struct {
	Kind           string
	Target         string
	DefaultRoute   string
	ClassifierKind string
	Routes         []RouteDef
}

// Route returns the named route from a.Routes, or false if no route with
// that name exists. Every Decision a router.Classifier produces names either
// a.DefaultRoute or one of a.Routes — both are guaranteed (by the loader's
// drop-if-inconsistent rule) to resolve here for any AliasDef actually
// present in a loaded Snapshot.
func (a AliasDef) Route(name string) (RouteDef, bool) {
	for _, r := range a.Routes {
		if r.Name == name {
			return r, true
		}
	}
	return RouteDef{}, false
}

// Snapshot is the routing-enforcement policy in effect at a point in time.
type Snapshot struct {
	// Enabled maps model_id -> models.enabled. A model absent from this map
	// (not yet synced by the api) is treated as not enabled.
	Enabled map[string]bool
	// Aliases maps alias -> AliasDef. Never chained: a static alias's Target
	// and a routed alias's route targets are looked up as-is, never
	// re-resolved through Aliases again (model_aliases guarantees a target is
	// never itself an alias).
	Aliases map[string]AliasDef
	// Residency is ResidencyAny or ResidencySelfHostedOnly.
	Residency string
}

// StaticTarget returns the target of a static alias, mirroring the
// pre-routed-alias Aliases map[string]string shape. Returns false for a
// routed alias or an unknown alias — callers that need routed-alias fields
// use Aliases directly.
func (s Snapshot) StaticTarget(alias string) (string, bool) {
	a, ok := s.Aliases[alias]
	if !ok || a.Kind != AliasKindStatic {
		return "", false
	}
	return a.Target, true
}

// Store exposes the current policy snapshot to request handlers.
// Implementations must be safe for concurrent use and must never perform
// per-request I/O — the handler calls Current() on every request.
type Store interface {
	// Current returns the latest successfully loaded snapshot and true, or a
	// zero Snapshot and false if no load has ever succeeded. Callers must
	// fail closed (503 policy_unavailable) when the second return value is
	// false.
	Current() (Snapshot, bool)
}
