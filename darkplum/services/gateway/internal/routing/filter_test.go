package routing

import "testing"

func TestApplyGlobFilter_EmptyPatternsReturnsAll(t *testing.T) {
	ids := []string{"us.anthropic.claude-3-5-sonnet", "us.meta.llama3-70b"}
	out, err := ApplyGlobFilter(ids, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != len(ids) {
		t.Fatalf("expected all ids to pass through, got %v", out)
	}
}

func TestApplyGlobFilter_MatchesSubset(t *testing.T) {
	ids := []string{
		"us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		"us.meta.llama3-70b-instruct-v1:0",
		"us.mistral.mistral-large-v1:0",
	}
	out, err := ApplyGlobFilter(ids, []string{"us.anthropic.*", "us.meta.*"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 matches, got %d (%v)", len(out), out)
	}
	got := map[string]bool{out[0]: true}
	if len(out) > 1 {
		got[out[1]] = true
	}
	if !got["us.anthropic.claude-3-5-sonnet-20241022-v2:0"] || !got["us.meta.llama3-70b-instruct-v1:0"] {
		t.Errorf("unexpected filtered set: %v", out)
	}
}

func TestApplyGlobFilter_NoMatchesReturnsEmpty(t *testing.T) {
	ids := []string{"us.mistral.mistral-large-v1:0"}
	out, err := ApplyGlobFilter(ids, []string{"us.anthropic.*"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected no matches, got %v", out)
	}
}

func TestApplyGlobFilter_InvalidPatternErrors(t *testing.T) {
	_, err := ApplyGlobFilter([]string{"anything"}, []string{"["})
	if err == nil {
		t.Fatal("expected an error for a malformed glob pattern")
	}
}
