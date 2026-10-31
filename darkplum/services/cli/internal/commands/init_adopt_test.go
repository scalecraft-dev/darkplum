package commands

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

const (
	adoptBucket = "darkplum-tfstate-123456789012-us-east-2-a1b2c3"
	adoptKey    = "darkplum/aws/dp-trial/terraform.tfstate"
	adoptKMS    = "arn:aws:kms:us-east-2:123456789012:key/abc"
)

func legacyBackend() string {
	return legacyBackendTF(backendSpec{Bucket: adoptBucket, Key: adoptKey, Region: "us-east-2", KMSKeyARN: adoptKMS})
}

// legacyRoot is a Makefile-era Terraform root: backend.tf + terraform.tfvars,
// plus the .terraform cache the old flow leaves.
func legacyRoot(t *testing.T, tfvars, backend string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "terraform", "aws")
	if err := os.MkdirAll(filepath.Join(dir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if content == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("terraform.tfvars", tfvars)
	write("backend.tf", backend)
	return dir
}

func adoptCmd(dir string) *InitCmd { return &InitCmd{Adopt: true, Dir: dir} }

func runAdoptT(t *testing.T, c *InitCmd, home install.Home) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := c.adopt(&out, home, func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) })
	return out.String(), err
}

func sum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestAdoptS3InstallRecordsStateAndRenamesOriginals(t *testing.T) {
	dir := legacyRoot(t, pinnedTFVars, legacyBackend())
	home := install.Home(t.TempDir())
	before := sum(t, filepath.Join(dir, "terraform.tfvars"))

	out, err := runAdoptT(t, adoptCmd(dir), home)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	in, err := install.Open(home, "dp-trial")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := in.Load()
	if err != nil {
		t.Fatal(err)
	}
	if rec.ClusterName != "dp-trial" || rec.Region != "us-east-2" || rec.Profile != "acme" || rec.AdoptedFrom == "" ||
		strings.Join(rec.AccountIDs, ",") != "123456789012" {
		t.Errorf("record = %+v", rec)
	}
	be := rec.Backend
	if be.Type != "s3" || be.Bucket != adoptBucket || be.Key != adoptKey || be.Region != "us-east-2" || be.KMSKeyID != adoptKMS || !be.Encrypt || !be.UseLockfile {
		t.Errorf("backend = %+v: the state must stay exactly where it is", be)
	}
	if sum(t, in.TFVars()) != before {
		t.Error("the workspace's settings are not byte-identical to the originals")
	}
	hcl, _ := os.ReadFile(in.BackendHCL())
	for _, want := range []string{`bucket       = "` + adoptBucket + `"`, `key          = "` + adoptKey + `"`, "encrypt      = true", "use_lockfile = true"} {
		if !strings.Contains(string(hcl), want) {
			t.Errorf("backend.hcl lacks %q:\n%s", want, hcl)
		}
	}
	// originals renamed, not deleted, and the undo is printed
	for _, gone := range []string{"backend.tf", "terraform.tfvars"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s still in place: a stale checkout could run Terraform against the same state", gone)
		}
		if _, err := os.Stat(filepath.Join(dir, gone+".adopted")); err != nil {
			t.Errorf("%s.adopted missing: %v", gone, err)
		}
	}
	if sum(t, filepath.Join(dir, "terraform.tfvars.adopted")) != before {
		t.Error("the renamed original changed")
	}
	if !strings.Contains(out, "to undo: mv ") || !strings.Contains(out, "stays where it is") {
		t.Errorf("output lacks the undo line or the no-move statement:\n%s", out)
	}
	// the workspace is private
	for _, p := range []string{in.RecordPath(), in.TFVars(), in.BackendHCL()} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v", p, st, err)
		}
	}
	// the .terraform cache of the old flow is left alone
	if _, err := os.Stat(filepath.Join(dir, ".terraform")); err != nil {
		t.Error("the old .terraform directory was touched")
	}
	// resolvable by the one-install rule
	if got, err := install.Resolve(home, ""); err != nil || got.Name != "dp-trial" {
		t.Errorf("Resolve = %v %v", got, err)
	}
}

func TestAdoptRefusals(t *testing.T) {
	cases := map[string]struct {
		tfvars, backend string
		mutate          func(t *testing.T, dir string)
		install         string
		detail          string
	}{
		"local state (no backend.tf)": {tfvars: pinnedTFVars, backend: "", detail: "keeps its state in a local file"},
		"backend is not s3": {tfvars: pinnedTFVars, detail: "not s3",
			backend: "terraform {\n  backend \"local\" {\n    path = \"x\"\n  }\n}\n"},
		"settings pin no account": {backend: legacyBackend(), detail: "pins no AWS account",
			tfvars: "aws_profile = \"acme\"\naws_region = \"us-east-2\"\ncluster_name = \"dp-trial\"\n"},
		"settings name no region": {backend: legacyBackend(), detail: "no region",
			tfvars: "cluster_name = \"dp-trial\"\nallowed_account_ids = [\"123456789012\"]\n"},
		"state bucket in another region": {tfvars: pinnedTFVars, detail: "the state bucket is in eu-west-1",
			backend: legacyBackendTF(backendSpec{Bucket: adoptBucket, Key: adoptKey, Region: "eu-west-1", KMSKeyARN: adoptKMS})},
		// someone else's state: the bucket belongs to another account than the install pins
		"state bucket from another account": {tfvars: pinnedTFVars, detail: "belongs to account 999999999999",
			backend: legacyBackendTF(backendSpec{Bucket: "darkplum-tfstate-999999999999-us-east-2-a1b2c3", Key: adoptKey, Region: "us-east-2", KMSKeyARN: adoptKMS})},
		"state bucket from another region by name": {tfvars: pinnedTFVars, detail: "belongs to account 123456789012 in eu-west-1",
			backend: legacyBackendTF(backendSpec{Bucket: "darkplum-tfstate-123456789012-eu-west-1-a1b2c3", Key: adoptKey, Region: "us-east-2", KMSKeyARN: adoptKMS})},
		"terraform last used a different state": {tfvars: pinnedTFVars, backend: legacyBackend(), detail: "half-way between two states",
			mutate: func(t *testing.T, dir string) {
				cache := `{"version":3,"backend":{"type":"s3","config":{"bucket":"darkplum-tfstate-123456789012-us-east-2-ffffff","key":"darkplum/aws/dp-trial/terraform.tfstate","region":"us-east-2"}}}`
				if err := os.WriteFile(filepath.Join(dir, ".terraform", "terraform.tfstate"), []byte(cache), 0o600); err != nil {
					t.Fatal(err)
				}
			}},
		"install name that is a path": {tfvars: pinnedTFVars, backend: legacyBackend(), install: "../escape", detail: "install name"},
		"install name with capitals":  {tfvars: pinnedTFVars, backend: legacyBackend(), install: "Acme", detail: "install name"},
		"cluster name that is not a valid name": {backend: legacyBackend(), detail: "install name",
			tfvars: "aws_region = \"us-east-2\"\ncluster_name = \"../x\"\nallowed_account_ids = [\"123456789012\"]\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := legacyRoot(t, tc.tfvars, tc.backend)
			if tc.mutate != nil {
				tc.mutate(t, dir)
			}
			home := install.Home(t.TempDir())
			snap := snapshot(t, dir)
			c := adoptCmd(dir)
			c.installName = tc.install
			out, err := runAdoptT(t, c, home)
			if err == nil {
				t.Fatalf("adopted:\n%s", out)
			}
			if !strings.Contains(out, tc.detail) {
				t.Errorf("output lacks %q:\n%s", tc.detail, out)
			}
			if names, _ := install.List(home); len(names) != 0 {
				t.Errorf("a refusal left a workspace: %v", names)
			}
			if _, err := os.Stat(filepath.Join(string(home), "installs")); err == nil {
				t.Error("a refusal created the installs directory")
			}
			if after := snapshot(t, dir); after != snap {
				t.Errorf("a refusal changed the legacy root:\nbefore %s\nafter  %s", snap, after)
			}
			// nothing escaped through a hostile name
			if _, err := os.Stat(filepath.Join(filepath.Dir(string(home)), "escape")); err == nil {
				t.Error("a path-like install name escaped the home")
			}
		})
	}
}

// snapshot is the names and content hashes of everything in dir.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if info.IsDir() {
			b.WriteString(rel + "/;")
			return nil
		}
		data, _ := os.ReadFile(p)
		b.WriteString(rel + ":" + string(data) + ";")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestAdoptRefusesAnExistingWorkspace(t *testing.T) {
	home := install.Home(t.TempDir())
	dir := legacyRoot(t, pinnedTFVars, legacyBackend())
	if out, err := runAdoptT(t, adoptCmd(dir), home); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	rec, _ := mustOpen(t, home, "dp-trial").Load()

	// the same install again (originals restored, as after the printed undo)
	dir2 := legacyRoot(t, pinnedTFVars, legacyBackend())
	snap := snapshot(t, dir2)
	out, err := runAdoptT(t, adoptCmd(dir2), home)
	if err == nil || !strings.Contains(out, "already exists") {
		t.Fatalf("second adoption: %v\n%s", err, out)
	}
	if snapshot(t, dir2) != snap {
		t.Error("the refused adoption renamed files")
	}
	again, _ := mustOpen(t, home, "dp-trial").Load()
	if !reflect.DeepEqual(again, rec) {
		t.Error("the existing workspace's record changed")
	}
}

func mustOpen(t *testing.T, h install.Home, name string) install.Install {
	t.Helper()
	in, err := install.Open(h, name)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestAdoptDryRunChangesNothing(t *testing.T) {
	dir := legacyRoot(t, pinnedTFVars, legacyBackend())
	home := install.Home(t.TempDir())
	snap := snapshot(t, dir)
	c := adoptCmd(dir)
	c.DryRun = true
	out, err := runAdoptT(t, c, home)
	if err != nil || !strings.Contains(out, "dry run") {
		t.Fatalf("%v\n%s", err, out)
	}
	if snapshot(t, dir) != snap {
		t.Error("dry run changed the legacy root")
	}
	if _, err := os.Stat(string(home) + "/installs"); err == nil {
		t.Error("dry run created a workspace")
	}
}

// If the originals cannot be renamed, the adoption is undone completely: no
// workspace is kept and the legacy root is exactly as it was.
func TestAdoptRollsBackWhenTheOriginalsCannotBeRenamed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := legacyRoot(t, pinnedTFVars, legacyBackend())
	home := install.Home(t.TempDir())
	snap := snapshot(t, dir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	out, err := runAdoptT(t, adoptCmd(dir), home)
	if err == nil {
		t.Fatalf("adopted into a read-only directory:\n%s", out)
	}
	if names, _ := install.List(home); len(names) != 0 {
		t.Errorf("workspace kept after a failed adoption: %v", names)
	}
	if _, err := os.Stat(filepath.Join(string(home), "installs", "dp-trial")); err == nil {
		t.Error("workspace directory left behind")
	}
	_ = os.Chmod(dir, 0o755)
	if snapshot(t, dir) != snap {
		t.Error("legacy root changed")
	}
}

func TestRenameAsideRestoresOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "missing")
	if err := os.WriteFile(a, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := renameAside("stamp", a, b); err == nil {
		t.Fatal("renaming a missing file succeeded")
	}
	if data, err := os.ReadFile(a); err != nil || string(data) != "a" {
		t.Errorf("the first file was not put back: %v %q", err, data)
	}
	if _, err := os.Stat(a + adoptedSuffix); err == nil {
		t.Error("a half-renamed file was left")
	}
	// a taken target name gets the stamp instead of being overwritten
	if err := os.WriteFile(a+adoptedSuffix, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := renameAside("S", a)
	if err != nil || !strings.HasSuffix(moved[0][1], adoptedSuffix+"-S") {
		t.Fatalf("moved = %v, %v", moved, err)
	}
	if data, _ := os.ReadFile(a + adoptedSuffix); string(data) != "older" {
		t.Error("an earlier .adopted file was overwritten")
	}
}

func TestBackendHCLQuotesHostileValues(t *testing.T) {
	got := renderBackendHCL(install.Backend{Bucket: `b"ucket${x}`, Key: "k", Region: "us-east-2", Encrypt: true, UseLockfile: true})
	if strings.Contains(got, `"b"ucket`) || !strings.Contains(got, `\"`) {
		t.Errorf("bucket not escaped:\n%s", got)
	}
	if strings.Contains(got, "kms_key_id") {
		t.Error("kms_key_id written when there is none")
	}
}

// legacyBackendTF is the backend.tf the Makefile-era init wrote: the same settings
// inside a terraform { backend "s3" { ... } } block.
func legacyBackendTF(s backendSpec) string {
	var b strings.Builder
	b.WriteString("terraform {\n  backend \"s3\" {\n")
	for _, l := range strings.Split(strings.TrimSpace(renderBackend(s)), "\n") {
		if strings.HasPrefix(l, "#") {
			continue
		}
		b.WriteString("    " + l + "\n")
	}
	b.WriteString("  }\n}\n")
	return b.String()
}
