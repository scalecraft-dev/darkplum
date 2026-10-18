package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/middleware"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
)

const vendorErrSentinel = "SENTINEL-acct-org-4242-leak"

type staticCreds struct{}

func (staticCreds) Key() (string, uint64, error) { return "sk-installed", 1, nil }

type verifyErrBody struct {
	Error struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		Status   *int   `json:"vendor_http_status"`
		ErrorTyp string `json:"vendor_error_type"`
	} `json:"error"`
}

func vendorHandler(baseURL string) *ProvidersHandler {
	a := anthropic.New(staticCreds{}, anthropic.Config{DefaultMaxTokens: 4096, BaseURL: baseURL})
	o := openai.New(staticCreds{}, openai.Config{BaseURL: baseURL})
	return NewProvidersHandler(context.Background(), &fakeRefreshManager{}, map[string]ProviderVendorConfig{
		"anthropic": {Verifier: a, Creds: staticCreds{}},
		"openai":    {Verifier: o, Creds: staticCreds{}},
	})
}

func verifyVia(t *testing.T, h *ProvidersHandler, vendor string) (*httptest.ResponseRecorder, verifyErrBody, string) {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	body, _ := json.Marshal(verifyRequest{Key: "sk-candidate"})
	w := doProvidersRequest(newTestProvidersRouter(h), http.MethodPost, "/v1/providers/"+vendor+"/verify", middleware.KeyTypeService, body)
	var out verifyErrBody
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out, logs.String()
}

func TestHandleVerify_VendorErrorOutcome(t *testing.T) {
	cases := []struct {
		status  int
		vendTyp string
		want    string
	}{
		{400, "invalid_request_error", "invalid_request_error"},
		{404, "not_found_error", "not_found_error"},
		{429, "rate_limit_error", "rate_limit_error"},
		{500, "api_error", "api_error"},
		{529, "overloaded_error", "overloaded_error"},
		{400, "Bad Type!<script>", "unknown"},
		{400, "", "unknown"},
	}
	for _, vendor := range []string{"anthropic", "openai"} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s_%d_%s", vendor, tc.status, tc.want), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, tc.vendTyp, vendorErrSentinel)
				}))
				defer srv.Close()
				w, out, logs := verifyVia(t, vendorHandler(srv.URL), vendor)
				if w.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
				}
				if out.Error.Code != "vendor_error" || out.Error.Status == nil || *out.Error.Status != tc.status || out.Error.ErrorTyp != tc.want {
					t.Fatalf("got %+v (body=%s)", out, w.Body.String())
				}
				if strings.Contains(w.Body.String(), vendorErrSentinel) || strings.Contains(logs, vendorErrSentinel) {
					t.Fatalf("vendor text leaked; body=%s logs=%s", w.Body.String(), logs)
				}
				if !strings.Contains(logs, "outcome=vendor_error") || !strings.Contains(logs, fmt.Sprintf("vendor_http_status=%d", tc.status)) ||
					!strings.Contains(logs, "vendor_error_type="+tc.want) {
					t.Errorf("diagnostic log line incomplete: %s", logs)
				}
			})
		}
	}
}

func TestHandleVerify_VendorAuthStillAuthFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"type":"authentication_error","message":%q}}`, vendorErrSentinel)
	}))
	defer srv.Close()
	for _, vendor := range []string{"anthropic", "openai"} {
		w, out, _ := verifyVia(t, vendorHandler(srv.URL), vendor)
		if w.Code != 422 || out.Error.Code != "auth_failed" || out.Error.Status != nil {
			t.Errorf("%s: got %d %s", vendor, w.Code, w.Body.String())
		}
	}
}

func TestHandleVerify_TransportFailuresStayUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + ln.Addr().String()
	ln.Close() // nothing listening: connection refused
	for _, base := range []string{refused, "http://no-such-host.invalid"} {
		for _, vendor := range []string{"anthropic", "openai"} {
			w, out, _ := verifyVia(t, vendorHandler(base), vendor)
			if w.Code != 422 || out.Error.Code != "unreachable" || out.Error.Status != nil {
				t.Errorf("%s %s: got %d %s", vendor, base, w.Code, w.Body.String())
			}
		}
	}
}

const wsRequiredVendorMsg = "This API key is not scoped to a workspace, so this request must include the anthropic-workspace-id header with the ID of the workspace to use."

func verifyBody(t *testing.T, h *ProvidersHandler, body string) (*httptest.ResponseRecorder, verifyErrBody, string) {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	w := doProvidersRequest(newTestProvidersRouter(h), http.MethodPost, "/v1/providers/anthropic/verify", middleware.KeyTypeService, []byte(body))
	var out verifyErrBody
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out, logs.String()
}

func TestHandleVerify_WorkspaceRequiredOutcome(t *testing.T) {
	var mu sync.Mutex
	var seenHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenHeaders = append(seenHeaders, r.Header.Get("anthropic-workspace-id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("anthropic-workspace-id") == "" {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, wsRequiredVendorMsg+" "+vendorErrSentinel)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"claude-haiku-4-5","type":"model","display_name":"x","created_at":"2025-01-01T00:00:00Z","max_tokens":8192}],"has_more":false}`)
	}))
	defer srv.Close()

	// Submitted key, no workspace: 422 workspace_required, static text only.
	w, out, logs := verifyBody(t, vendorHandler(srv.URL), `{"key":"sk-candidate"}`)
	if w.Code != 422 || out.Error.Code != "workspace_required" || out.Error.Status != nil {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	const wantMsg = "This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace."
	if out.Error.Message != wantMsg {
		t.Errorf("message = %q", out.Error.Message)
	}
	if strings.Contains(w.Body.String(), vendorErrSentinel) || strings.Contains(logs, vendorErrSentinel) || strings.Contains(w.Body.String(), "anthropic-workspace-id") {
		t.Fatalf("vendor text leaked; body=%s logs=%s", w.Body.String(), logs)
	}
	if !strings.Contains(logs, "outcome=workspace_required") || !strings.Contains(logs, "vendor_http_status=400") {
		t.Errorf("diagnostic log line incomplete: %s", logs)
	}

	// Installed key (no key in body) hits the same classification.
	h := vendorHandler(srv.URL)
	cfg := h.vendors["anthropic"]
	cfg.IsWorkspaceRequired = anthropic.IsWorkspaceRequiredError
	h.vendors["anthropic"] = cfg
	w, out, logs = verifyBody(t, h, `{}`)
	if w.Code != 422 || out.Error.Code != "workspace_required" {
		t.Fatalf("installed path: got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(logs, "outcome=workspace_required") {
		t.Errorf("installed path log: %s", logs)
	}

	// With a workspace id supplied the header is sent and the check succeeds.
	w, _, _ = verifyBody(t, vendorHandler(srv.URL), `{"key":"sk-candidate","workspace_id":"wrkspc_01"}`)
	if w.Code != 200 {
		t.Fatalf("with workspace: got %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if seenHeaders[len(seenHeaders)-1] != "wrkspc_01" {
		t.Errorf("verify did not forward workspace_id as the header: %v", seenHeaders)
	}
}

func TestHandleVerify_Other400StaysVendorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, "something else "+vendorErrSentinel)
	}))
	defer srv.Close()
	w, out, _ := verifyBody(t, vendorHandler(srv.URL), `{"key":"sk-candidate","workspace_id":"wrkspc_01"}`)
	if w.Code != 422 || out.Error.Code != "vendor_error" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleVerify_InstalledKeyHonoursWorkspaceOverrideAndValidatesID(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("anthropic-workspace-id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"claude-haiku-4-5","type":"model","display_name":"x","created_at":"2025-01-01T00:00:00Z","max_tokens":8192}],"has_more":false}`)
	}))
	defer srv.Close()

	if w, _, _ := verifyBody(t, vendorHandler(srv.URL), `{"workspace_id":"wrkspc_new"}`); w.Code != 200 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if w, _, _ := verifyBody(t, vendorHandler(srv.URL), `{"workspace_id":""}`); w.Code != 200 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	if len(seen) != 2 || seen[0] != "wrkspc_new" || seen[1] != "" {
		t.Errorf("installed-path override headers = %q", seen)
	}
	n := len(seen)
	mu.Unlock()

	for _, bad := range []string{`"a b"`, `"x\r\nInjected: 1"`, `"` + strings.Repeat("a", 65) + `"`} {
		w, _, _ := verifyBody(t, vendorHandler(srv.URL), `{"key":"sk-candidate","workspace_id":`+bad+`}`)
		if w.Code != 400 {
			t.Errorf("workspace_id %s: got %d, want 400", bad, w.Code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Error("a malformed workspace_id must be refused before any vendor call")
	}
}
