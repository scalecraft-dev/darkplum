package policy

import "github.com/scalecraft/darkplum/gateway/internal/routing"

// Principal levels for PolicyRow.Level and principal_routing_policies.
// principal_type (migration 015). These are also the exact values the
// "sources" object of GET /v1/policy/effective reports per field, plus
// SourceNone for a field no level in the chain set.
const (
	PrincipalTypeOrg    = "org"
	PrincipalTypeUser   = "user"
	PrincipalTypeAPIKey = "api_key"
	// SourceNone is the "sources" value for a field that every row in the
	// chain left null — i.e. the field's effective value is the unrestricted
	// default (all models / any residency / no default) rather than
	// something inherited from a specific level.
	SourceNone = "none"
)

// KeyTypeService mirrors middleware.KeyTypeService. Duplicated here rather
// than importing internal/middleware (which would be the only reason this
// package ever depended on it) — the same three-line judgment call
// handler.attributedUserID already makes for the identical reason: a service
// key's principal is the X-Darkplum-User-Id it presents, with no key-level
// row, ever (service_keys never gets rows in principal_routing_policies —
// see migration 015's CHECK constraint, which only permits 'org', 'user', or
// 'api_key').
const KeyTypeService = "service"

// PolicyRow is one level's policy — a row of principal_routing_policies (org,
// user, or api_key), or the equivalent for a level that has no row at all
// (every field nil, meaning "inherit"). Level is one of the PrincipalType*
// constants and is never itself nil/empty for a row actually passed to
// Effective.
type PolicyRow struct {
	Level string
	// Residency is nil (inherit) or a pointer to ResidencyCloudAccountOnly or
	// ResidencySelfHostedOnly. There is no explicit "any" value at
	// user/api_key level: only org can mean "any" by being nil with nothing
	// above it to inherit from — tightening only ever adds a stricter value
	// somewhere in the chain (see residencyRank), never loosens one a less
	// specific level already set.
	Residency *string
	// AllowedModels is nil (inherit) or a pointer to the row's allowlist —
	// concrete model ids only, never aliases. A non-nil pointer to an empty
	// slice is a deliberate "deny all" (see Effective).
	AllowedModels *[]string
	// DefaultModel is nil (inherit) or the row's preselected model/alias.
	DefaultModel *string
}

// PolicySources names, per field, the most specific level in the chain that
// set a non-null value for it — PrincipalTypeOrg/User/APIKey, or SourceNone
// if no level set it. This is a single value even for AllowedModels'
// set-intersection merge: the UI copy this drives ("your key's own
// allowlist", "your organisation's policy") is most useful naming the
// closest contributor to the principal being viewed, not every level that
// happened to also restrict the same field.
type PolicySources struct {
	AllowedModels string `json:"allowed_models"`
	Residency     string `json:"residency"`
	DefaultModel  string `json:"default_model"`
}

// EffectivePolicy is the merged routing policy in force for one principal —
// the result of Effective(chain) for that principal's org -> user -> api_key
// rows, precomputed once per Snapshot load (see Snapshot.EffectiveFor) rather
// than recomputed per request.
type EffectivePolicy struct {
	// AllowedModels is nil for "all models" (no level in the chain set an
	// allowlist), or a pointer to the intersection of every non-null
	// allowlist in the chain — concrete model ids. A non-nil pointer to an
	// empty slice means every model is denied.
	AllowedModels *[]string `json:"allowed_models"`
	// Residency is ResidencyAny or ResidencySelfHostedOnly — the strictest
	// value found anywhere in the chain.
	Residency string `json:"residency"`
	// DefaultModel is the first non-null default_model found walking the
	// chain from most specific to least specific (api_key, then user, then
	// org), or nil if none of them set one.
	DefaultModel *string       `json:"default_model"`
	Sources      PolicySources `json:"sources"`
}

// Allows reports whether model (always a resolved, concrete model id — never
// an alias) is permitted by e.AllowedModels. nil means unrestricted.
func (e EffectivePolicy) Allows(model string) bool {
	if e.AllowedModels == nil {
		return true
	}
	for _, m := range *e.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// residencyRank orders residency values from loosest to strictest: any=0 <
// cloud_account_only=1 < self_hosted_only=2. Used both by Effective's
// strictest-wins chain merge and by ResidencyPermits' ceiling comparison.
var residencyRank = map[string]int{
	ResidencyAny:              0,
	ResidencyCloudAccountOnly: 1,
	ResidencySelfHostedOnly:   2,
}

// residencyByRank is the inverse of residencyRank, indexed by rank —
// residencyByRank[residencyRank[v]] == v for every known v. Used to
// normalise an out-of-range rank (see residencyRankOf) back to a canonical
// wire value rather than leaking whatever unrecognised string produced it.
var residencyByRank = []string{ResidencyAny, ResidencyCloudAccountOnly, ResidencySelfHostedOnly}

// residencyRankOf returns v's rank, treating any value outside the three
// known residency constants as the strictest known rank (self_hosted_only) —
// the wire contract's "unknown residency value -> strictest" rule. This
// function is pure and does not log; a caller loading rows from Postgres
// that wants to warn about an unrecognised on-disk value (rather than
// silently tightening it) does so itself once per snapshot load — see
// pgstore.go's buildPrincipalPolicies.
func residencyRankOf(v string) int {
	if r, ok := residencyRank[v]; ok {
		return r
	}
	return residencyRank[ResidencySelfHostedOnly]
}

// residencyForRank returns the canonical residency value for rank, clamping
// an out-of-range rank to the strictest known value.
func residencyForRank(rank int) string {
	if rank < 0 || rank >= len(residencyByRank) {
		return ResidencySelfHostedOnly
	}
	return residencyByRank[rank]
}

// isKnownResidency reports whether v is one of the three Residency*
// constants. Used only for the "log once" diagnostic in pgstore.go's
// buildPrincipalPolicies — enforcement itself never needs this, since
// residencyRankOf already normalises an unknown value on every read.
func isKnownResidency(v string) bool {
	_, ok := residencyRank[v]
	return ok
}

// classResidencyCeiling maps a backend class to the loosest residency rank
// under which a route of that class remains permitted — i.e. the class is
// allowed whenever residencyRankOf(residency) <= this value. self_hosted is
// permitted under every residency, even the strictest; cloud_managed loses
// self_hosted_only; external_api is permitted only under "any". An
// unrecognised class has no entry, and ResidencyPermits refuses it under
// every residency including "any" — the deny-unknown-class-under-ALL-
// residencies rule (a route this gateway build cannot classify must never be
// treated as permitted by default).
var classResidencyCeiling = map[string]int{
	routing.ClassSelfHosted:   residencyRank[ResidencySelfHostedOnly],
	routing.ClassCloudManaged: residencyRank[ResidencyCloudAccountOnly],
	routing.ClassExternalAPI:  residencyRank[ResidencyAny],
}

// ResidencyPermits reports whether a route of the given backend class
// (routing.ClassSelfHosted/ClassCloudManaged/ClassExternalAPI) may be used
// under the given residency policy value. This is the single implementation
// of the residency ladder: any -> every known class; cloud_account_only ->
// self_hosted and cloud_managed; self_hosted_only -> self_hosted only.
// Callers should use this instead of comparing eff.Residency/route.Class
// directly, both because it centralises the ladder and because an
// unrecognised class is refused under every residency, including "any" —
// the deny-when-uncertain rule a direct equality check would miss.
func ResidencyPermits(residency, class string) bool {
	ceiling, known := classResidencyCeiling[class]
	if !known {
		return false
	}
	return residencyRankOf(residency) <= ceiling
}

// Effective merges a principal's policy chain — ordered from least specific
// to most specific, e.g. []PolicyRow{orgRow, userRow, apiKeyRow} — into a
// single EffectivePolicy. This is the ONE merge implementation for the
// gateway: pure (no I/O), so it is exercised directly by table tests, and
// reused by both request-time enforcement (via Snapshot.EffectiveFor's
// precomputed results) and GET /v1/policy/effective.
//
// Per-field semantics (see DESIGN4.md "Principals and policy"):
//   - AllowedModels: the intersection of every non-null allowlist in the
//     chain. No level sets one -> nil (all models). Any level sets an empty
//     slice -> the intersection stays empty (deny all) regardless of what
//     less specific levels allow, exactly like any other tightening.
//   - Residency: the strictest (highest-ranked, see residencyRank) value set
//     anywhere in the chain — any < cloud_account_only < self_hosted_only.
//     There is no way to loosen a stricter value set by a less specific
//     level; an unrecognised value is treated as the strictest known one.
//   - DefaultModel: the first non-null value walking the chain from MOST
//     specific to least specific (api_key overrides user overrides org) —
//     unlike the other two fields, this is an override, not a tightening.
//
// Sources are attributed to the most specific level that set a non-null
// value for that field (see PolicySources), computed by the same
// most-specific-first walk.
func Effective(chain []PolicyRow) EffectivePolicy {
	eff := EffectivePolicy{
		Residency: ResidencyAny,
		Sources:   PolicySources{AllowedModels: SourceNone, Residency: SourceNone, DefaultModel: SourceNone},
	}

	var allowed *[]string
	haveAllowedSource := false
	// residencyBestRank tracks the highest (strictest) rank seen so far,
	// starting below every real rank so the first row that sets Residency at
	// all (even to "any" explicitly, e.g. an org row after migration 016)
	// always wins the initial assignment and its Level is attributed as the
	// source.
	residencyBestRank := -1
	haveDefaultSource := false

	for i := len(chain) - 1; i >= 0; i-- {
		row := chain[i]

		if row.AllowedModels != nil {
			if allowed == nil {
				// make + copy, NOT append([]string(nil), ...): appending zero
				// elements to a nil slice returns nil, not a non-nil empty
				// slice, when *row.AllowedModels is itself a deliberate empty
				// "deny all" ([]string{}, never nil per PolicyRow's doc
				// comment). A nil *[]string here would still Allow()/lock-out
				// correctly in-process (nil and empty slices behave
				// identically to len()/range), but JSON-marshals as
				// "allowed_models": null — indistinguishable from "every
				// model" to every HTTP consumer of this package (GET/POST
				// /v1/policy/effective) once it leaves the process. make()
				// always returns non-nil, even for length 0.
				cp := make([]string, len(*row.AllowedModels))
				copy(cp, *row.AllowedModels)
				allowed = &cp
			} else {
				narrowed := intersect(*allowed, *row.AllowedModels)
				allowed = &narrowed
			}
			if !haveAllowedSource {
				eff.Sources.AllowedModels = row.Level
				haveAllowedSource = true
			}
		}

		if row.Residency != nil {
			if rank := residencyRankOf(*row.Residency); rank > residencyBestRank {
				residencyBestRank = rank
				eff.Residency = residencyForRank(rank)
				eff.Sources.Residency = row.Level
			}
		}

		if !haveDefaultSource && row.DefaultModel != nil {
			dm := *row.DefaultModel
			eff.DefaultModel = &dm
			eff.Sources.DefaultModel = row.Level
			haveDefaultSource = true
		}
	}

	eff.AllowedModels = allowed
	return eff
}

// PolicyOverride is a hypothetical substitution at exactly one level of a
// chain returned by Snapshot.ChainFor, used by POST /v1/policy/effective --
// the gateway's impact-preview surface -- to answer "what would this
// principal's effective policy be if this level's row were X" without
// writing anything to principal_routing_policies. Level is one of the
// PrincipalType* constants. Policy nil means "inherit": remove whatever row
// this level currently has and replace it with an unrestricted one, exactly
// like DELETE .../routing-policy does today.
type PolicyOverride struct {
	Level  string
	Policy *PolicyRow
}

// Apply returns a copy of chain (in Snapshot.ChainFor's canonical order) with
// the row whose Level equals o.Level replaced by o.Policy (Level is forced to
// o.Level regardless of what the caller set it to, so a mistake there can
// never silently substitute the wrong slot). If no row in chain has that
// Level -- only possible for Level == PrincipalTypeAPIKey when chain was
// built without a key id, since ChainFor never omits org or user -- chain is
// returned unmodified; HandleEffectivePolicyOverride rejects an api_key-level
// override with no api_key_id before ever reaching here.
func (o PolicyOverride) Apply(chain []PolicyRow) []PolicyRow {
	out := append([]PolicyRow(nil), chain...)
	for i, row := range out {
		if row.Level != o.Level {
			continue
		}
		if o.Policy == nil {
			out[i] = PolicyRow{Level: o.Level}
		} else {
			p := *o.Policy
			p.Level = o.Level
			out[i] = p
		}
		break
	}
	return out
}

// intersect returns the elements of a that are also present in b, preserving
// a's order. Used to narrow the running allowlist as Effective walks the
// chain — bounded by the (small, admin-configured) allowlist sizes, never by
// anything request-scoped.
func intersect(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, m := range b {
		set[m] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, m := range a {
		if _, ok := set[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

// EffectiveFor returns the effective policy for one principal, using the
// snapshot's precomputed KeyPolicies/UserPolicies/OrgPolicy — no merge work
// happens on this path, only map lookups and a pointer dereference.
//
// Resolution order:
//  1. keyID, when non-empty AND keyType is not KeyTypeService: KeyPolicies[keyID],
//     if that api_key has an explicit row (its precomputed EffectivePolicy
//     already folds in its owner's user row and the org row — see
//     buildPrincipalPolicies).
//  2. UserPolicies[userID], if that user has an explicit row (already folds
//     in the org row).
//  3. OrgPolicy — shared by every principal with no row of its own. This is
//     the "principals with no rows share the org set by pointer" case: every
//     caller reaching this branch receives a copy of the exact same
//     precomputed value, no per-request or per-principal allocation.
//
// A service key's keyID is never consulted (service_keys never gets a row —
// "whoever holds the service key can impersonate" is the honest limit
// documented on the enforcement side, not something this method can fix).
//
// If OrgPolicy itself is nil — only possible for a Snapshot built by hand
// (e.g. a policy.Snapshot{Enabled: ..., Residency: ...} literal in a test, or
// conceivably a pre-migration-015 Snapshot with none of the new fields
// populated) — this falls back to the plain Residency field with no
// allowlist/default, so pre-existing callers and tests that only set
// Enabled/Aliases/Residency keep working unchanged.
func (s Snapshot) EffectiveFor(userID, keyID, keyType string) EffectivePolicy {
	if keyID != "" && keyType != KeyTypeService {
		if kp, ok := s.KeyPolicies[keyID]; ok {
			return *kp
		}
	}
	if up, ok := s.UserPolicies[userID]; ok {
		return *up
	}
	if s.OrgPolicy != nil {
		return *s.OrgPolicy
	}
	return EffectivePolicy{
		Residency: s.Residency,
		Sources:   PolicySources{AllowedModels: SourceNone, Residency: SourceNone, DefaultModel: SourceNone},
	}
}
