package bundle

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func sample() fstest.MapFS {
	return fstest.MapFS{
		"terraform/aws/main.tf":                          {Data: []byte("# main")},
		"terraform/aws/.terraform.lock.hcl":              {Data: []byte("# lock")},
		"terraform/aws/modules/eks/main.tf":              {Data: []byte("# eks")},
		"terraform/aws/modules/eks/tests/eks.tftest.hcl": {Data: []byte("# test")},
		"k8s/base/kustomization.yaml":                    {Data: []byte("kind: Kustomization")},
		// decoys a maintainer's disk can have
		"terraform/aws/backend.tf":        {Data: []byte("bucket = x")},
		"terraform/aws/terraform.tfvars":  {Data: []byte("secret = y")},
		"terraform/aws/terraform.tfstate": {Data: []byte("{}")},
	}
}

func TestHashIsStableAndIgnoresDecoysAndTests(t *testing.T) {
	a, err := New(sample())
	if err != nil {
		t.Fatal(err)
	}
	clean := sample()
	delete(clean, "terraform/aws/backend.tf")
	delete(clean, "terraform/aws/terraform.tfvars")
	delete(clean, "terraform/aws/terraform.tfstate")
	delete(clean, "terraform/aws/modules/eks/tests/eks.tftest.hcl")
	b, err := New(clean)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() != b.Hash() {
		t.Errorf("hash depends on decoys/tests: %s vs %s", a.Hash(), b.Hash())
	}
	var paths []string
	for _, e := range a.Entries() {
		paths = append(paths, e.Path)
	}
	want := "k8s/base/kustomization.yaml,terraform/aws/.terraform.lock.hcl,terraform/aws/main.tf,terraform/aws/modules/eks/main.tf"
	if strings.Join(paths, ",") != want {
		t.Errorf("entries = %v", paths)
	}
	// content change -> different hash
	changed := sample()
	changed["terraform/aws/main.tf"] = &fstest.MapFile{Data: []byte("# main edited")}
	c, _ := New(changed)
	if c.Hash() == a.Hash() {
		t.Error("hash did not change with content")
	}
}

func TestEmptyBundleIsRefused(t *testing.T) {
	if _, err := New(fstest.MapFS{"terraform/aws/terraform.tfvars": {Data: []byte("x")}}); err == nil {
		t.Error("a tree of only denied files must be refused")
	}
}

func TestExtractVerifyAndIdempotence(t *testing.T) {
	b, _ := New(sample())
	dest := filepath.Join(t.TempDir(), "installs", "acme", "bundle", "1.2.3")
	if err := b.Extract(dest); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dest, b.Hash()); err != nil {
		t.Fatalf("fresh extraction does not verify: %v", err)
	}
	for _, gone := range []string{"terraform/aws/backend.tf", "terraform/aws/terraform.tfvars", "terraform/aws/modules/eks/tests"} {
		if _, err := os.Stat(filepath.Join(dest, gone)); err == nil {
			t.Errorf("%s was extracted", gone)
		}
	}
	info, err := os.Stat(filepath.Join(dest, "terraform/aws/main.tf"))
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Errorf("extracted files must be read-only: %v %v", info, err)
	}
	// Second extraction into the same dir is a no-op, not an error.
	if err := b.Extract(dest); err != nil {
		t.Errorf("re-extract: %v", err)
	}
	// no temp dirs left behind
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".extract-*"))
	if len(left) != 0 {
		t.Errorf("temporary extraction left behind: %v", left)
	}
}

func TestVerifyRefusesAnyTampering(t *testing.T) {
	b, _ := New(sample())
	cases := map[string]func(t *testing.T, dest string){
		"edited file": func(t *testing.T, d string) {
			p := filepath.Join(d, "terraform/aws/main.tf")
			must(t, os.Chmod(p, 0o600))
			must(t, os.WriteFile(p, []byte("# main\nresource \"x\" \"y\" {}"), 0o600))
		},
		"deleted file": func(t *testing.T, d string) {
			must(t, os.Remove(filepath.Join(d, "k8s/base/kustomization.yaml")))
		},
		"extra file (a dropped backend.tf)": func(t *testing.T, d string) {
			must(t, os.WriteFile(filepath.Join(d, "terraform/aws/backend.tf"), []byte("x"), 0o600))
		},
		"extra tfvars": func(t *testing.T, d string) {
			must(t, os.WriteFile(filepath.Join(d, "terraform/aws/terraform.tfvars"), []byte("x"), 0o600))
		},
		"terraform init inside the tree": func(t *testing.T, d string) {
			must(t, os.MkdirAll(filepath.Join(d, "terraform/aws/.terraform/providers"), 0o700))
			must(t, os.WriteFile(filepath.Join(d, "terraform/aws/.terraform/providers/x"), []byte("x"), 0o600))
		},
		"symlink": func(t *testing.T, d string) {
			must(t, os.Symlink("/etc/passwd", filepath.Join(d, "terraform/aws/link.tf")))
		},
		"marker changed": func(t *testing.T, d string) {
			p := filepath.Join(d, hashFile)
			must(t, os.Chmod(p, 0o600))
			must(t, os.WriteFile(p, []byte("0000\n"), 0o600))
		},
		"marker removed": func(t *testing.T, d string) {
			must(t, os.Remove(filepath.Join(d, hashFile)))
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "b")
			must(t, b.Extract(dest))
			tamper(t, dest)
			if err := Verify(dest, b.Hash()); err == nil {
				t.Error("tampered bundle verified")
			}
			// and extraction over a tampered dir refuses instead of trusting it
			if err := b.Extract(dest); err == nil {
				t.Error("Extract accepted a tampered existing directory")
			}
		})
	}
}

func TestVerifyRefusesAnotherBundle(t *testing.T) {
	a, _ := New(sample())
	other := sample()
	other["terraform/aws/main.tf"] = &fstest.MapFile{Data: []byte("# other release")}
	b, _ := New(other)
	dest := filepath.Join(t.TempDir(), "b")
	must(t, a.Extract(dest))
	if err := Verify(dest, b.Hash()); err == nil {
		t.Error("a different release's hash verified this tree")
	}
}

// The real embedded bundle hashes, extracts and verifies, and matches itself.
func TestEmbeddedBundleRoundTrip(t *testing.T) {
	b, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Entries()) < 50 {
		t.Fatalf("embedded bundle has %d files", len(b.Entries()))
	}
	dest := filepath.Join(t.TempDir(), "bundle")
	must(t, b.Extract(dest))
	must(t, Verify(dest, b.Hash()))
	// extracted tree has no test files and nothing local
	err = filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dest, p)
		if strings.HasSuffix(rel, ".tftest.hcl") || strings.Contains(rel, "tfvars") || strings.Contains(rel, "tfstate") || filepath.Base(rel) == "backend.tf" {
			t.Errorf("unexpected file in extracted bundle: %s", rel)
		}
		return nil
	})
	must(t, err)
	b2, _ := Embedded()
	if b.Hash() != b2.Hash() {
		t.Error("embedded bundle hash is not deterministic")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
