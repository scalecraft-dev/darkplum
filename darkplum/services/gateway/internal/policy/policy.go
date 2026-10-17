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

import "time"

// Residency values. Org-level rows (routing_policy.residency, and the org
// row of principal_routing_policies once migration 016 lands) may carry any
// of the three; principal (user/api_key) rows only ever carry
// ResidencyCloudAccountOnly or ResidencySelfHostedOnly explicitly — there is
// no explicit "any" at that level, only the absence of a row (nil), which
// inherits. See ResidencyPermits and Effective for the ladder these values
// form.
const (
	ResidencyAny              = "any"
	ResidencyCloudAccountOnly = "cloud_account_only"
	ResidencySelfHostedOnly   = "self_hosted_only"
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

// PrincipalCounts is the number of principals with an EXPLICIT policy row in
// principal_routing_policies at snapshot-load time — i.e. len(UserPolicies)
// and len(KeyPolicies), not every user/key that exists. Reported on /health
// as policy.principals.
type PrincipalCounts struct {
	Users   int
	APIKeys int
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
	// Residency is one of the Residency* constants. This is the org-level
	// value only (routing_policy.residency, or the org row of
	// principal_routing_policies once migration 015/016 lands) —
	// per-principal enforcement must use EffectiveFor's strictest value
	// instead, not this field directly.
	Residency string

	// OrgPolicy is the org level's own precomputed EffectivePolicy (chain of
	// exactly the org row). Every principal with no row of its own shares
	// this exact pointer via EffectiveFor — see buildPrincipalPolicies. Never
	// nil for a Snapshot loaded by PgStore; nil only for a Snapshot built by
	// hand (tests, or a field literal that only sets Enabled/Aliases/Residency).
	OrgPolicy *EffectivePolicy
	// UserPolicies maps user_id -> precomputed EffectivePolicy, one entry per
	// EXPLICIT row of principal_routing_policies with principal_type='user'
	// (chain: org row, then that user's row). A user with no row has no entry
	// here — EffectiveFor falls back to OrgPolicy for it.
	UserPolicies map[string]*EffectivePolicy
	// KeyPolicies maps api_key_id -> precomputed EffectivePolicy, one entry
	// per EXPLICIT row of principal_routing_policies with
	// principal_type='api_key' (chain: org row, then the key's owning user's
	// row if one exists, then the key's own row). A key with no row has no
	// entry here — EffectiveFor falls back to UserPolicies (or OrgPolicy) for
	// it. Never consulted for a service key (see EffectiveFor).
	KeyPolicies map[string]*EffectivePolicy
	// PrincipalCounts is len(UserPolicies)/len(KeyPolicies), reported as-is
	// on /health.
	PrincipalCounts PrincipalCounts

	// OrgRow is the org level's own raw (unmerged) policy row. Together with
	// UserRows and KeyRows, this is the input POST /v1/policy/effective's
	// override path needs to reconstruct a principal's chain and substitute
	// exactly one level -- OrgPolicy/UserPolicies/KeyPolicies only ever store
	// the ALREADY-MERGED Effective() result, which cannot be un-merged. See
	// ChainFor.
	OrgRow PolicyRow
	// UserRows maps user_id -> that user's own raw policy row, one entry per
	// EXPLICIT row of principal_routing_policies with principal_type='user' --
	// the same domain as UserPolicies' keys, but unmerged.
	UserRows map[string]PolicyRow
	// KeyRows maps api_key_id -> that key's own raw policy row, one entry per
	// EXPLICIT row of principal_routing_policies with principal_type='api_key'
	// -- the same domain as KeyPolicies' keys, but unmerged.
	KeyRows map[string]PolicyRow
	// KeyOwner maps api_key_id -> owning user_id for every NON-REVOKED api_key
	// in the customer's database, independent of whether that key has ever had
	// an explicit policy row of its own (unlike KeyRows, whose domain is only
	// keys with a row). Used by the POST /v1/policy/effective override path to
	// answer "every key owned by this user" for its "keys" list -- see
	// UserKeys, the inverse mapping precomputed alongside it.
	KeyOwner map[string]string
	// UserKeys maps user_id -> the ids of every non-revoked api_key it owns,
	// the inverse of KeyOwner precomputed once per snapshot load rather than
	// scanned from KeyOwner per request.
	UserKeys map[string][]string

	// VendorFilters maps vendor name ("anthropic", "openai") -> its
	// DB-stored model filter row (migration 017's vendor_provider_settings,
	// written by the api's PUT .../providers/:vendor/key|filter). A vendor
	// with no row (never installed via the UI, or the table doesn't exist
	// yet on a customer database that hasn't run migration 017) simply has
	// no entry — routing.vendorFilterFunc then falls back to the
	// <VENDOR>_MODEL_FILTER env pair, exactly like today. See
	// PgStore.loadVendorFilters.
	VendorFilters map[string]VendorFilterRow
}

// VendorFilterRow is one vendor's row of vendor_provider_settings, as read
// by PgStore.loadVendorFilters: the model_filter glob patterns an admin
// installed from Admin > Models, and the timestamp they were set at — the
// "db" side of routing.resolveVendorFilter's last-writer-wins comparison
// against the <VENDOR>_MODEL_FILTER_SET_AT env value.
type VendorFilterRow struct {
	Filter []string
	SetAt  time.Time
	// WorkspaceID is vendor_provider_settings.workspace_id (migration 019),
	// "" when NULL/unset. Rides on the same row (and so the same SetAt
	// last-writer-wins stamp) as Filter. Anthropic only.
	WorkspaceID string
}

// ChainFor returns the raw, unmerged policy chain for a principal in the
// canonical org -> user[-> api_key] order Effective expects: org and user
// rows are always present; the api_key row is appended only when keyID is
// non-empty. A level with no explicit row in the snapshot is represented as
// a bare PolicyRow{Level: ...} (every field nil = inherit) rather than
// omitted, so PolicyOverride.Apply always has a row at every requested level
// to substitute into, regardless of whether an admin has ever touched it.
//
// Unlike EffectiveFor (the request-time enforcement path, which looks up
// precomputed EffectivePolicy values and never touches raw rows), ChainFor
// exists solely for POST /v1/policy/effective's override preview -- the one
// place the gateway needs to re-run Effective() with one level's row swapped
// out rather than read an already-merged result.
func (s Snapshot) ChainFor(userID, keyID string) []PolicyRow {
	org := s.OrgRow
	org.Level = PrincipalTypeOrg

	user, ok := s.UserRows[userID]
	if !ok {
		user = PolicyRow{}
	}
	user.Level = PrincipalTypeUser

	chain := []PolicyRow{org, user}
	if keyID != "" {
		key, ok := s.KeyRows[keyID]
		if !ok {
			key = PolicyRow{}
		}
		key.Level = PrincipalTypeAPIKey
		chain = append(chain, key)
	}
	return chain
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
