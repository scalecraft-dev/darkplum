package handler

import (
	"fmt"
	"strings"
	"testing"
)

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

func TestValidRouteName(t *testing.T) {
	ok32 := "a" + repeat("b", 31)    // 32 chars, max length
	tooLong := "a" + repeat("b", 32) // 33 chars

	valid := []string{"general", "code", "a", "route-1", "route_1", ok32}
	for _, v := range valid {
		if !validRouteName(v) {
			t.Errorf("validRouteName(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "-leading-dash", "_leading-underscore", "Uppercase", "has space", "has.dot", tooLong}
	for _, v := range invalid {
		if validRouteName(v) {
			t.Errorf("validRouteName(%q) = true, want false", v)
		}
	}
}

func TestValidRouteDescription(t *testing.T) {
	if validRouteDescription("") {
		t.Error("empty description should be invalid")
	}
	if !validRouteDescription("a") {
		t.Error("1-character description should be valid")
	}
	if !validRouteDescription(repeat("a", 500)) {
		t.Error("500-character description should be valid")
	}
	if validRouteDescription(repeat("a", 501)) {
		t.Error("501-character description should be invalid")
	}
}

func TestNormalizeKeyword(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"  Code  ", "code", true},
		{"BUILTIN:CODE", "builtin:code", true},
		{"", "", false},
		{"   ", "", false},
		{repeat("a", 64), repeat("a", 64), true},
		{repeat("a", 65), "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeKeyword(tc.in)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("normalizeKeyword(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func validRouteInput(name, target string) putAliasRouteInput {
	return putAliasRouteInput{RouteName: name, Description: "a description", TargetModelID: target, Keywords: nil}
}

func TestValidateRoutedAliasShape_Valid(t *testing.T) {
	req := putAliasRequest{
		DefaultRoute: "general",
		Classifier:   &putAliasClassifierInput{Kind: "rules"},
		Routes: []putAliasRouteInput{
			validRouteInput("general", "llama3"),
			{RouteName: "code", Description: "code stuff", TargetModelID: "llama3-code", Keywords: []string{"  Code  ", "builtin:code"}},
		},
	}
	normalized, err := validateRoutedAliasShape(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(normalized) != 2 {
		t.Fatalf("len(normalized) = %d, want 2", len(normalized))
	}
	if normalized[1].Keywords[0] != "code" || normalized[1].Keywords[1] != "builtin:code" {
		t.Errorf("keywords not case-folded/trimmed: %+v", normalized[1].Keywords)
	}
}

func TestValidateRoutedAliasShape_NoRoutes(t *testing.T) {
	_, err := validateRoutedAliasShape(putAliasRequest{DefaultRoute: "general", Classifier: &putAliasClassifierInput{Kind: "rules"}})
	if err == nil {
		t.Fatal("expected error for zero routes")
	}
}

func TestValidateRoutedAliasShape_InvalidRouteName(t *testing.T) {
	req := putAliasRequest{
		DefaultRoute: "General",
		Classifier:   &putAliasClassifierInput{Kind: "rules"},
		Routes:       []putAliasRouteInput{validRouteInput("General", "llama3")},
	}
	if _, err := validateRoutedAliasShape(req); err == nil {
		t.Fatal("expected error for invalid route_name")
	}
}

func TestValidateRoutedAliasShape_DuplicateRouteNames(t *testing.T) {
	req := putAliasRequest{
		DefaultRoute: "general",
		Classifier:   &putAliasClassifierInput{Kind: "rules"},
		Routes: []putAliasRouteInput{
			validRouteInput("general", "llama3"),
			validRouteInput("general", "llama3-v2"),
		},
	}
	_, err := validateRoutedAliasShape(req)
	if err == nil || !strings.Contains(err.Error(), "duplicate route name") {
		t.Fatalf("err = %v, want a duplicate route name error", err)
	}
}

func TestValidateRoutedAliasShape_DefaultRouteNotInRoutes(t *testing.T) {
	req := putAliasRequest{
		DefaultRoute: "missing",
		Classifier:   &putAliasClassifierInput{Kind: "rules"},
		Routes:       []putAliasRouteInput{validRouteInput("general", "llama3")},
	}
	_, err := validateRoutedAliasShape(req)
	if err == nil || !strings.Contains(err.Error(), "default_route") {
		t.Fatalf("err = %v, want a default_route error", err)
	}
}

func TestValidateRoutedAliasShape_TooManyKeywords(t *testing.T) {
	kws := make([]string, 33)
	for i := range kws {
		kws[i] = fmt.Sprintf("kw%d", i)
	}
	req := putAliasRequest{
		DefaultRoute: "general",
		Classifier:   &putAliasClassifierInput{Kind: "rules"},
		Routes:       []putAliasRouteInput{{RouteName: "general", Description: "d", TargetModelID: "llama3", Keywords: kws}},
	}
	if _, err := validateRoutedAliasShape(req); err == nil {
		t.Fatal("expected error for >32 keywords")
	}
}

func TestValidateRoutedAliasShape_ClassifierKindValidation(t *testing.T) {
	baseRoutes := []putAliasRouteInput{validRouteInput("general", "llama3")}

	cases := []struct {
		name       string
		classifier *putAliasClassifierInput
		wantErr    bool
	}{
		{"rules with no model id", &putAliasClassifierInput{Kind: "rules"}, false},
		{"rules with model id set", &putAliasClassifierInput{Kind: "rules", ModelID: strPtr("llama3")}, true},
		{"semantic_router with no model id", &putAliasClassifierInput{Kind: "semantic_router"}, false},
		{"semantic_router with model id set", &putAliasClassifierInput{Kind: "semantic_router", ModelID: strPtr("llama3")}, true},
		{"model kind is rejected outright, with model id", &putAliasClassifierInput{Kind: "model", ModelID: strPtr("llama3")}, true},
		{"model kind is rejected outright, without model id", &putAliasClassifierInput{Kind: "model"}, true},
		{"unknown kind", &putAliasClassifierInput{Kind: "bogus"}, true},
		{"nil classifier", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := putAliasRequest{DefaultRoute: "general", Classifier: tc.classifier, Routes: baseRoutes}
			_, err := validateRoutedAliasShape(req)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateRoutedAliasShape_ModelClassifierKindRejected asserts the exact
// 400 message a PUT with {"classifier":{"kind":"model"}} gets: the in-house
// model classifier is gone, and this must never be confused with the
// generic "unknown kind" error.
func TestValidateRoutedAliasShape_ModelClassifierKindRejected(t *testing.T) {
	req := putAliasRequest{
		DefaultRoute: "general",
		Classifier:   &putAliasClassifierInput{Kind: "model", ModelID: strPtr("llama3")},
		Routes:       []putAliasRouteInput{validRouteInput("general", "llama3")},
	}
	_, err := validateRoutedAliasShape(req)
	if err == nil {
		t.Fatal("expected an error for classifier kind 'model'")
	}
	wantMsg := "classifier kind 'model' is no longer supported; use 'rules' or 'semantic_router'"
	if err.Error() != wantMsg {
		t.Errorf("err = %q, want %q", err.Error(), wantMsg)
	}
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
