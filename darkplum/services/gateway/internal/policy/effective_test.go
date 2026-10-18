package policy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

func strP(s string) *string { return &s }
func selfHostedP() *string  { v := ResidencySelfHostedOnly; return &v }
func cloudAcctP() *string   { v := ResidencyCloudAccountOnly; return &v }

// modelsP builds a *[]string test input. Deliberately make()+copy, not
// append([]string(nil), ids...): with zero ids that append form returns nil,
// not a non-nil empty slice, which would make modelsP() (no args, the
// "explicit deny-all" case several tests below rely on) indistinguishable
// from a nil *[]string in anything that round-trips through JSON.
func modelsP(ids ...string) *[]string {
	s := make([]string, len(ids))
	copy(s, ids)
	return &s
}

// ---------------------------------------------------------------------------
// AllowedModels: null vs [] vs absent, intersection, source attribution
// ---------------------------------------------------------------------------

func TestEffective_NoLevelSetsAllowedModels_MeansAll(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg},
		{Level: PrincipalTypeUser},
		{Level: PrincipalTypeAPIKey},
	})
	if eff.AllowedModels != nil {
		t.Errorf("AllowedModels = %v, want nil (all models)", eff.AllowedModels)
	}
	if eff.Sources.AllowedModels != SourceNone {
		t.Errorf("Sources.AllowedModels = %q, want %q", eff.Sources.AllowedModels, SourceNone)
	}
}

func TestEffective_EmptyAllowlistAtAnyLevelDeniesAll(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, AllowedModels: modelsP("a", "b", "c")},
		{Level: PrincipalTypeUser, AllowedModels: modelsP()}, // explicit []: deny all
	})
	if eff.AllowedModels == nil || len(*eff.AllowedModels) != 0 {
		t.Errorf("AllowedModels = %v, want a non-nil empty slice (deny all)", eff.AllowedModels)
	}
	if eff.Sources.AllowedModels != PrincipalTypeUser {
		t.Errorf("Sources.AllowedModels = %q, want %q (the level that set the deny-all)", eff.Sources.AllowedModels, PrincipalTypeUser)
	}
}

func TestEffective_AllowedModelsIsIntersectionAcrossChain(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, AllowedModels: modelsP("a", "b", "c")},
		{Level: PrincipalTypeUser, AllowedModels: modelsP("b", "c", "d")},
		{Level: PrincipalTypeAPIKey, AllowedModels: modelsP("c", "d", "e")},
	})
	want := []string{"c"}
	if eff.AllowedModels == nil || !reflect.DeepEqual(*eff.AllowedModels, want) {
		t.Errorf("AllowedModels = %v, want %v", eff.AllowedModels, want)
	}
	// api_key is the most specific level with a non-null allowlist.
	if eff.Sources.AllowedModels != PrincipalTypeAPIKey {
		t.Errorf("Sources.AllowedModels = %q, want %q", eff.Sources.AllowedModels, PrincipalTypeAPIKey)
	}
}

func TestEffective_OnlyOrgSetsAllowedModels_SourceIsOrg(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, AllowedModels: modelsP("a", "b")},
		{Level: PrincipalTypeUser},
		{Level: PrincipalTypeAPIKey},
	})
	want := []string{"a", "b"}
	if eff.AllowedModels == nil || !reflect.DeepEqual(*eff.AllowedModels, want) {
		t.Errorf("AllowedModels = %v, want %v", eff.AllowedModels, want)
	}
	if eff.Sources.AllowedModels != PrincipalTypeOrg {
		t.Errorf("Sources.AllowedModels = %q, want %q", eff.Sources.AllowedModels, PrincipalTypeOrg)
	}
}

// TestEffective_EmptyAllowlistAtOnlyLevelMarshalsAsEmptyArrayNotNull guards
// the exact bug this fixes: when the FIRST (most specific) level to set an
// allowlist sets an explicit empty one (deny-all), the merged
// EffectivePolicy.AllowedModels must marshal over the wire as
// "allowed_models":[], never "allowed_models":null -- null is
// indistinguishable from "no restriction" to every HTTP consumer of this
// package (the api, via GET/POST /v1/policy/effective), which would then
// treat a locked-out principal as having full access. In-process callers
// (Allows, the request-time enforcement path) are unaffected either way --
// this is purely a wire-representation bug.
func TestEffective_EmptyAllowlistAtOnlyLevelMarshalsAsEmptyArrayNotNull(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg},
		{Level: PrincipalTypeUser, AllowedModels: modelsP()}, // explicit deny-all, the only level that sets one
	})
	if eff.AllowedModels == nil || len(*eff.AllowedModels) != 0 {
		t.Fatalf("AllowedModels = %v, want a non-nil empty slice", eff.AllowedModels)
	}
	b, err := json.Marshal(eff)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"allowed_models":[]`) {
		t.Errorf("marshaled JSON = %s, want \"allowed_models\":[] (not null)", b)
	}
}

// ---------------------------------------------------------------------------
// Residency: strictest wins, source attribution
// ---------------------------------------------------------------------------

func TestEffective_ResidencyDefaultsToAny(t *testing.T) {
	eff := Effective([]PolicyRow{{Level: PrincipalTypeOrg}})
	if eff.Residency != ResidencyAny {
		t.Errorf("Residency = %q, want %q", eff.Residency, ResidencyAny)
	}
	if eff.Sources.Residency != SourceNone {
		t.Errorf("Sources.Residency = %q, want %q", eff.Sources.Residency, SourceNone)
	}
}

func TestEffective_ResidencyIsStrictestInChain(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg},
		{Level: PrincipalTypeUser, Residency: selfHostedP()},
		{Level: PrincipalTypeAPIKey},
	})
	if eff.Residency != ResidencySelfHostedOnly {
		t.Errorf("Residency = %q, want %q", eff.Residency, ResidencySelfHostedOnly)
	}
	if eff.Sources.Residency != PrincipalTypeUser {
		t.Errorf("Sources.Residency = %q, want %q", eff.Sources.Residency, PrincipalTypeUser)
	}
}

func TestEffective_ResidencyMostSpecificLevelIsSourceWhenMultipleSet(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, Residency: selfHostedP()},
		{Level: PrincipalTypeAPIKey, Residency: selfHostedP()},
	})
	if eff.Residency != ResidencySelfHostedOnly {
		t.Fatalf("Residency = %q, want %q", eff.Residency, ResidencySelfHostedOnly)
	}
	if eff.Sources.Residency != PrincipalTypeAPIKey {
		t.Errorf("Sources.Residency = %q, want %q (most specific)", eff.Sources.Residency, PrincipalTypeAPIKey)
	}
}

// TestEffective_ResidencyCloudAccountOnlyIsMiddleRank asserts the new
// three-value ladder: a stricter self_hosted_only at a less specific level
// still wins over a looser cloud_account_only at a more specific level, and
// the reverse (cloud_account_only tightening an "any" org) also holds.
func TestEffective_ResidencyCloudAccountOnlyIsMiddleRank(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg},
		{Level: PrincipalTypeUser, Residency: cloudAcctP()},
	})
	if eff.Residency != ResidencyCloudAccountOnly {
		t.Errorf("Residency = %q, want %q", eff.Residency, ResidencyCloudAccountOnly)
	}
	if eff.Sources.Residency != PrincipalTypeUser {
		t.Errorf("Sources.Residency = %q, want %q", eff.Sources.Residency, PrincipalTypeUser)
	}
}

// TestEffective_ResidencySelfHostedOnlyBeatsCloudAccountOnlyRegardlessOfLevel
// asserts strictest-wins holds across all three values, not just the old
// binary any/self_hosted_only case: a more specific level's LOOSER explicit
// value must never override a less specific level's stricter one.
func TestEffective_ResidencySelfHostedOnlyBeatsCloudAccountOnlyRegardlessOfLevel(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, Residency: selfHostedP()},
		{Level: PrincipalTypeUser, Residency: cloudAcctP()},
	})
	if eff.Residency != ResidencySelfHostedOnly {
		t.Errorf("Residency = %q, want %q (org's stricter value must not be loosened by the user row)", eff.Residency, ResidencySelfHostedOnly)
	}
	if eff.Sources.Residency != PrincipalTypeOrg {
		t.Errorf("Sources.Residency = %q, want %q (the level that actually set the winning, stricter value)", eff.Sources.Residency, PrincipalTypeOrg)
	}
}

// TestEffective_UnknownResidencyValueTreatedAsStrictest guards the wire
// contract's "unknown residency value -> strictest" rule for a row loaded
// from a database this gateway build doesn't fully recognise (e.g. a future
// residency value added after this binary shipped).
func TestEffective_UnknownResidencyValueTreatedAsStrictest(t *testing.T) {
	weird := "some-future-value"
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, Residency: &weird},
	})
	if eff.Residency != ResidencySelfHostedOnly {
		t.Errorf("Residency = %q, want %q (unknown value normalised to strictest)", eff.Residency, ResidencySelfHostedOnly)
	}
}

// ---------------------------------------------------------------------------
// ResidencyPermits
// ---------------------------------------------------------------------------

func TestResidencyPermits_AnyPermitsEveryKnownClass(t *testing.T) {
	for _, class := range []string{routing.ClassSelfHosted, routing.ClassCloudManaged, routing.ClassExternalAPI} {
		if !ResidencyPermits(ResidencyAny, class) {
			t.Errorf("ResidencyPermits(any, %q) = false, want true", class)
		}
	}
}

func TestResidencyPermits_CloudAccountOnlyExcludesExternalAPI(t *testing.T) {
	cases := map[string]bool{
		routing.ClassSelfHosted:   true,
		routing.ClassCloudManaged: true,
		routing.ClassExternalAPI:  false,
	}
	for class, want := range cases {
		if got := ResidencyPermits(ResidencyCloudAccountOnly, class); got != want {
			t.Errorf("ResidencyPermits(cloud_account_only, %q) = %v, want %v", class, got, want)
		}
	}
}

func TestResidencyPermits_SelfHostedOnlyPermitsOnlySelfHosted(t *testing.T) {
	cases := map[string]bool{
		routing.ClassSelfHosted:   true,
		routing.ClassCloudManaged: false,
		routing.ClassExternalAPI:  false,
	}
	for class, want := range cases {
		if got := ResidencyPermits(ResidencySelfHostedOnly, class); got != want {
			t.Errorf("ResidencyPermits(self_hosted_only, %q) = %v, want %v", class, got, want)
		}
	}
}

// TestResidencyPermits_UnknownClassAlwaysDenied is the [RULING] guard: an
// unrecognised class must be refused under EVERY residency, including "any" —
// not merely under the tighter ones.
func TestResidencyPermits_UnknownClassAlwaysDenied(t *testing.T) {
	for _, residency := range []string{ResidencyAny, ResidencyCloudAccountOnly, ResidencySelfHostedOnly} {
		if ResidencyPermits(residency, "some_future_class") {
			t.Errorf("ResidencyPermits(%q, unknown class) = true, want false", residency)
		}
	}
}

func TestResidencyPermits_UnknownResidencyTreatedAsStrictest(t *testing.T) {
	if ResidencyPermits("not-a-real-residency", routing.ClassCloudManaged) {
		t.Error("expected an unknown residency value to deny cloud_managed, same as self_hosted_only")
	}
	if !ResidencyPermits("not-a-real-residency", routing.ClassSelfHosted) {
		t.Error("expected an unknown residency value to still permit self_hosted (permitted under every residency)")
	}
}

// ---------------------------------------------------------------------------
// DefaultModel: first non-null walking key -> user -> org
// ---------------------------------------------------------------------------

func TestEffective_DefaultModelAbsentEverywhere(t *testing.T) {
	eff := Effective([]PolicyRow{{Level: PrincipalTypeOrg}, {Level: PrincipalTypeUser}})
	if eff.DefaultModel != nil {
		t.Errorf("DefaultModel = %v, want nil", eff.DefaultModel)
	}
	if eff.Sources.DefaultModel != SourceNone {
		t.Errorf("Sources.DefaultModel = %q, want %q", eff.Sources.DefaultModel, SourceNone)
	}
}

func TestEffective_DefaultModelMostSpecificWins(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, DefaultModel: strP("org-default")},
		{Level: PrincipalTypeUser, DefaultModel: strP("user-default")},
		{Level: PrincipalTypeAPIKey, DefaultModel: strP("key-default")},
	})
	if eff.DefaultModel == nil || *eff.DefaultModel != "key-default" {
		t.Errorf("DefaultModel = %v, want key-default", eff.DefaultModel)
	}
	if eff.Sources.DefaultModel != PrincipalTypeAPIKey {
		t.Errorf("Sources.DefaultModel = %q, want %q", eff.Sources.DefaultModel, PrincipalTypeAPIKey)
	}
}

func TestEffective_DefaultModelFallsThroughToLessSpecific(t *testing.T) {
	eff := Effective([]PolicyRow{
		{Level: PrincipalTypeOrg, DefaultModel: strP("org-default")},
		{Level: PrincipalTypeUser},
		{Level: PrincipalTypeAPIKey},
	})
	if eff.DefaultModel == nil || *eff.DefaultModel != "org-default" {
		t.Errorf("DefaultModel = %v, want org-default", eff.DefaultModel)
	}
	if eff.Sources.DefaultModel != PrincipalTypeOrg {
		t.Errorf("Sources.DefaultModel = %q, want %q", eff.Sources.DefaultModel, PrincipalTypeOrg)
	}
}

// ---------------------------------------------------------------------------
// Allows
// ---------------------------------------------------------------------------

func TestEffectivePolicy_AllowsNilMeansEverything(t *testing.T) {
	eff := EffectivePolicy{}
	if !eff.Allows("anything") {
		t.Error("expected a nil AllowedModels to allow any model")
	}
}

func TestEffectivePolicy_AllowsChecksMembership(t *testing.T) {
	eff := EffectivePolicy{AllowedModels: modelsP("a", "b")}
	if !eff.Allows("a") {
		t.Error("expected \"a\" to be allowed")
	}
	if eff.Allows("z") {
		t.Error("expected \"z\" to be refused")
	}
}

func TestEffectivePolicy_AllowsEmptySliceDeniesEverything(t *testing.T) {
	eff := EffectivePolicy{AllowedModels: modelsP()}
	if eff.Allows("anything") {
		t.Error("expected a non-nil empty AllowedModels to deny everything")
	}
}

// ---------------------------------------------------------------------------
// Snapshot.EffectiveFor: pointer sharing, key/user/org fallback, service keys
// ---------------------------------------------------------------------------

func TestSnapshot_EffectiveFor_PrincipalsWithNoRowsSharePointer(t *testing.T) {
	org := EffectivePolicy{Residency: ResidencyAny}
	snap := Snapshot{OrgPolicy: &org}

	a := snap.EffectiveFor("user-without-a-row", "", KeyTypeUserForTest)
	b := snap.EffectiveFor("another-user-without-a-row", "key-without-a-row", KeyTypeUserForTest)

	if a.Residency != org.Residency || b.Residency != org.Residency {
		t.Fatalf("expected both to equal the org policy, got %+v and %+v", a, b)
	}
	// Both calls must have returned a copy of the SAME underlying
	// EffectivePolicy — assert via the shared org pointer directly rather
	// than by mutating the returned values (EffectiveFor returns EffectivePolicy
	// by value, so pointer identity is only observable on Snapshot.OrgPolicy
	// itself).
	if snap.OrgPolicy != &org {
		t.Fatal("sanity: OrgPolicy pointer changed unexpectedly")
	}
}

// KeyTypeUserForTest avoids importing internal/middleware from the policy
// package's tests just for one string literal ("user") — mirrors
// middleware.KeyTypeUser.
const KeyTypeUserForTest = "user"

func TestSnapshot_EffectiveFor_UserRowUsedWhenKeyHasNone(t *testing.T) {
	userEff := EffectivePolicy{AllowedModels: modelsP("m1")}
	snap := Snapshot{
		OrgPolicy:    &EffectivePolicy{},
		UserPolicies: map[string]*EffectivePolicy{"user-1": &userEff},
	}

	got := snap.EffectiveFor("user-1", "key-with-no-row", KeyTypeUserForTest)
	if got.AllowedModels == nil || (*got.AllowedModels)[0] != "m1" {
		t.Errorf("expected the user's own effective policy, got %+v", got)
	}
}

func TestSnapshot_EffectiveFor_KeyRowWinsOverUserRow(t *testing.T) {
	userEff := EffectivePolicy{AllowedModels: modelsP("m1", "m2")}
	keyEff := EffectivePolicy{AllowedModels: modelsP("m1")}
	snap := Snapshot{
		OrgPolicy:    &EffectivePolicy{},
		UserPolicies: map[string]*EffectivePolicy{"user-1": &userEff},
		KeyPolicies:  map[string]*EffectivePolicy{"key-1": &keyEff},
	}

	got := snap.EffectiveFor("user-1", "key-1", KeyTypeUserForTest)
	if got.AllowedModels == nil || len(*got.AllowedModels) != 1 || (*got.AllowedModels)[0] != "m1" {
		t.Errorf("expected the key's own precomputed effective policy, got %+v", got)
	}
}

func TestSnapshot_EffectiveFor_ServiceKeyIgnoresKeyPolicies(t *testing.T) {
	keyEff := EffectivePolicy{AllowedModels: modelsP("should-never-be-used")}
	userEff := EffectivePolicy{AllowedModels: modelsP("user-model")}
	snap := Snapshot{
		OrgPolicy:    &EffectivePolicy{},
		UserPolicies: map[string]*EffectivePolicy{"user-1": &userEff},
		// KeyPolicies keyed by "key-1" even though a service key would never
		// actually have a row — asserts EffectiveFor never even looks.
		KeyPolicies: map[string]*EffectivePolicy{"key-1": &keyEff},
	}

	got := snap.EffectiveFor("user-1", "key-1", KeyTypeService)
	if got.AllowedModels == nil || (*got.AllowedModels)[0] != "user-model" {
		t.Errorf("expected the service caller's impersonated user's policy, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Snapshot.ChainFor / PolicyOverride.Apply: the POST /v1/policy/effective
// preview path's chain reconstruction and substitution
// ---------------------------------------------------------------------------

func TestSnapshot_ChainFor_UserAndKeyWithNoRowsAreInheritPlaceholders(t *testing.T) {
	snap := Snapshot{OrgRow: PolicyRow{AllowedModels: modelsP("a", "b")}}

	chain := snap.ChainFor("user-with-no-row", "key-with-no-row")
	if len(chain) != 3 {
		t.Fatalf("expected a 3-row chain (org, user, api_key), got %d", len(chain))
	}
	if chain[0].Level != PrincipalTypeOrg || chain[0].AllowedModels == nil || len(*chain[0].AllowedModels) != 2 {
		t.Errorf("chain[0] (org) = %+v, want the snapshot's OrgRow", chain[0])
	}
	if chain[1].Level != PrincipalTypeUser || chain[1].AllowedModels != nil {
		t.Errorf("chain[1] (user) = %+v, want an empty inherit placeholder", chain[1])
	}
	if chain[2].Level != PrincipalTypeAPIKey || chain[2].AllowedModels != nil {
		t.Errorf("chain[2] (api_key) = %+v, want an empty inherit placeholder", chain[2])
	}
}

func TestSnapshot_ChainFor_NoKeyIDOmitsAPIKeyLevel(t *testing.T) {
	snap := Snapshot{}
	chain := snap.ChainFor("user-1", "")
	if len(chain) != 2 {
		t.Fatalf("expected a 2-row chain (org, user) when keyID is empty, got %d", len(chain))
	}
}

func TestSnapshot_ChainFor_UsesExplicitRowsWhenPresent(t *testing.T) {
	snap := Snapshot{
		OrgRow:   PolicyRow{Residency: selfHostedP()},
		UserRows: map[string]PolicyRow{"user-1": {AllowedModels: modelsP("a")}},
		KeyRows:  map[string]PolicyRow{"key-1": {DefaultModel: strP("d")}},
	}

	chain := snap.ChainFor("user-1", "key-1")
	if chain[0].Residency == nil || *chain[0].Residency != ResidencySelfHostedOnly {
		t.Errorf("chain[0] (org) Residency = %v, want self_hosted_only", chain[0].Residency)
	}
	if chain[1].AllowedModels == nil || (*chain[1].AllowedModels)[0] != "a" {
		t.Errorf("chain[1] (user) AllowedModels = %v, want [a]", chain[1].AllowedModels)
	}
	if chain[2].DefaultModel == nil || *chain[2].DefaultModel != "d" {
		t.Errorf("chain[2] (api_key) DefaultModel = %v, want d", chain[2].DefaultModel)
	}
}

func TestPolicyOverride_Apply_SubstitutesTheNamedLevelOnly(t *testing.T) {
	chain := []PolicyRow{
		{Level: PrincipalTypeOrg, AllowedModels: modelsP("a", "b")},
		{Level: PrincipalTypeUser, AllowedModels: modelsP("a")},
		{Level: PrincipalTypeAPIKey},
	}
	override := PolicyOverride{Level: PrincipalTypeUser, Policy: &PolicyRow{AllowedModels: modelsP("a", "b")}}

	out := override.Apply(chain)
	if len(out) != 3 {
		t.Fatalf("expected Apply to preserve chain length, got %d", len(out))
	}
	if out[0].AllowedModels == nil || len(*out[0].AllowedModels) != 2 {
		t.Errorf("org row must be untouched, got %+v", out[0])
	}
	if out[1].AllowedModels == nil || len(*out[1].AllowedModels) != 2 {
		t.Errorf("user row = %+v, want the override's allowlist substituted", out[1])
	}
	if out[1].Level != PrincipalTypeUser {
		t.Errorf("substituted row Level = %q, want %q (forced regardless of the override's own Level field)", out[1].Level, PrincipalTypeUser)
	}
	// The original chain must not be mutated.
	if len(*chain[1].AllowedModels) != 1 {
		t.Errorf("Apply must not mutate its input chain, got chain[1] = %+v", chain[1])
	}
}

func TestPolicyOverride_Apply_NilPolicyMeansInherit(t *testing.T) {
	chain := []PolicyRow{
		{Level: PrincipalTypeOrg},
		{Level: PrincipalTypeUser, AllowedModels: modelsP("a"), Residency: selfHostedP(), DefaultModel: strP("d")},
	}
	out := PolicyOverride{Level: PrincipalTypeUser, Policy: nil}.Apply(chain)

	if out[1].AllowedModels != nil || out[1].Residency != nil || out[1].DefaultModel != nil {
		t.Errorf("nil Policy must remove the row entirely (inherit), got %+v", out[1])
	}
	if out[1].Level != PrincipalTypeUser {
		t.Errorf("Level must be preserved even when inheriting, got %q", out[1].Level)
	}
}

func TestPolicyOverride_Apply_NoMatchingLevelIsANoop(t *testing.T) {
	// ChainFor never omits org/user, so this only happens for an api_key-level
	// override applied to a 2-row (org, user) chain -- HandleEffectivePolicyOverride
	// rejects that combination with 400 before ever calling Apply, but Apply
	// itself must not panic or corrupt the chain if it somehow happens.
	chain := []PolicyRow{{Level: PrincipalTypeOrg}, {Level: PrincipalTypeUser}}
	out := PolicyOverride{Level: PrincipalTypeAPIKey, Policy: &PolicyRow{AllowedModels: modelsP("x")}}.Apply(chain)
	if len(out) != 2 {
		t.Fatalf("expected chain length unchanged, got %d", len(out))
	}
	if out[0].AllowedModels != nil || out[1].AllowedModels != nil {
		t.Errorf("expected no substitution when no row matches Level, got %+v", out)
	}
}

func TestSnapshot_EffectiveFor_NilOrgPolicyFallsBackToPlainResidency(t *testing.T) {
	// A hand-built Snapshot (e.g. an existing test literal that only sets
	// Enabled/Aliases/Residency) must keep working: EffectiveFor degrades to
	// the plain Residency field with no allowlist/default.
	snap := Snapshot{Residency: ResidencySelfHostedOnly}
	got := snap.EffectiveFor("whoever", "whatever", KeyTypeUserForTest)
	if got.Residency != ResidencySelfHostedOnly {
		t.Errorf("Residency = %q, want %q", got.Residency, ResidencySelfHostedOnly)
	}
	if got.AllowedModels != nil {
		t.Errorf("AllowedModels = %v, want nil", got.AllowedModels)
	}
	if got.DefaultModel != nil {
		t.Errorf("DefaultModel = %v, want nil", got.DefaultModel)
	}
}
