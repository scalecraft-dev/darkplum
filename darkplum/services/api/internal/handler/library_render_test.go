package handler

import (
	"strings"
	"testing"
	"time"
)

func TestRender_YamlScalarQuotesControlCharacters(t *testing.T) {
	for _, in := range []string{"a\tb", "a\rb", "a\nb", "x: y", " lead"} {
		if got := yamlScalar(in); !strings.HasPrefix(got, `"`) {
			t.Errorf("yamlScalar(%q) = %q, want quoted", in, got)
		}
	}
	if yamlScalar("plain name") != "plain name" {
		t.Error("plain scalar quoted")
	}
}

func TestRender_NormalizeBeforeHashing(t *testing.T) {
	a := objectSHA256([]byte("x\ny\n"))
	for _, v := range []string{"x\ny", "x\ny\n\n\n", "x\r\ny\r\n"} {
		if objectSHA256([]byte(v)) != a {
			t.Errorf("%q hashes differently from the canonical form", v)
		}
	}
	if objectSHA256([]byte("x\nz\n")) == a {
		t.Error("different content hashes the same")
	}
}

func TestRender_FrontMatterCarriesIDAndRoundTrips(t *testing.T) {
	it := libraryItem{ID: "11111111-1111-4111-8111-111111111111", Kind: "skill", Name: "A\tb", Description: "c: d", Slug: "a-b", Body: "text\n\n"}
	_, _, b := renderExport(it)
	if !strings.HasPrefix(string(b), "---\ndarkplum_id: "+it.ID+"\n") {
		t.Fatalf("bytes = %q", b)
	}
	if objectItemID("skill", string(b)) != it.ID {
		t.Error("id not read back")
	}
	in, _, msg := parseImport(importRequest{Filename: "a-b.md", Content: string(b), Kind: "skill"})
	if msg != "" || in.Name != "A\tb" || in.Description != "c: d" || in.Body != "text" {
		t.Errorf("parse = %+v %q", in, msg)
	}
	tool := libraryItem{ID: it.ID, Kind: "tool", Name: "T", Slug: "t", Tool: &libraryToolInfo{ServerURL: "https://h.example/x"}}
	_, _, tb := renderExport(tool)
	if objectItemID("tool", string(tb)) != it.ID || strings.Contains(string(tb), "credential") {
		t.Errorf("tool bytes = %s", tb)
	}
	if objectItemID("skill", "---\ndarkplum_id: not-a-uuid\n---\n") != "" {
		t.Error("invalid id accepted")
	}
}

func TestSyncBackoffGrowsAndCaps(t *testing.T) {
	if syncBackoff(1) != 5*time.Second || syncBackoff(2) != 10*time.Second {
		t.Error("early backoff wrong")
	}
	if syncBackoff(100) != 15*time.Minute {
		t.Errorf("cap = %v", syncBackoff(100))
	}
}
