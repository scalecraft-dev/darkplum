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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Kubectl is the only program a cluster handle will run. Code outside this
// package that needs the name uses this constant: the literal is banned there.
const Kubectl = "kubectl"

// Exec runs one command, optionally with stdin, and returns its stdout. Secrets
// go through stdin, never argv.
type Exec interface {
	Run(stdin []byte, name string, args ...string) ([]byte, error)
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

// Terraform runs terraform in dir with env added to the environment and returns
// stdout and stderr separately. A non-zero exit is the error.
func Terraform(dir string, env []string, args ...string) (stdout []byte, stderr string, err error) {
	cmd := exec.Command("terraform", append([]string{"-chdir=" + dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.Bytes(), errb.String(), err
}

// kubectlExec shells out to kubectl and nothing else. env is added to the
// inherited environment (the AWS_PROFILE a kubeconfig's exec credential plugin
// needs, and KUBECONFIG so plugins agree).
type kubectlExec struct{ env []string }

func (k kubectlExec) Run(stdin []byte, name string, args ...string) ([]byte, error) {
	if name != "kubectl" {
		return nil, fmt.Errorf("%w: %q through a cluster handle", ErrNotAllowed, name)
	}
	c := exec.Command(name, args...)
	if len(k.env) > 0 {
		c.Env = append(os.Environ(), k.env...)
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
