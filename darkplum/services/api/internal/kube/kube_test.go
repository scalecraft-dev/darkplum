package kube

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeAPIServer is a *httptest.Server plus the last request it received,
// for asserting method/path/content-type/body/bearer token without a real
// Kubernetes API server.
type fakeAPIServer struct {
	*httptest.Server
	lastMethod      string
	lastPath        string
	lastContentType string
	lastAuth        string
	lastBody        []byte
	status          int
	getCount        int
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.getCount++
		}
		f.lastMethod = r.Method
		f.lastPath = r.URL.Path
		f.lastContentType = r.Header.Get("Content-Type")
		f.lastAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		f.lastBody = body
		w.WriteHeader(f.status)
	})
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Close)
	return f
}

// writerFor builds a Writer pointed at the fake apiserver, with a temp
// service-account directory containing token/ca.crt/namespace.
func writerFor(t *testing.T, f *fakeAPIServer, initialToken, namespace string) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), certPEM, 0o600); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(initialToken), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "namespace"), []byte(namespace), 0o600); err != nil {
		t.Fatalf("write namespace: %v", err)
	}

	host, port, err := net.SplitHostPort(f.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split fake apiserver address: %v", err)
	}

	return &Writer{saDir: dir, host: host, port: port}, dir
}

func TestWriter_SetSecretKey(t *testing.T) {
	f := newFakeAPIServer(t)
	w, _ := writerFor(t, f, "tok-1", "darkplum-system")

	if err := w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("sk-ant-secretvalue")); err != nil {
		t.Fatalf("SetSecretKey: %v", err)
	}

	if f.lastMethod != http.MethodPatch {
		t.Fatalf("method = %s, want PATCH", f.lastMethod)
	}
	if f.lastPath != "/api/v1/namespaces/darkplum-system/secrets/darkplum-provider-keys" {
		t.Fatalf("path = %s", f.lastPath)
	}
	if f.lastContentType != "application/merge-patch+json" {
		t.Fatalf("content-type = %s", f.lastContentType)
	}
	if f.lastAuth != "Bearer tok-1" {
		t.Fatalf("authorization = %s", f.lastAuth)
	}

	var got map[string]map[string]string
	if err := json.Unmarshal(f.lastBody, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("sk-ant-secretvalue"))
	if got["data"]["anthropic-api-key"] != want {
		t.Fatalf("body data.anthropic-api-key = %q, want %q", got["data"]["anthropic-api-key"], want)
	}
	if len(got["data"]) != 1 {
		t.Fatalf("expected exactly one data key, got %v", got["data"])
	}

	if f.getCount != 0 {
		t.Fatalf("expected zero GET requests, got %d", f.getCount)
	}
}

func TestWriter_RemoveSecretKey(t *testing.T) {
	f := newFakeAPIServer(t)
	w, _ := writerFor(t, f, "tok-1", "darkplum-system")

	if err := w.RemoveSecretKey(context.Background(), "openai-api-key"); err != nil {
		t.Fatalf("RemoveSecretKey: %v", err)
	}

	if string(f.lastBody) != `{"data":{"openai-api-key":null}}` {
		t.Fatalf("body = %s, want exact null-key merge patch", f.lastBody)
	}
	if f.lastPath != "/api/v1/namespaces/darkplum-system/secrets/darkplum-provider-keys" {
		t.Fatalf("path = %s", f.lastPath)
	}
	if f.getCount != 0 {
		t.Fatalf("expected zero GET requests, got %d", f.getCount)
	}
}

func TestWriter_SetConfig(t *testing.T) {
	f := newFakeAPIServer(t)
	w, _ := writerFor(t, f, "tok-1", "darkplum-system")

	if err := w.SetConfig(context.Background(), map[string]string{
		"ANTHROPIC_MODEL_FILTER":        "claude-*",
		"ANTHROPIC_MODEL_FILTER_SET_AT": "2026-09-19T00:00:00Z",
	}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	if f.lastPath != "/api/v1/namespaces/darkplum-system/configmaps/darkplum-provider-config" {
		t.Fatalf("path = %s", f.lastPath)
	}
	if f.lastContentType != "application/merge-patch+json" {
		t.Fatalf("content-type = %s", f.lastContentType)
	}

	var got map[string]map[string]string
	if err := json.Unmarshal(f.lastBody, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["data"]["ANTHROPIC_MODEL_FILTER"] != "claude-*" {
		t.Fatalf("filter = %q", got["data"]["ANTHROPIC_MODEL_FILTER"])
	}
	if got["data"]["ANTHROPIC_MODEL_FILTER_SET_AT"] != "2026-09-19T00:00:00Z" {
		t.Fatalf("set_at = %q", got["data"]["ANTHROPIC_MODEL_FILTER_SET_AT"])
	}
	if f.getCount != 0 {
		t.Fatalf("expected zero GET requests, got %d", f.getCount)
	}
}

// TestWriter_TokenReReadOnEveryCall proves the token is read fresh from disk
// on every call, not cached at construction -- a projected service account
// token rotates on disk without the api process restarting.
func TestWriter_TokenReReadOnEveryCall(t *testing.T) {
	f := newFakeAPIServer(t)
	w, dir := writerFor(t, f, "tok-1", "darkplum-system")

	if err := w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("first-key-value")); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if f.lastAuth != "Bearer tok-1" {
		t.Fatalf("first call authorization = %s", f.lastAuth)
	}

	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok-2-rotated"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}

	if err := w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("second-key-value")); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if f.lastAuth != "Bearer tok-2-rotated" {
		t.Fatalf("second call authorization = %s, want the rotated token", f.lastAuth)
	}
}

func TestWriter_ErrorClassification(t *testing.T) {
	tests := []struct {
		status  int
		wantErr error
	}{
		{http.StatusNotFound, ErrObjectMissing},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusInternalServerError, ErrUnavailable},
		{http.StatusBadGateway, ErrUnavailable},
	}
	for _, tt := range tests {
		f := newFakeAPIServer(t)
		f.status = tt.status
		w, _ := writerFor(t, f, "tok-1", "darkplum-system")

		err := w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("value"))
		if err == nil || !errors.Is(err, tt.wantErr) {
			t.Fatalf("status %d: err = %v, want wrapping %v", tt.status, err, tt.wantErr)
		}
	}
}

func TestWriter_Available(t *testing.T) {
	f := newFakeAPIServer(t)
	w, dir := writerFor(t, f, "tok-1", "darkplum-system")

	if !w.Available() {
		t.Fatal("expected Available() true with host/port set and every SA file present")
	}

	if err := os.Remove(filepath.Join(dir, "token")); err != nil {
		t.Fatalf("remove token: %v", err)
	}
	if w.Available() {
		t.Fatal("expected Available() false once the token file is missing")
	}
}

func TestWriter_NotInCluster(t *testing.T) {
	w := &Writer{saDir: t.TempDir(), host: "", port: ""}
	if w.Available() {
		t.Fatal("expected Available() false with no host/port and no SA files")
	}
	err := w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("value"))
	if !errors.Is(err, ErrNotInCluster) {
		t.Fatalf("err = %v, want ErrNotInCluster", err)
	}
}

// TestWriter_NeverFollowsRedirect proves the client refuses to follow a
// redirect from the "API server": a 307 draws exactly one request (the
// bearer token is sent once, never resent to wherever Location points), and
// the 307 itself classifies as ErrUnavailable.
func TestWriter_NeverFollowsRedirect(t *testing.T) {
	var requests int
	var authHeaders []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requests++
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		w.Header().Set("Location", "https://attacker.invalid/steal")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), certPEM, 0o600); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok-redirect"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "namespace"), []byte("darkplum-system"), 0o600); err != nil {
		t.Fatalf("write namespace: %v", err)
	}
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	w := &Writer{saDir: dir, host: host, port: port}

	err = w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("value"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable for an unfollowed redirect", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want exactly 1 (no second request to the redirect target)", requests)
	}
	if len(authHeaders) != 1 || authHeaders[0] != "Bearer tok-redirect" {
		t.Fatalf("authorization headers = %v, want exactly one send of the bearer token", authHeaders)
	}
}

// selfSignedTestCAPEM generates a throwaway self-signed certificate's PEM
// bytes -- used only as distinct CA content for TestWriterTransportFor_*,
// never presented by an actual TLS server in these tests. Go's
// httptest.NewTLSServer always hands out the SAME baked-in certificate
// across every instance in a process, so two httptest servers can't be used
// to produce two different ca.crt contents the way a real cluster's CA
// rotation would.
func selfSignedTestCAPEM(t *testing.T, commonName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestWriterTransportFor_RebuildsOnlyOnCAChange proves transportFor caches
// and reuses the built *http.Transport (and its connection pool) across
// calls whose ca.crt content is unchanged, and rebuilds it only once that
// content actually changes -- e.g. a cluster CA rotation.
func TestWriterTransportFor_RebuildsOnlyOnCAChange(t *testing.T) {
	w := &Writer{}
	ca1 := selfSignedTestCAPEM(t, "ca-1")
	ca2 := selfSignedTestCAPEM(t, "ca-2")

	t1, err := w.transportFor(ca1)
	if err != nil {
		t.Fatalf("transportFor(ca1): %v", err)
	}
	t1Again, err := w.transportFor(append([]byte(nil), ca1...)) // same content, different slice
	if err != nil {
		t.Fatalf("transportFor(ca1) again: %v", err)
	}
	if t1 != t1Again {
		t.Fatal("expected the same *http.Transport for unchanged ca.crt content")
	}

	t2, err := w.transportFor(ca2)
	if err != nil {
		t.Fatalf("transportFor(ca2): %v", err)
	}
	if t2 == t1 {
		t.Fatal("expected a rebuilt *http.Transport once ca.crt content changed")
	}
}

// TestWriter_ReusesTransportAcrossCalls proves SetSecretKey itself goes
// through the cached transport, not a fresh one per call.
func TestWriter_ReusesTransportAcrossCalls(t *testing.T) {
	f := newFakeAPIServer(t)
	w, _ := writerFor(t, f, "tok-1", "darkplum-system")

	if err := w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("value-1")); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstTransport := w.transport
	if firstTransport == nil {
		t.Fatal("expected a cached transport after the first call")
	}

	if err := w.SetSecretKey(context.Background(), "anthropic-api-key", []byte("value-2")); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if w.transport != firstTransport {
		t.Fatal("expected the same *http.Transport to be reused when ca.crt is unchanged")
	}
}

func TestNetJoinHostPort(t *testing.T) {
	cases := []struct {
		name string
		host string
		port string
		want string
	}{
		{"IPv4", "10.0.0.1", "443", "10.0.0.1:443"},
		{"unbracketed IPv6 gets bracketed", "::1", "6443", "[::1]:6443"},
		{"already-bracketed IPv6 stays single-bracketed", "[::1]", "6443", "[::1]:6443"},
		{"hostname", "kubernetes.default.svc", "443", "kubernetes.default.svc:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := netJoinHostPort(tc.host, tc.port); got != tc.want {
				t.Errorf("netJoinHostPort(%q, %q) = %q, want %q", tc.host, tc.port, got, tc.want)
			}
		})
	}
}
