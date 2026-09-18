package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

func strs(ss ...string) *[]string { return &ss }

func TestModelAllowed(t *testing.T) {
	if !modelAllowed(nil, "anything") {
		t.Fatal("nil (unrestricted) must allow any id")
	}
	restricted := strs("a", "b")
	if !modelAllowed(restricted, "a") {
		t.Fatal("expected member id to be allowed")
	}
	if modelAllowed(restricted, "c") {
		t.Fatal("expected non-member id to be disallowed")
	}
	empty := strs()
	if modelAllowed(empty, "a") {
		t.Fatal("expected an empty allowlist to allow nothing")
	}
}

func TestValidPrincipalResidency(t *testing.T) {
	self := residencySelfHostedOnly
	any := residencyAny
	cases := []struct {
		name string
		in   *string
		want bool
	}{
		{"nil (inherit) is valid", nil, true},
		{"self_hosted_only is valid", &self, true},
		{"any is rejected (nothing to override with)", &any, false},
		{"garbage is rejected", strPtrLocal("bogus"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPrincipalResidency(tc.in); got != tc.want {
				t.Errorf("validPrincipalResidency(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func strPtrLocal(s string) *string { return &s }

func TestOrgPolicyTightened_Residency(t *testing.T) {
	if !orgPolicyTightened(residencyAny, residencySelfHostedOnly, nil, nil) {
		t.Fatal("any -> self_hosted_only must be a tighten")
	}
	if orgPolicyTightened(residencySelfHostedOnly, residencyAny, nil, nil) {
		t.Fatal("self_hosted_only -> any must not be a tighten")
	}
	if orgPolicyTightened(residencyAny, residencyAny, nil, nil) {
		t.Fatal("no residency change must not be a tighten")
	}
}

func TestOrgPolicyTightened_AllowedModels(t *testing.T) {
	if !orgPolicyTightened(residencyAny, residencyAny, nil, strs("a")) {
		t.Fatal("nil -> restricted must be a tighten")
	}
	if orgPolicyTightened(residencyAny, residencyAny, strs("a"), nil) {
		t.Fatal("restricted -> nil (loosening) must not be a tighten")
	}
	if !orgPolicyTightened(residencyAny, residencyAny, strs("a", "b"), strs("a")) {
		t.Fatal("dropping a previously-allowed id must be a tighten")
	}
	if orgPolicyTightened(residencyAny, residencyAny, strs("a"), strs("a", "b")) {
		t.Fatal("adding an id (superset) must not be a tighten")
	}
	if orgPolicyTightened(residencyAny, residencyAny, strs("a"), strs("a")) {
		t.Fatal("identical allowlist must not be a tighten")
	}
}

func TestParseDisplayName_AbsentIsNoChange(t *testing.T) {
	value, changed, err := parseDisplayName(nil)
	if err != nil || changed || value != nil {
		t.Fatalf("parseDisplayName(nil) = (%v, %v, %v), want (nil, false, nil)", value, changed, err)
	}
}

func TestParseDisplayName_NullClears(t *testing.T) {
	raw := json.RawMessage("null")
	value, changed, err := parseDisplayName(raw)
	if err != nil || !changed || value != nil {
		t.Fatalf("parseDisplayName(null) = (%v, %v, %v), want (nil, true, nil)", value, changed, err)
	}
}

func TestParseDisplayName_SetsValue(t *testing.T) {
	raw := json.RawMessage(`"Llama 3 (fast)"`)
	value, changed, err := parseDisplayName(raw)
	if err != nil || !changed || value == nil || *value != "Llama 3 (fast)" {
		t.Fatalf("parseDisplayName = (%v, %v, %v), want (\"Llama 3 (fast)\", true, nil)", value, changed, err)
	}
}

func TestParseDisplayName_LengthBounds(t *testing.T) {
	empty := json.RawMessage(`""`)
	if _, _, err := parseDisplayName(empty); err == nil {
		t.Fatal("expected an error for an empty display_name")
	}
	tooLong := json.RawMessage(`"` + strings.Repeat("a", 101) + `"`)
	if _, _, err := parseDisplayName(tooLong); err == nil {
		t.Fatal("expected an error for a display_name over 100 characters")
	}
	exactly100 := json.RawMessage(`"` + strings.Repeat("a", 100) + `"`)
	if _, changed, err := parseDisplayName(exactly100); err != nil || !changed {
		t.Fatalf("expected exactly-100-char display_name to be accepted, got err=%v changed=%v", err, changed)
	}
}

func TestParseDisplayName_NonStringErrors(t *testing.T) {
	raw := json.RawMessage(`42`)
	if _, _, err := parseDisplayName(raw); err == nil {
		t.Fatal("expected an error for a non-string, non-null display_name")
	}
}
