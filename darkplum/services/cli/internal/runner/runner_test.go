package runner

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func claim() Claim {
	return Claim{Name: "dp-trial", Region: "us-east-2", Profile: "acme", AccountIDs: []string{"123456789012"}}
}

// aws answers describe-cluster with the given ARN/endpoint/CA and the caller.
type aws struct {
	arn, endpoint, ca, caller string
	describeErr, callerErr    error
	calls                     [][]string
}

func goodAWS() *aws {
	return &aws{arn: "arn:aws:eks:us-east-2:123456789012:cluster/dp-trial", endpoint: "https://ABC.eks.amazonaws.com",
		ca: base64.StdEncoding.EncodeToString([]byte("CERTIFICATE")), caller: "123456789012"}
}

func (a *aws) run(args ...string) (string, error) {
	a.calls = append(a.calls, args)
	j := strings.Join(args, " ")
	switch {
	case strings.Contains(j, "eks describe-cluster"):
		if a.describeErr != nil {
			return "", a.describeErr
		}
		b, _ := json.Marshal(map[string]string{"endpoint": a.endpoint, "ca": a.ca, "arn": a.arn})
		return string(b), nil
	case strings.Contains(j, "sts get-caller-identity"):
		return a.caller + "\n", a.callerErr
	}
	return "", errors.New("unexpected aws " + j)
}

func TestValidateIssuesAHandleOnlyForTheInstallsCluster(t *testing.T) {
	a := goodAWS()
	v, err := Validate(claim(), a.run)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid() || v.Name() != "dp-trial" || v.Account() != "123456789012" || v.Endpoint() != a.endpoint || v.CA() != a.ca {
		t.Errorf("handle = %+v", v)
	}
	for _, c := range a.calls {
		if !strings.HasPrefix(strings.Join(c, " "), "--profile acme ") || !strings.Contains(strings.Join(c, " "), "--region us-east-2") {
			t.Errorf("aws call without the install's profile and region: %v", c)
		}
	}
}

// Every way the cluster or the credentials can be the wrong ones is a refusal and
// no handle: the cross-account / wrong-cluster denial tests.
func TestValidateDenials(t *testing.T) {
	cases := map[string]struct {
		mut    func(*aws, *Claim)
		detail string
	}{
		"credentials for another account": {func(a *aws, _ *Claim) { a.caller = "999999999999" }, "credentials are for account 999999999999"},
		"cluster in another account than the install pins": {func(a *aws, c *Claim) {
			c.AccountIDs = []string{"111111111111"}
		}, "not the install's account 111111111111"},
		"cluster and credentials both in an unpinned account": {func(a *aws, c *Claim) {
			a.arn = "arn:aws:eks:us-east-2:999999999999:cluster/dp-trial"
			a.caller = "999999999999"
		}, "not the install's account 123456789012"},
		"ARN of another cluster name":  {func(a *aws, _ *Claim) { a.arn = "arn:aws:eks:us-east-2:123456789012:cluster/other" }, "does not match dp-trial"},
		"ARN of another region":        {func(a *aws, _ *Claim) { a.arn = "arn:aws:eks:eu-west-1:123456789012:cluster/dp-trial" }, "does not match dp-trial"},
		"ARN of another service":       {func(a *aws, _ *Claim) { a.arn = "arn:aws:ecs:us-east-2:123456789012:cluster/dp-trial" }, "does not match dp-trial"},
		"ARN with a short account":     {func(a *aws, _ *Claim) { a.arn = "arn:aws:eks:us-east-2:1234:cluster/dp-trial" }, "does not match dp-trial"},
		"ARN with a name suffix":       {func(a *aws, _ *Claim) { a.arn = "arn:aws:eks:us-east-2:123456789012:cluster/dp-trial-2" }, "does not match dp-trial"},
		"plain http endpoint":          {func(a *aws, _ *Claim) { a.endpoint = "http://x" }, "no usable endpoint"},
		"no certificate":               {func(a *aws, _ *Claim) { a.ca = "" }, "no usable endpoint"},
		"certificate not base64":       {func(a *aws, _ *Claim) { a.ca = "%%%" }, "no usable endpoint"},
		"credentials unreadable":       {func(a *aws, _ *Claim) { a.callerErr = errors.New("expired") }, "cannot read who the AWS credentials"},
		"empty caller":                 {func(a *aws, _ *Claim) { a.caller = "" }, "cannot read who the AWS credentials"},
		"describe fails":               {func(a *aws, _ *Claim) { a.describeErr = errors.New("AccessDenied") }, "cannot read cluster dp-trial"},
		"settings without a region":    {func(_ *aws, c *Claim) { c.Region = "" }, "no region"},
		"settings that pin no account": {func(_ *aws, c *Claim) { c.AccountIDs = nil }, "pin no AWS account"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, c := goodAWS(), claim()
			tc.mut(a, &c)
			v, err := Validate(c, a.run)
			if err == nil || v.Valid() {
				t.Fatalf("got a handle for: %s", name)
			}
			var rf *Refusal
			if !errors.As(err, &rf) || !strings.Contains(rf.Detail, tc.detail) || rf.Hint == "" {
				t.Errorf("refusal = %+v, want detail containing %q and a hint", rf, tc.detail)
			}
			// a refusal handle runs nothing
			if _, err := v.Exec().Run(nil, "kubectl", "get", "pods"); err == nil {
				t.Error("kubectl ran through a refused handle")
			}
		})
	}
}

func TestValidateReportsAMissingClusterAsNotFound(t *testing.T) {
	a := goodAWS()
	a.describeErr = errors.New("exit status 254: An error occurred (ResourceNotFoundException) when calling the DescribeCluster operation")
	_, err := Validate(claim(), a.run)
	if !errors.Is(err, ErrClusterNotFound) {
		t.Errorf("err = %v, want ErrClusterNotFound", err)
	}
}

// The credential check happens against the cluster's own account, so a stale
// profile cannot validate a cluster it should not touch, and nothing is read
// before the claim is complete.
func TestValidateAsksAWSNothingForAnIncompleteClaim(t *testing.T) {
	a := goodAWS()
	c := claim()
	c.AccountIDs = nil
	_, _ = Validate(c, a.run)
	if len(a.calls) != 0 {
		t.Errorf("aws was called for an unpinned install: %v", a.calls)
	}
}

func TestHandleIsPinnedToItsPrivateKubeconfig(t *testing.T) {
	v, _ := Validate(claim(), goodAWS().run)
	if got := v.Flags(); got != nil {
		t.Errorf("flags before WithKubeconfig = %v", got)
	}
	if _, err := v.Exec().Run(nil, "kubectl", "get", "pods"); err == nil {
		t.Error("kubectl ran before the handle was pinned to a private kubeconfig")
	}
	p := v.WithKubeconfig("/w/kubeconfig")
	if strings.Join(p.Flags(), " ") != "--kubeconfig /w/kubeconfig --context darkplum" {
		t.Errorf("flags = %v", p.Flags())
	}
	if strings.Join(p.Env(), " ") != "AWS_PROFILE=acme KUBECONFIG=/w/kubeconfig" {
		t.Errorf("env = %v", p.Env())
	}
	if v.Kubeconfig() != "" {
		t.Error("WithKubeconfig changed the original handle")
	}
	// The zero handle is inert.
	var z ValidatedCluster
	if z.Valid() || z.Flags() != nil || z.Env() != nil || z.WithKubeconfig("/x").Valid() {
		t.Error("zero handle is not inert")
	}
	if _, err := z.Exec().Run(nil, "kubectl", "version"); err == nil {
		t.Error("zero handle ran kubectl")
	}
	if _, err := z.PortForward("ns", "svc/x").Open(t.Context()); err == nil {
		t.Error("zero handle opened a tunnel")
	}
}

// A cluster handle runs kubectl and nothing else, whatever it is asked.
func TestHandleExecRunsOnlyKubectl(t *testing.T) {
	v, _ := Validate(claim(), goodAWS().run)
	p := v.WithKubeconfig("/w/kubeconfig")
	for _, name := range []string{"aws", "terraform", "helm", "sh", "/usr/bin/kubectl", "kubectl ", ""} {
		if _, err := p.Exec().Run(nil, name, "x"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("handle ran %q: %v", name, err)
		}
	}
}

func TestToolRefusesEverythingButAwsAndTerraform(t *testing.T) {
	for _, name := range []string{"kubectl", "helm", "sh", "bash", "rm", "", "aws2", "/usr/local/bin/aws"} {
		if _, err := Tool(name, "x"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Tool(%q) = %v", name, err)
		}
		if _, err := ToolStdout(name, "x"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("ToolStdout(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"helm", "sh", ""} {
		if _, err := Which(name); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Which(%q) = %v", name, err)
		}
	}
}

func TestLocalKindOnlyAdmitsLoopbackKindContexts(t *testing.T) {
	server := func(s string) func(string) (string, error) {
		return func(string) (string, error) { return s, nil }
	}
	for _, ok := range []struct{ ctx, server string }{
		{"kind-darkplum", "https://127.0.0.1:52000"},
		{"kind-dev", "https://localhost:6443"},
		{"kind-a.b", "https://[::1]:6443"},
	} {
		k, err := newLocalKind(ok.ctx, server(ok.server))
		if err != nil || k.Context() != ok.ctx {
			t.Errorf("%s %s refused: %v", ok.ctx, ok.server, err)
		}
		if strings.Join(k.Flags(), " ") != "--context "+ok.ctx {
			t.Errorf("flags = %v", k.Flags())
		}
	}
	for _, bad := range []struct{ ctx, server string }{
		{"darkplum", "https://127.0.0.1:1"},
		{"prod", "https://127.0.0.1:1"},
		{"arn:aws:eks:us-east-2:1:cluster/x", "https://127.0.0.1:1"},
		{"kind-", "https://127.0.0.1:1"},
		{"Kind-x", "https://127.0.0.1:1"},
		{"kind-x y", "https://127.0.0.1:1"},
		{"kind-prod", "https://ABC.gr7.us-east-2.eks.amazonaws.com"}, // named like kind, points at a real cluster
		{"kind-prod", "https://10.0.0.5:6443"},
		{"kind-prod", "http://127.0.0.1:8080"},
		{"kind-prod", "https://127.0.0.1.evil.com:443"},
		{"kind-prod", ""},
	} {
		if k, err := newLocalKind(bad.ctx, server(bad.server)); err == nil || k.Context() != "" {
			t.Errorf("%q -> %q admitted", bad.ctx, bad.server)
		}
	}
	if _, err := newLocalKind("kind-x", func(string) (string, error) { return "", errors.New("no such context") }); err == nil {
		t.Error("a context kubectl cannot read was admitted")
	}
	// The zero LocalKind is inert and a LocalKind is not a ValidatedCluster.
	if _, err := (LocalKind{}).Exec().Run(nil, "kubectl", "get", "pods"); err == nil {
		t.Error("zero LocalKind ran kubectl")
	}
}
