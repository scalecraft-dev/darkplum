package pricing

import (
	"math/big"
	"testing"
	"time"
)

func rat(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }
func sp(s string) *string   { return &s }

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestMatchSpecificity(t *testing.T) {
	mk := func(id, prov, pat string, region *string, from time.Time, to *time.Time) Price {
		return Price{ID: id, Provider: prov, ModelPattern: pat, Region: region, Input: rat("1"), Output: rat("2"), EffectiveFrom: from, EffectiveTo: to}
	}
	at := t0.Add(24 * time.Hour)
	later := t0.Add(48 * time.Hour)
	cases := []struct {
		name     string
		prices   []Price
		provider string
		model    string
		region   *string
		t        time.Time
		want     string // ID, "" = unpriced
	}{
		{"longer literal prefix wins", []Price{mk("a", "anthropic", "claude-*", nil, t0, nil), mk("b", "anthropic", "claude-sonnet-*", nil, t0, nil)}, "anthropic", "claude-sonnet-4", nil, at, "b"},
		{"exact beats glob", []Price{mk("a", "anthropic", "claude-sonnet-*", nil, t0, nil), mk("b", "anthropic", "claude-sonnet-4", nil, t0, nil)}, "anthropic", "claude-sonnet-4", nil, at, "b"},
		{"region-specific over NULL", []Price{mk("a", "bedrock", "m*", nil, t0, nil), mk("b", "bedrock", "m*", sp("eu-west-1"), t0, nil)}, "bedrock", "m1", sp("eu-west-1"), at, "b"},
		{"region mismatch falls to NULL", []Price{mk("a", "bedrock", "m*", nil, t0, nil), mk("b", "bedrock", "m*", sp("eu-west-1"), t0, nil)}, "bedrock", "m1", sp("us-east-1"), at, "a"},
		{"event without region ignores regional row", []Price{mk("b", "bedrock", "m*", sp("eu-west-1"), t0, nil)}, "bedrock", "m1", nil, at, ""},
		{"latest effective_from wins", []Price{mk("a", "openai", "gpt-*", nil, t0, nil), mk("b", "openai", "gpt-*", nil, t0.Add(time.Hour), nil)}, "openai", "gpt-4", nil, at, "b"},
		{"prefix beats region", []Price{mk("a", "bedrock", "m*", sp("eu"), t0, nil), mk("b", "bedrock", "mo*", nil, t0, nil)}, "bedrock", "mo1", sp("eu"), at, "b"},
		{"before effective_from", []Price{mk("a", "openai", "gpt-*", nil, later, nil)}, "openai", "gpt-4", nil, at, ""},
		{"effective_to exclusive", []Price{mk("a", "openai", "gpt-*", nil, t0, &at)}, "openai", "gpt-4", nil, at, ""},
		{"effective_from inclusive", []Price{mk("a", "openai", "gpt-*", nil, at, nil)}, "openai", "gpt-4", nil, at, "a"},
		{"closed row yields to successor", []Price{mk("a", "openai", "gpt-*", nil, t0, &later), mk("b", "openai", "gpt-*", nil, later, nil)}, "openai", "gpt-4", nil, later, "b"},
		{"provider mismatch", []Price{mk("a", "openai", "*", nil, t0, nil)}, "anthropic", "x", nil, at, ""},
		{"glob star does not cross slash", []Price{mk("a", "vllm", "meta-llama/*", nil, t0, nil)}, "vllm", "meta-llama/L3", nil, at, "a"},
		{"global model prices at global row despite in-region match", []Price{
			mk("g", "bedrock", "global.anthropic.claude-haiku-4-5*", sp("global"), t0, nil),
			mk("r", "bedrock", "*anthropic.claude-haiku-4-5*", sp("us-east-1"), t0, nil),
			mk("n", "bedrock", "*anthropic.claude-haiku-4-5*", nil, t0, nil)}, "bedrock", "global.anthropic.claude-haiku-4-5-20251001-v1:0", sp("us-east-1"), at, "g"},
		{"global model with no global row is unpriced", []Price{
			mk("r", "bedrock", "*anthropic.claude-haiku-4-5*", sp("us-east-1"), t0, nil),
			mk("n", "bedrock", "*anthropic.claude-haiku-4-5*", nil, t0, nil)}, "bedrock", "global.anthropic.claude-haiku-4-5-20251001-v1:0", sp("us-east-1"), at, ""},
		{"us model prices at in-region row, never global", []Price{
			mk("g", "bedrock", "*anthropic.claude-haiku-4-5*", sp("global"), t0, nil),
			mk("r", "bedrock", "*anthropic.claude-haiku-4-5*", sp("us-east-1"), t0, nil)}, "bedrock", "us.anthropic.claude-haiku-4-5-20251001-v1:0", sp("us-east-1"), at, "r"},
		{"global row never prices a non-global model", []Price{
			mk("g", "bedrock", "*anthropic.claude-haiku-4-5*", sp("global"), t0, nil)}, "bedrock", "us.anthropic.claude-haiku-4-5-20251001-v1:0", sp("global"), at, ""},
		{"no match", []Price{mk("a", "openai", "gpt-5*", nil, t0, nil)}, "openai", "gpt-4", nil, at, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Match(tc.prices, tc.provider, tc.model, tc.region, tc.t)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want unpriced, got %s", got.ID)
				}
				return
			}
			if got == nil || got.ID != tc.want {
				t.Fatalf("want %s, got %v", tc.want, got)
			}
		})
	}
}

func TestCostCents(t *testing.T) {
	p := Price{Input: rat("3"), Output: rat("15"), CacheRead: rat("0.3"), CacheWrite: rat("3.75")}
	// 1M uncached*3 + 1M cr*0.3 + 1M cw*3.75 + 1M out*15 = $22.05 = 2205 cents
	got := FormatCents(p.CostCents(Usage{Uncached: 1e6, CacheRead: 1e6, CacheCreate: 1e6, Completion: 1e6}))
	if got != "2205.0000" {
		t.Fatalf("got %s", got)
	}
	// Fallback: NULL cache prices bill at input.
	q := Price{Input: rat("3"), Output: rat("15")}
	got = FormatCents(q.CostCents(Usage{Uncached: 0, CacheRead: 1e6, CacheCreate: 1e6}))
	if got != "600.0000" {
		t.Fatalf("fallback got %s", got)
	}
	// Small: 1000 in at $3/M = 0.3 cents.
	if got := FormatCents(q.CostCents(Usage{Uncached: 1000})); got != "0.3000" {
		t.Fatalf("small got %s", got)
	}
	if got := FormatCents(q.CostCents(Usage{})); got != "0.0000" {
		t.Fatalf("zero got %s", got)
	}
}

func TestUncachedPromptClamps(t *testing.T) {
	if UncachedPrompt(100, 30, 20) != 50 || UncachedPrompt(10, 30, 20) != 0 {
		t.Fatal("clamp")
	}
}

func TestValidation(t *testing.T) {
	if ValidatePattern("[") == nil || ValidatePattern("") == nil || ValidatePattern("claude-*") != nil {
		t.Fatal("pattern validation")
	}
	for _, bad := range []string{"-1", "abc", "1/2", "1000000"} {
		if _, err := ParseRate(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
	if FormatRate(rat("3.000000")) != "3" || FormatRate(rat("0.30")) != "0.3" || FormatRate(rat("0")) != "0" {
		t.Fatal("FormatRate")
	}
}

func TestBoundaries(t *testing.T) {
	to := t0.Add(time.Hour)
	b := Boundaries([]Price{{EffectiveFrom: t0, EffectiveTo: &to}, {EffectiveFrom: to}})
	if len(b) != 2 || !b[0].Equal(t0) || !b[1].Equal(to) {
		t.Fatalf("%v", b)
	}
}

func TestParseSeedRejectsBad(t *testing.T) {
	for _, s := range []string{
		`{"entries":[{"provider":"nope","model_pattern":"x","input_per_mtok":1,"output_per_mtok":1}]}`,
		`{"entries":[{"provider":"openai","model_pattern":"[","input_per_mtok":1,"output_per_mtok":1}]}`,
		`{"entries":[{"provider":"openai","model_pattern":"x","input_per_mtok":1}]}`,
		`{"entries":[{"provider":"openai","model_pattern":"x","input_per_mtok":-1,"output_per_mtok":1}]}`,
	} {
		if _, err := parseSeed([]byte(s)); err == nil {
			t.Fatalf("should reject %s", s)
		}
	}
	if _, err := parseSeed(seedJSON); err != nil {
		t.Fatalf("embedded seed invalid: %v", err)
	}
}

func TestParseSeedAcceptsAsOfNotesAndGlobalRegion(t *testing.T) {
	data := []byte(`{"generated_at":"x","sources":[],"as_of_notes":{"bedrock":"note"},"entries":[
		{"provider":"bedrock","model_pattern":"global.anthropic.*","region":"global","input_per_mtok":1,"output_per_mtok":5}]}`)
	f, err := parseSeed(data)
	if err != nil || len(f.Entries) != 1 || f.Entries[0].Region == nil || *f.Entries[0].Region != "global" {
		t.Fatalf("parse: %v %+v", err, f)
	}
}
