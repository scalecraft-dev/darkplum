package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

type recorder struct{ args [][]string }

func (r *recorder) Run(_ []byte, _ string, args ...string) ([]byte, error) {
	r.args = append(r.args, args)
	return nil, nil
}

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

// ForTest puts the selectors a real handle would add in front of every call, so
// assertions on argv match production; with none, nothing is added.
func TestForTestFlagsAndNamespace(t *testing.T) {
	rec := &recorder{}
	c := ForTest(rec, "ns", "--kubeconfig", "/abs/kubeconfig", "--context", "darkplum")
	_, _ = c.Run(nil, "get", "pods")
	_, _ = c.NS(nil, "get", "pods")
	want := [][]string{
		{"--kubeconfig", "/abs/kubeconfig", "--context", "darkplum", "get", "pods"},
		{"--kubeconfig", "/abs/kubeconfig", "--context", "darkplum", "-n", "ns", "get", "pods"},
	}
	if !reflect.DeepEqual(rec.args, want) {
		t.Errorf("args = %q, want %q", rec.args, want)
	}
	if c.Namespace() != "ns" {
		t.Errorf("namespace = %q", c.Namespace())
	}
	rec.args = nil
	_, _ = ForTest(rec, "ns").Run(nil, "get", "pods")
	if !reflect.DeepEqual(rec.args, [][]string{{"get", "pods"}}) {
		t.Errorf("a client without flags must not add any: %q", rec.args)
	}
}

// A client built from a handle never carries selectors itself: the handle's Exec
// adds them (and refuses the caller's), and it runs nothing until the kubeconfig
// is written.
func TestClientsFromHandlesRunNothingUnpinned(t *testing.T) {
	if _, err := ForCluster("ns", validated(t)).NS(nil, "get", "pods"); err == nil {
		t.Error("kubectl ran through a handle with no private kubeconfig")
	}
	h, err := validated(t).WriteKubeconfig(filepath.Join(t.TempDir(), "kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ForCluster("ns", h).Run(nil, "get", "pods", "--context=prod"); err == nil {
		t.Error("a client accepted an override of the pinned context")
	}
	if _, err := ForCluster("ns", runner.ValidatedCluster{}).Run(nil, "get", "pods"); err == nil {
		t.Error("kubectl ran through the zero handle")
	}
	if _, err := ForLocalKind("ns", runner.LocalKind{}).Run(nil, "get", "pods"); err == nil {
		t.Error("kubectl ran through the zero LocalKind")
	}
}

type ctxRunner struct {
	plain, withCtx int
}

func (r *ctxRunner) Run([]byte, string, ...string) ([]byte, error) {
	r.plain++
	return []byte("p"), nil
}
func (r *ctxRunner) RunContext(context.Context, []byte, string, ...string) ([]byte, error) {
	r.withCtx++
	return []byte("c"), nil
}

type plainRunner struct{ n int }

func (r *plainRunner) Run([]byte, string, ...string) ([]byte, error) { r.n++; return []byte("p"), nil }

func TestWithContext(t *testing.T) {
	// a runner that can be interrupted is used through its context
	cr := &ctxRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	c := ForTest(cr, "ns").WithContext(ctx)
	if out, _ := c.NS(nil, "get", "pods"); out != "c" || cr.withCtx != 1 || cr.plain != 0 {
		t.Errorf("out %q, ctx calls %d, plain calls %d", out, cr.withCtx, cr.plain)
	}
	// once cancelled, nothing starts, whichever runner it is
	cancel()
	if _, err := c.NS(nil, "get", "pods"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	pr := &plainRunner{}
	c2 := ForTest(pr, "ns").WithContext(ctx)
	if _, err := c2.Run(nil, "get", "pods"); !errors.Is(err, context.Canceled) || pr.n != 0 {
		t.Errorf("a call started after cancel: err %v, calls %d", err, pr.n)
	}
	// the original client is unchanged, and the copy targets the same namespace
	if _, err := ForTest(pr, "ns").Run(nil, "get", "pods"); err != nil || pr.n != 1 {
		t.Errorf("a client without a context stopped working: %v", err)
	}
	if c2.Namespace() != "ns" {
		t.Error("namespace changed")
	}
}
