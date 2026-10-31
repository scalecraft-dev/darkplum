// Package kube wraps kubectl behind a Runner interface so command sequences are
// testable and secrets only ever reach kubectl via stdin (never argv).
//
// A Client that really runs kubectl can only be built from a runner handle: a
// ValidatedCluster (an install's cluster, checked against AWS) or a LocalKind (a
// kind cluster on this machine). The handle's Exec puts the pinned --kubeconfig
// and --context in front of every call itself and refuses callers' arguments that
// would select anything else. This package never starts a process, and its
// fields are unexported so no other package can build or retarget a Client.
package kube

import (
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// Runner executes an external command with optional stdin. The real
// implementation comes from a runner handle; tests inject a recording fake.
type Runner = runner.Exec

// Client runs kubectl against a fixed namespace.
type Client struct {
	r  Runner
	ns string
	// flags are added to the arguments before the runner sees them. Real clients
	// leave this empty (the handle's Exec adds the selectors); ForTest sets it so a
	// fake observes the same argv a real call produces.
	flags []string
}

// ForCluster returns a client that only ever talks to the install's validated
// cluster, through the install's private kubeconfig.
func ForCluster(namespace string, v runner.ValidatedCluster) *Client {
	return &Client{r: v.Exec(), ns: namespace}
}

// ForLocalKind returns a client that only ever talks to one local kind context
// (the explicit --context escape).
func ForLocalKind(namespace string, k runner.LocalKind) *Client {
	return &Client{r: k.Exec(), ns: namespace}
}

// ForTest builds a client around a fake Runner. flags stand in for the selectors a
// real handle would prepend, so assertions on the argv match production. It is for
// _test.go files only; internal/lint fails the build on any other use.
func ForTest(r Runner, namespace string, flags ...string) *Client {
	return &Client{r: r, ns: namespace, flags: flags}
}

// Namespace is the namespace NS targets.
func (c *Client) Namespace() string { return c.ns }

// Run invokes kubectl with the given args verbatim (no implicit namespace).
func (c *Client) Run(stdin []byte, args ...string) (string, error) {
	out, err := c.r.Run(stdin, runner.Kubectl, append(append([]string{}, c.flags...), args...)...)
	return string(out), err
}

// NS invokes kubectl with -n <namespace> prepended.
func (c *Client) NS(stdin []byte, args ...string) (string, error) {
	return c.Run(stdin, append([]string{"-n", c.ns}, args...)...)
}
