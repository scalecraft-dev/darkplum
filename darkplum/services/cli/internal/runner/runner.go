// Package runner is the only place the CLI starts external programs.
//
// TestNoExecOutsideRunner (in internal/lint) fails the build on any other
// import of os/exec or any use of a tool name as a command anywhere else, so the
// rules below cannot be bypassed by a new command forgetting them.
//
// The rules are about which cluster a command can act on, because "the wrong
// cluster" is the failure this tool must make impossible, not unlikely:
//
//   - kubectl is reachable only through a handle. A ValidatedCluster comes only
//     from Validate, which has checked that the cluster exists, that its ARN names
//     the install's region and name, and that both the cluster's account and the
//     caller's credentials are the account the install is pinned to. A LocalKind
//     comes only from NewLocalKind: a context named kind-* whose API server is on
//     this machine. There is no way to construct either from raw strings, and no
//     function here runs kubectl without one.
//   - Every kubectl invocation carries --kubeconfig <private file> --context
//     darkplum (or --context kind-*), so the operator's current context, ~/.kube
//     and $KUBECONFIG never decide where a command lands.
//   - aws and terraform run through Tool and Terraform; Tool refuses kubectl,
//     helm and any other name.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Kubectl is the only program a cluster handle will run. Code outside this
// package that needs the name uses this constant: the literal is banned there.
const Kubectl = "kubectl"

// Exec runs one command, optionally with stdin, and returns its stdout. Secrets
// go through stdin, never argv.
type Exec interface {
	Run(stdin []byte, name string, args ...string) ([]byte, error)
}

// ContextExec is an Exec that a context can stop: a cancelled context interrupts
// the running command (SIGINT first, a kill if it does not leave). Real cluster
// handles are ContextExecs; a fake need not be.
type ContextExec interface {
	Exec
	RunContext(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// ErrNotAllowed is returned when a caller asks the runner for a program it will
// not run on that path.
var ErrNotAllowed = errors.New("runner: program not allowed here")

// Tool runs aws or terraform and returns trimmed stdout. On failure the error
// carries the program's stderr (the AWS CLI reports codes such as
// ResourceNotFoundException there). Anything else, kubectl and helm in
// particular, is refused.
func Tool(name string, args ...string) (string, error) {
	if name != "aws" && name != "terraform" {
		return "", fmt.Errorf("%w: %q", ErrNotAllowed, name)
	}
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return out, fmt.Errorf("%w: %s", err, msg)
		}
	}
	return out, err
}

// ToolStdout is Tool with stderr kept out of the error and the value: callers
// parse the output (a quota number), and CLI deprecation or SSO warnings on
// stderr would corrupt it.
func ToolStdout(name string, args ...string) (string, error) {
	if name != "aws" && name != "terraform" {
		return "", fmt.Errorf("%w: %q", ErrNotAllowed, name)
	}
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// Which reports where a tool the CLI depends on is on PATH. It runs nothing.
func Which(name string) (string, error) {
	switch name {
	case "aws", "terraform", "kubectl", "gcloud":
		return exec.LookPath(name)
	}
	return "", fmt.Errorf("%w: %q", ErrNotAllowed, name)
}

// kubectlExec shells out to kubectl and nothing else. prefix is the pinned
// selectors (--kubeconfig <file> --context <ctx>), put in front of the caller's
// arguments here and nowhere else; env is added to the inherited environment (the
// AWS_PROFILE a kubeconfig's exec credential plugin needs, and KUBECONFIG). check,
// when set, runs before every call.
type kubectlExec struct {
	prefix, env []string
	check       func() error
}

func (k kubectlExec) Run(stdin []byte, name string, args ...string) ([]byte, error) {
	return k.RunContext(context.Background(), stdin, name, args...)
}

// RunContext is Run that stops when ctx is done. A rollout wait is the long
// call: an interrupt must not leave it blocking for its timeout.
func (k kubectlExec) RunContext(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	if name != Kubectl {
		return nil, fmt.Errorf("%w: %q through a cluster handle", ErrNotAllowed, name)
	}
	if k.check != nil {
		if err := k.check(); err != nil {
			return nil, err
		}
	}
	if err := screenArgs(args); err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, name, append(append([]string{}, k.prefix...), args...)...)
	c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
	c.WaitDelay = 5 * time.Second
	// KUBERC=off: a ~/.kube/kuberc cannot add default flags (a --context, an alias)
	// to what a handle runs.
	c.Env = append(os.Environ(), append([]string{"KUBERC=off"}, k.env...)...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		if ctx.Err() != nil {
			return out.Bytes(), fmt.Errorf("%s: interrupted: %w", name, ctx.Err())
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("%s: %s", name, msg)
	}
	return out.Bytes(), nil
}

// selectorFlags are the kubectl flags that choose which cluster, credentials or
// identity a call uses. A handle fixes all of them, so a caller may not pass any.
var selectorFlags = []string{
	"--kubeconfig", "--context", "--cluster", "--server", "--user", "--token", "--as", "--as-group", "--as-uid",
	"--certificate-authority", "--client-certificate", "--client-key", "--insecure-skip-tls-verify", "--tls-server-name",
	"--username", "--password",
}

// screenArgs refuses an argument that is, or starts with, a selector flag (in
// "--flag", "--flag=value" or, for the one short form kubectl has, "-s<server>"
// shape). Scanning stops at the first "--": what follows belongs to the command a
// subcommand runs (kubectl exec POD -- sh -c ...), not to kubectl, and cannot
// change where kubectl connects.
func screenArgs(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		for _, f := range selectorFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("runner: kubectl argument %q selects a cluster or identity; a handle fixes those", a)
			}
		}
		if strings.HasPrefix(a, "-s") && !strings.HasPrefix(a, "--") {
			return fmt.Errorf("runner: kubectl argument %q (-s is --server) selects a cluster; a handle fixes that", a)
		}
	}
	return nil
}

// noCluster is what the zero handle executes: nothing.
type noCluster struct{}

func (noCluster) Run([]byte, string, ...string) ([]byte, error) {
	return nil, errors.New("runner: no validated cluster")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
