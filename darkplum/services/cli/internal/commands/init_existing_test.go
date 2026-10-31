package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

const bedrockInstallTF = "# kept comment\naws_profile = \"acme\"\nallowed_account_ids = [\"" + testAccount + "\"]\naws_region = \"us-east-2\"\n" +
	"azs = [\"us-east-2a\", \"us-east-2b\"]\ncluster_name = \"darkplum\"\nvpc_cidr = \"10.20.0.0/16\"\ngpu_desired_size = 0\n" +
	"model_name = \"Qwen/Qwen2.5-7B-Instruct\"\n"

const gpuInstallTF = "aws_profile = \"acme\"\nallowed_account_ids = [\"" + testAccount + "\"]\naws_region = \"us-east-2\"\n" +
	"azs = [\"us-east-2a\", \"us-east-2b\"]\ncluster_name = \"darkplum\"\ngpu_desired_size = 2\ngpu_instance_types = [\"g5.xlarge\"]\n" +
	"model_name = \"Qwen/Qwen2.5-7B-Instruct\"\nmodel_revision = \"a09a35458c702b33eeacc393d103063234e8bc28\"\nserved_model_name = \"qwen-2.5-7b\"\n" +
	"web_domain = \"acme.com\"\nweb_allowed_cidrs = [\"10.0.0.0/16\"]\n"

func existingDir(t *testing.T, tf string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "terraform.tfvars")
	if err := os.WriteFile(path, []byte(tf), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func existingCmd(dir string) *InitCmd {
	return &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1}
}

// The words a user must never be pointed at.
func assertNoFiles(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{"tfvars", "terraform.", ".tf ", "backend.hcl", "edit the file"} {
		if strings.Contains(out, bad) {
			t.Errorf("output points at files (%q):\n%s", bad, out)
		}
	}
}

func TestInitOnAnExistingInstallShowsSettingsAndLeavesThemByDefault(t *testing.T) {
	dir, path := existingDir(t, gpuInstallTF)
	env, out := testEnv(t, newFakeCloud(), nil, "\n") // the default answer
	if err := existingCmd(dir).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	o := out.String()
	for _, want := range []string{"This install, as it is now", "region", "us-east-2", testAccount, "acme",
		"Bedrock + self-hosted: 2 × g5.xlarge, Qwen/Qwen2.5-7B-Instruct", "https://darkplum.acme.com", "(fixed)", "What do you want to change?",
		"Change the private address", "Change model hosting", "nothing changed"} {
		if !strings.Contains(o, want) {
			t.Errorf("lacks %q:\n%s", want, o)
		}
	}
	assertNoFiles(t, o)
	if readFile(t, path) != gpuInstallTF {
		t.Error("settings were modified by a run that changed nothing")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("a backup was written for no change")
	}
}

func TestInitChangeHostingBedrockToGPU(t *testing.T) {
	dir, path := existingDir(t, bedrockInstallTF)
	// menu 3 (model hosting), mode 2 (GPU), model 1 (Qwen), 2 GPU nodes, confirm (default yes)
	env, out := testEnv(t, newFakeCloud(), nil, "3\n2\n1\n2\n\n")
	if err := existingCmd(dir).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, path)
	for _, want := range []string{"gpu_desired_size = 2\n", `gpu_instance_types = ["g5.xlarge"]`, `model_revision = "a09a35458c702b33eeacc393d103063234e8bc28"`, `served_model_name = "qwen-2.5-7b"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %s:\n%s", want, got)
		}
	}
	if strings.Count(got, "gpu_desired_size") != 1 || strings.Count(got, "\nmodel_name") != 1 {
		t.Errorf("a changed setting appears twice:\n%s", got)
	}
	// everything else is untouched, comment included
	for _, keep := range []string{"# kept comment", `aws_profile = "acme"`, `azs = ["us-east-2a", "us-east-2b"]`, `vpc_cidr = "10.20.0.0/16"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost %s:\n%s", keep, got)
		}
	}
	if readFile(t, path+".bak") != bedrockInstallTF {
		t.Error("no backup of the previous settings")
	}
	if !strings.Contains(out.String(), "darkplum deploy") {
		t.Errorf("does not say how to apply:\n%s", out)
	}
	assertNoFiles(t, out.String())
}

func TestInitChangeHostingGPUToBedrockWarns(t *testing.T) {
	dir, path := existingDir(t, gpuInstallTF)
	env, out := testEnv(t, newFakeCloud(), nil, "3\n1\n\n") // model hosting, Bedrock only, confirm
	if err := existingCmd(dir).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "gpu_desired_size = 0\n") || strings.Contains(got, "gpu_instance_types") || strings.Count(got, "gpu_desired_size") != 1 {
		t.Errorf("settings:\n%s", got)
	}
	if !strings.Contains(got, "web_domain") {
		t.Error("the address was lost")
	}
	if !strings.Contains(out.String(), "removes the GPU nodes") {
		t.Errorf("no warning that GPU nodes go away:\n%s", out)
	}
}

func TestInitChangeHostingRefusals(t *testing.T) {
	t.Run("zones that do not offer the GPU", func(t *testing.T) {
		dir, path := existingDir(t, bedrockInstallTF)
		cloud := newFakeCloud()
		cloud.zones = []string{"us-east-2a"} // us-east-2b lost g5
		env, out := testEnv(t, cloud, nil, "3\n2\n1\n1\n\n")
		if err := existingCmd(dir).execute(env); err == nil || !IsReported(err) {
			t.Fatalf("err = %v\n%s", err, out)
		}
		if readFile(t, path) != bedrockInstallTF || !strings.Contains(out.String(), "us-east-2b") {
			t.Errorf("settings changed or the zone is not named:\n%s", out)
		}
	})
	t.Run("credentials for another account", func(t *testing.T) {
		dir, path := existingDir(t, bedrockInstallTF)
		cloud := newFakeCloud()
		cloud.id.Account = "999999999999"
		env, out := testEnv(t, cloud, nil, "3\n")
		if err := existingCmd(dir).execute(env); err == nil || !IsReported(err) {
			t.Fatalf("err = %v", err)
		}
		if readFile(t, path) != bedrockInstallTF || !strings.Contains(out.String(), "credentials are for 999999999999") {
			t.Errorf("out:\n%s", out)
		}
	})
	t.Run("declining the confirmation writes nothing", func(t *testing.T) {
		dir, path := existingDir(t, bedrockInstallTF)
		env, _ := testEnv(t, newFakeCloud(), nil, "3\n2\n1\n1\nn\n")
		if err := existingCmd(dir).execute(env); !errors.Is(err, errAborted) {
			t.Fatalf("err = %v", err)
		}
		if readFile(t, path) != bedrockInstallTF {
			t.Error("written despite No")
		}
	})
	t.Run("dry run", func(t *testing.T) {
		dir, path := existingDir(t, bedrockInstallTF)
		c := existingCmd(dir)
		c.DryRun, c.Mode = true, "bedrock"
		env, out := testEnv(t, newFakeCloud(), nil, "3\n")
		if err := c.execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if readFile(t, path) != bedrockInstallTF || !strings.Contains(out.String(), "dry run") {
			t.Errorf("out:\n%s", out)
		}
	})
}

// Changing an address that exists replaces its lines; it never leaves two.
func TestInitChangeTheAddressReplacesIt(t *testing.T) {
	dir, path := existingDir(t, gpuInstallTF)
	cloud := newFakeCloud()
	cloud.hzones["acme.org"] = hostedZone{ID: "ZORG", Name: "acme.org"}
	// menu 1 (change the address), new domain, default networks, confirm
	env, out := testEnv(t, cloud, nil, "1\nacme.org\n\n\n")
	if err := existingCmd(dir).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, path)
	if strings.Count(got, "web_domain") != 1 || !strings.Contains(got, `web_domain = "acme.org"`) || strings.Contains(got, "acme.com") {
		t.Errorf("settings:\n%s", got)
	}
	if !strings.Contains(got, "gpu_desired_size = 2") {
		t.Error("another setting was disturbed")
	}
}

func TestInitStartOverStillAsksEverything(t *testing.T) {
	dir, path := existingDir(t, bedrockInstallTF)
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Profile: "acme", Domain: "acme.com"}
	env, _ := testEnv(t, newFakeCloud(), nil, "2\n\n\n") // Start over, networks, the overwrite question defaults to No
	if err := c.execute(env); !errors.Is(err, errAborted) {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, path) != bedrockInstallTF {
		t.Error("overwritten")
	}
}

func TestInitScriptsAreNotInterceptedByTheMenu(t *testing.T) {
	dir, _ := existingDir(t, bedrockInstallTF)
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	err := yesCmd(dir, nil).execute(env)
	if err == nil || strings.Contains(out.String(), "What do you want to change?") {
		t.Errorf("--yes must keep its old behaviour (refuse without --force): %v\n%s", err, out)
	}
}

// A bare `init` on a machine that has an install asks which one is meant, instead
// of quietly making a second.
func TestInitPicksAmongExistingInstalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DARKPLUM_HOME", home)
	h, _ := install.DefaultHome()
	in, err := install.Open(h, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Save(install.Record{Name: "acme", ClusterName: "acme", Region: "us-east-2", AccountIDs: []string{testAccount},
		Backend: install.Backend{Type: "s3", Bucket: "b", Key: "k", Region: "us-east-2"}, Bundle: "v1.2.3-abcdef"}); err != nil {
		t.Fatal(err)
	}
	tf := strings.ReplaceAll(bedrockInstallTF, `cluster_name = "darkplum"`, `cluster_name = "acme"`)
	if err := os.WriteFile(in.TFVars(), []byte(tf), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("review the existing one", func(t *testing.T) {
		env, out := testEnv(t, newFakeCloud(), nil, "1\n\n") // Review and change acme, then Nothing
		c := &InitCmd{Cloud: "aws", ClusterName: "darkplum", GPUCount: 1}
		if err := c.execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		o := out.String()
		if !strings.Contains(o, "Review and change acme") || !strings.Contains(o, "This install, as it is now") || !strings.Contains(o, "v1.2.3-abcdef") {
			t.Errorf("out:\n%s", o)
		}
		assertNoFiles(t, o)
	})
	t.Run("create another", func(t *testing.T) {
		env, _ := testEnv(t, newFakeCloud(), nil, "2\n") // Create another install, then the input ends
		c := &InitCmd{Cloud: "aws", ClusterName: "second", GPUCount: 1, Profile: "acme"}
		if err := c.execute(env); !errors.Is(err, errAborted) {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, "installs", "second", "terraform.tfvars")); err == nil {
			t.Error("a half-answered init wrote settings")
		}
	})
	t.Run("--install names it", func(t *testing.T) {
		env, out := testEnv(t, newFakeCloud(), nil, "\n") // straight to the settings, then Nothing
		c := &InitCmd{Cloud: "aws", ClusterName: "darkplum", GPUCount: 1, installName: "acme"}
		if err := c.execute(env); err != nil || !strings.Contains(out.String(), "This install, as it is now") {
			t.Fatalf("%v\n%s", err, out)
		}
	})
	t.Run("the cluster name is an install", func(t *testing.T) {
		env, out := testEnv(t, newFakeCloud(), nil, "\n")
		c := &InitCmd{Cloud: "aws", ClusterName: "acme", GPUCount: 1}
		if err := c.execute(env); err != nil || strings.Contains(out.String(), "You already have an install") {
			t.Fatalf("%v\n%s", err, out)
		}
	})
}

func TestRewriteTFVars(t *testing.T) {
	in := "# c\na = 1\nb = \"x\"\n\nc = [\"y\"]\n"
	got, err := rewriteTFVars(in, func(k string) bool { return k == "b" }, []tfvarsKV{{"d", 3}, {"b", "z"}})
	if err != nil || got != "# c\na = 1\n\nc = [\"y\"]\nd = 3\nb = \"z\"\n" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := rewriteTFVars("a = 1", func(string) bool { return false }, nil); err != nil || got != "a = 1\n" {
		t.Errorf("a missing final newline is not added: %q %v", got, err)
	}
}

func TestRewriteTFVarsRefusesWhatItCannotEditSafely(t *testing.T) {
	for name, in := range map[string]string{
		"multi-line list": "a = 1\nweb_allowed_cidrs = [\n  \"10.0.0.0/16\",\n]\nb = 2\n",
		"heredoc":         "a = <<EOT\nx\nEOT\n",
		"stray line":      "a = 1\n  \"10.0.0.0/16\",\n",
		"unclosed map":    "tags = {\n  k = \"v\"\n}\n",
	} {
		if _, err := rewriteTFVars(in, func(k string) bool { return k == "web_allowed_cidrs" }, []tfvarsKV{{"web_domain", "x.com"}}); err == nil {
			t.Errorf("%s: edited", name)
		}
	}
	// brackets inside a quoted value are not structure
	if _, err := rewriteTFVars("a = \"x[\"\nb = 1\n", func(k string) bool { return k == "b" }, nil); err != nil {
		t.Errorf("quoted bracket: %v", err)
	}
	// a value that does not survive its own rendering is caught on read-back
	if _, err := rewriteTFVars("a = 1\n", func(string) bool { return false }, []tfvarsKV{{"bad key", "v"}}); err == nil {
		t.Error("a key that cannot read back was written")
	}
}

func TestChangeHostingRefusesAMultiLineSettingsFile(t *testing.T) {
	dir, path := existingDir(t, bedrockInstallTF+"web_allowed_cidrs = [\n  \"10.0.0.0/16\",\n]\n")
	env, out := testEnv(t, newFakeCloud(), nil, "3\n2\n1\n1\n\n")
	if err := existingCmd(dir).execute(env); err == nil || !IsReported(err) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.HasSuffix(readFile(t, path), "]\n") || strings.Contains(readFile(t, path), "gpu_desired_size = 1") {
		t.Error("a multi-line file was edited")
	}
}
