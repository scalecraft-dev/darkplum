package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// DoctorCmd groups read-only environment and health diagnostics. It provisions
// nothing and never holds a credential — every check shells out under the
// operator's own local cloud CLI / kubeconfig.
type DoctorCmd struct {
	Check DoctorCheckCmd `cmd:"" help:"Check the local environment can run an install (tools, cloud creds, GPU capacity)."`
}

// DoctorCheckCmd is `darkplum doctor check` — validation that the local
// environment can actually run an install, BEFORE any `terraform apply` is
// attempted. It checks required tools, cloud credentials, and GPU capacity so a
// missing prerequisite fails in seconds instead of minutes into a provision.
type DoctorCheckCmd struct {
	Cloud   string `help:"Target cloud provider." enum:"aws,gcp" default:"aws"`
	Region  string `help:"Region to check GPU capacity in — must match aws_region in your terraform.tfvars. Defaults per cloud." default:""`
	Profile string `help:"AWS profile to check against — must match aws_profile in terraform.tfvars, else the check hits the wrong account. Defaults to the standard AWS credential chain." default:""`

	Tfvars     string `help:"Path to the terraform.tfvars written by 'darkplum init'. Takes profile, region, GPU type and AZs from it and checks the credentials belong to the account it names. 'make deploy' passes this." default:""`
	MinSession int    `help:"Fail when the AWS session expires within this many minutes (an SSO login that lapses mid-apply strands a half-built cluster). 0 only reports it." name:"min-session" default:"0"`
}

// checkStatus is the outcome of a single check.
type checkStatus int

const (
	checkPass checkStatus = iota
	checkWarn
	checkFail
)

type checkResult struct {
	name   string
	status checkStatus
	detail string // shown dimmed after the name (e.g. resolved path, account id)
	hint   string // remediation, shown only on warn/fail
}

// Run executes `darkplum doctor check --cloud aws|gcp`.
func (d *DoctorCheckCmd) Run(_ *Globals) error {
	var tf tfvarsInfo
	if d.Tfvars != "" {
		var err error
		if tf, err = readTFVars(d.Tfvars); err != nil {
			return fmt.Errorf("%w — run ./bin/darkplum init first", err)
		}
		if d.Profile == "" {
			d.Profile = tf.Profile
		}
		if d.Region == "" {
			d.Region = tf.Region
		}
	}
	region := d.Region
	if region == "" {
		region = defaultRegion(d.Cloud)
	}

	header := fmt.Sprintf("%s   %s %s   %s %s",
		sHeader("▸ doctor · check"),
		sDim("cloud"), sInfo(d.Cloud),
		sDim("region"), sInfo(region))
	if d.Cloud == "aws" && d.Profile != "" {
		header += fmt.Sprintf("   %s %s", sDim("profile"), sInfo(d.Profile))
	}
	fmt.Printf("%s\n\n", header)

	// Tools every install needs regardless of cloud: Terraform and kubectl. (jq and
	// docker were the Makefile flow's; images are copied by darkplum itself now.)
	checks := []checkResult{
		checkTool("terraform"),
		checkTool(runner.Kubectl),
	}

	switch d.Cloud {
	case "aws":
		checks = append(checks,
			checkTool("aws"),
			checkAWSCreds(d.Profile),
			checkAWSSession(d.Profile, d.MinSession),
		)
		if d.Tfvars != "" {
			checks = append(checks, checkAccountMatch(d.Profile, tf.AccountIDs))
		}
		gpuType, wantGPU := defaultGPUInstance, true
		if d.Tfvars != "" {
			wantGPU = tf.GPUDesired > 0
			if tf.GPUType != "" {
				gpuType = tf.GPUType
			}
		}
		if wantGPU {
			checks = append(checks, checkAWSGPUQuota(region, d.Profile, gpuType))
		}
		if d.Tfvars != "" && len(tf.AZs) > 0 {
			types := []string{defaultCPUInstance}
			if wantGPU {
				types = append(types, gpuType)
			}
			checks = append(checks, checkOfferings(newAWSCloud(d.Profile, nil), region, tf.AZs, types))
		}
	case "gcp":
		checks = append(checks,
			checkTool("gcloud"),
			checkGCPCreds(),
			checkGCPProject(),
			gcpRootNotReady(),
		)
	}

	failed := 0
	for _, c := range checks {
		printCheck(c)
		if c.status == checkFail {
			failed++
		}
	}

	fmt.Println()
	if failed > 0 {
		return fmt.Errorf("%d install check(s) failed — resolve the above before installing", failed)
	}
	fmt.Println(sSuccess("✓ install checks passed — ready to install"))
	return nil
}

func defaultRegion(cloud string) string {
	if cloud == "gcp" {
		return "us-central1"
	}
	return "us-east-2"
}

// run executes a command and returns its trimmed stdout. Stderr is deliberately
// NOT merged in: callers parse this value (e.g. a quota number), and CLI
// deprecation/SSO warnings on stderr would corrupt the parse.
func run(name string, args ...string) (string, error) { return runner.ToolStdout(name, args...) }

func checkTool(name string) checkResult {
	if path, err := runner.Which(name); err == nil {
		return checkResult{name: name, status: checkPass, detail: path}
	}
	return checkResult{name: name, status: checkFail, hint: "not on PATH — install it and retry"}
}

// awsArgs prefixes `--profile <p>` when a profile is set, so doctor checks the
// same account the install will use (var.aws_profile) rather than whatever the
// ambient default credential chain resolves to.
func awsArgs(profile string, args ...string) []string { return runner.ProfileArgs(profile, args...) }

func checkAWSCreds(profile string) checkResult {
	out, err := run("aws", awsArgs(profile, "sts", "get-caller-identity", "--query", "Account", "--output", "text")...)
	if err != nil {
		return checkResult{name: "aws credentials", status: checkFail,
			hint: "run `aws configure` / `aws sso login`, set AWS_PROFILE, or pass --profile <name>"}
	}
	return checkResult{name: "aws credentials", status: checkPass, detail: "account " + out}
}

// checkAWSGPUQuota reads the EC2 "Running On-Demand G and VT instances" quota
// (code L-DB2E81BA), whose value is a vCPU count. The instance type decides how
// many vCPUs it needs (g5.xlarge: 4); a fresh account often has this set to 0,
// which is the most common silent cause of a `terraform apply` that succeeds but
// never schedules the GPU node.
func checkAWSGPUQuota(region, profile, instance string) checkResult {
	out, err := run("aws", awsArgs(profile, quotaArgs(region)...)...)
	return gpuQuotaResult(out, err, region, instance)
}

func quotaArgs(region string) []string {
	return []string{"service-quotas", "get-service-quota",
		"--service-code", "ec2", "--quota-code", "L-DB2E81BA",
		"--region", region, "--query", "Quota.Value", "--output", "text"}
}

func gpuQuotaResult(out string, err error, region, instance string) checkResult {
	need := 4
	if gi, ok := gpuInstances[instance]; ok {
		need = gi.VCPU
	}
	want := fmt.Sprintf("need %d for %s", need, instance)
	if err != nil {
		return checkResult{name: "GPU vCPU quota", status: checkWarn,
			detail: "could not read",
			hint:   "verify 'Running On-Demand G and VT instances' quota in " + region + " (" + want + ")"}
	}
	v, perr := strconv.ParseFloat(out, 64)
	if perr != nil {
		return checkResult{name: "GPU vCPU quota", status: checkWarn,
			detail: "unexpected value " + out,
			hint:   "verify 'Running On-Demand G and VT instances' quota in " + region}
	}
	detail := fmt.Sprintf("%g vCPUs (%s)", v, want)
	if v < float64(need) {
		return checkResult{name: "GPU vCPU quota", status: checkFail, detail: detail,
			hint: "request an increase for 'Running On-Demand G and VT instances' — or, if you install into a different account, re-run with --profile/--region to match it"}
	}
	return checkResult{name: "GPU vCPU quota", status: checkPass, detail: detail}
}

// awsSessionExpiry returns when the resolved credentials stop working. A zero
// time means they do not expire (static keys).
func awsSessionExpiry(profile string) (time.Time, error) {
	out, err := run("aws", awsArgs(profile, "configure", "export-credentials", "--format", "process")...)
	if err != nil {
		return time.Time{}, err
	}
	var c struct {
		Expiration string `json:"Expiration"`
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		return time.Time{}, err
	}
	if c.Expiration == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, c.Expiration)
}

// checkAWSSession fails when the session would lapse inside an install
// (~35-50 minutes of apply plus the platform deploy).
func checkAWSSession(profile string, minMinutes int) checkResult {
	return sessionResult(awsSessionExpiry, profile, minMinutes, time.Now())
}

func sessionResult(expiry func(string) (time.Time, error), profile string, minMinutes int, now time.Time) checkResult {
	exp, err := expiry(profile)
	if err != nil {
		return checkResult{name: "session lifetime", status: checkWarn, detail: "could not read",
			hint: "needs AWS CLI >= 2.9; make sure the session lasts the whole install (about 90 minutes)"}
	}
	if exp.IsZero() {
		return checkResult{name: "session lifetime", status: checkPass, detail: "long-lived credentials"}
	}
	left := exp.Sub(now).Round(time.Minute)
	detail := fmt.Sprintf("%d min remaining", int(left.Minutes()))
	if minMinutes > 0 && left < time.Duration(minMinutes)*time.Minute {
		return checkResult{name: "session lifetime", status: checkFail, detail: detail,
			hint: fmt.Sprintf("the install needs about %d minutes — run `aws sso login` (add --profile <name>) and retry; to proceed with less, lower the bar: make deploy MIN_SESSION=<minutes> (or --min-session)", minMinutes)}
	}
	return checkResult{name: "session lifetime", status: checkPass, detail: detail}
}

// checkAccountMatch refuses to deploy under credentials for an account other
// than the one init wrote into terraform.tfvars (the provider pin would refuse
// it later; failing here costs seconds, not a planned-then-refused apply).
func checkAccountMatch(profile string, want []string) checkResult {
	got, err := run("aws", awsArgs(profile, "sts", "get-caller-identity", "--query", "Account", "--output", "text")...)
	if err != nil {
		return checkResult{name: "account match", status: checkFail, hint: "credentials not usable — run `aws sso login`"}
	}
	return accountMatchResult(got, want)
}

func accountMatchResult(got string, want []string) checkResult {
	if len(want) == 0 {
		return checkResult{name: "account match", status: checkWarn, detail: "tfvars pins no account",
			hint: "re-run ./bin/darkplum init so the install is pinned to one account"}
	}
	for _, w := range want {
		if w == got {
			return checkResult{name: "account match", status: checkPass, detail: "account " + got}
		}
	}
	return checkResult{name: "account match", status: checkFail,
		detail: fmt.Sprintf("credentials are for %s, tfvars pins %s", got, strings.Join(want, ",")),
		hint:   "use the profile for the pinned account (aws_profile in terraform.tfvars) or re-run ./bin/darkplum init"}
}

// checkOfferings re-verifies that every AZ in tfvars still offers the instance
// types (capacity offerings change; init checked this at write time).
func checkOfferings(a *awsCloud, region string, azs, types []string) checkResult {
	offered, err := a.OfferedZones(region, types...)
	if err != nil {
		return checkResult{name: "instance offerings", status: checkWarn, detail: "could not read",
			hint: "verify " + strings.Join(types, ", ") + " are offered in " + strings.Join(azs, ", ")}
	}
	have := map[string]bool{}
	for _, z := range offered {
		have[z] = true
	}
	var missing []string
	for _, z := range azs {
		if !have[z] {
			missing = append(missing, z)
		}
	}
	if len(missing) > 0 {
		return checkResult{name: "instance offerings", status: checkFail, detail: strings.Join(missing, ", ") + " lack " + strings.Join(types, "+"),
			hint: "re-run ./bin/darkplum init to pick zones that offer them"}
	}
	return checkResult{name: "instance offerings", status: checkPass, detail: strings.Join(types, ", ") + " in " + strings.Join(azs, ", ")}
}

func checkGCPCreds() checkResult {
	out, err := run("gcloud", "auth", "list",
		"--filter=status:ACTIVE", "--format=value(account)")
	if err != nil || out == "" {
		return checkResult{name: "gcp credentials", status: checkFail,
			hint: "run `gcloud auth login` (or `gcloud auth application-default login`)"}
	}
	return checkResult{name: "gcp credentials", status: checkPass, detail: out}
}

func checkGCPProject() checkResult {
	out, err := run("gcloud", "config", "get-value", "project")
	if err != nil || out == "" || out == "(unset)" {
		return checkResult{name: "gcp project", status: checkFail,
			hint: "run `gcloud config set project <PROJECT_ID>`"}
	}
	return checkResult{name: "gcp project", status: checkPass, detail: out}
}

// gcpRootNotReady is an honest blocker: the terraform/gcp root module does not
// exist yet, so a GCP install cannot complete. It fails the check (rather than
// warns) so the exit code never claims GCP is "ready to install" — but the
// tool/credential checks above still run, so the operator can prep ahead.
func gcpRootNotReady() checkResult {
	return checkResult{name: "gcp terraform root", status: checkFail,
		detail: "not yet available",
		hint:   "the terraform/gcp root module is planned; AWS is the only installable cloud today"}
}

func printCheck(c checkResult) { printCheckTo(os.Stdout, c) }

// printCheckTo renders one check row; init reuses it for every answer it records.
func printCheckTo(w io.Writer, c checkResult) {
	var mark string
	switch c.status {
	case checkPass:
		mark = sSuccess("✓")
	case checkWarn:
		mark = sWarn("!")
	case checkFail:
		mark = sFail("✗")
	}
	line := fmt.Sprintf("  %s %-20s", mark, c.name)
	if c.detail != "" {
		line += " " + sDim(c.detail)
	}
	fmt.Fprintln(w, line)
	if c.hint != "" && c.status != checkPass {
		fmt.Fprintf(w, "      %s\n", sDim("→ "+c.hint))
	}
}
