package commands

import (
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

func loadRecordAt(t *testing.T, dir, name string) install.Record {
	t.Helper()
	in, err := install.OpenDir(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := in.Load()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A new install: off unless asked for, and the question says what it costs.
func TestInitRouterNewInstall(t *testing.T) {
	for name, tc := range map[string]struct {
		answers string
		flag    string
		want    bool
	}{
		"the question defaults to off": {"\n\n\n", "", false},
		"yes at the question":          {"\ny\n\n", "", true},
		"flag on asks nothing":         {"\n\n", "on", true},
		"flag off asks nothing":        {"\n\n", "off", false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			env, out := testEnv(t, newFakeCloud(), nil, tc.answers)
			c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Profile: "acme", Domain: "acme.com", SemanticRouter: tc.flag}
			if err := c.execute(env); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			rec := loadRecordAt(t, dir, "darkplum")
			if rec.SemanticRouter != tc.want {
				t.Errorf("SemanticRouter = %v, want %v\n%s", rec.SemanticRouter, tc.want, out)
			}
			asked := strings.Contains(out.String(), "Install the semantic router?")
			if asked != (tc.flag == "") {
				t.Errorf("asked = %v with flag %q", asked, tc.flag)
			}
			if tc.want && !strings.Contains(out.String(), "Semantic router: on") {
				t.Errorf("summary does not say it is on:\n%s", out)
			}
		})
	}
}

// --yes asks nothing and does not decide for an install that already has a setting.
func TestInitRouterYesKeepsTheInstallsSetting(t *testing.T) {
	dir, in, rec := liveInstall(t)
	rec.SemanticRouter = true
	if err := in.Save(rec); err != nil {
		t.Fatal(err)
	}
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Force = true }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got, _ := in.Load()
	if !got.SemanticRouter {
		t.Error("a --yes re-run turned the router off")
	}
	// an explicit flag does decide
	env, out = testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Force, c.SemanticRouter = true, "off" }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := in.Load(); got.SemanticRouter {
		t.Error("--semantic-router off did not turn it off")
	}
}

func TestInitExistingInstallShowsTheRouterAndChangesIt(t *testing.T) {
	dir, in, rec := liveInstall(t)
	// the settings view has a row for it, off by default; "4" is the router choice
	env, out := testEnv(t, newFakeCloud(), nil, "4\ny\n")
	if err := existingCmd(dir).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"router", "off", "Change the semantic router", "semantic router on; deploy installs it", "darkplum deploy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	got, _ := in.Load()
	if !got.SemanticRouter {
		t.Fatal("the record does not say on")
	}
	// the identity and everything else is as it was
	rec.SemanticRouter = true
	sameRecord(t, in, rec)
	if readFile(t, dir+"/terraform.tfvars") != bedrockInstallTF {
		t.Error("the router is not Terraform's: the settings file changed")
	}
	// the view now says on; turning it off again
	env, out = testEnv(t, newFakeCloud(), nil, "4\nn\n")
	if err := existingCmd(dir).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "the in-cluster intent classifier") || !strings.Contains(out.String(), "deploy removes it") {
		t.Errorf("\n%s", out)
	}
	if got, _ := in.Load(); got.SemanticRouter {
		t.Error("still on")
	}
	// answering as it already is changes nothing
	env, out = testEnv(t, newFakeCloud(), nil, "4\n\n")
	if err := existingCmd(dir).execute(env); err != nil || !strings.Contains(out.String(), "nothing changed") {
		t.Errorf("%v\n%s", err, out)
	}
}

// The question states what the router can see and where its pod can connect, so the
// answer is informed.
func TestRouterQuestionStatesPromptAccessAndEgress(t *testing.T) {
	for _, want := range []string{"text of the prompts", "port 443", "Hugging Face"} {
		if !strings.Contains(routerPitch, want) {
			t.Errorf("the router question does not say %q:\n%s", want, routerPitch)
		}
	}
}
