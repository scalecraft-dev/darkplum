package policy

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// isUndefinedTableError
// ---------------------------------------------------------------------------

func TestIsUndefinedTableError_MatchesSQLSTATE42P01(t *testing.T) {
	err := &pgconn.PgError{Code: "42P01", Message: `relation "principal_routing_policies" does not exist`}
	if !isUndefinedTableError(err) {
		t.Error("expected a 42P01 PgError to be classified as undefined_table")
	}
}

func TestIsUndefinedTableError_OtherCodesAreNotClassified(t *testing.T) {
	err := &pgconn.PgError{Code: "42703", Message: "undefined_column"}
	if isUndefinedTableError(err) {
		t.Error("expected a non-42P01 PgError not to be classified as undefined_table")
	}
	if isUndefinedTableError(nil) {
		t.Error("expected a nil error not to be classified as undefined_table")
	}
}

// ---------------------------------------------------------------------------
// buildPrincipalPolicies: 42P01 tolerance (zero rows), org residency fallback
// ---------------------------------------------------------------------------

// TestBuildPrincipalPolicies_ZeroRowsFallsBackToRoutingPolicyResidency models
// the 42P01-tolerance path: loadFromDB passes rows=nil (the table doesn't
// exist yet) and the already-fetched routing_policy.residency as fallback.
func TestBuildPrincipalPolicies_ZeroRowsFallsBackToRoutingPolicyResidency(t *testing.T) {
	orgPolicy, users, keys, counts := buildPrincipalPolicies(nil, ResidencySelfHostedOnly)

	if orgPolicy == nil {
		t.Fatal("expected a non-nil org policy even with zero rows")
	}
	if orgPolicy.Residency != ResidencySelfHostedOnly {
		t.Errorf("org Residency = %q, want %q (from routing_policy fallback)", orgPolicy.Residency, ResidencySelfHostedOnly)
	}
	if orgPolicy.AllowedModels != nil {
		t.Errorf("org AllowedModels = %v, want nil", orgPolicy.AllowedModels)
	}
	if len(users) != 0 || len(keys) != 0 {
		t.Errorf("expected zero explicit user/key policies, got users=%d keys=%d", len(users), len(keys))
	}
	if counts != (PrincipalCounts{}) {
		t.Errorf("counts = %+v, want zero value", counts)
	}
}

func TestBuildPrincipalPolicies_ZeroRowsWithAnyResidencyFallback(t *testing.T) {
	orgPolicy, _, _, _ := buildPrincipalPolicies(nil, ResidencyAny)
	if orgPolicy.Residency != ResidencyAny {
		t.Errorf("org Residency = %q, want %q", orgPolicy.Residency, ResidencyAny)
	}
}

// TestBuildPrincipalPolicies_ExplicitOrgRowWinsOverFallback asserts that once
// the org row actually exists, it is authoritative — the routing_policy
// fallback is only consulted when no org row was found at all.
func TestBuildPrincipalPolicies_ExplicitOrgRowWinsOverFallback(t *testing.T) {
	rows := []rawPrincipalPolicyRow{
		{PrincipalType: PrincipalTypeOrg, Residency: nil}, // explicit "any"
	}
	orgPolicy, _, _, _ := buildPrincipalPolicies(rows, ResidencySelfHostedOnly)
	if orgPolicy.Residency != ResidencyAny {
		t.Errorf("org Residency = %q, want %q (explicit org row overrides the fallback)", orgPolicy.Residency, ResidencyAny)
	}
}

// ---------------------------------------------------------------------------
// buildPrincipalPolicies: chain assembly, including a key's owner lookup
// ---------------------------------------------------------------------------

func TestBuildPrincipalPolicies_UserRowChainsWithOrg(t *testing.T) {
	orgModels := []string{"a", "b"}
	userModels := []string{"a"}
	rows := []rawPrincipalPolicyRow{
		{PrincipalType: PrincipalTypeOrg, AllowedModels: &orgModels},
		{PrincipalType: PrincipalTypeUser, UserID: strP("user-1"), AllowedModels: &userModels},
	}
	_, users, _, counts := buildPrincipalPolicies(rows, ResidencyAny)

	up, ok := users["user-1"]
	if !ok {
		t.Fatal("expected user-1 to have a precomputed effective policy")
	}
	if up.AllowedModels == nil || len(*up.AllowedModels) != 1 || (*up.AllowedModels)[0] != "a" {
		t.Errorf("user-1 AllowedModels = %v, want [a] (intersected with org)", up.AllowedModels)
	}
	if counts.Users != 1 {
		t.Errorf("counts.Users = %d, want 1", counts.Users)
	}
}

// TestBuildPrincipalPolicies_KeyChainsThroughOwningUser is the core
// cross-table-join assertion: a key's effective policy must incorporate its
// OWNING USER's row, found via KeyOwnerUserID (api_keys.user_id), not just
// the org row and the key's own row.
func TestBuildPrincipalPolicies_KeyChainsThroughOwningUser(t *testing.T) {
	userModels := []string{"a", "b"}
	rows := []rawPrincipalPolicyRow{
		{PrincipalType: PrincipalTypeUser, UserID: strP("user-1"), AllowedModels: &userModels},
		{PrincipalType: PrincipalTypeAPIKey, APIKeyID: strP("key-1"), KeyOwnerUserID: strP("user-1")},
	}
	_, _, keys, counts := buildPrincipalPolicies(rows, ResidencyAny)

	kp, ok := keys["key-1"]
	if !ok {
		t.Fatal("expected key-1 to have a precomputed effective policy")
	}
	// The key row itself sets no AllowedModels, so the key's ceiling must be
	// exactly its owning user's allowlist — "a key can never reach more than
	// its owner."
	want := []string{"a", "b"}
	if kp.AllowedModels == nil || len(*kp.AllowedModels) != len(want) {
		t.Errorf("key-1 AllowedModels = %v, want %v (inherited from its owning user)", kp.AllowedModels, want)
	}
	if kp.Sources.AllowedModels != PrincipalTypeUser {
		t.Errorf("key-1 Sources.AllowedModels = %q, want %q", kp.Sources.AllowedModels, PrincipalTypeUser)
	}
	if counts.APIKeys != 1 {
		t.Errorf("counts.APIKeys = %d, want 1", counts.APIKeys)
	}
}

// TestBuildPrincipalPolicies_KeyOwnerWithNoRowOfItsOwn covers a key whose
// owner has never touched routing policy: the chain is just org+key.
func TestBuildPrincipalPolicies_KeyOwnerWithNoRowOfItsOwn(t *testing.T) {
	keyModels := []string{"only-this-one"}
	rows := []rawPrincipalPolicyRow{
		{PrincipalType: PrincipalTypeAPIKey, APIKeyID: strP("key-1"), KeyOwnerUserID: strP("user-with-no-row"), AllowedModels: &keyModels},
	}
	_, _, keys, _ := buildPrincipalPolicies(rows, ResidencyAny)

	kp := keys["key-1"]
	if kp == nil || kp.AllowedModels == nil || len(*kp.AllowedModels) != 1 || (*kp.AllowedModels)[0] != "only-this-one" {
		t.Errorf("got %+v, want AllowedModels=[only-this-one]", kp)
	}
}

// TestBuildPrincipalPolicies_MalformedRowsAreDroppedNotPanicked defends the
// principal_type CHECK constraint's invariants without trusting them to hold
// forever, mirroring buildAliases' drop-if-inconsistent tolerance.
func TestBuildPrincipalPolicies_MalformedRowsAreDroppedNotPanicked(t *testing.T) {
	rows := []rawPrincipalPolicyRow{
		{PrincipalType: PrincipalTypeUser, UserID: nil},
		{PrincipalType: PrincipalTypeAPIKey, APIKeyID: nil},
		{PrincipalType: "workspace"}, // not yet a supported level
	}
	_, users, keys, counts := buildPrincipalPolicies(rows, ResidencyAny)
	if len(users) != 0 || len(keys) != 0 || counts != (PrincipalCounts{}) {
		t.Errorf("expected every malformed/unrecognised row to be dropped, got users=%v keys=%v counts=%+v", users, keys, counts)
	}
}
