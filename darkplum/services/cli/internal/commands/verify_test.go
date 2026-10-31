package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

const verifyRole = "arn:aws:iam::111122223333:role/darkplum-gateway"

type verifyHarness struct {
	calls   []string
	account string
	decide  func(resource string) (string, error)
	outputs map[string]json.RawMessage
	outErr  error
	out     bytes.Buffer
}

func newVerifyHarness() *verifyHarness {
	return &verifyHarness{account: "111122223333",
		decide: func(string) (string, error) { return "implicitDeny", nil },
		outputs: map[string]json.RawMessage{
			"gateway_role_arn":    json.RawMessage(`"` + verifyRole + `"`),
			"inference_region":    json.RawMessage(`"us-east-2"`),
			"bedrock_invoke_arns": json.RawMessage(`[]`),
		}}
}

func (h *verifyHarness) env() *verifyEnv {
	return &verifyEnv{out: &h.out,
		rec: install.Record{Name: "acme", ClusterName: "acme", Region: "us-east-2", Profile: "acme", AccountIDs: []string{"111122223333"}},
		aws: func(args ...string) (string, error) {
			h.calls = append(h.calls, strings.Join(args, " "))
			if args[0] == "sts" {
				return h.account, nil
			}
			for i, a := range args {
				if a == "--resource-arns" {
					return h.decide(args[i+1])
				}
			}
			return "", errors.New("unexpected aws call")
		},
		outputs: func() (map[string]json.RawMessage, error) { return h.outputs, h.outErr }}
}

func TestVerifyPasses(t *testing.T) {
	h := newVerifyHarness()
	if err := h.env().run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	o := h.out.String()
	for _, want := range []string{"install acme, cluster acme, caller account 111122223333", "no Bedrock models are allowed; checking denials only",
		"Layer 1 allowlist behaves as configured."} {
		if !strings.Contains(o, want) {
			t.Errorf("lacks %q:\n%s", want, o)
		}
	}
	if strings.Contains(o, "\x1b[") {
		t.Error("evidence has colour codes")
	}
}

func TestVerifyFailureIsAReportedNonzero(t *testing.T) {
	h := newVerifyHarness()
	h.decide = func(r string) (string, error) {
		if strings.Contains(r, "darkplum.not-an-allowed") {
			return "allowed", nil
		}
		return "implicitDeny", nil
	}
	err := h.env().run(context.Background(), false)
	if err == nil || !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.out.String(), "Layer 1 allowlist does NOT behave as configured.") {
		t.Errorf("out:\n%s", h.out.String())
	}
}

func TestVerifyExpiredSessionIsNotAPolicyFailure(t *testing.T) {
	h := newVerifyHarness()
	h.decide = func(string) (string, error) { return "", errors.New("An error occurred (ExpiredToken)") }
	err := h.env().run(context.Background(), false)
	if err == nil || !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	o := h.out.String()
	if strings.Contains(o, "does NOT behave") || !strings.Contains(o, "aws sso login --profile acme") || !strings.Contains(o, "iam:SimulatePrincipalPolicy") {
		t.Errorf("out:\n%s", o)
	}
	// an expired session already at the identity check
	h = newVerifyHarness()
	e := h.env()
	e.aws = func(...string) (string, error) { return "", errors.New("Token has expired") }
	if err := e.run(context.Background(), false); err == nil || !strings.Contains(err.Error(), "aws sso login --profile acme") {
		t.Errorf("err = %v", err)
	}
}

// Cross-account: the evidence must be about THIS install's account.
func TestVerifyRefusesTheWrongAccount(t *testing.T) {
	h := newVerifyHarness()
	h.account = "999999999999"
	err := h.env().run(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "pinned to 111122223333") {
		t.Errorf("err = %v", err)
	}
	for _, c := range h.calls {
		if strings.Contains(c, "simulate-principal-policy") {
			t.Error("simulated as the wrong account")
		}
	}
	h = newVerifyHarness()
	h.outputs["gateway_role_arn"] = json.RawMessage(`"arn:aws:iam::999999999999:role/other"`)
	if err := h.env().run(context.Background(), false); err == nil || !strings.Contains(err.Error(), "not pinned to") {
		t.Errorf("a role from another account was simulated: %v", err)
	}
	for _, c := range h.calls {
		if strings.Contains(c, "simulate-principal-policy") {
			t.Error("simulated a foreign role")
		}
	}
}

func TestVerifyNoOutputsAndJSON(t *testing.T) {
	h := newVerifyHarness()
	h.outputs = map[string]json.RawMessage{}
	if err := h.env().run(context.Background(), false); err == nil || !strings.Contains(err.Error(), "darkplum deploy") {
		t.Errorf("err = %v", err)
	}
	h = newVerifyHarness()
	h.outErr = errors.New("state is locked")
	if err := h.env().run(context.Background(), false); err == nil || !strings.Contains(err.Error(), "state is locked") {
		t.Errorf("err = %v", err)
	}
	h = newVerifyHarness()
	if err := h.env().run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	var j struct {
		Passed bool `json:"passed"`
		Report struct{ Checks []map[string]any }
	}
	if err := json.Unmarshal(h.out.Bytes(), &j); err != nil || !j.Passed || len(j.Report.Checks) != 3 {
		t.Errorf("json = %s (%v)", h.out.String(), err)
	}
}

func TestVerifyGrammar(t *testing.T) {
	for _, args := range [][]string{{"verify"}, {"verify", "--json"}, {"--install", "acme", "verify"}} {
		var cli CLI
		p, err := kong.New(&cli, kong.Name("darkplum"), kong.Exit(func(int) {}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Parse(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}
