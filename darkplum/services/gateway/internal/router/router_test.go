package router

import (
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
)

func TestNormalizeRouteToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already normalized", "computer-science", "computer-science"},
		{"spaces to dashes", "computer science", "computer-science"},
		{"underscores to dashes", "computer_science", "computer-science"},
		{"mixed case", "Computer Science", "computer-science"},
		{"leading/trailing whitespace trimmed", "  computer science  ", "computer-science"},
		{"collapses repeated separators", "computer   science", "computer-science"},
		{"collapses mixed repeated separators", "computer_ -science", "computer-science"},
		{"trailing separator trimmed", "computer_science_", "computer-science"},
		{"leading separator trimmed", "_computer_science", "computer-science"},
		{"single word unchanged", "general", "general"},
		{"empty string", "", ""},
		{"only separators", "   ___", ""},
		{"other sentinel", "other", "other"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRouteToken(tc.in); got != tc.want {
				t.Errorf("normalizeRouteToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMatchRouteName_LooseMatchesFallBackAfterEqualFold(t *testing.T) {
	routes := []policy.RouteDef{
		{Name: "computer-science"},
		{Name: "general"},
	}

	cases := []struct {
		name      string
		category  string
		wantRoute string
		wantOK    bool
	}{
		{"exact case-insensitive match (EqualFold path)", "Computer-Science", "computer-science", true},
		{"space-separated category matches dashed route", "computer science", "computer-science", true},
		{"underscore-separated category matches dashed route", "Computer_Science", "computer-science", true},
		{"no match", "astrology", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, ok := matchRouteName(routes, tc.category)
			if ok != tc.wantOK || name != tc.wantRoute {
				t.Errorf("matchRouteName(%q) = (%q, %v), want (%q, %v)", tc.category, name, ok, tc.wantRoute, tc.wantOK)
			}
		})
	}
}
