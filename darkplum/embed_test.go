package darkplum

import (
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func files(t *testing.T, fsys fs.FS) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// forbidden is deliberately NOT built on Denied: it is the independent statement
// of what must never ship, so a bug in Denied cannot hide itself.
func forbidden(p string) bool {
	b := p[strings.LastIndex(p, "/")+1:]
	return b == "backend.tf" || b == "tfplan" || (strings.HasSuffix(b, ".hcl") && b != ".terraform.lock.hcl") ||
		strings.Contains(b, "tfvars") || strings.Contains(b, "tfstate") ||
		strings.HasSuffix(b, ".bak") || strings.HasSuffix(b, ".tfplan") ||
		strings.Contains("/"+p+"/", "/.terraform/") || strings.Contains("/"+p+"/", "/.pre-init/") ||
		strings.Contains("/"+p+"/", "/.darkplum/") || strings.Contains("/"+p+"/", "/.git/")
}

// The embedded bundle, on whatever tree it was built from, holds nothing that
// is local configuration or state, and holds the real thing.
func TestEmbedHygiene(t *testing.T) {
	got := files(t, Tree())
	if len(got) < 50 {
		t.Fatalf("suspiciously small bundle: %d files", len(got))
	}
	for _, f := range got {
		if forbidden(f) {
			t.Errorf("forbidden file in the embedded bundle: %s", f)
		}
		if !strings.HasPrefix(f, "terraform/aws/") && !strings.HasPrefix(f, "k8s/") {
			t.Errorf("unexpected top-level path in the bundle: %s", f)
		}
	}
	for _, want := range []string{
		"terraform/aws/main.tf",
		"terraform/aws/variables.tf",
		"terraform/aws/.terraform.lock.hcl",
		"terraform/aws/modules/eks/main.tf",
		"terraform/aws/modules/ecr/main.tf",
		"k8s/base/kustomization.yaml",
		"k8s/templates/vllm-model.yaml",
		// The semantic router, rendered from its chart in CI (no helm on the
		// operator's machine): the CLI applies these files.
		"k8s/semantic-router/rendered.yaml",
		"k8s/semantic-router/network-policies.yaml",
		"k8s/semantic-router/values.yaml",
	} {
		if _, err := fs.Stat(Tree(), want); err != nil {
			t.Errorf("bundle is missing %s: %v", want, err)
		}
	}
	// The example file is documentation for a path the CLI no longer asks users
	// to touch; it is not matched by the patterns and must not sneak in.
	if _, err := fs.Stat(Tree(), "terraform/aws/terraform.tfvars.example"); err == nil {
		t.Error("terraform.tfvars.example must not be embedded")
	}
	// Nothing outside the two embedded roots, and no service code, can be in the
	// tree: services are separate modules.
	for _, f := range got {
		if strings.HasPrefix(f, "services/") {
			t.Errorf("service code embedded: %s", f)
		}
	}
}

// Tree never shows a file the working tree has on disk and Denied hides, whatever
// is lying around next to the code at build time.
func TestTreeHidesWhatIsOnDisk(t *testing.T) {
	onDisk := files(t, os.DirFS("."))
	tree := map[string]bool{}
	for _, f := range files(t, Tree()) {
		tree[f] = true
	}
	for _, f := range onDisk {
		if (strings.HasPrefix(f, "terraform/aws/") || strings.HasPrefix(f, "k8s/")) && forbidden(f) && tree[f] {
			t.Errorf("%s is on disk and leaked into the bundle", f)
		}
	}
}

// The filter is load-bearing: given the decoys a maintainer's disk has, the raw
// patterns would ship them and Filter removes every one.
func TestFilterRemovesDecoys(t *testing.T) {
	decoys := []string{
		"terraform/aws/backend.tf",
		"terraform/aws/backend.hcl",
		"terraform/aws/modules/eks/override.hcl",
		"terraform/aws/terraform.tfvars",
		"terraform/aws/terraform.tfvars.json",
		"terraform/aws/terraform.tfstate",
		"terraform/aws/terraform.tfstate.backup",
		"terraform/aws/main.tf.bak",
		"terraform/aws/tfplan",
		"terraform/aws/.terraform/providers/x",
		"terraform/aws/.pre-init/old.tfstate",
		"terraform/aws/.darkplum/install.json",
		"terraform/aws/modules/eks/terraform.tfvars",
		"terraform/aws/modules/vpc/.terraform/modules/x.tf",
		"terraform/aws/modules/rds/terraform.tfstate",
		"k8s/base/leak.tfstate.backup",
		"k8s/overlays/dev/kustomization.yaml.bak",
		"k8s/.darkplum/state",
	}
	keep := []string{
		"terraform/aws/main.tf",
		"terraform/aws/.terraform.lock.hcl",
		"terraform/aws/modules/eks/main.tf",
		"k8s/base/kustomization.yaml",
	}
	m := fstest.MapFS{}
	for _, p := range append(append([]string{}, decoys...), keep...) {
		m[p] = &fstest.MapFile{Data: []byte("x")}
	}
	if got := len(files(t, m)); got != len(decoys)+len(keep) {
		t.Fatalf("setup: %d files", got)
	}
	got := files(t, Filter(m))
	sort.Strings(keep)
	if strings.Join(got, ",") != strings.Join(keep, ",") {
		t.Errorf("filtered tree = %v, want exactly %v", got, keep)
	}
	for _, d := range decoys {
		if _, err := fs.ReadFile(Filter(m), d); err == nil {
			t.Errorf("decoy %s is readable through the filter", d)
		}
		if !forbidden(d) {
			t.Errorf("test bug: decoy %s is not forbidden", d)
		}
	}
}

// Everything on the independent forbidden list is denied by Denied.
func TestDeniedCoversForbidden(t *testing.T) {
	for _, p := range []string{
		"terraform/aws/backend.tf", "terraform/aws/backend.hcl", "x/y.tftest.hcl", "a/b/terraform.tfvars", "x.tfvars.json", "terraform.tfstate",
		"terraform.tfstate.backup", "tfplan", "run.tfplan", "main.tf.bak", "a/.terraform/b", "a/.pre-init/b", "a/.darkplum/b",
	} {
		if !forbidden(p) {
			t.Errorf("test bug: %s not forbidden", p)
		}
		if !Denied(p) {
			t.Errorf("Denied(%q) = false", p)
		}
	}
	for _, p := range []string{"terraform/aws/main.tf", "terraform/aws/.terraform.lock.hcl", "k8s/base/api/deployment.yaml"} {
		if Denied(p) {
			t.Errorf("Denied(%q) = true", p)
		}
	}
}
