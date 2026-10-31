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
	arn, endpoint, ca, caller, status string
	describeErr, callerErr            error
	calls                             [][]string
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
		b, _ := json.Marshal(map[string]string{"endpoint": a.endpoint, "ca": a.ca, "arn": a.arn, "status": a.status})
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
	if !v.Valid() || v.Name() != "dp-trial" || v.Account() != "123456789012" || v.Endpoint() != a.endpoint {
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

// A cluster that is still being created is "not ready", which a resumed deploy
// must be able to continue past; the same cluster in ANOTHER account is a refusal.
func TestValidateNotReadyIsDistinctFromARefusal(t *testing.T) {
	for name, mut := range map[string]func(*aws){
		"CREATING":    func(a *aws) { a.status = "CREATING"; a.endpoint = "" },
		"no endpoint": func(a *aws) { a.endpoint = "" },
		"PENDING":     func(a *aws) { a.status = "PENDING" },
	} {
		t.Run(name, func(t *testing.T) {
			a := goodAWS()
			mut(a)
			v, err := Validate(claim(), a.run)
			if v.Valid() || !errors.Is(err, ErrClusterNotReady) {
				t.Fatalf("handle=%v err=%v, want ErrClusterNotReady and no handle", v.Valid(), err)
			}
		})
	}
	a := goodAWS()
	a.status, a.endpoint = "CREATING", ""
	a.arn = "arn:aws:eks:us-east-2:999999999999:cluster/dp-trial"
	a.caller = "999999999999"
	if _, err := Validate(claim(), a.run); err == nil || errors.Is(err, ErrClusterNotReady) {
		t.Errorf("a cluster in an unpinned account is a refusal, got %v", err)
	}
	for _, st := range []string{"ACTIVE", "UPDATING"} {
		a := goodAWS()
		a.status = st
		if v, err := Validate(claim(), a.run); err != nil || !v.Valid() {
			t.Errorf("%s: %v", st, err)
		}
	}
}
