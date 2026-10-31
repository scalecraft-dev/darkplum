package commands

import "context"

// Forwarder is a local tunnel to the install's web service: the same front
// door a browser uses (nginx in front of the api), reached without publishing
// anything. Tests replace it with an httptest server. The real one is
// runner.PortForward, which only exists for a validated cluster.
type Forwarder interface {
	// Open starts a tunnel (closing any previous one) and returns its base URL.
	Open(ctx context.Context) (string, error)
	// Done is closed when the tunnel opened last exits.
	Done() <-chan struct{}
	// Close stops the tunnel. Safe to call more than once.
	Close()
}

// newKubectlForwarder is the tunnel to svc/darkplum-web of the validated cluster.
func newKubectlForwarder(t clusterTarget, namespace string) Forwarder {
	return t.h.PortForward(namespace, "svc/darkplum-web")
}
