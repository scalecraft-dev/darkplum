package policy

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
	// Residency is nil (inherit) or a pointer to ResidencySelfHostedOnly.
	// There is no explicit "any" value at user/api_key level: only org can
	// mean "any" by being nil with nothing above it to inherit from —
	// tightening only ever adds a self_hosted_only somewhere in the chain,
	// never removes one a less specific level already set.
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
//   - Residency: the strictest value in the chain — self_hosted_only if ANY
//     level sets it, "any" otherwise. There is no way to loosen a
//     self_hosted_only set by a less specific level.
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
	haveResidencySource := false
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

		if row.Residency != nil && *row.Residency == ResidencySelfHostedOnly {
			eff.Residency = ResidencySelfHostedOnly
			if !haveResidencySource {
				eff.Sources.Residency = row.Level
				haveResidencySource = true
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
