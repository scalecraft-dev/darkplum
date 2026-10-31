package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testHandle is a handle that passed the real Validate against a fake aws,
// pinned to a private kubeconfig path.
func testHandle(t *testing.T, profile string) ValidatedCluster {
	t.Helper()
	v, err := Validate(Claim{Name: "dp-trial", Region: "us-east-2", Profile: profile, AccountIDs: []string{"123456789012"}}, fakeAWS("123456789012"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := v.WriteKubeconfig(filepath.Join(t.TempDir(), "kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func fakeAWS(caller string) AWSFunc {
	return func(args ...string) (string, error) {
		j := strings.Join(args, " ")
		switch {
		case strings.Contains(j, "eks describe-cluster"):
			b, _ := json.Marshal(map[string]string{"endpoint": "https://ABC123.gr7.us-east-2.eks.amazonaws.com",
				"ca": base64.StdEncoding.EncodeToString([]byte("CERTIFICATE")), "arn": "arn:aws:eks:us-east-2:123456789012:cluster/dp-trial"})
			return string(b), nil
		case strings.Contains(j, "sts get-caller-identity"):
			return caller + "\n", nil
		}
		return "", fmt.Errorf("unexpected aws %s", j)
	}
}

func TestForwardingLineParse(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(l string) {
		if m := forwardingRe.FindStringSubmatch(l); m != nil {
			got = append(got, m[1])
		}
	}}
	_, _ = w.Write([]byte("Forwarding from 127.0.0.1:5"))
	_, _ = w.Write([]byte("4321 -> 80\nHandling connection for 54321\n"))
	if fmt.Sprint(got) != "[54321]" {
		t.Errorf("parsed ports %v", got)
	}
	h := testHandle(t, "acme")
	f := h.PortForward("darkplum-system", "svc/darkplum-web")
	if a := f.args(); strings.Join(a, " ") !=
		"--kubeconfig "+h.Kubeconfig()+" --context "+Context+" -n darkplum-system port-forward --address 127.0.0.1 svc/darkplum-web :80" {
		t.Errorf("port-forward args = %q", a)
	}
	if strings.Join(f.env, " ") != "AWS_PROFILE=acme KUBECONFIG="+h.Kubeconfig() {
		t.Errorf("port-forward env = %q", f.env)
	}
}

// A backend on another port still gets an OS-picked local port (":8000" means
// "any local port"), bound to loopback, through the pinned selectors.
func TestPortForwardPortArgs(t *testing.T) {
	h := testHandle(t, "acme")
	f := h.PortForwardPort("darkplum-inference", "svc/vllm-qwen", 8000)
	if a := strings.Join(f.args(), " "); a !=
		"--kubeconfig "+h.Kubeconfig()+" --context "+Context+" -n darkplum-inference port-forward --address 127.0.0.1 svc/vllm-qwen :8000" {
		t.Errorf("port-forward args = %q", a)
	}
}

// fakeKubectl puts a script named kubectl first on PATH.
func fakeKubectl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestKubectlForwarderLifecycle(t *testing.T) {
	t.Run("parses the port and stops the process on Close", func(t *testing.T) {
		fakeKubectl(t, `echo "Forwarding from 127.0.0.1:43210 -> 80"; echo "Forwarding from [::1]:43210 -> 80"; exec sleep 30`)
		f := testHandle(t, "").PortForward("darkplum-system", "svc/darkplum-web")
		base, err := f.Open(context.Background())
		if err != nil || base != "http://127.0.0.1:43210" {
			t.Fatalf("Open = %q, %v", base, err)
		}
		done := f.Done()
		start := time.Now()
		f.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Close left kubectl running")
		}
		if time.Since(start) > 2*time.Second {
			t.Error("Close was slow")
		}
	})
	t.Run("Done closes when kubectl dies", func(t *testing.T) {
		fakeKubectl(t, `echo "Forwarding from 127.0.0.1:43211 -> 80"; sleep 0.2; exit 1`)
		f := testHandle(t, "").PortForward("darkplum-system", "svc/darkplum-web")
		defer f.Close()
		if _, err := f.Open(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-f.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done did not close when kubectl exited")
		}
	})
	t.Run("a refusal is reported from stderr", func(t *testing.T) {
		fakeKubectl(t, `echo "error: services \"darkplum-web\" not found" >&2; exit 1`)
		f := testHandle(t, "").PortForward("darkplum-system", "svc/darkplum-web")
		_, err := f.Open(context.Background())
		if err == nil || !strings.Contains(err.Error(), "darkplum-web") {
			t.Fatalf("Open = %v", err)
		}
	})
	t.Run("Ctrl-C leaves the tunnel for the sign-out; Close stops it", func(t *testing.T) {
		fakeKubectl(t, `echo "Forwarding from 127.0.0.1:43212 -> 80"; exec sleep 30`)
		f := testHandle(t, "").PortForward("darkplum-system", "svc/darkplum-web")
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := f.Open(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case <-f.Done():
			t.Fatal("cancelling the run killed the tunnel before configure could sign out through it")
		case <-time.After(300 * time.Millisecond):
		}
		f.Close()
		select {
		case <-f.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Close left kubectl running")
		}
	})
}
