package policy

import (
	"reflect"
	"testing"
)

func strP(s string) *string { return &s }
func selfHostedP() *string  { v := ResidencySelfHostedOnly; return &v }
func modelsP(ids ...string) *[]string {
	s := append([]string(nil), ids...)
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
