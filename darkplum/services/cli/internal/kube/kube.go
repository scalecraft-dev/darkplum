// Package kube wraps kubectl behind a Runner interface so command sequences are
// testable and secrets only ever reach kubectl via stdin (never argv).
package kube

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes an external command with optional stdin. The real
// implementation shells out; tests inject a recording fake.
type Runner interface {
	Run(stdin []byte, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(stdin []byte, name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
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
}

func New(namespace string) *Client {
	return &Client{Runner: execRunner{}, Namespace: namespace}
}

// Run invokes kubectl with the given args verbatim (no implicit namespace).
func (c *Client) Run(stdin []byte, args ...string) (string, error) {
	out, err := c.Runner.Run(stdin, "kubectl", args...)
	return string(out), err
}

// NS invokes kubectl with -n <namespace> prepended.
func (c *Client) NS(stdin []byte, args ...string) (string, error) {
	return c.Run(stdin, append([]string{"-n", c.Namespace}, args...)...)
}
