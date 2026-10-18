// Package kube writes exactly two Kubernetes objects the admin UI's vendor
// key install flow needs (Secret darkplum-provider-keys, ConfigMap
// darkplum-provider-config), using raw in-cluster HTTPS -- no client-go, no
// generic object model, nothing beyond one PATCH call shape. This is
// deliberate: the api's ServiceAccount is RBAC-scoped to "patch" on exactly
// those two named objects (see PLAN-UI's "Objects and deploy" section), so a
// general-purpose Kubernetes client here would be reaching for permissions
// the api's own RBAC role can never grant it anyway.
//
// Every call re-reads the token, CA certificate, and namespace from
// /var/run/secrets/kubernetes.io/serviceaccount rather than caching them at
// construction time: a projected service account token rotates (kubelet
// refreshes it on disk periodically, well under its own TTL), and a cached
// copy would eventually be rejected by the API server as expired.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Errors returned by Writer's methods. Every one is a fixed, static message
// -- callers must never wrap the request or response BODY into these, since
// the request body carries a vendor key and a Kubernetes error response can
// echo back arbitrary caller-supplied content.
var (
	// ErrNotInCluster means the process is not running as a Kubernetes pod
	// (no KUBERNETES_SERVICE_HOST/_PORT, or the projected service account
	// files are missing/unreadable) -- expected for local dev, docker-compose,
	// or any deployment mode other than the customer's cluster.
	ErrNotInCluster = errors.New("not running inside a Kubernetes cluster")
	// ErrObjectMissing means the API server returned 404 for the target
	// Secret/ConfigMap -- the platform's base manifests (which create both
	// objects with no data stanza) were never applied to this cluster,
	// typically because it was deployed before UI key install shipped.
	ErrObjectMissing = errors.New("target Kubernetes object does not exist")
	// ErrForbidden means the API server returned 403 -- the darkplum-api
	// ServiceAccount's RBAC role does not permit this call.
	ErrForbidden = errors.New("Kubernetes API server refused this request (RBAC)")
	// ErrUnavailable covers every other failure: a network error, a timeout,
	// or any non-2xx/403/404 status.
	ErrUnavailable = errors.New("Kubernetes API server unavailable")
)

// saDir is the standard projected service account mount path.
const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// requestTimeout bounds one PATCH call end to end.
const requestTimeout = 10 * time.Second

// secretName and configMapName are the two objects this package ever
// touches, both in the api's own namespace (read from the service account
// mount, never hard-coded, so this works whatever namespace the platform is
// installed into).
const (
	secretName    = "darkplum-provider-keys"
	configMapName = "darkplum-provider-config"
)

// ProviderObjects is the interface internal/handler/providers.go depends on
// -- satisfied by *Writer in production and fakeable in handler tests
// without an apiserver.
type ProviderObjects interface {
	// Available reports whether a write is even worth attempting: running
	// inside Kubernetes with a readable service account mount. It does NOT
	// guarantee the subsequent call succeeds (RBAC and object-existence
	// failures still surface from the write methods themselves), only that
	// this isn't, e.g., a local dev process with no cluster at all.
	Available() bool
	// SetSecretKey merge-patches one data key of the provider-keys Secret to
	// base64(value).
	SetSecretKey(ctx context.Context, name string, value []byte) error
	// RemoveSecretKey merge-patches one data key of the provider-keys Secret
	// to JSON null, deleting it.
	RemoveSecretKey(ctx context.Context, name string) error
	// SetConfig merge-patches every key in kv into the provider-config
	// ConfigMap's data.
	SetConfig(ctx context.Context, kv map[string]string) error
}

// Writer implements ProviderObjects via raw in-cluster HTTPS PATCH calls
// (Content-Type: application/merge-patch+json). It issues no GET, LIST, or
// WATCH of any kind -- the api's RBAC role grants patch only, so nothing
// here could use a broader verb even by accident.
type Writer struct {
	// saDir, host, and port are overridable so tests can point a Writer at
	// an httptest TLS server and a temp service-account directory instead of
	// the real in-cluster paths/env. Zero values fall back to the real
	// mount and KUBERNETES_SERVICE_HOST/_PORT.
	saDir string
	host  string
	port  string

	// mu guards transport and transportCA -- see transportFor's doc comment.
	mu          sync.Mutex
	transport   *http.Transport
	transportCA []byte
}

// NewWriter returns a Writer configured for the real in-cluster mount and
// environment.
func NewWriter() *Writer {
	return &Writer{}
}

func (w *Writer) dir() string {
	if w.saDir != "" {
		return w.saDir
	}
	return saDir
}

func (w *Writer) hostPort() (host, port string) {
	host, port = w.host, w.port
	if host == "" {
		host = os.Getenv("KUBERNETES_SERVICE_HOST")
	}
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	return host, port
}

// Available reports whether the host/port env and every service account
// file are present. It performs no network call.
func (w *Writer) Available() bool {
	host, port := w.hostPort()
	if host == "" || port == "" {
		return false
	}
	for _, f := range []string{"token", "ca.crt", "namespace"} {
		if _, err := os.Stat(filepath.Join(w.dir(), f)); err != nil {
			return false
		}
	}
	return true
}

// creds re-reads the token, CA certificate (raw PEM, NOT yet parsed into a
// pool -- see transportFor for why), and namespace from the service account
// mount. Called on every PATCH, never cached -- see the package doc comment.
func (w *Writer) creds() (token string, namespace string, caPEM []byte, err error) {
	host, port := w.hostPort()
	if host == "" || port == "" {
		return "", "", nil, ErrNotInCluster
	}

	tokenBytes, err := os.ReadFile(filepath.Join(w.dir(), "token"))
	if err != nil {
		return "", "", nil, ErrNotInCluster
	}
	caBytes, err := os.ReadFile(filepath.Join(w.dir(), "ca.crt"))
	if err != nil {
		return "", "", nil, ErrNotInCluster
	}
	nsBytes, err := os.ReadFile(filepath.Join(w.dir(), "namespace"))
	if err != nil {
		return "", "", nil, ErrNotInCluster
	}

	return strings.TrimSpace(string(tokenBytes)), strings.TrimSpace(string(nsBytes)), caBytes, nil
}

// transportFor returns a *http.Transport whose TLS root pool is built from
// caPEM, reusing the previously built Transport (and its underlying
// connection pool) across calls whenever caPEM is unchanged from the last
// call -- a fresh *http.Transport per PATCH would open a new TLS connection
// to the API server on every single call instead of reusing one, and would
// silently throw away any keep-alive. The CA is only re-parsed and the
// Transport only rebuilt when ca.crt's on-disk content actually changes
// (e.g. a cluster CA rotation), matching creds()'s "re-read every call, but
// only ACT on a change" contract for the token (which is compared by the
// API server itself, not here) and the CA (which is compared here, since
// nothing else would ever notice a stale pool).
func (w *Writer) transportFor(caPEM []byte) (*http.Transport, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.transport != nil && bytes.Equal(w.transportCA, caPEM) {
		return w.transport, nil
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, ErrNotInCluster
	}
	t := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	w.transport = t
	w.transportCA = append([]byte(nil), caPEM...)
	return t, nil
}

// patchResponseReadLimit bounds how much of the API server's PATCH response
// body this package will ever read, before discarding it unread into
// io.Discard -- see patch's doc comment on why the body is drained at all
// rather than left unread on the connection (so the connection can be
// reused) but never actually inspected.
const patchResponseReadLimit = 4096

// patch issues one merge-patch PATCH to /api/v1/namespaces/{ns}/{resource}
// with body, classifying the result into one of this package's four errors.
//
// A Kubernetes PATCH response body is the FULL updated object -- for
// secrets/darkplum-provider-keys that means every data key's base64 value,
// including the vendor key this exact request just wrote. The api's RBAC
// role only grants "patch" (never "get"), but patch's own response is still
// a read-back: a compromised or misbehaving API server could return
// anything in that body, so it is drained into io.Discard, bounded by
// patchResponseReadLimit, and NEVER unmarshaled, logged, or returned in an
// error. This is hygiene against a byte of that body accidentally reaching
// a log line or response somewhere down the line -- it is not a trust
// boundary the RBAC role verb already establishes; the api's own code is
// what must not misuse this response, since the verb itself does not
// prevent the API server from sending it.
//
// The client never follows a redirect (CheckRedirect returns
// http.ErrUseLastResponse): a 3xx here would mean a proxy or a compromised
// component in front of the real API server is trying to redirect this
// PATCH -- including its Bearer token -- somewhere else entirely, and a
// stock net/http client would follow it and resend the Authorization header
// to that new destination by default. Any 3xx status is classified the same
// as every other unrecognized status: ErrUnavailable.
func (w *Writer) patch(ctx context.Context, resource string, body []byte) error {
	token, namespace, caPEM, err := w.creds()
	if err != nil {
		return err
	}
	transport, err := w.transportFor(caPEM)
	if err != nil {
		return err
	}

	host, port := w.hostPort()
	target := fmt.Sprintf("https://%s/api/v1/namespaces/%s/%s", netJoinHostPort(host, port), namespace, resource)

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: build request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{
		Timeout:   requestTimeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, classifyTransportErr(err))
	}
	defer resp.Body.Close()
	// Drain and discard -- never read the apiserver's response body into an
	// error, log line, or anything else that could echo request content back
	// (see this function's doc comment: a PATCH response DOES contain the
	// full object, so this is hygiene, not enforcement).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, patchResponseReadLimit))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusNotFound:
		return ErrObjectMissing
	case http.StatusForbidden:
		return ErrForbidden
	default:
		// Covers every 3xx (redirect attempts, refused above by
		// CheckRedirect) along with every other unrecognized status.
		return ErrUnavailable
	}
}

// classifyTransportErr reduces a net/http transport error down to a short,
// static-shaped description (its type, not its full text) so a DNS/TLS
// error message that happens to have interpolated something caller-supplied
// can never reach an error string. In practice a transport error here is a
// connection/timeout failure, never one that echoes request content, but
// this keeps the contract airtight rather than relying on that always being
// true of every net/http failure mode.
func classifyTransportErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "connection error"
	}
}

func netJoinHostPort(host, port string) string {
	// IPv6 literal hosts need bracketing; KUBERNETES_SERVICE_HOST is almost
	// always IPv4, but handle it correctly regardless.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// SetSecretKey merge-patches {"data":{name: base64(value)}} into the
// provider-keys Secret.
func (w *Writer) SetSecretKey(ctx context.Context, name string, value []byte) error {
	return w.SetNamedSecretKey(ctx, secretName, name, value)
}

// BillingKeysSecret is the optional Secret holding the vendor billing
// (admin-scope) keys, mounted read-only into the api as files.
const BillingKeysSecret = "darkplum-billing-keys"

// ToolCredentialsSecret holds the optional bearer tokens library tool items
// (MCP servers) need, one data key per item id, mounted read-only into the
// api as files like the billing keys.
const ToolCredentialsSecret = "darkplum-tool-credentials"

// BillingSecrets is the narrow interface the billing-key routes depend on,
// satisfied by *Writer and fakeable without an apiserver.
type BillingSecrets interface {
	Available() bool
	SetNamedSecretKey(ctx context.Context, secret, name string, value []byte) error
	RemoveNamedSecretKey(ctx context.Context, secret, name string) error
}

// SetNamedSecretKey merge-patches {"data":{name: base64(value)}} into the
// named Secret. The api's Role only grants patch on the specific Secret names
// it manages, so an unknown secret fails ErrForbidden.
func (w *Writer) SetNamedSecretKey(ctx context.Context, secret, name string, value []byte) error {
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{name: base64.StdEncoding.EncodeToString(value)},
	})
	if err != nil {
		return fmt.Errorf("%w: encode patch body", ErrUnavailable)
	}
	return w.patch(ctx, "secrets/"+secret, body)
}

// RemoveSecretKey merge-patches {"data":{name: null}} into the provider-keys
// Secret, deleting that one data key.
func (w *Writer) RemoveSecretKey(ctx context.Context, name string) error {
	return w.RemoveNamedSecretKey(ctx, secretName, name)
}

// RemoveNamedSecretKey deletes one data key of the named Secret.
func (w *Writer) RemoveNamedSecretKey(ctx context.Context, secret, name string) error {
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{name: nil},
	})
	if err != nil {
		return fmt.Errorf("%w: encode patch body", ErrUnavailable)
	}
	return w.patch(ctx, "secrets/"+secret, body)
}

// SetConfig merge-patches {"data": kv} into the provider-config ConfigMap.
func (w *Writer) SetConfig(ctx context.Context, kv map[string]string) error {
	data := make(map[string]any, len(kv))
	for k, v := range kv {
		data[k] = v
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return fmt.Errorf("%w: encode patch body", ErrUnavailable)
	}
	return w.patch(ctx, "configmaps/"+configMapName, body)
}
