// Package iamcheck proves the gateway role's Bedrock allowlist (Layer 1) with
// IAM policy simulation: the role must be allowed to invoke a model the install
// permits and must be implicitly denied everything else. Layer 1 is the cloud's
// own enforcement; this is the evidence that it behaves as configured, and it
// changes nothing (iam:SimulatePrincipalPolicy is read-only).
//
// The package is self-contained: Run takes an AWS command function and the
// install's Terraform outputs, nothing else.
package iamcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// Action is the one action simulated.
const Action = "bedrock:InvokeModel"

// Decisions IAM reports.
const (
	Allowed      = "allowed"
	ImplicitDeny = "implicitDeny"
	ExplicitDeny = "explicitDeny"
)

// Outputs are the Terraform outputs the check needs.
type Outputs struct {
	GatewayRoleARN    string   `json:"gateway_role_arn"`
	InferenceRegion   string   `json:"inference_region"`
	BedrockInvokeARNs []string `json:"bedrock_invoke_arns"`
}

// ErrNoRole means the stack has no gateway role output (not applied yet).
var ErrNoRole = errors.New("no gateway_role_arn output: the infrastructure has not been applied; run `darkplum deploy`")

// FromTerraform picks the needed outputs out of `terraform output -json` values.
func FromTerraform(raw map[string]json.RawMessage) (Outputs, error) {
	var o Outputs
	get := func(name string, into any) error {
		v, ok := raw[name]
		if !ok || string(v) == "null" {
			return nil
		}
		if err := json.Unmarshal(v, into); err != nil {
			return fmt.Errorf("terraform output %s: %w", name, err)
		}
		return nil
	}
	if err := get("gateway_role_arn", &o.GatewayRoleARN); err != nil {
		return o, err
	}
	if err := get("inference_region", &o.InferenceRegion); err != nil {
		return o, err
	}
	if err := get("bedrock_invoke_arns", &o.BedrockInvokeARNs); err != nil {
		return o, err
	}
	return o, nil
}

// Check is one simulated request.
type Check struct {
	Expect   string `json:"expect"`
	Resource string `json:"resource"`
	Got      string `json:"got,omitempty"`
	Pass     bool   `json:"pass"`
	// Error is set when the simulation itself could not run (expired session, no
	// permission to simulate): the policy was not evaluated, which is not a policy failure.
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Report is the evidence.
type Report struct {
	Role     string    `json:"role"`
	Account  string    `json:"account"`
	Region   string    `json:"region"`
	Action   string    `json:"action"`
	Time     time.Time `json:"time"`
	NoteLine string    `json:"note,omitempty"` // for example "no Bedrock models are allowed"
	Checks   []Check   `json:"checks"`
}

// Passed is true when every check ran and matched.
func (r Report) Passed() bool {
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	return len(r.Checks) > 0
}

// Failed counts checks that evaluated to the wrong decision.
func (r Report) Failed() int {
	n := 0
	for _, c := range r.Checks {
		if !c.Pass && c.Error == "" {
			n++
		}
	}
	return n
}

// Unevaluated counts checks that could not run.
func (r Report) Unevaluated() int {
	n := 0
	for _, c := range r.Checks {
		if c.Error != "" {
			n++
		}
	}
	return n
}

// SimulationError is returned (with the Report) when a simulation could not run.
type SimulationError struct{ First string }

func (e *SimulationError) Error() string {
	return "IAM policy simulation could not run: " + e.First
}

var roleRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::([0-9]{12}):role/\S+$`)

// RoleAccount is the account of a role ARN.
func RoleAccount(arn string) (string, bool) {
	m := roleRe.FindStringSubmatch(arn)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Run simulates bedrock:InvokeModel as the gateway role against:
//
//  1. a model the allowlist names (when it names any): allowed;
//  2. an Anthropic inference profile in the install's own account: implicitDeny;
//  3. a foundation model nobody allowed: implicitDeny;
//  4. an Anthropic foundation model in a region that is not the install's: implicitDeny.
//
// A deny that is not implicit (an explicit Deny statement) is reported as a
// failure: the allowlist is what is being proven. The error is non-nil only
// when the check could not be set up or a simulation could not run; a policy
// that evaluates wrongly is a failed Report, not an error.
func Run(ctx context.Context, aws runner.AWSFunc, o Outputs) (Report, error) {
	if o.GatewayRoleARN == "" {
		return Report{}, ErrNoRole
	}
	account, ok := RoleAccount(o.GatewayRoleARN)
	if !ok {
		return Report{}, fmt.Errorf("gateway_role_arn %q is not an IAM role ARN", o.GatewayRoleARN)
	}
	if o.InferenceRegion == "" {
		return Report{}, errors.New("no inference_region output: the infrastructure has not been applied; run `darkplum deploy`")
	}
	rep := Report{Role: o.GatewayRoleARN, Account: account, Region: o.InferenceRegion, Action: Action, Time: time.Now().UTC()}

	allowed := ""
	for _, a := range o.BedrockInvokeARNs {
		if strings.Contains(a, "inference-profile/") {
			allowed = a
			break
		}
	}
	var checks []Check
	switch {
	case allowed != "":
		// Wildcards in the allowlist stand for any model id: use a concrete one.
		checks = append(checks, Check{Expect: Allowed, Resource: strings.TrimSuffix(allowed, "*") + trailing(allowed)})
	case len(o.BedrockInvokeARNs) == 0:
		rep.NoteLine = "no Bedrock models are allowed; checking denials only"
	default:
		rep.NoteLine = "the allowlist holds no inference profile to test as allowed; checking denials only"
	}
	checks = append(checks,
		Check{Expect: ImplicitDeny, Resource: "arn:aws:bedrock:" + o.InferenceRegion + ":" + account + ":inference-profile/global.anthropic.claude-example-v1:0"},
		Check{Expect: ImplicitDeny, Resource: "arn:aws:bedrock:" + o.InferenceRegion + "::foundation-model/darkplum.not-an-allowed-model-v1:0"},
		Check{Expect: ImplicitDeny, Resource: "arn:aws:bedrock:ap-southeast-7::foundation-model/anthropic.claude-example-v1:0"},
	)

	first := ""
	for i := range checks {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		c := &checks[i]
		got, err := simulate(aws, o.GatewayRoleARN, c.Resource)
		switch {
		case err != nil:
			c.Error = err.Error()
			if first == "" {
				first = c.Error
			}
		default:
			c.Got = got
			c.Pass = got == c.Expect
			if got == ExplicitDeny && c.Expect == ImplicitDeny {
				c.Note = "denied by an explicit Deny statement, not by the allowlist"
			}
		}
	}
	rep.Checks = checks
	if first != "" {
		return rep, &SimulationError{First: first}
	}
	return rep, nil
}

// trailing is "example-v1:0" when the allowlist ARN ends in a wildcard, so the
// concrete resource is a model id the wildcard covers; otherwise nothing.
func trailing(arn string) string {
	if strings.HasSuffix(arn, "*") {
		return "example-v1:0"
	}
	return ""
}

func simulate(aws runner.AWSFunc, role, resource string) (string, error) {
	out, err := aws("iam", "simulate-principal-policy", "--policy-source-arn", role, "--action-names", Action,
		"--resource-arns", resource, "--query", "EvaluationResults[0].EvalDecision", "--output", "text")
	if err != nil {
		return "", errors.New(firstLine(err.Error()))
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "None" {
		return "", errors.New("IAM returned no evaluation result")
	}
	return out, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// WriteText writes the report as plain text, with no colour: it is meant to be
// pasted into an audit ticket. Context lines (who ran it, which install) are
// added by the caller before this.
func (r Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "Simulating %s as %s\n", r.Action, r.Role)
	fmt.Fprintf(w, "  account %s, inference region %s, at %s\n", r.Account, r.Region, r.Time.Format(time.RFC3339))
	if r.NoteLine != "" {
		fmt.Fprintf(w, "  (%s)\n", r.NoteLine)
	}
	for _, c := range r.Checks {
		switch {
		case c.Error != "":
			fmt.Fprintf(w, "  ERROR expected %s, could not evaluate (%s)  %s\n", c.Expect, c.Error, c.Resource)
		case c.Pass:
			fmt.Fprintf(w, "  ok    %s  %s\n", c.Expect, c.Resource)
		default:
			fmt.Fprintf(w, "  FAIL  expected %s, got %s  %s\n", c.Expect, c.Got, c.Resource)
			if c.Note != "" {
				fmt.Fprintf(w, "        (%s)\n", c.Note)
			}
		}
	}
	switch {
	case r.Unevaluated() > 0:
		fmt.Fprintln(w, "Layer 1 allowlist NOT evaluated: the simulation could not run (this is not a policy result).")
	case r.Passed():
		fmt.Fprintln(w, "Layer 1 allowlist behaves as configured.")
	default:
		fmt.Fprintln(w, "Layer 1 allowlist does NOT behave as configured.")
	}
}
