package commands

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/alecthomas/kong"

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
			c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", Region: "us-east-2", Mode: "bedrock", Profile: "acme", Domain: "acme.com", SemanticRouter: tc.flag}
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

// An explicit --semantic-router on an existing install applies that one change,
// with --yes and without --force, and leaves everything else alone.
func TestInitSemanticRouterFlagOnAnExistingInstallYes(t *testing.T) {
	dir, in, rec := liveInstall(t)
	// --yes also carries --domain, as the README tells a script to.
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.SemanticRouter = "on" }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"semantic router on; deploy installs it; every other setting is unchanged", "text of the prompts"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "existing files") || strings.Contains(out.String(), "pass --force") {
		t.Errorf("asked for --force:\n%s", out)
	}
	rec.SemanticRouter = true
	sameRecord(t, in, rec)
	if got, _ := in.Load(); !got.SemanticRouter {
		t.Fatal("the record does not say on")
	}
	if readFile(t, dir+"/terraform.tfvars") != bedrockInstallTF {
		t.Error("the settings file changed")
	}

	env, out = testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.SemanticRouter = "off" }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := in.Load(); got.SemanticRouter || !strings.Contains(out.String(), "semantic router off; deploy removes it") {
		t.Errorf("not turned off:\n%s", out)
	}
}

// Interactive: the menu is not shown, but turning the router on keeps its y/N
// (the flag only sets the default); declining leaves it off.
func TestInitSemanticRouterFlagOnAnExistingInstallInteractive(t *testing.T) {
	dir, in, _ := liveInstall(t)
	env, out := testEnv(t, newFakeCloud(), nil, "y\n") // the disclosure question is still asked
	c := existingCmd(dir)
	c.SemanticRouter = "on"
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "What do you want to change?") || strings.Contains(out.String(), "nothing changed") {
		t.Errorf("flag ignored in favour of the menu:\n%s", out)
	}
	if !strings.Contains(out.String(), "text of the prompts") {
		t.Errorf("the question was not asked:\n%s", out)
	}
	if got, _ := in.Load(); !got.SemanticRouter {
		t.Fatalf("not on:\n%s", out)
	}
	// declining
	dir2, in2, _ := liveInstall(t)
	env, out = testEnv(t, newFakeCloud(), nil, "n\n")
	c = existingCmd(dir2)
	c.SemanticRouter = "on"
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := in2.Load(); got.SemanticRouter {
		t.Error("turned on despite n")
	}
}

// Without the flag, --yes on an existing install still refuses (the overwrite
// guard is unchanged), and --force still means the full rewrite.
func TestInitYesWithoutASettingFlagStillRefusesOnAnExistingInstall(t *testing.T) {
	dir, in, _ := liveInstall(t)
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	err := yesCmd(dir, nil).execute(env)
	if err == nil || !strings.Contains(out.String(), "--force") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if got, _ := in.Load(); got.SemanticRouter {
		t.Error("changed")
	}
}

// The other setting flags take the same path: --allowed-cidrs on an install that
// has an address changes the address only.
func TestInitAllowedCIDRsFlagOnAnExistingInstallYes(t *testing.T) {
	dir, path := existingDir(t, gpuInstallTF)
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	c := yesCmd(dir, func(c *InitCmd) { c.AllowedCIDRs = "10.9.0.0/16" })
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "10.9.0.0/16") || !strings.Contains(got, "gpu_desired_size = 2") || strings.Contains(out.String(), "--force") {
		t.Errorf("%s\n%s", got, out)
	}
}

// A --domain that differs from the install's address is not applied by a script:
// it would create or abandon DNS and a certificate. Nothing is written, not even
// the router change that came with it.
func TestInitYesRefusesToMoveTheAddressAsAFlag(t *testing.T) {
	dir, path := existingDir(t, gpuInstallTF)
	for name, mut := range map[string]func(*InitCmd){
		"another domain":   func(c *InitCmd) { c.Domain = "typo.com" },
		"another hostname": func(c *InitCmd) { c.Hostname = "chat.acme.com" },
		"another zone":     func(c *InitCmd) { c.ZoneID = "ZOTHER" },
	} {
		t.Run(name, func(t *testing.T) {
			cloud := newFakeCloud()
			env, out := testEnv(t, cloud, nil, "-")
			err := yesCmd(dir, func(c *InitCmd) { c.SemanticRouter = "on"; mut(c) }).execute(env)
			if err == nil || !strings.Contains(out.String(), "--force") || cloud.createCalls != 0 {
				t.Fatalf("err = %v creates = %d\n%s", err, cloud.createCalls, out)
			}
			if readFile(t, path) != gpuInstallTF {
				t.Error("settings changed")
			}
		})
	}
	// restating the current domain is not a change
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Domain = "ACME.com" }).execute(env); err == nil || !strings.Contains(out.String(), "existing files") {
		t.Errorf("a restated domain must keep the old guard: %v\n%s", err, out)
	}
}

// --yes --model keeps the install's node count unless --gpu-count is given, and
// naming the current model is not a change.
func TestInitYesModelKeepsTheGPUCount(t *testing.T) {
	dir, path := existingDir(t, gpuInstallTF) // 2 nodes, Qwen
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Model = "Qwen/Qwen2.5-7B-Instruct" }).execute(env); err == nil {
		t.Fatalf("the current model restated is no change, so the old guard applies\n%s", out)
	}
	if readFile(t, path) != gpuInstallTF {
		t.Fatal("changed")
	}
	other := ""
	for _, m := range modelCatalog {
		if m.ID != "Qwen/Qwen2.5-7B-Instruct" {
			other = m.ID
			break
		}
	}
	if other == "" {
		t.Skip("one catalog model")
	}
	env, out = testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Model = other }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := readFile(t, path); !strings.Contains(got, "gpu_desired_size = 2\n") || !strings.Contains(got, other) {
		t.Errorf("count not kept:\n%s", got)
	}
	// an explicit count is a change on its own
	dir, path = existingDir(t, gpuInstallTF)
	env, out = testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.GPUCount = 3 }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := readFile(t, path); !strings.Contains(got, "gpu_desired_size = 3\n") || !strings.Contains(got, "Qwen/Qwen2.5-7B-Instruct") {
		t.Errorf("\n%s", got)
	}
}

// Several flags at once: one summary, one "unchanged" line, and --dry-run writes
// nothing.
func TestInitSeveralFlagsOnAnExistingInstall(t *testing.T) {
	dir, in, _ := liveInstall(t)
	path := dir + "/terraform.tfvars"
	if err := os.WriteFile(path, []byte(gpuInstallTF), 0o600); err != nil {
		t.Fatal(err)
	}
	mk := func(c *InitCmd) {
		c.SemanticRouter, c.AllowedCIDRs, c.GPUCount = "on", "10.9.0.0/16", 3
	}
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { mk(c); c.DryRun = true }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := in.Load(); got.SemanticRouter || readFile(t, path) != gpuInstallTF {
		t.Fatalf("--dry-run wrote:\n%s", out)
	}
	env, out = testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, mk).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := strings.Count(out.String(), "every other setting is unchanged"); n != 1 {
		t.Errorf("%d summaries:\n%s", n, out)
	}
	for _, want := range []string{"model hosting", "semantic router", "private address"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	got := readFile(t, path)
	if !strings.Contains(got, "gpu_desired_size = 3") || !strings.Contains(got, "10.9.0.0/16") {
		t.Errorf("\n%s", got)
	}
	if r, _ := in.Load(); !r.SemanticRouter {
		t.Error("router not on")
	}
}

// secondIdentityFails answers the first identity lookup (the hosting step's) and
// reports another account from then on (the address step's), so that step fails
// after hosting and the router have been applied. onSecond runs at that moment.
type secondIdentityFails struct {
	*fakeCloud
	calls    int
	onSecond func()
}

func (f *secondIdentityFails) Identity() (identity, error) {
	f.calls++
	if f.calls >= 2 {
		if f.onSecond != nil {
			f.onSecond()
		}
		return identity{Account: "999999999999", Arn: "arn:aws:iam::999999999999:user/x"}, nil
	}
	return f.fakeCloud.Identity()
}

// Hosting and the router succeed, the address step then fails: the settings and the
// record are byte-identical to before, and the output says so.
func TestInitSeveralFlagsLaterStepFailsPutsEverythingBack(t *testing.T) {
	dir, _, _ := liveInstall(t)
	path := dir + "/terraform.tfvars"
	if err := os.WriteFile(path, []byte(gpuInstallTF), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeRec, err := os.ReadFile(dir + "/install.json")
	if err != nil {
		t.Fatal(err)
	}
	cloud := &secondIdentityFails{fakeCloud: newFakeCloud()}
	env, out := testEnv(t, cloud, nil, "-")
	err = yesCmd(dir, func(c *InitCmd) { c.SemanticRouter, c.AllowedCIDRs, c.GPUCount = "on", "10.9.0.0/16", 3 }).execute(env)
	if err == nil || cloud.calls < 2 {
		t.Fatalf("the address step should have failed (identity calls %d): %v\n%s", cloud.calls, err, out)
	}
	if readFile(t, path) != gpuInstallTF {
		t.Errorf("settings not put back:\n%s", readFile(t, path))
	}
	afterRec, _ := os.ReadFile(dir + "/install.json")
	if string(afterRec) != string(beforeRec) {
		t.Errorf("record not put back:\n%s\n%s", beforeRec, afterRec)
	}
	if !strings.Contains(out.String(), "none of the requested changes was kept") {
		t.Errorf("does not say nothing was kept:\n%s", out)
	}
}

// When the put-back itself fails, the output says what is still changed instead of
// claiming nothing was kept.
func TestInitSeveralFlagsPutBackFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir, _, _ := liveInstall(t)
	path := dir + "/terraform.tfvars"
	if err := os.WriteFile(path, []byte(gpuInstallTF), 0o600); err != nil {
		t.Fatal(err)
	}
	cloud := &secondIdentityFails{fakeCloud: newFakeCloud(), onSecond: func() { _ = os.Chmod(dir, 0o500) }}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	env, out := testEnv(t, cloud, nil, "-")
	err := yesCmd(dir, func(c *InitCmd) { c.SemanticRouter, c.AllowedCIDRs, c.GPUCount = "on", "10.9.0.0/16", 3 }).execute(env)
	if err == nil {
		t.Fatalf("expected a failure\n%s", out)
	}
	if strings.Contains(out.String(), "none of the requested changes was kept") || !strings.Contains(out.String(), "could not put the install's settings back") {
		t.Errorf("must say what is still changed:\n%s", out)
	}
}

// Interactive: the user confirms the router, then declines the address; the abort
// puts the router back and says so.
func TestInitSeveralFlagsAbortedAtTheAddressSaysNothingWasKept(t *testing.T) {
	dir, in, _ := liveInstall(t)
	path := dir + "/terraform.tfvars"
	if err := os.WriteFile(path, []byte(gpuInstallTF), 0o600); err != nil {
		t.Fatal(err)
	}
	env, out := testEnv(t, newFakeCloud(), nil, "y\nn\n") // router y; address n
	c := existingCmd(dir)
	c.SemanticRouter, c.AllowedCIDRs, c.Domain = "on", "10.9.0.0/16", "acme.com"
	err := c.execute(env)
	if !errors.Is(err, errAborted) {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if r, _ := in.Load(); r.SemanticRouter || readFile(t, path) != gpuInstallTF {
		t.Errorf("a change was kept:\n%s", out)
	}
	if !strings.Contains(out.String(), "none of the requested changes was kept") {
		t.Errorf("an abort must say so too:\n%s", out)
	}
}

// --gpu-count is "given" only when it is on the command line (kong leaves 0
// otherwise); 0 is not a legal count, so it never collides with a real one.
func TestInitGPUCountGivenThroughKong(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		count int
		set   bool
	}{
		"not given":  {[]string{"init", "--yes"}, 1, false},
		"given":      {[]string{"init", "--yes", "--gpu-count", "3"}, 3, true},
		"given as 1": {[]string{"init", "--yes", "--gpu-count=1"}, 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			var cli CLI
			p, err := kong.New(&cli, kong.Name("darkplum"), kong.Exit(func(int) {}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			env, out := testEnv(t, newFakeCloud(), nil, "-")
			if err := cli.Init.validateLocal(env); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if cli.Init.GPUCount != tc.count || cli.Init.gpuCountSet != tc.set {
				t.Errorf("count %d set %v, want %d %v", cli.Init.GPUCount, cli.Init.gpuCountSet, tc.count, tc.set)
			}
		})
	}
}

// A count-only change reuses the pinned model: no catalog or Hugging Face lookup,
// so a gated, custom or unreachable one does not block it.
func TestInitCountOnlyChangeDoesNotResolveTheModel(t *testing.T) {
	tf := strings.Replace(gpuInstallTF, "Qwen/Qwen2.5-7B-Instruct", "acme/private-model", 1)
	tf = strings.Replace(tf, "qwen-2.5-7b", "private-model", 1)
	dir, path := existingDir(t, tf)
	env, out := testEnv(t, newFakeCloud(), &failingHF{}, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.GPUCount = 4 }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, path)
	for _, want := range []string{"gpu_desired_size = 4\n", `model_name = "acme/private-model"`, `served_model_name = "private-model"`, `model_revision = "a09a35458c702b33eeacc393d103063234e8bc28"`} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %s:\n%s", want, got)
		}
	}
}

type failingHF struct{}

func (failingHF) Lookup(context.Context, string) (hfModel, error) {
	return hfModel{}, errors.New("Hugging Face must not be called for a count-only change")
}
func (failingHF) endpoint() string { return "" }

// Flags that only repeat the install are not changes: the default GPU type when
// the settings name none, and a zone, hostname or certificate it already has.
func TestInitRestatedFlagsAreNotChanges(t *testing.T) {
	raw := parseTFVars(gpuInstallTF + "web_zone_id = \"Z123ABC\"\nweb_certificate_arn = \"arn:aws:acm:us-east-2:111122223333:certificate/x\"\n")
	c := &InitCmd{ZoneID: "Z123ABC", CertificateARN: "arn:aws:acm:us-east-2:111122223333:certificate/x", Hostname: "darkplum.acme.com", GPUInstance: "g5.xlarge"}
	c.dropRestatedFlags(raw)
	if c.ZoneID != "" || c.CertificateARN != "" || c.Hostname != "" {
		t.Errorf("restated web flags kept: %+v", c)
	}
	if c.changesSettings(raw) {
		t.Error("restating the install (default GPU type included) is not a change")
	}
	c.GPUInstance = "g5.12xlarge"
	if !c.changesSettings(raw) {
		t.Error("another GPU type is a change")
	}
}

// The refusal says what it is about: giving an address to an install with none is
// not "changing the address this install has".
func TestInitYesRefusalWordsAnAddressFromNothing(t *testing.T) {
	dir, _ := existingDir(t, bedrockInstallTF)
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	err := yesCmd(dir, func(c *InitCmd) { c.Hostname = "chat.acme.com" }).execute(env)
	if err == nil || !strings.Contains(out.String(), "give this install a private address") || strings.Contains(out.String(), "change the address this install has") {
		t.Errorf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "pass --force") {
		t.Errorf("a script is pointed at --force:\n%s", out)
	}
}

// A flag that is not applied is said so, not dropped.
func TestInitExistingInstallNamesIgnoredFlags(t *testing.T) {
	dir, _, _ := liveInstall(t)
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	err := yesCmd(dir, func(c *InitCmd) { c.SemanticRouter, c.Throwaway, c.Profile = "on", true, "other" }).execute(env)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"--throwaway: ignored", "--profile: ignored", "--domain: ignored"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	// a region the install is not in stays refused on this path
	env, out = testEnv(t, newFakeCloud(), nil, "-")
	err = yesCmd(dir, func(c *InitCmd) { c.SemanticRouter, c.Region = "on", "eu-west-1" }).execute(env)
	if err == nil || !strings.Contains(out.String(), "cannot move") {
		t.Errorf("%v\n%s", err, out)
	}
}
