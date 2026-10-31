package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var forwardingRe = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// PortForward runs `kubectl port-forward` on a port the OS picks, bound to
// 127.0.0.1 only. The process gets its own process group, so the terminal's
// Ctrl-C reaches only the CLI, which then stops kubectl itself, on every exit
// path. It exists only for a validated cluster: see ValidatedCluster.PortForward.
type PortForward struct {
	flags              []string // --kubeconfig <private> --context darkplum
	namespace, service string
	env                []string
	check              func() error
	openTimeout        time.Duration

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

// PortForward returns a tunnel to service (for example "svc/darkplum-web", port
// 80) in namespace on this cluster. Nothing is started until Open. A handle with
// no pinned kubeconfig opens nothing.
func (v ValidatedCluster) PortForward(namespace, service string) *PortForward {
	pf := &PortForward{flags: v.Flags(), namespace: namespace, service: service,
		env: v.Env(), openTimeout: 30 * time.Second}
	if k, ok := v.Exec().(kubectlExec); ok {
		pf.check = k.check
	}
	return pf
}

func (f *PortForward) args() []string {
	return append(append([]string{}, f.flags...), "-n", f.namespace,
		"port-forward", "--address", "127.0.0.1", f.service, ":80")
}

// Open starts a tunnel (closing any previous one) and returns its base URL.
func (f *PortForward) Open(ctx context.Context) (string, error) {
	if len(f.flags) == 0 {
		return "", errors.New("runner: no validated cluster to forward to")
	}
	if f.check != nil {
		if err := f.check(); err != nil {
			return "", err
		}
	}
	f.Close()
	// Not tied to ctx: a Ctrl-C must leave the tunnel up long enough for
	// configure to sign its session out through it. Close stops it, and it is
	// deferred on every path; startup is cancelled explicitly below.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "kubectl", f.args()...)
	cmd.Env = append(os.Environ(), append([]string{"KUBERC=off"}, f.env...)...)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	port := make(chan string, 1)
	var stderr lockedBuffer
	cmd.Stdout = &lineWriter{fn: func(line string) {
		if m := forwardingRe.FindStringSubmatch(line); m != nil {
			select {
			case port <- m[1]:
			default:
			}
		}
	}}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start kubectl port-forward: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	// Recorded before the port is known, so a Close during startup stops it.
	f.mu.Lock()
	f.cmd, f.done = cmd, done
	f.mu.Unlock()

	select {
	case p := <-port:
		return "http://127.0.0.1:" + p, nil
	case <-done:
		msg := firstLine(stderr.String())
		if msg == "" {
			msg = "kubectl port-forward exited"
		}
		return "", errors.New(msg)
	case <-time.After(f.openTimeout):
		_ = killProcessGroup(cmd)
		<-done
		return "", fmt.Errorf("kubectl port-forward did not start within %s", f.openTimeout)
	case <-ctx.Done():
		_ = killProcessGroup(cmd)
		<-done
		return "", ctx.Err()
	}
}

// Done is closed when the tunnel opened last exits.
func (f *PortForward) Done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	return f.done
}

// Close stops the tunnel. Safe to call more than once.
func (f *PortForward) Close() {
	f.mu.Lock()
	cmd, done := f.cmd, f.done
	f.cmd, f.done = nil, nil
	f.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = killProcessGroup(cmd)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// lineWriter calls fn for each complete line written to it.
type lineWriter struct {
	buf []byte
	fn  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(strings.TrimSpace(string(w.buf[:i])))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// lockedBuffer keeps what kubectl wrote to stderr (read after it exits).
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
