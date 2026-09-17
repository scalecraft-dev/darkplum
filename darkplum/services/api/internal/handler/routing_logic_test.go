package handler

import "testing"

func TestValidAliasFormat(t *testing.T) {
	valid := []string{
		"default",
		"a",
		"gpt-4o",
		"gpt_4o",
		"gpt.4o",
		"a0",
		"x123456789012345678901234567890123456789012", // 44 chars, well under 63
	}
	for _, v := range valid {
		if !validAliasFormat(v) {
			t.Errorf("validAliasFormat(%q) = false, want true", v)
		}
	}

	invalid := []string{
		"",
		"-leading-dash",
		".leading-dot",
		"Uppercase",
		"has space",
		"has/slash",
		"emoji-🚀",
		string(make([]byte, 64)), // too long and NUL bytes, definitely invalid
	}
	for _, v := range invalid {
		if validAliasFormat(v) {
			t.Errorf("validAliasFormat(%q) = true, want false", v)
		}
	}
}

func TestValidAliasFormat_LengthBoundary(t *testing.T) {
	// CHECK constraint: '^[a-z0-9][a-z0-9._-]{0,62}$' => max 63 characters total.
	ok63 := "a" + repeat("b", 62)
	if !validAliasFormat(ok63) {
		t.Errorf("expected 63-char alias to be valid")
	}
	tooLong := "a" + repeat("b", 63)
	if validAliasFormat(tooLong) {
		t.Errorf("expected 64-char alias to be invalid")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestValidResidency(t *testing.T) {
	if !validResidency("any") {
		t.Error(`validResidency("any") = false, want true`)
	}
	if !validResidency("self_hosted_only") {
		t.Error(`validResidency("self_hosted_only") = false, want true`)
	}
	for _, v := range []string{"", "ANY", "self-hosted-only", "cloud_only", "bogus"} {
		if validResidency(v) {
			t.Errorf("validResidency(%q) = true, want false", v)
		}
	}
}
