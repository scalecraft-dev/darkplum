// Package kube wraps kubectl behind a Runner interface so command sequences are
// testable and secrets only ever reach kubectl via stdin (never argv).
//
// A Client that really runs kubectl can only be built from a runner handle: a
// ValidatedCluster (an install's cluster, checked against AWS) or a LocalKind (a
// kind cluster on this machine). This package never starts a process itself.
package kube

import (
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// Runner executes an external command with optional stdin. The real
// implementation comes from a runner handle; tests inject a recording fake.
type Runner = runner.Exec

// Client runs kubectl against a fixed namespace.
type Client struct {
	Runner    Runner
	Namespace string
	// Kubeconfig, when set, is passed as --kubeconfig on every invocation (and
	// exported as KUBECONFIG, so plugins and helm agree): the client then
	// never reads the operator's own kubeconfig.
	Kubeconfig string
	// Context, when set, is passed as --context on every invocation, so the
	// client acts on that cluster whatever the kubeconfig's current context is.
	Context string
	// Env is what the runner adds to the environment; kept here so other
	// kubectl processes (port-forward) run with the same credentials.
	Env []string
}

// ForCluster returns a client that only ever talks to the install's validated
// cluster, through the install's private kubeconfig.
func ForCluster(namespace string, v runner.ValidatedCluster) *Client {
	return &Client{Runner: v.Exec(), Namespace: namespace, Kubeconfig: v.Kubeconfig(), Context: runner.Context, Env: v.Env()}
}

// ForLocalKind returns a client that only ever talks to one local kind context
// of the operator's own kubeconfig (the explicit --context escape).
func ForLocalKind(namespace string, k runner.LocalKind) *Client {
	return &Client{Runner: k.Exec(), Namespace: namespace, Context: k.Context()}
}

// Flags are the selectors every kubectl invocation by this client carries.
func (c *Client) Flags() []string {
	var f []string
	if c.Kubeconfig != "" {
		f = append(f, "--kubeconfig", c.Kubeconfig)
	}
	if c.Context != "" {
		f = append(f, "--context", c.Context)
	}
	return f
}

// Run invokes kubectl with the given args verbatim (no implicit namespace).
func (c *Client) Run(stdin []byte, args ...string) (string, error) {
	out, err := c.Runner.Run(stdin, runner.Kubectl, append(c.Flags(), args...)...)
	return string(out), err
}

// NS invokes kubectl with -n <namespace> prepended.
func (c *Client) NS(stdin []byte, args ...string) (string, error) {
	return c.Run(stdin, append([]string{"-n", c.Namespace}, args...)...)
}
