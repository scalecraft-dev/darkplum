package router

import (
	"context"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
)

func aliasWithRoutes(routes ...policy.RouteDef) policy.AliasDef {
	return policy.AliasDef{
		Kind:         policy.AliasKindRouted,
		DefaultRoute: "general",
		Routes:       append(routes, policy.RouteDef{Name: "general", Description: "default", Target: "llama-3-8b"}),
	}
}

func TestDecideRules_NoMatchFallsBackToDefault(t *testing.T) {
	alias := aliasWithRoutes(policy.RouteDef{Name: "code", Target: "qwen-coder", Keywords: []string{"debug"}})

	d := DecideRules(alias, Input{Text: "what's the weather like"})

	if d.RouteName != "general" || d.Reason != ReasonRulesDefault {
		t.Errorf("got %+v", d)
	}
}

func TestDecideRules_LiteralKeywordMatchIsCaseFolded(t *testing.T) {
	alias := aliasWithRoutes(policy.RouteDef{Name: "code", Target: "qwen-coder", Keywords: []string{"debug"}})

	d := DecideRules(alias, Input{Text: "please help me DEBUG this function"})

	if d.RouteName != "code" {
		t.Fatalf("expected route code, got %+v", d)
	}
	if d.Reason != "rules:code:debug" {
		t.Errorf("reason = %q, want rules:code:debug", d.Reason)
	}
}

// TestDecideRules_NoRegex asserts a regex metacharacter in a keyword is
// treated as a literal substring, not a pattern.
func TestDecideRules_NoRegex(t *testing.T) {
	alias := aliasWithRoutes(policy.RouteDef{Name: "special", Target: "m", Keywords: []string{"a.b"}})

	if d := DecideRules(alias, Input{Text: "contains a.b literally"}); d.RouteName != "special" {
		t.Errorf("expected literal substring match, got %+v", d)
	}
	if d := DecideRules(alias, Input{Text: "contains axb, which a dot-any-char pattern would match but a literal substring must not"}); d.RouteName != "general" {
		t.Errorf("expected no match for a regex-only hit, got %+v", d)
	}
}

// TestDecideRules_LowestPositionWins: two routes could both match; the one
// earlier in alias.Routes order (lower position) must win.
func TestDecideRules_LowestPositionWins(t *testing.T) {
	alias := policy.AliasDef{
		Kind:         policy.AliasKindRouted,
		DefaultRoute: "general",
		Routes: []policy.RouteDef{
			{Name: "first", Target: "m1", Keywords: []string{"urgent"}},
			{Name: "second", Target: "m2", Keywords: []string{"urgent"}},
			{Name: "general", Target: "m3"},
		},
	}

	d := DecideRules(alias, Input{Text: "this is urgent"})
	if d.RouteName != "first" {
		t.Errorf("expected the lowest-position matching route to win, got %+v", d)
	}
}

func TestDecideRules_BuiltinCode_CodeFence(t *testing.T) {
	alias := aliasWithRoutes(policy.RouteDef{Name: "code", Target: "qwen-coder", Keywords: []string{BuiltinCode}})

	d := DecideRules(alias, Input{Text: "here is my code:\n```go\nfunc main() {}\n```"})
	if d.RouteName != "code" || d.Reason != "rules:code:"+BuiltinCode {
		t.Errorf("got %+v", d)
	}
}

func TestDecideRules_BuiltinCode_DiffHeader(t *testing.T) {
	alias := aliasWithRoutes(policy.RouteDef{Name: "code", Target: "qwen-coder", Keywords: []string{BuiltinCode}})

	for _, text := range []string{"diff --git a/x b/x", "@@ -1,3 +1,4 @@\n+added"} {
		if d := DecideRules(alias, Input{Text: text}); d.RouteName != "code" {
			t.Errorf("text %q: expected builtin:code to match, got %+v", text, d)
		}
	}
}

func TestDecideRules_BuiltinLong_ChecksOriginalLenNotTruncatedText(t *testing.T) {
	alias := aliasWithRoutes(policy.RouteDef{Name: "long", Target: "big-context-model", Keywords: []string{BuiltinLong}})

	// Text itself is short (as if already truncated to 2000 chars), but
	// OriginalLen (the untruncated length) exceeds the 8000 threshold.
	d := DecideRules(alias, Input{Text: "short", OriginalLen: 8001})
	if d.RouteName != "long" {
		t.Errorf("expected builtin:long to match on OriginalLen, got %+v", d)
	}

	d2 := DecideRules(alias, Input{Text: "short", OriginalLen: 8000})
	if d2.RouteName != "general" {
		t.Errorf("expected builtin:long NOT to match at exactly the threshold, got %+v", d2)
	}
}

func TestDecideRules_BuiltinMultimodal(t *testing.T) {
	alias := aliasWithRoutes(policy.RouteDef{Name: "vision", Target: "vlm", Keywords: []string{BuiltinMultimodal}})

	if d := DecideRules(alias, Input{Text: "describe this", HasNonText: true}); d.RouteName != "vision" {
		t.Errorf("expected builtin:multimodal to match, got %+v", d)
	}
	if d := DecideRules(alias, Input{Text: "describe this", HasNonText: false}); d.RouteName != "general" {
		t.Errorf("expected no match without a non-text block, got %+v", d)
	}
}

// TestRulesClassifier_ImplementsClassifierAndNeverAborts confirms the
// Classifier-interface wrapper never returns an error (rules are pure/sync).
func TestRulesClassifier_ImplementsClassifierAndNeverAborts(t *testing.T) {
	c := NewRulesClassifier()
	alias := aliasWithRoutes()
	d, err := c.Classify(context.Background(), alias, Input{Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.RouteName != "general" {
		t.Errorf("got %+v", d)
	}
}
