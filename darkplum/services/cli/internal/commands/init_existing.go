package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

// Running `darkplum init` again is how an install's settings are looked at and
// changed. It shows what the install has now, says what cannot change, and asks
// what to change. Nothing here mentions files: the settings are the install's,
// wherever they are kept.

// pickInstall decides which install an interactive `init` is about. It returns
// the install's name, or "" when init should make a new one. With --install the
// named install must exist; with a cluster name that is already an install, that
// install; with other installs on the machine, the operator chooses (a bare
// `darkplum init` must not quietly make a second install next to the one they
// meant).
func (c *InitCmd) pickInstall(env *initEnv, home install.Home) (string, error) {
	names, err := install.List(home)
	if err != nil {
		return "", err
	}
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	switch {
	case c.installName != "" && has(c.installName):
		return c.installName, nil
	case has(c.ClusterName):
		return c.ClusterName, nil
	case len(names) == 0:
		return "", nil
	}
	opts := make([]Option, 0, len(names)+1)
	for _, n := range names {
		opts = append(opts, Option{"Review and change " + n, "an install on this machine"})
	}
	opts = append(opts, Option{"Create another install", "named " + c.ClusterName})
	i, err := env.prompt.Select("You already have an install. What do you want to do?", opts, 0)
	if err != nil {
		return "", err
	}
	if i == len(names) {
		return "", nil
	}
	return names[i], nil
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// existingInstall shows the install at dir and asks what to change. done is true
// when init has nothing more to do (changed, left alone, or failed); false means
// the operator chose to start over and the full interview follows.
func (c *InitCmd) existingInstall(env *initEnv, dir string, raw map[string]string) (done bool, err error) {
	w := env.out
	tf, err := readTFVars(filepath.Join(dir, "terraform.tfvars"))
	if err != nil {
		return true, env.fail("install settings", firstLine(err.Error()), "choose Start over to answer the questions again", "cannot read the install's settings")
	}
	var rec install.Record
	if in, oerr := install.OpenDir(dir, c.ClusterName); oerr == nil {
		rec, _ = in.Load()
	}
	describeInstall(w, c.ClusterName, tf, rec)

	web := "Add the private address"
	if raw["web_domain"] != "" {
		web = "Change the private address"
	}
	i, err := env.prompt.Select("What do you want to change?", []Option{
		{web, "the name your team opens, and the networks that may reach it"},
		{"Start over", "asks every question again"},
		{"Change model hosting", "Bedrock only, or add a self-hosted model on a GPU"},
		{"Nothing", "leave the install as it is"},
	}, 3)
	if err != nil {
		return true, err
	}
	switch i {
	case 0:
		return true, c.mergeWeb(env, dir)
	case 1:
		return false, nil
	case 2:
		return true, c.changeHosting(env, dir, tf)
	}
	fmt.Fprintln(w, sDim("nothing changed"))
	return true, nil
}

// describeInstall prints the install's current settings.
func describeInstall(w interface{ Write([]byte) (int, error) }, name string, tf tfvarsInfo, rec install.Record) {
	row := func(k, v string) { fmt.Fprintf(w, "  %s  %s\n", sDim(fmt.Sprintf("%-8s", k)), v) }
	first := func(k string) string {
		if v := tfStrings(tf.Raw[k]); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	fmt.Fprintf(w, "%s\n", sHeader("▸ This install, as it is now"))
	row("install", name)
	cluster := first("cluster_name")
	if cluster == "" {
		cluster = name
	}
	row("cluster", cluster+" "+sDim("(fixed once the install exists)"))
	row("region", tf.Region+" "+sDim("(fixed)"))
	row("account", strings.Join(tf.AccountIDs, ", ")+" "+sDim("(fixed)"))
	if tf.Profile != "" {
		row("profile", tf.Profile)
	} else {
		row("profile", sDim("the default credential chain"))
	}
	if tf.GPUDesired > 0 {
		row("models", fmt.Sprintf("Bedrock + self-hosted: %d × %s, %s", tf.GPUDesired, orDefault(tf.GPUType, defaultGPUInstance), orDefault(first("model_name"), modelCatalog[0].ID)))
	} else {
		row("models", "Bedrock only")
	}
	if a := webAddressFrom(tf); a.hostname != "" {
		row("address", a.url()+" "+sDim("(from "+strings.Join(a.allowed, ", ")+")"))
	} else {
		row("address", sDim("none yet"))
	}
	if tf.Raw["db_deletion_protection"] == "false" {
		row("database", sWarn("test install: one-day backups, no deletion protection"))
	}
	if rec.Bundle != "" {
		row("deployed", rec.Bundle)
	} else {
		row("deployed", sDim("not yet"))
	}
	fmt.Fprintln(w)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// changeHosting switches an install between Bedrock only and Bedrock plus a
// self-hosted model, or changes the self-hosted model and the number of GPU
// nodes. The cluster's zones are fixed, so a GPU type those zones do not offer is
// refused rather than moved.
func (c *InitCmd) changeHosting(env *initEnv, dir string, tf tfvarsInfo) error {
	w := env.out
	if tf.Region == "" || len(tf.AccountIDs) == 0 {
		return env.fail("install settings", "no region or account", "choose Start over to answer the questions again", "the install's settings are incomplete")
	}
	s := &settings{profile: tf.Profile, region: tf.Region, gpuCount: c.GPUCount}
	record := func(r checkResult) {
		printCheckTo(w, r)
		s.checks = append(s.checks, r)
	}
	cloud := env.newCloud(tf.Profile)
	fmt.Fprintf(w, "%s resolving AWS identity…\n", sDim("…"))
	id, err := cloud.Identity()
	if err != nil {
		return env.fail("aws credentials", firstLine(err.Error()), "run `aws sso login` / `aws configure` for the install's profile", "cannot resolve AWS credentials")
	}
	if !contains(tf.AccountIDs, id.Account) {
		return env.fail("aws account", fmt.Sprintf("credentials are for %s, the install is in %s", id.Account, strings.Join(tf.AccountIDs, ",")),
			"use the install's profile", "credentials for another account")
	}
	s.account = id.Account
	record(checkResult{name: "aws account", status: checkPass, detail: id.Account + " (the install's account)"})

	current := 0
	if tf.GPUDesired > 0 {
		current = 1
	}
	mode := c.Mode
	if mode == "" && c.Model != "" {
		mode = "gpu"
	}
	if mode == "" {
		i, err := env.prompt.Select("How should Darkplum run models?", []Option{
			{"Bedrock only", "no GPU"},
			{"Self-hosted GPU", "adds a self-hosted model on a g5 GPU node"},
		}, current)
		if err != nil {
			return err
		}
		mode = []string{"bedrock", "gpu"}[i]
	}
	s.gpu = mode == "gpu"
	s.model = modelCatalog[0]

	var kvs []tfvarsKV
	drop := map[string]bool{"gpu_desired_size": true, "gpu_instance_types": true, "model_name": true, "model_revision": true, "served_model_name": true}
	if s.gpu {
		if err := c.chooseModel(env, s, record); err != nil {
			return err
		}
		count := s.gpuCount
		if env.interactive && c.GPUCount == 1 {
			def := tf.GPUDesired
			if def < 1 {
				def = 1
			}
			ans, err := env.prompt.Ask("GPU nodes (each self-hosted model takes one)", strconv.Itoa(def), func(v string) error {
				n, e := strconv.Atoi(strings.TrimSpace(v))
				if e != nil || n < 1 || n > 8 {
					return fmt.Errorf("enter a number from 1 to 8")
				}
				return nil
			})
			if err != nil {
				return err
			}
			count, _ = strconv.Atoi(strings.TrimSpace(ans))
		}
		s.gpuCount = count
		// The cluster's zones are fixed: the GPU type has to be offered in every one.
		offered, err := cloud.OfferedZones(tf.Region, defaultCPUInstance, s.gpuInstance)
		if err != nil {
			return env.fail("availability zones", firstLine(err.Error()), "needs ec2:DescribeInstanceTypeOfferings in "+tf.Region, "could not read instance offerings")
		}
		for _, az := range tf.AZs {
			if !contains(offered, az) {
				return env.fail("availability zones", fmt.Sprintf("%s is not offered in %s", s.gpuInstance, az),
					"this install's zones are fixed; choose a model that fits another GPU type, or create a new install", "the GPU type is not offered in the install's zones")
			}
		}
		record(checkResult{name: "availability zones", status: checkPass, detail: s.gpuInstance + " is offered in " + strings.Join(tf.AZs, ", ")})
		q := cloud.GPUQuota(tf.Region, s.gpuInstance)
		record(q)
		if q.status == checkFail {
			if !env.interactive {
				return reportedErr("GPU quota too low for %s in %s", s.gpuInstance, tf.Region)
			}
			ok, err := env.prompt.Confirm("Continue anyway? The GPU node will not schedule until the quota is raised", false)
			if err != nil {
				return err
			}
			if !ok {
				return reportedErr("stopped: raise the GPU quota (or choose Bedrock only) and run init again")
			}
		}
		kvs = []tfvarsKV{{"gpu_desired_size", s.gpuCount}, {"gpu_instance_types", []string{s.gpuInstance}},
			{"model_name", s.model.ID}, {"model_revision", s.model.Revision}, {"served_model_name", s.model.Served}}
	} else {
		// As a fresh Bedrock-only install records it: no GPU, the default model named explicitly.
		kvs = []tfvarsKV{{"gpu_desired_size", 0}, {"model_name", s.model.ID}, {"model_revision", s.model.Revision}, {"served_model_name", s.model.Served}}
	}

	path := filepath.Join(dir, "terraform.tfvars")
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := rewriteTFVars(string(old), func(k string) bool { return drop[k] }, kvs)

	fmt.Fprintln(w)
	if tf.GPUDesired > 0 && !s.gpu {
		fmt.Fprintf(w, "  %s applying this removes the GPU nodes and the self-hosted model. `darkplum deploy` shows the plan before it changes anything.\n", sWarn("!"))
	}
	if c.DryRun {
		fmt.Fprintf(w, "%s\n%s\n%s\n", sHeader("settings (dry run — not saved)"), text, sSuccess("✓ dry run complete — nothing was changed"))
		return nil
	}
	if env.interactive {
		ok, err := env.prompt.Confirm("Save this change to the install?", true)
		if err != nil {
			return err
		}
		if !ok {
			return errAborted
		}
	}
	if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(text)); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%s\n", sSuccess("✓ model hosting changed; every other setting is unchanged"))
	fmt.Fprintf(w, "  Next: %s to apply it\n", sCmd("darkplum deploy"))
	return nil
}

// rewriteTFVars removes the lines whose key drop reports, keeps every other line
// (comments included) as it was, and appends kvs.
func rewriteTFVars(text string, drop func(key string) bool, kvs []tfvarsKV) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if m := tfLineRe.FindStringSubmatch(line); m != nil && drop(m[1]) {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n") + "\n"
	for _, kv := range kvs {
		out += fmt.Sprintf("%s = %s\n", kv.Key, hclValue(kv.Value))
	}
	return out
}

// webKeys are the settings of the private address.
var webKeys = map[string]bool{"web_domain": true, "web_hostname": true, "web_zone_id": true, "web_certificate_arn": true, "web_allowed_cidrs": true}
