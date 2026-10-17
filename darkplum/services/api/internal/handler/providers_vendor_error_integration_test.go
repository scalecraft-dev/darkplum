package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The gateway's vendor_error 422 must reach the admin as the closed api
// message with the two fields, on all three routes, with any gateway-supplied
// message text (and malformed type/status values) discarded.
func TestVendorError_PassThroughOnVerifyPutKeyPutFilter(t *testing.T) {
	const sentinel = "SENTINEL-vendor-text-9911"
	routes := []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPost, "/api/v1/providers/anthropic/verify", map[string]any{"key": "sk-ant-abcdefghijklmnopqrstuvwx"}},
		{http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{"key": "sk-ant-abcdefghijklmnopqrstuvwx", "model_filter": []string{"claude-*"}}},
		{http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{"model_filter": []string{"claude-*"}}},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			pool := setupTestDB(t)
			gw := newFakeProviderGateway(t)
			gw.verifyStatus = http.StatusUnprocessableEntity
			gw.verifyBody = map[string]any{"error": map[string]any{
				"code": "vendor_error", "message": sentinel,
				"vendor_http_status": 400, "vendor_error_type": "invalid_request_error",
			}}
			ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
			r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

			resp := doJSON(t, r, rt.method, rt.path, rt.body)
			if resp.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
			}
			want := `{"error":{"code":"vendor_error","message":"Anthropic answered HTTP 400 (invalid_request_error) to the key check. Nothing was stored.","vendor_http_status":400,"vendor_error_type":"invalid_request_error"}}`
			if strings.TrimSpace(resp.Body.String()) != want {
				t.Fatalf("body = %s\nwant   %s", resp.Body.String(), want)
			}
			if len(fk.secrets) != 0 {
				t.Fatal("a secret was stored on a vendor_error")
			}
		})
	}
}

func TestVendorError_MalformedGatewayFieldsAreReplaced(t *testing.T) {
	got := translateVendorError("openai", 99999, "Evil <b>type")
	if got.VendorHTTPStatus != 502 || got.VendorErrorType != "unknown" {
		t.Fatalf("got %+v", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "Evil") {
		t.Fatalf("unsanitised type leaked: %s", b)
	}
	if !strings.HasPrefix(got.Message, "OpenAI answered HTTP 502 (unknown)") {
		t.Fatalf("message = %q", got.Message)
	}
}
