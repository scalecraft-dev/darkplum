package libkey

import (
	"errors"
	"strings"
	"testing"
)

func TestForItem(t *testing.T) {
	k, err := ForItem(ScopeUser, "a-b-x-com", "legal/contracts", "skill", "nda-review")
	if err != nil || k.String() != "people/a-b-x-com/legal/contracts/nda-review.skill.md" {
		t.Fatalf("got %q, %v", k, err)
	}
	k, _ = ForItem(ScopeOrg, "ignored", "", "tool", "jira")
	if k.String() != "org/jira.tool.json" {
		t.Errorf("org key = %q", k)
	}
	k, _ = ForItem(ScopeOrg, "", "a", "context", "x")
	if k.String() != "org/a/x.context.md" {
		t.Errorf("context key = %q", k)
	}
}

func TestForItemRejects(t *testing.T) {
	long := strings.Repeat("a", 65)
	cases := map[string][4]string{ // folderPath, kind, slug, person
		"traversal":      {"../etc", "skill", "x", "p"},
		"dot folder":     {"a/./b", "skill", "x", "p"},
		"empty segment":  {"a//b", "skill", "x", "p"},
		"uppercase":      {"Legal", "skill", "x", "p"},
		"unicode":        {"légal", "skill", "x", "p"},
		"segment len":    {long, "skill", "x", "p"},
		"depth 6":        {"a/b/c/d/e/f", "skill", "x", "p"},
		"reserved dir":   {"con", "skill", "x", "p"},
		"reserved slug":  {"", "skill", "nul", "p"},
		"reserved com1":  {"", "skill", "com1", "p"},
		"slash in slug":  {"", "skill", "a/b", "p"},
		"slug len":       {"", "skill", strings.Repeat("a", 101), "p"},
		"bad kind":       {"", "agent", "x", "p"},
		"empty person":   {"", "skill", "x", ""},
		"person slash":   {"", "skill", "x", "a/b"},
		"leading hyphen": {"-a", "skill", "x", "p"},
	}
	for name, c := range cases {
		if _, err := ForItem(ScopeUser, c[3], c[0], c[1], c[2]); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := ForItem("bogus", "p", "", "skill", "x"); !errors.Is(err, ErrInvalid) {
		t.Error("bogus scope accepted")
	}
	// Five 64-char folders + a 100-char slug stays under the cap.
	seg := strings.Repeat("a", 64)
	if _, err := ForItem(ScopeUser, seg, strings.Join([]string{seg, seg, seg, seg, seg}, "/"), "context", strings.Repeat("b", 100)); err != nil {
		t.Errorf("worst legal key refused: %v", err)
	}
}

func TestForFolderAndParseRoundTrip(t *testing.T) {
	k, err := ForFolder(ScopeUser, "me", "a/b")
	if err != nil || k.String() != "people/me/a/b/" {
		t.Fatalf("marker = %q, %v", k, err)
	}
	if _, err := ForFolder(ScopeOrg, "", ""); err == nil {
		t.Error("root marker accepted")
	}
	in, _ := ForItem(ScopeUser, "me", "a/b", "tool", "jira")
	loc, err := Parse(in.String())
	if err != nil || loc != (Location{Scope: ScopeUser, Person: "me", FolderPath: "a/b", Kind: "tool", Slug: "jira"}) {
		t.Errorf("parse = %+v, %v", loc, err)
	}
	for _, bad := range []string{"", "other/x.skill.md", "org/x.md", "org/", "people/me/x.skill.md/", "people/x.skill.md", "org/../x.skill.md", "org/A.skill.md"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestClass(t *testing.T) {
	if Class("org/a/x.skill.md") != "org/" || Class("people/me/a/x.skill.md") != "people/me/" || Class("x") != "" {
		t.Error("Class wrong")
	}
}
