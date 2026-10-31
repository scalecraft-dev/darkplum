package kube

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// validated is a handle that passed the real Validate against a fake aws.
func validated(t *testing.T) runner.ValidatedCluster {
	t.Helper()
	v, err := runner.Validate(runner.Claim{Name: "x", Region: "us-east-2", Profile: "acme", AccountIDs: []string{"123456789012"}},
		func(args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "describe-cluster") {
				b, _ := json.Marshal(map[string]string{"endpoint": "https://e.eks.amazonaws.com",
					"ca": base64.StdEncoding.EncodeToString([]byte("CERT")), "arn": "arn:aws:eks:us-east-2:123456789012:cluster/x"})
				return string(b), nil
			}
			return "123456789012", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type recorder struct{ args [][]string }

func (r *recorder) Run(_ []byte, _ string, args ...string) ([]byte, error) {
	r.args = append(r.args, args)
	return nil, nil
}

func TestPinnedContextOnEveryCall(t *testing.T) {
	rec := &recorder{}
	c := &Client{Runner: rec, Namespace: "ns", Context: "arn:aws:eks:us-east-2:1:cluster/x"}
	_, _ = c.Run(nil, "get", "pods")
	_, _ = c.NS(nil, "get", "pods")
	want := [][]string{
		{"--context", "arn:aws:eks:us-east-2:1:cluster/x", "get", "pods"},
		{"--context", "arn:aws:eks:us-east-2:1:cluster/x", "-n", "ns", "get", "pods"},
	}
	if !reflect.DeepEqual(rec.args, want) {
		t.Errorf("args = %q, want %q", rec.args, want)
	}

	rec.args = nil
	k := ForCluster("ns", validated(t).WithKubeconfig("/abs/.darkplum/kubeconfig"))
	k.Runner = rec
	_, _ = k.NS(nil, "get", "pods")
	if want := []string{"--kubeconfig", "/abs/.darkplum/kubeconfig", "--context", "darkplum", "-n", "ns", "get", "pods"}; !reflect.DeepEqual(rec.args[0], want) {
		t.Errorf("args = %q, want %q", rec.args[0], want)
	}
	if !reflect.DeepEqual(k.Env, []string{"AWS_PROFILE=acme", "KUBECONFIG=/abs/.darkplum/kubeconfig"}) {
		t.Errorf("env = %q: kubectl plugins must see the same kubeconfig", k.Env)
	}

	rec.args = nil
	(&Client{Runner: rec, Namespace: "ns"}).Run(nil, "get", "pods") //nolint:errcheck
	if !reflect.DeepEqual(rec.args, [][]string{{"get", "pods"}}) {
		t.Errorf("unpinned client must not add --context: %q", rec.args)
	}
}

// A client built from a handle with no pinned kubeconfig runs nothing: there is
// no path to kubectl that skips the private kubeconfig.
func TestUnpinnedHandleRunsNothing(t *testing.T) {
	k := ForCluster("ns", validated(t))
	if _, err := k.NS(nil, "get", "pods"); err == nil {
		t.Error("kubectl ran through a handle with no private kubeconfig")
	}
	if _, err := ForCluster("ns", runner.ValidatedCluster{}).Run(nil, "get", "pods"); err == nil {
		t.Error("kubectl ran through the zero handle")
	}
	if _, err := ForLocalKind("ns", runner.LocalKind{}).Run(nil, "get", "pods"); err == nil {
		t.Error("kubectl ran through the zero LocalKind")
	}
}
