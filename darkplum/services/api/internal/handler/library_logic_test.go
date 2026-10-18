package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Contract Review":        "contract-review",
		"  --Hello, World!--  ":  "hello-world",
		"ünïcode & symbols":      "n-code-symbols",
		"":                       "item",
		"!!!":                    "item",
		strings.Repeat("a", 150): strings.Repeat("a", 100),
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFrontMatter(t *testing.T) {
	keys, dropped, body := frontMatter("---\nname: Contract review\ndescription: \"Reads: contracts\"\nkind: skill\nversion: 2\ntools: [a, b]\n  nested: x\n---\n\nDo the thing.\n")
	if keys["name"] != "Contract review" || keys["description"] != "Reads: contracts" || keys["kind"] != "skill" {
		t.Errorf("keys = %v", keys)
	}
	if strings.Join(dropped, ",") != "version,tools,nested: x" {
		t.Errorf("dropped = %v", dropped)
	}
	if body != "Do the thing.\n" {
		t.Errorf("body = %q", body)
	}
	// No front matter: everything is body.
	keys, dropped, body = frontMatter("# Title\n\ntext")
	if len(keys) != 0 || dropped != nil || body != "# Title\n\ntext" {
		t.Errorf("plain: keys=%v dropped=%v body=%q", keys, dropped, body)
	}
	// Unterminated fence: treated as body, not swallowed.
	_, _, body = frontMatter("---\nname: x\nno end")
	if !strings.HasPrefix(body, "---") {
		t.Errorf("unterminated: body = %q", body)
	}
}

func TestParseImport(t *testing.T) {
	// A Claude-style SKILL.md: the body is taken, the extra keys are listed.
	in, pv, msg := parseImport(importRequest{Filename: "SKILL.md", Kind: "skill", Content: "---\nname: pdf-tools\ndescription: Work with PDFs\nallowed-tools: Bash\n---\n# Steps\n1. Read\n"})
	if msg != "" {
		t.Fatalf("msg = %q", msg)
	}
	if in.Kind != "skill" || in.Name != "pdf-tools" || in.Description != "Work with PDFs" || !strings.HasPrefix(in.Body, "# Steps") {
		t.Errorf("in = %+v", in)
	}
	if len(pv.Dropped) != 1 || pv.Dropped[0] != "allowed-tools" || pv.Slug != "pdf-tools" {
		t.Errorf("preview = %+v", pv)
	}
	// Front matter kind overrides the tab; filename names the item.
	in, _, msg = parseImport(importRequest{Filename: "style-guide.md", Kind: "skill", Content: "---\nkind: context\n---\nUse British spelling."})
	if msg != "" || in.Kind != "context" || in.Name != "style guide" {
		t.Errorf("in = %+v msg=%q", in, msg)
	}
	// A tool definition.
	in, pv, msg = parseImport(importRequest{Filename: "jira.json", Content: `{"kind":"tool","name":"Jira","server_url":"https://mcp.corp.example/jira","allowed_tools":["search"],"secret":"x"}`})
	if msg != "" || in.Kind != "tool" || in.Name != "Jira" {
		t.Fatalf("tool import: in=%+v msg=%q", in, msg)
	}
	var cfg toolConfig
	if err := json.Unmarshal([]byte(in.Body), &cfg); err != nil || cfg.ServerURL != "https://mcp.corp.example/jira" || len(cfg.AllowedTools) != 1 {
		t.Errorf("tool body = %s", in.Body)
	}
	if len(pv.Dropped) != 1 || pv.Dropped[0] != "secret" {
		t.Errorf("tool dropped = %v", pv.Dropped)
	}
	// Refusals.
	for name, req := range map[string]importRequest{
		"empty body":        {Filename: "x.md", Kind: "skill", Content: "---\nname: x\n---\n"},
		"tool via md":       {Filename: "x.md", Kind: "skill", Content: "---\nkind: tool\n---\nbody"},
		"json not a tool":   {Filename: "x.json", Content: `{"kind":"skill","name":"x"}`},
		"json without url":  {Filename: "x.json", Content: `{"name":"x"}`},
		"bad url scheme":    {Filename: "x.json", Content: `{"name":"x","server_url":"ftp://h/p"}`},
		"url with userinfo": {Filename: "x.json", Content: `{"name":"x","server_url":"https://u:p@h/p"}`},
	} {
		if _, _, msg := parseImport(req); msg == "" {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func TestRenderExportRoundTrip(t *testing.T) {
	it := libraryItem{Kind: "skill", Name: "Contract: review", Slug: "contract-review", Description: "it's \"quoted\"", Body: "Line 1\n\nLine 2"}
	name, ct, body := renderExport(it)
	if name != "contract-review.md" || !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("name=%q ct=%q", name, ct)
	}
	in, _, msg := parseImport(importRequest{Filename: name, Kind: "context", Content: string(body)})
	if msg != "" || in.Kind != "skill" || in.Name != it.Name || in.Description != it.Description || in.Body != it.Body {
		t.Errorf("round trip: in=%+v msg=%q\nfile:\n%s", in, msg, body)
	}
	tool := libraryItem{Kind: "tool", Name: "Jira", Slug: "jira", Tool: &libraryToolInfo{ServerURL: "https://mcp.corp.example/jira"}}
	tool.Tool.Credential.Set = true
	tool.Tool.Credential.Last4 = "abcd"
	name, _, body = renderExport(tool)
	if name != "jira.json" || strings.Contains(string(body), "abcd") || strings.Contains(string(body), "credential") {
		t.Errorf("tool export leaked credential info: %s", body)
	}
}

func TestValidateLibraryInput(t *testing.T) {
	ok := libraryInput{Kind: "skill", Name: " Name ", Description: "d", Body: "b"}
	if msg := validateLibraryInput(&ok, false); msg != "" || ok.Name != "Name" {
		t.Errorf("ok: %q %+v", msg, ok)
	}
	for name, in := range map[string]libraryInput{
		"bad kind":    {Kind: "x", Name: "n", Body: "b"},
		"no name":     {Kind: "skill", Name: " ", Body: "b"},
		"no body":     {Kind: "skill", Name: "n", Body: " "},
		"big body":    {Kind: "skill", Name: "n", Body: strings.Repeat("x", libraryBodyMaxBytes+1)},
		"bad slug":    {Kind: "skill", Name: "n", Body: "b", Slug: strPtr("Bad Slug")},
		"bad vis":     {Kind: "skill", Name: "n", Body: "b", Visibility: strPtr("public")},
		"tool body":   {Kind: "tool", Name: "n", Body: "not json"},
		"tool extra":  {Kind: "tool", Name: "n", Body: `{"server_url":"https://h/p","token":"x"}`},
		"tool no url": {Kind: "tool", Name: "n", Body: `{}`},
	} {
		in := in
		if msg := validateLibraryInput(&in, false); msg == "" {
			t.Errorf("%s: expected a refusal", name)
		}
	}
	tool := libraryInput{Kind: "tool", Name: "n", Body: `{"server_url": " https://mcp.corp.example/x ", "allowed_tools": [" a "]}`}
	if msg := validateLibraryInput(&tool, false); msg != "" || tool.Body != `{"server_url":"https://mcp.corp.example/x","allowed_tools":["a"]}` {
		t.Errorf("tool normalised body = %s (%q)", tool.Body, msg)
	}
}

func TestParseImport_BOMOnJSON(t *testing.T) {
	in, _, msg := parseImport(importRequest{Filename: "t.json", Content: "\xEF\xBB\xBF{\"name\":\"T\",\"server_url\":\"https://mcp.corp.example/x\"}"})
	if msg != "" || in.Kind != "tool" || in.Name != "T" {
		t.Errorf("in=%+v msg=%q", in, msg)
	}
}

func TestToolHostTooBroad(t *testing.T) {
	for host, bad := range map[string]bool{"*.com": true, "*.corp.example": false, "mcp.corp.example": false, "169.254.169.254": true, "127.0.0.1": false, "0.0.0.0": true, "10.0.0.5": false} {
		if got := toolHostTooBroad(host) != ""; got != bad {
			t.Errorf("toolHostTooBroad(%q) refused=%v, want %v", host, got, bad)
		}
	}
}
