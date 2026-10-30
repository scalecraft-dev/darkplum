// Package kube wraps kubectl behind a Runner interface so command sequences are
// testable and secrets only ever reach kubectl via stdin (never argv).
package kube

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes an external command with optional stdin. The real
// implementation shells out; tests inject a recording fake.
type Runner interface {
	Run(stdin []byte, name string, args ...string) ([]byte, error)
}

// execRunner shells out. env is added to the inherited environment (e.g. the
// AWS_PROFILE a kubeconfig's exec credential plugin needs).
type execRunner struct{ env []string }

func (r execRunner) Run(stdin []byte, name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	if len(r.env) > 0 {
		c.Env = append(os.Environ(), r.env...)
	}
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("%s: %s", name, msg)
	}
	return out.Bytes(), nil
}

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

// New returns a client on the kubeconfig's current context.
func New(namespace string) *Client {
	return &Client{Runner: execRunner{}, Namespace: namespace}
}

// NewPinned returns a client that only ever talks to kubeContext, read from
// kubeconfig (or, when kubeconfig is "", from the operator's own kubeconfig:
// the explicit --context escape).
func NewPinned(namespace, kubeconfig, kubeContext string, env []string) *Client {
	if kubeconfig != "" {
		env = append(append([]string{}, env...), "KUBECONFIG="+kubeconfig)
	}
	return &Client{Runner: execRunner{env: env}, Namespace: namespace, Kubeconfig: kubeconfig, Context: kubeContext, Env: env}
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
	out, err := c.Runner.Run(stdin, "kubectl", append(c.Flags(), args...)...)
	return string(out), err
}

// NS invokes kubectl with -n <namespace> prepended.
func (c *Client) NS(stdin []byte, args ...string) (string, error) {
	return c.Run(stdin, append([]string{"-n", c.Namespace}, args...)...)
}
