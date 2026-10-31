package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

// A fresh init creates the install in its workspace, and nothing in the checkout:
// the next command finds it, and it does not look like a Makefile-era install.
func TestFreshInitCreatesTheWorkspaceNotCheckoutFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DARKPLUM_HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	cloud := newFakeCloud()
	env, out := testEnv(t, cloud, nil, "-")
	c := &InitCmd{Cloud: "aws", Yes: true, ClusterName: "acme", GPUCount: 1, Domain: "acme.com", Profile: "acme"}
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("init wrote into the checkout: %v", entries)
	}
	ws := filepath.Join(home, "installs", "acme")
	for _, f := range []string{"install.json", "terraform.tfvars", "backend.hcl"} {
		st, err := os.Stat(filepath.Join(ws, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, st, err)
		}
	}
	in, rec, err := openInstall("")
	if err != nil || in.Name != "acme" {
		t.Fatalf("the next command cannot find the install: %v", err)
	}
	if rec.ClusterName != "acme" || rec.Region != "us-east-2" || rec.Profile != "acme" || len(rec.AccountIDs) != 1 || rec.AccountIDs[0] != testAccount ||
		rec.Backend.Type != "s3" || rec.Backend.Bucket == "" || !strings.Contains(rec.Backend.KMSKeyID, "kms") || rec.AdoptedFrom != "" {
		t.Errorf("record = %+v", rec)
	}
	// backend.hcl and the record agree on where the state is
	hcl, _ := os.ReadFile(filepath.Join(ws, "backend.hcl"))
	if !strings.Contains(string(hcl), rec.Backend.Bucket) || !strings.Contains(string(hcl), rec.Backend.Key) {
		t.Errorf("backend.hcl and install.json disagree:\n%s\n%+v", hcl, rec.Backend)
	}
	if legacyRootHere() {
		t.Error("a brand-new install reads as a Makefile-era one")
	}
	// status finds it too: a refused cluster, not "no install"
	row := noInstallRow(nil)
	if !strings.Contains(row.hint, "darkplum init") || strings.Contains(row.detail, "Makefile") {
		t.Errorf("row = %+v", row)
	}
}

// A re-run keeps when the install was created and what bundle it last used.
func TestInitRerunKeepsTheRecordsHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DARKPLUM_HOME", home)
	t.Chdir(t.TempDir())
	run := func(extra func(*InitCmd)) {
		env, out := testEnv(t, newFakeCloud(), nil, "-")
		c := &InitCmd{Cloud: "aws", Yes: true, ClusterName: "acme", GPUCount: 1, Domain: "acme.com", Profile: "acme", Force: true}
		if extra != nil {
			extra(c)
		}
		if err := c.execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	run(nil)
	in, _ := install.Open(install.Home(home), "acme")
	rec, _ := in.Load()
	rec.Bundle = "1.2.3-abcdef012345"
	rec.Created = rec.Created.AddDate(0, -1, 0)
	if err := in.Save(rec); err != nil {
		t.Fatal(err)
	}
	run(nil)
	again, err := in.Load()
	if err != nil || again.Bundle != rec.Bundle || !again.Created.Equal(rec.Created) {
		t.Errorf("re-run lost history: %+v %v", again, err)
	}
}

// A Makefile-era root in the checkout (the darkplum/ directory or the repo root
// above it) sends init to --adopt, and nothing is written.
func TestInitInACheckoutWithAMakefileEraRootAsksForAdopt(t *testing.T) {
	for _, root := range []string{filepath.Join("terraform", "aws"), filepath.Join("darkplum", "terraform", "aws")} {
		t.Run(root, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("DARKPLUM_HOME", home)
			cwd := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cwd, root), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, root, "terraform.tfvars"), []byte("aws_region = \"us-east-2\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)
			cloud := newFakeCloud()
			env, out := testEnv(t, cloud, nil, "-")
			c := &InitCmd{Cloud: "aws", Yes: true, ClusterName: "acme", GPUCount: 1, Domain: "acme.com", Profile: "acme"}
			if err := c.execute(env); err == nil || !strings.Contains(out.String(), "init --adopt") {
				t.Fatalf("want a refusal pointing at --adopt, got %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(home, "installs")); err == nil || cloud.createCalls != 0 {
				t.Error("a refusal created something")
			}
		})
	}
}
