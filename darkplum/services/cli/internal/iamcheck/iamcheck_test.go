package iamcheck

import (
	"errors"
	"strings"
	"testing"
)

const role = "arn:aws:iam::123456789012:role/dp-trial-gateway"

type sim struct {
	calls   [][]string
	answers map[string]string // resource substring -> decision
	err     error
}

func (s *sim) run(args ...string) (string, error) {
	s.calls = append(s.calls, args)
	if s.err != nil {
		return "", s.err
	}
	j := strings.Join(args, " ")
	for sub, d := range s.answers {
		if strings.Contains(j, sub) {
			return d + "\n", nil
		}
	}
	return "implicitDeny", nil
}

var arns = []string{"arn:aws:bedrock:us-east-2:123456789012:inference-profile/us.anthropic.claude-*", "arn:aws:bedrock:*::foundation-model/anthropic.claude-*"}

func TestAllowedAndDeniedAsConfigured(t *testing.T) {
	s := &sim{answers: map[string]string{"inference-profile/us.anthropic.claude-example-v1:0": "allowed"}}
	rep, err := Run(s.run, Input{RoleARN: role, Region: "us-east-2", Profile: "acme", InvokeARNs: arns})
	if err != nil || !rep.OK() || len(rep.Checks) != 4 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	for _, c := range s.calls {
		if c[0] != "--profile" || c[1] != "acme" {
			t.Errorf("simulate without the install's profile: %v", c)
		}
	}
}

func TestRoleWiderThanConfiguredFails(t *testing.T) {
	// The role allows a foundation model the install never listed.
	s := &sim{answers: map[string]string{"inference-profile/us.anthropic.claude-example-v1:0": "allowed", "darkplum.not-an-allowed-model": "allowed"}}
	rep, err := Run(s.run, Input{RoleARN: role, Region: "us-east-2", InvokeARNs: arns})
	if err != nil || rep.OK() {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if f := rep.Failed(); len(f) != 1 || f[0].Got != "allowed" {
		t.Errorf("failed = %+v", f)
	}
}

func TestRoleThatDeniesTheAllowedModelFails(t *testing.T) {
	rep, err := Run((&sim{}).run, Input{RoleARN: role, Region: "us-east-2", InvokeARNs: arns})
	if err != nil || rep.OK() {
		t.Fatalf("an allowed model that IAM denies must fail: %+v %v", rep, err)
	}
}

func TestNoAllowedModelsChecksDenialsOnly(t *testing.T) {
	rep, err := Run((&sim{}).run, Input{RoleARN: role, Region: "us-east-2"})
	if err != nil || !rep.OK() || !rep.NoAllowedModels || len(rep.Checks) != 3 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}

func TestSimulationErrorIsAnErrorNotAPass(t *testing.T) {
	rep, err := Run((&sim{err: errors.New("AccessDenied: iam:SimulatePrincipalPolicy")}).run, Input{RoleARN: role, Region: "us-east-2", InvokeARNs: arns})
	if err == nil || rep.OK() {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}

func TestRefusesMalformedInput(t *testing.T) {
	for _, in := range []Input{
		{RoleARN: "", Region: "us-east-2"},
		{RoleARN: "arn:aws:iam::12345:role/x", Region: "us-east-2"},
		{RoleARN: role + " --evil", Region: "us-east-2"},
		{RoleARN: role, Region: "us-east-2; rm"},
	} {
		s := &sim{}
		if _, err := Run(s.run, in); err == nil || len(s.calls) != 0 {
			t.Errorf("%+v: err=%v calls=%d", in, err, len(s.calls))
		}
	}
}
