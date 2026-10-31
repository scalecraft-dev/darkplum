package iamcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const role = "arn:aws:iam::111122223333:role/darkplum-gateway"

var outputs = Outputs{GatewayRoleARN: role, InferenceRegion: "us-east-2",
	BedrockInvokeARNs: []string{"arn:aws:bedrock:us-east-2::foundation-model/anthropic.*", "arn:aws:bedrock:us-east-2:111122223333:inference-profile/us.anthropic.claude-sonnet-*"}}

// fakeAWS answers by resource and records every call.
type fakeAWS struct {
	calls   [][]string
	byRes   map[string]string
	err     error
	nothing bool
}

func (f *fakeAWS) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return "", f.err
	}
	var res string
	for i, a := range args {
		if a == "--resource-arns" {
			res = args[i+1]
		}
	}
	if f.nothing {
		return "None", nil
	}
	if d, ok := f.byRes[res]; ok {
		return d, nil
	}
	return ImplicitDeny, nil
}

func allowedBehaviour() *fakeAWS {
	return &fakeAWS{byRes: map[string]string{"arn:aws:bedrock:us-east-2:111122223333:inference-profile/us.anthropic.claude-sonnet-example-v1:0": Allowed}}
}

func TestRunPassesWhenTheAllowlistBehaves(t *testing.T) {
	f := allowedBehaviour()
	rep, err := Run(context.Background(), f.run, outputs)
	if err != nil || !rep.Passed() {
		t.Fatalf("%v %+v", err, rep)
	}
	if len(f.calls) != 4 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	// the exact aws invocation, read-only, with no profile or region of its own
	want := "iam simulate-principal-policy --policy-source-arn " + role + " --action-names bedrock:InvokeModel --resource-arns "
	for _, c := range f.calls {
		if !strings.HasPrefix(strings.Join(c, " "), want) {
			t.Errorf("call = %v", c)
		}
	}
	wantRes := []string{
		"arn:aws:bedrock:us-east-2:111122223333:inference-profile/us.anthropic.claude-sonnet-example-v1:0",
		"arn:aws:bedrock:us-east-2:111122223333:inference-profile/global.anthropic.claude-example-v1:0",
		"arn:aws:bedrock:us-east-2::foundation-model/darkplum.not-an-allowed-model-v1:0",
		"arn:aws:bedrock:ap-southeast-7::foundation-model/anthropic.claude-example-v1:0",
	}
	for i, c := range rep.Checks {
		if c.Resource != wantRes[i] {
			t.Errorf("check %d resource = %s", i, c.Resource)
		}
	}
	var buf bytes.Buffer
	rep.WriteText(&buf)
	o := buf.String()
	if !strings.Contains(o, "Layer 1 allowlist behaves as configured.") || strings.Contains(o, "\x1b[") ||
		!strings.Contains(o, "  ok    allowed  arn:aws:bedrock") || !strings.Contains(o, "account 111122223333") {
		t.Errorf("evidence text:\n%s", o)
	}
}

func TestRunFailsOnEveryWrongDecision(t *testing.T) {
	cases := map[string]struct {
		res, got string
		note     bool
	}{
		"allowed model not allowed":     {"arn:aws:bedrock:us-east-2:111122223333:inference-profile/us.anthropic.claude-sonnet-example-v1:0", ImplicitDeny, false},
		"global profile is allowed":     {"arn:aws:bedrock:us-east-2:111122223333:inference-profile/global.anthropic.claude-example-v1:0", Allowed, false},
		"unlisted model is allowed":     {"arn:aws:bedrock:us-east-2::foundation-model/darkplum.not-an-allowed-model-v1:0", Allowed, false},
		"other region allowed":          {"arn:aws:bedrock:ap-southeast-7::foundation-model/anthropic.claude-example-v1:0", Allowed, false},
		"explicit deny is not the list": {"arn:aws:bedrock:ap-southeast-7::foundation-model/anthropic.claude-example-v1:0", ExplicitDeny, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := allowedBehaviour()
			f.byRes[tc.res] = tc.got
			rep, err := Run(context.Background(), f.run, outputs)
			if err != nil {
				t.Fatalf("a wrong decision is a failed report, not an error: %v", err)
			}
			if rep.Passed() || rep.Failed() != 1 || rep.Unevaluated() != 0 {
				t.Fatalf("passed %v failed %d", rep.Passed(), rep.Failed())
			}
			var buf bytes.Buffer
			rep.WriteText(&buf)
			if !strings.Contains(buf.String(), "FAIL  expected") || !strings.Contains(buf.String(), "Layer 1 allowlist does NOT behave as configured.") {
				t.Errorf("text:\n%s", buf.String())
			}
			if tc.note && !strings.Contains(buf.String(), "explicit Deny statement") {
				t.Errorf("no explanation for an explicit deny:\n%s", buf.String())
			}
		})
	}
}

// An AWS error (expired SSO, no permission to simulate) must not read as a policy failure.
func TestRunDistinguishesAnAWSErrorFromAPolicyResult(t *testing.T) {
	f := &fakeAWS{err: errors.New("exit status 255: An error occurred (ExpiredToken) when calling SimulatePrincipalPolicy\nmore")}
	rep, err := Run(context.Background(), f.run, outputs)
	var se *SimulationError
	if !errors.As(err, &se) || !strings.Contains(se.First, "ExpiredToken") || strings.Contains(se.First, "more") {
		t.Fatalf("err = %v", err)
	}
	if rep.Failed() != 0 || rep.Unevaluated() != 4 || rep.Passed() {
		t.Errorf("failed %d unevaluated %d", rep.Failed(), rep.Unevaluated())
	}
	var buf bytes.Buffer
	rep.WriteText(&buf)
	if strings.Contains(buf.String(), "does NOT behave") || !strings.Contains(buf.String(), "NOT evaluated") || !strings.Contains(buf.String(), "ERROR") {
		t.Errorf("text:\n%s", buf.String())
	}
	f = &fakeAWS{nothing: true}
	if _, err := Run(context.Background(), f.run, outputs); !errors.As(err, &se) {
		t.Errorf("an empty evaluation was taken for a decision: %v", err)
	}
}

func TestRunWithNoAllowedModelsChecksDenialsOnly(t *testing.T) {
	for name, arns := range map[string][]string{
		"none":                 nil,
		"no inference profile": {"arn:aws:bedrock:us-east-2::foundation-model/anthropic.*"},
	} {
		o := outputs
		o.BedrockInvokeARNs = arns
		f := &fakeAWS{}
		rep, err := Run(context.Background(), f.run, o)
		if err != nil || !rep.Passed() || len(f.calls) != 3 || rep.NoteLine == "" {
			t.Errorf("%s: %v passed=%v calls=%d note=%q", name, err, rep.Passed(), len(f.calls), rep.NoteLine)
		}
		if name == "none" && rep.NoteLine != "no Bedrock models are allowed; checking denials only" {
			t.Errorf("note = %q", rep.NoteLine)
		}
	}
}

func TestAllowedARNWithoutWildcardIsUsedAsIs(t *testing.T) {
	o := outputs
	o.BedrockInvokeARNs = []string{"arn:aws:bedrock:us-east-2:111122223333:inference-profile/us.anthropic.claude-x-v1:0"}
	f := &fakeAWS{byRes: map[string]string{o.BedrockInvokeARNs[0]: Allowed}}
	rep, err := Run(context.Background(), f.run, o)
	if err != nil || !rep.Passed() || rep.Checks[0].Resource != o.BedrockInvokeARNs[0] {
		t.Errorf("%v %+v", err, rep.Checks)
	}
}

func TestRunSetupErrors(t *testing.T) {
	f := &fakeAWS{}
	if _, err := Run(context.Background(), f.run, Outputs{}); !errors.Is(err, ErrNoRole) {
		t.Errorf("err = %v", err)
	}
	if _, err := Run(context.Background(), f.run, Outputs{GatewayRoleARN: "arn:aws:iam::1:role/x", InferenceRegion: "r"}); err == nil {
		t.Error("a malformed role ARN was accepted")
	}
	if _, err := Run(context.Background(), f.run, Outputs{GatewayRoleARN: role}); err == nil || !strings.Contains(err.Error(), "inference_region") {
		t.Errorf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, f.run, outputs); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("aws ran %d times for a setup error or a cancelled run", len(f.calls))
	}
}

func TestFromTerraform(t *testing.T) {
	raw := map[string]json.RawMessage{
		"gateway_role_arn":    json.RawMessage(`"` + role + `"`),
		"inference_region":    json.RawMessage(`"us-east-2"`),
		"bedrock_invoke_arns": json.RawMessage(`["a","b"]`),
	}
	o, err := FromTerraform(raw)
	if err != nil || o.GatewayRoleARN != role || o.InferenceRegion != "us-east-2" || len(o.BedrockInvokeARNs) != 2 {
		t.Errorf("%+v %v", o, err)
	}
	raw["bedrock_invoke_arns"] = json.RawMessage(`null`)
	if o, err := FromTerraform(raw); err != nil || len(o.BedrockInvokeARNs) != 0 {
		t.Errorf("null list: %+v %v", o, err)
	}
	raw["inference_region"] = json.RawMessage(`5`)
	if _, err := FromTerraform(raw); err == nil {
		t.Error("a malformed output was accepted")
	}
	if o, err := FromTerraform(nil); err != nil || o.GatewayRoleARN != "" {
		t.Errorf("empty: %+v %v", o, err)
	}
}

func TestRoleAccount(t *testing.T) {
	if a, ok := RoleAccount(role); !ok || a != "111122223333" {
		t.Error(a, ok)
	}
	for _, bad := range []string{"", "arn:aws:iam::1:role/x", "arn:aws:iam::111122223333:user/x", "arn:aws:s3:::b"} {
		if _, ok := RoleAccount(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
