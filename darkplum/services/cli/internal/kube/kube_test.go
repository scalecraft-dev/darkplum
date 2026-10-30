package kube

import (
	"reflect"
	"testing"
)

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
	k := NewPinned("ns", "/abs/.darkplum/kubeconfig", "darkplum", []string{"AWS_PROFILE=acme"})
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
