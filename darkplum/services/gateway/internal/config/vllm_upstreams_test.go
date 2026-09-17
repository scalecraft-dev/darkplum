package config

import "testing"

func TestParseVLLMUpstreams_Empty(t *testing.T) {
	got, err := parseVLLMUpstreams("", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestParseVLLMUpstreams_ValidMultiple(t *testing.T) {
	raw := "team-a=http://vllm-team-a.darkplum-inference.svc.cluster.local:8000,team-b=http://vllm-team-b.darkplum-inference.svc.cluster.local:8000"
	got, err := parseVLLMUpstreams(raw, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 upstreams, got %d (%v)", len(got), got)
	}
	if got[0].Name != "team-a" || got[1].Name != "team-b" {
		t.Errorf("unexpected names: %+v", got)
	}
}

func TestParseVLLMUpstreams_InvalidCases(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"missing equals", "team-a-http://host:8000"},
		{"empty url", "team-a="},
		{"uppercase name rejected", "TeamA=http://vllm-inference.darkplum-inference.svc.cluster.local:8000"},
		{"name too long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa=http://vllm-inference.darkplum-inference.svc.cluster.local:8000"},
		{"name with invalid chars", "team_a=http://vllm-inference.darkplum-inference.svc.cluster.local:8000"},
		{"reserved name default", "default=http://vllm-inference.darkplum-inference.svc.cluster.local:8000"},
		{"duplicate name", "team-a=http://vllm-inference.darkplum-inference.svc.cluster.local:8000,team-a=http://other.darkplum-inference.svc.cluster.local:8000"},
		{"loopback url rejected in prod", "team-a=http://127.0.0.1:8000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseVLLMUpstreams(tc.raw, false); err == nil {
				t.Errorf("expected an error for %q", tc.raw)
			}
		})
	}
}

func TestParseVLLMUpstreams_LoopbackAllowedInDevMode(t *testing.T) {
	got, err := parseVLLMUpstreams("dev=http://127.0.0.1:8000", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].URL != "http://127.0.0.1:8000" {
		t.Errorf("unexpected result: %v", got)
	}
}

func TestParseGlobList_Empty(t *testing.T) {
	got, err := parseGlobList("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestParseGlobList_Valid(t *testing.T) {
	got, err := parseGlobList("us.anthropic.*, us.meta.*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "us.anthropic.*" || got[1] != "us.meta.*" {
		t.Errorf("unexpected result: %v", got)
	}
}

func TestParseGlobList_InvalidPattern(t *testing.T) {
	if _, err := parseGlobList("["); err == nil {
		t.Error("expected an error for a malformed glob pattern")
	}
}
