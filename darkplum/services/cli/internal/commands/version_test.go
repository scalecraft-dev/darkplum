package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
)

func goldenManifest(t *testing.T, name string) release.Manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", ".github", "scripts", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	m, err := release.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func renderVersion(t *testing.T, c *VersionCmd, m release.Manifest) string {
	t.Helper()
	b, err := bundle.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.render(&out, m, b); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestVersionShowsReleaseDigestsAndThirdPartyPins(t *testing.T) {
	out := renderVersion(t, &VersionCmd{}, goldenManifest(t, "release.golden.json"))
	for _, want := range []string{
		"darkplum 1.2.3 (release)",
		"commit      " + strings.Repeat("a", 40),
		"gateway   ghcr.io/scalecraft-dev/darkplum-gateway@sha256:" + strings.Repeat("1", 64),
		"web       ghcr.io/scalecraft-dev/darkplum-web@sha256:" + strings.Repeat("3", 64),
		"vllm/vllm-openai:v0.6.6@sha256:6956052a3491118df885b37e240abebaf839b62495c3d54e617cb8401c54717b",
		"bundle      ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "DEV BUILD") {
		t.Error("a release is labelled a dev build")
	}
}

func TestVersionLabelsDevAndRCBuilds(t *testing.T) {
	dev := renderVersion(t, &VersionCmd{}, release.Manifest{Schema: 1, Channel: release.Dev, Version: "dev"})
	if !strings.Contains(dev, "DEV BUILD") || strings.Contains(dev, "/darkplum-") {
		t.Errorf("dev output:\n%s", dev)
	}
	rc := renderVersion(t, &VersionCmd{}, goldenManifest(t, "rc.golden.json"))
	if !strings.Contains(rc, "RELEASE CANDIDATE") || !strings.Contains(rc, "darkplum-rc-api@") {
		t.Errorf("rc output:\n%s", rc)
	}
}

func TestVersionJSONAndBundleManifest(t *testing.T) {
	out := renderVersion(t, &VersionCmd{JSON: true}, goldenManifest(t, "release.golden.json"))
	var doc versionDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if doc.Manifest.Version != "1.2.3" || doc.Bundle.Files < 50 || len(doc.Bundle.SHA256) != 64 || len(doc.ThirdParty) < 3 || len(doc.Bundle.List) != 0 {
		t.Errorf("doc = %+v", doc)
	}

	list := renderVersion(t, &VersionCmd{BundleManifest: true}, goldenManifest(t, "release.golden.json"))
	lines := strings.Split(strings.TrimSpace(list), "\n")
	if len(lines) < 50 {
		t.Fatalf("manifest has %d lines", len(lines))
	}
	for _, l := range lines {
		f := strings.SplitN(l, "  ", 2)
		if len(f) != 2 || len(f[0]) != 64 {
			t.Fatalf("bad manifest line %q", l)
		}
		// what the release pipeline allowlists: nothing local can be listed
		for _, bad := range []string{"backend.tf", "tfvars", "tfstate", ".terraform/", ".pre-init", ".darkplum", ".bak", "tftest.hcl", "services/"} {
			if strings.Contains(f[1], bad) && !strings.HasSuffix(f[1], "terraform.tfvars.example") {
				t.Errorf("manifest lists %s", f[1])
			}
		}
	}
	if !strings.Contains(list, "terraform/aws/main.tf") || !strings.Contains(list, "k8s/base/kustomization.yaml") {
		t.Error("manifest lacks the bundle's core files")
	}
}

func TestVersionCommandIsRegistered(t *testing.T) {
	var cli CLI
	if cli.Version.JSON || cli.Version.BundleManifest {
		t.Fatal("unexpected defaults")
	}
}
