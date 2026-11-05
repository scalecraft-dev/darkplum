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
	byRole  map[string]string // "<role>|<action>|<resource>"
	err     error
	nothing bool
}

func (f *fakeAWS) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return "", f.err
	}
	var res, pol, act string
	for i, a := range args {
		switch a {
		case "--resource-arns":
			res = args[i+1]
		case "--policy-source-arn":
			pol = args[i+1]
		case "--action-names":
			act = args[i+1]
		}
	}
	if d, ok := f.byRole[pol+"|"+act+"|"+res]; ok {
		return d, nil
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

const apiRole = "arn:aws:iam::111122223333:role/darkplum-api"
const libBkt = "arn:aws:s3:::acme-library-111122223333"
const libObj = "arn:aws:s3:::acme-library-111122223333/library/v1/org/darkplum-check.skill.md"

var libOutputs = func() Outputs {
	o := outputs
	o.APIRoleARN, o.LibraryStoreURL = apiRole, "s3://acme-library-111122223333/library/v1/"
	return o
}()

// libraryBehaviour is a healthy install: the api reads its prefix, the gateway
// is explicitly denied, everything else is implicitly denied.
func libraryBehaviour() *fakeAWS {
	f := allowedBehaviour()
	f.byRole = map[string]string{role + "|s3:GetObject|" + libObj: ExplicitDeny}
	for _, a := range []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion"} {
		f.byRole[apiRole+"|"+a+"|"+libObj] = Allowed
	}
	for _, a := range []string{"s3:ListBucket", "s3:ListBucketVersions"} {
		f.byRole[apiRole+"|"+a+"|"+libBkt] = Allowed
	}
	return f
}

func TestRunProvesTheLibraryBucketBoundary(t *testing.T) {
	f := libraryBehaviour()
	rep, err := Run(context.Background(), f.run, libOutputs)
	if err != nil || !rep.Passed() || len(rep.Checks) != 13 {
		t.Fatalf("%v passed=%v checks=%d", err, rep.Passed(), len(rep.Checks))
	}
	var buf bytes.Buffer
	rep.WriteText(&buf)
	for _, want := range []string{
		"ok    allowed  s3:GetObject as darkplum-api  " + libObj,
		"ok    explicitDeny  s3:GetObject as darkplum-gateway  " + libObj,
		"ok    implicitDeny  s3:GetObject as darkplum-api  arn:aws:s3:::darkplum-not-the-library-111122223333/",
		"ok    implicitDeny  s3:GetObject as darkplum-api  arn:aws:s3:::acme-library-111122223333/not-the-library/",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q in:\n%s", want, buf.String())
		}
	}
}

func TestRunFailsWhenTheLibraryBoundaryIsWrong(t *testing.T) {
	other := "arn:aws:s3:::darkplum-not-the-library-111122223333/library/v1/org/darkplum-check.skill.md"
	outside := "arn:aws:s3:::acme-library-111122223333/not-the-library/darkplum-check"
	for name, mut := range map[string]func(*fakeAWS){
		"api cannot read the library":                                   func(f *fakeAWS) { f.byRole[apiRole+"|s3:GetObject|"+libObj] = ImplicitDeny },
		"api cannot put":                                                func(f *fakeAWS) { f.byRole[apiRole+"|s3:PutObject|"+libObj] = ImplicitDeny },
		"api cannot delete":                                             func(f *fakeAWS) { f.byRole[apiRole+"|s3:DeleteObject|"+libObj] = ImplicitDeny },
		"api cannot purge a version":                                    func(f *fakeAWS) { f.byRole[apiRole+"|s3:DeleteObjectVersion|"+libObj] = ImplicitDeny },
		"api cannot list (404 would be 403)":                            func(f *fakeAWS) { f.byRole[apiRole+"|s3:ListBucket|"+libBkt] = ImplicitDeny },
		"api cannot list versions":                                      func(f *fakeAWS) { f.byRole[apiRole+"|s3:ListBucketVersions|"+libBkt] = ImplicitDeny },
		"api can read another bucket":                                   func(f *fakeAWS) { f.byRole[apiRole+"|s3:GetObject|"+other] = Allowed },
		"api can read outside prefix":                                   func(f *fakeAWS) { f.byRole[apiRole+"|s3:GetObject|"+outside] = Allowed },
		"gateway can read the bucket":                                   func(f *fakeAWS) { f.byRole[role+"|s3:GetObject|"+libObj] = Allowed },
		"gateway only implicitly denied (the explicit deny is missing)": func(f *fakeAWS) { f.byRole[role+"|s3:GetObject|"+libObj] = ImplicitDeny },
	} {
		t.Run(name, func(t *testing.T) {
			f := libraryBehaviour()
			mut(f)
			rep, err := Run(context.Background(), f.run, libOutputs)
			if err != nil || rep.Passed() || rep.Failed() != 1 {
				t.Fatalf("err=%v passed=%v failed=%d", err, rep.Passed(), rep.Failed())
			}
		})
	}
}

func TestLibraryChecksAreSkippedWithANoteWhenHalfTheOutputsAreMissing(t *testing.T) {
	for name, mut := range map[string]func(*Outputs){
		"no api role":  func(o *Outputs) { o.APIRoleARN = "" },
		"no store url": func(o *Outputs) { o.LibraryStoreURL = "" },
		"gcs url":      func(o *Outputs) { o.LibraryStoreURL = "gs://b/library/v1/" },
	} {
		o := libOutputs
		mut(&o)
		f := libraryBehaviour()
		rep, err := Run(context.Background(), f.run, o)
		if err != nil || !rep.Passed() || len(rep.Checks) != 4 || !strings.Contains(rep.NoteLine, "Library") {
			t.Errorf("%s: %v passed=%v checks=%d note=%q", name, err, rep.Passed(), len(rep.Checks), rep.NoteLine)
		}
	}
	// No bucket (the default install): the checks do not apply, and the note
	// says the Library is in Postgres rather than telling the operator to deploy.
	no := false
	o := libOutputs
	o.LibraryBucketEnabled, o.LibraryStoreURL = &no, ""
	if rep, err := Run(context.Background(), libraryBehaviour().run, o); err != nil || !rep.Passed() || len(rep.Checks) != 4 ||
		!strings.Contains(rep.NoteLine, "Postgres") || strings.Contains(rep.NoteLine, "darkplum deploy") {
		t.Errorf("no bucket: %v passed=%v checks=%d note=%q", err, rep.Passed(), len(rep.Checks), rep.NoteLine)
	}
	// neither output: applied before the Library existed, nothing to say here
	f := allowedBehaviour()
	if rep, err := Run(context.Background(), f.run, outputs); err != nil || len(rep.Checks) != 4 || rep.NoteLine != "" {
		t.Errorf("old state: %v %d %q", err, len(rep.Checks), rep.NoteLine)
	}
}

func TestFromTerraformReadsTheLibraryOutputs(t *testing.T) {
	raw := map[string]json.RawMessage{
		"api_role_arn":           json.RawMessage(`"` + apiRole + `"`),
		"library_store_url":      json.RawMessage(`"s3://b/library/v1/"`),
		"library_bucket_enabled": json.RawMessage(`true`),
	}
	o, err := FromTerraform(raw)
	if err != nil || o.APIRoleARN != apiRole || o.LibraryStoreURL != "s3://b/library/v1/" || o.LibraryBucketEnabled == nil || !*o.LibraryBucketEnabled {
		t.Errorf("%+v %v", o, err)
	}
	delete(raw, "library_bucket_enabled")
	if o, err := FromTerraform(raw); err != nil || o.LibraryBucketEnabled != nil {
		t.Errorf("absent library_bucket_enabled must stay nil: %+v %v", o, err)
	}
}
