package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestProxyChat_GatewayErrorPassthrough verifies that a gateway error body
// (the OpenAI-compatible {"error":{"message","type","code"}} envelope used
// for model_not_found / model_not_enabled / model_residency_violation /
// policy_unavailable) reaches the browser unchanged — same status code, same
// JSON body — for both stream:true and stream:false requests. ProxyChat's
// streaming branch is selected by the *request's* stream flag, not the
// response's actual content, so a small non-SSE error body must still make it
// through the chunked-copy path intact.
func TestProxyChat_GatewayErrorPassthrough(t *testing.T) {
	for _, code := range []struct {
		status int
		body   string
	}{
		{http.StatusNotFound, `{"error":{"message":"model \"x\" is not available on this gateway","type":"invalid_request_error","code":"model_not_found"}}`},
		{http.StatusForbidden, `{"error":{"message":"model \"x\" is not enabled","type":"permission_error","code":"model_not_enabled"}}`},
		{http.StatusForbidden, `{"error":{"message":"residency policy forbids this model","type":"permission_error","code":"model_residency_violation"}}`},
		{http.StatusServiceUnavailable, `{"error":{"message":"policy snapshot unavailable","type":"service_unavailable","code":"policy_unavailable"}}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/stream=%v", code.status, stream), func(t *testing.T) {
				gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(code.status)
					_, _ = w.Write([]byte(code.body))
				}))
				defer gw.Close()

				gin.SetMode(gin.TestMode)
				h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000")
				r := gin.New()
				r.POST("/v1/chat/completions", h.ProxyChat)

				reqBody := fmt.Sprintf(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":%v}`, stream)
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)

				if rec.Code != code.status {
					t.Fatalf("status = %d, want %d; body = %s", rec.Code, code.status, rec.Body.String())
				}

				var got, want map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("response body is not valid JSON: %v (body = %q)", err, rec.Body.String())
				}
				if err := json.Unmarshal([]byte(code.body), &want); err != nil {
					t.Fatalf("test fixture body is not valid JSON: %v", err)
				}
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Fatalf("body mismatch:\n got:  %s\n want: %s", gotJSON, wantJSON)
				}
			})
		}
	}
}
