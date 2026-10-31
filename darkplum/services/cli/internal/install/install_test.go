package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
)

func rec(name string) Record {
	return Record{
		Name: name, ClusterName: name, Region: "us-east-2", AccountIDs: []string{"123456789012"},
		Backend: Backend{Type: "s3", Bucket: "darkplum-tfstate-123456789012-us-east-2-a1b2c3", Key: "darkplum/aws/" + name + "/terraform.tfstate",
			Region: "us-east-2", Encrypt: true, UseLockfile: true},
	}
}

func mk(t *testing.T, h Home, names ...string) {
	t.Helper()
	for _, n := range names {
		in, err := Open(h, n)
		if err != nil {
			t.Fatal(err)
		}
		if err := in.Save(rec(n)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidNameRefusesPathsAndShapes(t *testing.T) {
	for _, ok := range []string{"darkplum", "acme-prod", "a1", "x" + strings.Repeat("y", 38) + "z"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a", ".", "..", "../x", "a/b", `a\b`, "A", "1abc", "abc-", "a--b", "has space", "x" + strings.Repeat("y", 40),
		"install.json", "a.b", "~", "a\x00b"} {
		if err := ValidName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if _, err := Open(Home(t.TempDir()), bad); err == nil {
			t.Errorf("Open(%q) accepted", bad)
		}
	}
}

func TestDefaultHome(t *testing.T) {
	t.Setenv("DARKPLUM_HOME", "")
	t.Setenv("HOME", "/home/op")
	h, err := DefaultHome()
	if err != nil || h != Home("/home/op/.darkplum") {
		t.Errorf("default home = %q, %v", h, err)
	}
	t.Setenv("DARKPLUM_HOME", "/srv/dp")
	if h, _ := DefaultHome(); h != Home("/srv/dp") {
		t.Errorf("DARKPLUM_HOME ignored: %q", h)
	}
	t.Setenv("DARKPLUM_HOME", "relative/dir")
	if _, err := DefaultHome(); err == nil {
		t.Error("a relative DARKPLUM_HOME must be refused")
	}
}

func TestResolveNeverGuesses(t *testing.T) {
	h := Home(t.TempDir())
	if _, err := Resolve(h, ""); !errors.Is(err, ErrNoInstall) {
		t.Errorf("no installs: %v", err)
	}
	mk(t, h, "alpha")
	in, err := Resolve(h, "")
	if err != nil || in.Name != "alpha" {
		t.Errorf("exactly one install must resolve implicitly: %v %v", in, err)
	}
	mk(t, h, "beta")
	if _, err := Resolve(h, ""); err == nil || !strings.Contains(err.Error(), "--install") {
		t.Errorf("two installs and no flag must be an error naming --install, got %v", err)
	}
	if in, err := Resolve(h, "beta"); err != nil || in.Name != "beta" {
		t.Errorf("explicit: %v %v", in, err)
	}
	if _, err := Resolve(h, "gamma"); !errors.Is(err, ErrNoInstall) {
		t.Errorf("unknown install: %v", err)
	}
	if _, err := Resolve(h, "../alpha"); err == nil {
		t.Error("a path as an install name must be refused")
	}
}

// Nothing ambient selects an install: not the environment, not the working directory.
func TestResolveIgnoresAmbientState(t *testing.T) {
	h := Home(t.TempDir())
	mk(t, h, "alpha", "beta")
	for _, k := range []string{"DARKPLUM_INSTALL", "INSTALL", "CLUSTER_NAME", "AWS_PROFILE", "KUBECONFIG"} {
		t.Setenv(k, "alpha")
	}
	wd := filepath.Join(h.installs(), "alpha")
	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(h, ""); err == nil {
		t.Error("with two installs, environment variables and the working directory must not pick one")
	}
}

// Two installs never see each other's files or settings.
func TestInstallsAreIsolated(t *testing.T) {
	h := Home(t.TempDir())
	mk(t, h, "alpha", "beta")
	a, _ := Open(h, "alpha")
	b, _ := Open(h, "beta")
	if err := a.WriteFile("terraform.tfvars", []byte(`aws_region = "us-east-2"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.TFVars()); err == nil {
		t.Error("beta sees alpha's settings")
	}
	for _, p := range []string{a.TFVars(), a.Kubeconfig(), a.TFData(), a.LogsDir(), a.BundleDir("1.0.0"), a.RecordPath(), a.BackendHCL()} {
		if !strings.HasPrefix(p, a.Dir()+string(filepath.Separator)) {
			t.Errorf("%s escapes alpha's workspace %s", p, a.Dir())
		}
		if strings.HasPrefix(p, b.Dir()+string(filepath.Separator)) {
			t.Errorf("%s lies inside beta's workspace", p)
		}
	}
	ra, err := a.Load()
	rb, err2 := b.Load()
	if err != nil || err2 != nil || ra.ClusterName != "alpha" || rb.ClusterName != "beta" {
		t.Errorf("records mixed up: %+v %+v %v %v", ra, rb, err, err2)
	}
}

func TestWriteFileRefusesPaths(t *testing.T) {
	h := Home(t.TempDir())
	mk(t, h, "alpha")
	a, _ := Open(h, "alpha")
	for _, bad := range []string{"../beta/install.json", "a/b", "/etc/passwd", "..", "."} {
		if err := a.WriteFile(bad, []byte("x")); err == nil {
			t.Errorf("WriteFile(%q) accepted", bad)
		}
	}
}

func TestRecordPermissionsAndRoundTrip(t *testing.T) {
	h := Home(t.TempDir())
	mk(t, h, "alpha")
	a, _ := Open(h, "alpha")
	info, err := os.Stat(a.RecordPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("record perms: %v %v", info, err)
	}
	d, _ := os.Stat(a.Dir())
	if d.Mode().Perm() != 0o700 {
		t.Errorf("workspace dir perms %v", d.Mode().Perm())
	}
	got, err := a.Load()
	if err != nil || got.Schema != 1 || got.Backend.Bucket == "" {
		t.Errorf("round trip: %+v %v", got, err)
	}
}

func TestLoadRefusesBadRecords(t *testing.T) {
	h := Home(t.TempDir())
	mk(t, h, "alpha", "beta")
	a, _ := Open(h, "alpha")
	cases := map[string]string{
		"unknown field":   `{"schema":1,"name":"alpha","extra":1}`,
		"wrong schema":    `{"schema":2,"name":"alpha"}`,
		"other name":      `{"schema":1,"name":"beta","cluster_name":"beta","region":"us-east-2","account_ids":["123456789012"],"backend":{"type":"s3","bucket":"b","key":"k","region":"r"}}`,
		"no account":      `{"schema":1,"name":"alpha","cluster_name":"alpha","region":"us-east-2","account_ids":[],"backend":{"type":"s3","bucket":"b","key":"k","region":"r"}}`,
		"bad account":     `{"schema":1,"name":"alpha","cluster_name":"alpha","region":"us-east-2","account_ids":["12345"],"backend":{"type":"s3","bucket":"b","key":"k","region":"r"}}`,
		"local backend":   `{"schema":1,"name":"alpha","cluster_name":"alpha","region":"us-east-2","account_ids":["123456789012"],"backend":{"type":"local"}}`,
		"incomplete s3":   `{"schema":1,"name":"alpha","cluster_name":"alpha","region":"us-east-2","account_ids":["123456789012"],"backend":{"type":"s3","bucket":"b"}}`,
		"not json":        `nope`,
		"empty":           ``,
		"traversal in id": `{"schema":1,"name":"../x"}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(a.RecordPath(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Load(); err == nil {
				t.Error("bad record loaded")
			}
		})
	}
	// Save refuses to write a record for another install into this workspace.
	r := rec("beta")
	if err := a.Save(r); err == nil {
		t.Error("alpha's workspace accepted beta's record")
	}
}

func TestBundlePrepareVerifyAndPrune(t *testing.T) {
	h := Home(t.TempDir())
	mk(t, h, "alpha")
	a, _ := Open(h, "alpha")
	b, err := bundle.New(fstest.MapFS{
		"terraform/aws/main.tf":       {Data: []byte("# main")},
		"k8s/base/kustomization.yaml": {Data: []byte("kind: Kustomization")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		dir, err := a.PrepareBundle(b, v)
		if err != nil {
			t.Fatal(err)
		}
		if dir != filepath.Join(a.BundleDir(v), "terraform", "aws") {
			t.Errorf("terraform dir = %s", dir)
		}
		if err := a.VerifyBundle(b, v); err != nil {
			t.Errorf("fresh bundle %s does not verify: %v", v, err)
		}
	}
	// a terraform init inside the version dir is caught
	if err := os.MkdirAll(filepath.Join(a.TerraformDir("1.2.0"), ".terraform"), 0o700); err == nil {
		_ = os.WriteFile(filepath.Join(a.TerraformDir("1.2.0"), ".terraform", "x"), []byte("x"), 0o600)
	}
	if err := a.VerifyBundle(b, "1.2.0"); err == nil {
		t.Error("a bundle with a .terraform inside verified")
	}
	if err := a.PruneBundles("1.2.0", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	left, _ := os.ReadDir(filepath.Join(a.Dir(), "bundle"))
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "1.1.0,1.2.0" {
		t.Errorf("after prune: %v", names)
	}
	if _, err := a.PrepareBundle(b, ""); err == nil {
		t.Error("empty version accepted")
	}
	// a hostile version string cannot leave the workspace
	dir, err := a.PrepareBundle(b, "../../../evil")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, a.Dir()+string(filepath.Separator)) {
		t.Errorf("version string escaped the workspace: %s", dir)
	}
}
