package commands

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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

	// Tools every install needs regardless of cloud.
	checks := []checkResult{
		checkTool("terraform"),
		checkTool("kubectl"),
		checkTool("jq"),
		checkTool("docker"),
	}

	switch d.Cloud {
	case "aws":
		checks = append(checks,
			checkTool("aws"),
			checkAWSCreds(d.Profile),
			checkAWSGPUQuota(region, d.Profile),
		)
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
func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

func checkTool(name string) checkResult {
	if path, err := exec.LookPath(name); err == nil {
		return checkResult{name: name, status: checkPass, detail: path}
	}
	return checkResult{name: name, status: checkFail, hint: "not on PATH — install it and retry"}
}

// awsArgs prefixes `--profile <p>` when a profile is set, so doctor checks the
// same account the install will use (var.aws_profile) rather than whatever the
// ambient default credential chain resolves to.
func awsArgs(profile string, args ...string) []string {
	if profile != "" {
		return append([]string{"--profile", profile}, args...)
	}
	return args
}

func checkAWSCreds(profile string) checkResult {
	out, err := run("aws", awsArgs(profile, "sts", "get-caller-identity", "--query", "Account", "--output", "text")...)
	if err != nil {
		return checkResult{name: "aws credentials", status: checkFail,
			hint: "run `aws configure` / `aws sso login`, set AWS_PROFILE, or pass --profile <name>"}
	}
	return checkResult{name: "aws credentials", status: checkPass, detail: "account " + out}
}

// checkAWSGPUQuota reads the EC2 "Running On-Demand G and VT instances" quota
// (code L-DB2E81BA), whose value is a vCPU count. g5.xlarge needs 4 vCPUs; a
// fresh account often has this set to 0, which is the most common silent cause
// of a `terraform apply` that succeeds but never schedules the GPU node.
func checkAWSGPUQuota(region, profile string) checkResult {
	out, err := run("aws", awsArgs(profile, "service-quotas", "get-service-quota",
		"--service-code", "ec2", "--quota-code", "L-DB2E81BA",
		"--region", region, "--query", "Quota.Value", "--output", "text")...)
	if err != nil {
		return checkResult{name: "GPU vCPU quota", status: checkWarn,
			detail: "could not read",
			hint:   "verify 'Running On-Demand G and VT instances' quota in " + region + " (need >=4 for g5.xlarge)"}
	}
	v, perr := strconv.ParseFloat(out, 64)
	if perr != nil {
		return checkResult{name: "GPU vCPU quota", status: checkWarn,
			detail: "unexpected value " + out,
			hint:   "verify 'Running On-Demand G and VT instances' quota in " + region}
	}
	detail := fmt.Sprintf("%g vCPUs", v)
	if v < 4 {
		return checkResult{name: "GPU vCPU quota", status: checkFail, detail: detail,
			hint: "request an increase for 'Running On-Demand G and VT instances' (need >=4 for g5.xlarge) — or, if you install into a different account, re-run with --profile/--region to match it"}
	}
	return checkResult{name: "GPU vCPU quota", status: checkPass, detail: detail}
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

func printCheck(c checkResult) {
	var mark string
	switch c.status {
	case checkPass:
		mark = sSuccess("✓")
	case checkWarn:
		mark = sInfo("!")
	case checkFail:
		mark = sFail("✗")
	}
	line := fmt.Sprintf("  %s %-20s", mark, c.name)
	if c.detail != "" {
		line += " " + sDim(c.detail)
	}
	fmt.Println(line)
	if c.hint != "" && c.status != checkPass {
		fmt.Printf("      %s\n", sDim("→ "+c.hint))
	}
}
