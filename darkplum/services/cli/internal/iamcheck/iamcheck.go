// Package iamcheck is the Layer 1 self-test: it asks IAM, with
// simulate-principal-policy, whether the gateway's role may invoke exactly the
// Bedrock models the install allows and nothing else. It reads IAM and changes
// nothing, and it is self-contained so both `darkplum deploy` (at its end) and a
// `verify` command can call it.
//
// The answers come from the cloud's own policy evaluation, not from this tool's
// reading of the policy: if the role is wider than the install says, this fails.
package iamcheck

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// Input is what to simulate. InvokeARNs is the bedrock_invoke_arns Terraform
// output: the exact ARNs the role may invoke.
type Input struct {
	RoleARN    string
	Region     string // the inference region (inference_region output)
	Profile    string // AWS profile of the install ("" = default chain)
	InvokeARNs []string
}

// Check is one simulated call.
type Check struct {
	Resource string
	Want     string // "allowed" or "implicitDeny"
	Got      string
}

// OK reports whether IAM answered as the install says it should.
func (c Check) OK() bool { return c.Want == c.Got }

// Report is every simulation made.
type Report struct {
	Role   string
	Checks []Check
	// NoAllowedModels is set when the install allows no inference profile, so only
	// denials were checked.
	NoAllowedModels bool
}

// OK is true when every check answered as expected (and there was at least one).
func (r Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK() {
			return false
		}
	}
	return len(r.Checks) > 0
}

// Failed lists the checks that did not.
func (r Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK() {
			out = append(out, c)
		}
	}
	return out
}

var (
	roleRe   = regexp.MustCompile(`^arn:aws[a-z-]*:iam::([0-9]{12}):role/[A-Za-z0-9+=,.@_/-]+$`)
	regionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
)

// Run simulates bedrock:InvokeModel as the role: one resource the install allows
// (if it allows any), then three it must not: an inference profile no pattern
// names, a foundation model that is not in the allowlist, and a model in a region
// the role has no business in. An error means IAM could not be asked (missing
// iam:SimulatePrincipalPolicy, expired session); it is never reported as a pass.
func Run(aws runner.AWSFunc, in Input) (Report, error) {
	m := roleRe.FindStringSubmatch(in.RoleARN)
	if m == nil {
		return Report{}, fmt.Errorf("iamcheck: %q is not an IAM role ARN", in.RoleARN)
	}
	if !regionRe.MatchString(in.Region) {
		return Report{}, fmt.Errorf("iamcheck: %q is not a region", in.Region)
	}
	account := m[1]
	rep := Report{Role: in.RoleARN}

	sim := func(resource string) (string, error) {
		out, err := aws(runner.ProfileArgs(in.Profile, "iam", "simulate-principal-policy",
			"--policy-source-arn", in.RoleARN, "--action-names", "bedrock:InvokeModel",
			"--resource-arns", resource, "--query", "EvaluationResults[0].EvalDecision", "--output", "text")...)
		if err != nil {
			return "", fmt.Errorf("iamcheck: IAM could not simulate %s: %w", resource, err)
		}
		return strings.TrimSpace(out), nil
	}
	check := func(want, resource string) error {
		got, err := sim(resource)
		if err != nil {
			return err
		}
		rep.Checks = append(rep.Checks, Check{Resource: resource, Want: want, Got: got})
		return nil
	}

	if sample := allowedSample(in.InvokeARNs); sample != "" {
		if err := check("allowed", sample); err != nil {
			return rep, err
		}
	} else {
		rep.NoAllowedModels = true
	}
	for _, r := range []string{
		fmt.Sprintf("arn:aws:bedrock:%s:%s:inference-profile/global.anthropic.claude-example-v1:0", in.Region, account),
		fmt.Sprintf("arn:aws:bedrock:%s::foundation-model/darkplum.not-an-allowed-model-v1:0", in.Region),
		"arn:aws:bedrock:ap-southeast-7::foundation-model/anthropic.claude-example-v1:0",
	} {
		if err := check("implicitDeny", r); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// allowedSample turns the first inference-profile ARN of the allowlist into a
// concrete resource the pattern must allow: a trailing * becomes a model name.
func allowedSample(arns []string) string {
	for _, a := range arns {
		if strings.Contains(a, "inference-profile/") {
			if strings.HasSuffix(a, "*") {
				return strings.TrimSuffix(a, "*") + "example-v1:0"
			}
			return a
		}
	}
	return ""
}
