package commands

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
)

// kcall is one recorded kubectl invocation.
type kcall struct {
	args  []string
	stdin []byte
}

func (c kcall) joined() string { return strings.Join(c.args, " ") }

// scriptKube is a kubectl stand-in: handlers are tried in order and the first whose
// pattern is contained in the argument line answers. Every call is recorded with
// its stdin, so tests can assert on argv (no secret values) and on payloads.
type scriptKube struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []kcall
	handlers []skHandler
	tl       *timeline // when set, every call is also recorded here, in order across fakes
}

type skHandler struct {
	pattern string
	fn      func(args []string, stdin []byte) ([]byte, error)
}

const skKubeconfig = "/ws/kubeconfig"

func newScriptKube(t *testing.T) *scriptKube { return &scriptKube{t: t} }

// on registers an answer for calls whose argument line contains pattern.
func (k *scriptKube) on(pattern string, out string, err error) *scriptKube {
	return k.onFn(pattern, func([]string, []byte) ([]byte, error) { return []byte(out), err })
}

func (k *scriptKube) onFn(pattern string, fn func(args []string, stdin []byte) ([]byte, error)) *scriptKube {
	k.handlers = append(k.handlers, skHandler{pattern, fn})
	return k
}

func (k *scriptKube) Run(stdin []byte, name string, args ...string) ([]byte, error) {
	k.mu.Lock()
	k.calls = append(k.calls, kcall{append([]string{}, args...), append([]byte(nil), stdin...)})
	handlers := append([]skHandler(nil), k.handlers...)
	k.mu.Unlock()
	if k.tl != nil {
		k.tl.add("kubectl " + strings.Join(args, " "))
	}
	joined := strings.Join(args, " ")
	for _, h := range handlers {
		if strings.Contains(joined, h.pattern) {
			return h.fn(args, stdin)
		}
	}
	return nil, fmt.Errorf("unexpected kubectl %s", joined)
}

// client is a kube.Client whose argv carries the selectors a real handle adds.
func (k *scriptKube) client(ns string) *kube.Client {
	return kube.ForTest(k, ns, "--kubeconfig", skKubeconfig, "--context", "darkplum")
}

// matching returns the calls whose argument line contains pattern.
func (k *scriptKube) matching(pattern string) []kcall {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []kcall
	for _, c := range k.calls {
		if strings.Contains(c.joined(), pattern) {
			out = append(out, c)
		}
	}
	return out
}

// assertPinned fails when any call lacks the pinned selectors, and when a secret
// value appears in argv.
func (k *scriptKube) assertClean(secrets ...string) {
	k.t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range k.calls {
		if len(c.args) < 4 || c.args[0] != "--kubeconfig" || c.args[1] != skKubeconfig || c.args[2] != "--context" || c.args[3] != "darkplum" {
			k.t.Errorf("kubectl call without the pinned selectors: %v", c.args)
		}
		for _, s := range secrets {
			if s != "" && strings.Contains(c.joined(), s) {
				k.t.Errorf("a secret value is in kubectl argv: %v", c.args)
			}
		}
	}
}

const notFoundErr = `kubectl: Error from server (NotFound): secrets "%s" not found`

func secretNotFound(name string) error { return fmt.Errorf(notFoundErr, name) }

// timeline is one ordered record shared by every fake in a test, so a test can
// assert that one thing happened before another across terraform, aws and kubectl.
type timeline struct {
	mu sync.Mutex
	ev []string
}

func (t *timeline) add(s string) { t.mu.Lock(); t.ev = append(t.ev, s); t.mu.Unlock() }

// first is the index of the first event containing s, or -1.
func (t *timeline) first(s string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, e := range t.ev {
		if strings.Contains(e, s) {
			return i
		}
	}
	return -1
}

// last is the index of the last event containing s, or -1.
func (t *timeline) last(s string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.ev) - 1; i >= 0; i-- {
		if strings.Contains(t.ev[i], s) {
			return i
		}
	}
	return -1
}

func (t *timeline) count(s string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, e := range t.ev {
		if strings.Contains(e, s) {
			n++
		}
	}
	return n
}

func (t *timeline) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.ev, "\n")
}
