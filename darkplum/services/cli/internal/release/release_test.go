package release

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenDir is .github/scripts/testdata at the repository root: the manifests
// release-json.py writes, parsed here by the code that will trust them.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("..", "..", "..", "..", "..", ".github", "scripts", "testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsesWhatTheWorkflowWrites(t *testing.T) {
	m, err := Parse(golden(t, "release.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Channel != Release || m.Version != "1.2.3" || m.RunID != "42" || m.Images["api"].Digest != "sha256:"+strings.Repeat("2", 64) {
		t.Errorf("manifest = %+v", m)
	}
	if got := m.Images["gateway"].Ref(); got != "ghcr.io/scalecraft-dev/darkplum-gateway@sha256:"+strings.Repeat("1", 64) {
		t.Errorf("ref = %s", got)
	}
	rc, err := Parse(golden(t, "rc.golden.json"))
	if err != nil || rc.Channel != RC || rc.Images["web"].Repository != "ghcr.io/scalecraft-dev/darkplum-rc-web" {
		t.Errorf("rc manifest = %+v, %v", rc, err)
	}
}

func TestIdentityIsExactAndSeparatesReleaseFromRC(t *testing.T) {
	m, _ := Parse(golden(t, "release.golden.json"))
	id, err := m.Identity()
	if err != nil {
		t.Fatal(err)
	}
	gh := "https://github.com/scalecraft-dev/darkplum"
	want := Identity{
		Issuer:              "https://token.actions.githubusercontent.com",
		SAN:                 gh + "/.github/workflows/_build-sign.yml@refs/tags/v1.2.3",
		BuildConfigURI:      gh + "/.github/workflows/release.yml@refs/tags/v1.2.3",
		SourceRepositoryURI: gh,
		SourceRepositoryRef: "refs/tags/v1.2.3",
		Trigger:             "push",
		Commit:              strings.Repeat("a", 40),
		RunInvocationURI:    gh + "/actions/runs/42/attempts/1",
	}
	if id != want {
		t.Errorf("identity = %+v\nwant       %+v", id, want)
	}
	rcm, _ := Parse(golden(t, "rc.golden.json"))
	rid, _ := rcm.Identity()
	if rid.BuildConfigURI != gh+"/.github/workflows/prerelease.yml@refs/heads/prerelease/makefile-removal" || rid.BuildConfigURI == id.BuildConfigURI {
		t.Errorf("rc caller identity = %s", rid.BuildConfigURI)
	}
	for _, v := range []string{id.SAN, id.BuildConfigURI, id.RunInvocationURI} {
		if strings.ContainsAny(v, "*^$()[]|\\") {
			t.Errorf("identity value %q looks like a pattern", v)
		}
	}
}

func TestDevBuildHasNoIdentity(t *testing.T) {
	old := encoded
	defer func() { encoded = old }()
	encoded = ""
	m, err := Current()
	if err != nil || m.Channel != Dev {
		t.Fatalf("Current = %+v, %v", m, err)
	}
	if _, err := m.Identity(); err == nil {
		t.Error("a dev build produced an identity")
	}
	// A dev manifest cannot be forged by passing one to Parse.
	if _, err := Parse([]byte(`{"schema":1,"channel":"dev","version":"dev"}`)); err == nil {
		t.Error("Parse accepted a dev manifest")
	}
}

func TestCurrentReadsTheLinkerValue(t *testing.T) {
	old := encoded
	defer func() { encoded = old }()
	encoded = base64.StdEncoding.EncodeToString(golden(t, "release.golden.json"))
	m, err := Current()
	if err != nil || m.Version != "1.2.3" {
		t.Fatalf("Current = %+v, %v", m, err)
	}
	for _, bad := range []string{"!!!not base64", base64.StdEncoding.EncodeToString([]byte("{")), base64.StdEncoding.EncodeToString([]byte("[]"))} {
		encoded = bad
		if _, err := Current(); err == nil {
			t.Errorf("Current accepted %q", bad)
		}
	}
}

// Every way a manifest can be inconsistent is refused, whichever field is wrong.
func TestParseRefusals(t *testing.T) {
	var base map[string]any
	if err := json.Unmarshal(golden(t, "release.golden.json"), &base); err != nil {
		t.Fatal(err)
	}
	clone := func() map[string]any {
		b, _ := json.Marshal(base)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	img := func(m map[string]any, svc string) map[string]any {
		return m["images"].(map[string]any)[svc].(map[string]any)
	}
	cases := map[string]func(m map[string]any){
		"unknown field":                 func(m map[string]any) { m["extra"] = true },
		"schema 2":                      func(m map[string]any) { m["schema"] = 2 },
		"unknown channel":               func(m map[string]any) { m["channel"] = "beta" },
		"tag does not match version":    func(m map[string]any) { m["ref"] = "refs/tags/v1.2.4" },
		"release from a branch":         func(m map[string]any) { m["ref"] = "refs/heads/main" },
		"rc workflow on the release":    func(m map[string]any) { m["workflow"] = ".github/workflows/prerelease.yml" },
		"some other workflow":           func(m map[string]any) { m["workflow"] = ".github/workflows/evil.yml" },
		"short commit":                  func(m map[string]any) { m["commit"] = "abc" },
		"non numeric run":               func(m map[string]any) { m["run_id"] = "4x" },
		"other owner":                   func(m map[string]any) { m["repository"] = "evil/darkplum" },
		"missing image":                 func(m map[string]any) { delete(m["images"].(map[string]any), "web") },
		"extra image":                   func(m map[string]any) { m["images"].(map[string]any)["x"] = img(m, "web") },
		"tag instead of digest":         func(m map[string]any) { img(m, "api")["digest"] = "latest" },
		"short digest":                  func(m map[string]any) { img(m, "api")["digest"] = "sha256:abc" },
		"upper case digest":             func(m map[string]any) { img(m, "api")["digest"] = "sha256:" + strings.Repeat("A", 64) },
		"image from the rc repository":  func(m map[string]any) { img(m, "api")["repository"] = "ghcr.io/scalecraft-dev/darkplum-rc-api" },
		"image from another registry":   func(m map[string]any) { img(m, "api")["repository"] = "docker.io/scalecraft-dev/darkplum-api" },
		"image of another service":      func(m map[string]any) { img(m, "api")["repository"] = "ghcr.io/scalecraft-dev/darkplum-web" },
		"version with trailing newline": func(m map[string]any) { m["version"] = "1.2.3\n" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := clone()
			mut(m)
			b, _ := json.Marshal(m)
			if _, err := Parse(b); err == nil {
				t.Errorf("accepted: %s", b)
			}
		})
	}
	if _, err := Parse([]byte(string(golden(t, "release.golden.json")) + `{}`)); err == nil {
		t.Error("trailing data accepted")
	}
}

func TestRCRefusesReleaseShapes(t *testing.T) {
	rc := string(golden(t, "rc.golden.json"))
	for name, bad := range map[string]string{
		"rc from main":     strings.Replace(rc, "refs/heads/prerelease/makefile-removal", "refs/heads/main", 1),
		"rc from a tag":    strings.Replace(rc, "refs/heads/prerelease/makefile-removal", "refs/tags/v1.2.3", 1),
		"release version":  strings.Replace(rc, "0.0.0-rc.7+abcdef0", "1.2.3", 1),
		"release workflow": strings.Replace(rc, "prerelease.yml", "release.yml", 1),
		"release images":   strings.Replace(rc, "darkplum-rc-api", "darkplum-api", 1),
	} {
		if bad == rc {
			t.Fatalf("%s: replacement did nothing", name)
		}
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
