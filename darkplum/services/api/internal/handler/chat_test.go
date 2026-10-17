package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestProxyChat_RoutingHeadersForwarded covers the gateway's
// X-Darkplum-Model/X-Darkplum-Route/X-Darkplum-Router-Reason response
// headers being copied through to the browser unchanged, for both
// stream:true and stream:false requests (set before the first byte on both
// paths per DESIGN3's Semantics section).
func TestProxyChat_RoutingHeadersForwarded(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Darkplum-Model", "llama3-code")
				w.Header().Set("X-Darkplum-Route", "code")
				w.Header().Set("X-Darkplum-Router-Reason", "rules:code:builtin:code")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"x","choices":[]}`))
			}))
			defer gw.Close()

			gin.SetMode(gin.TestMode)
			h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000")
			r := gin.New()
			r.POST("/v1/chat/completions", h.ProxyChat)

			reqBody := fmt.Sprintf(`{"model":"smart","messages":[{"role":"user","content":"hi"}],"stream":%v}`, stream)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Darkplum-Model"); got != "llama3-code" {
				t.Errorf("X-Darkplum-Model = %q, want llama3-code", got)
			}
			if got := rec.Header().Get("X-Darkplum-Route"); got != "code" {
				t.Errorf("X-Darkplum-Route = %q, want code", got)
			}
			if got := rec.Header().Get("X-Darkplum-Router-Reason"); got != "rules:code:builtin:code" {
				t.Errorf("X-Darkplum-Router-Reason = %q, want rules:code:builtin:code", got)
			}
		})
	}
}

// TestProxyChat_StreamOutlivesServerWriteTimeout proves ProxyChat's
// SetWriteDeadline(zero) call actually defeats the server's WriteTimeout for
// this route (item 5, PLAN v3.1): a stream that legitimately takes longer
// than a (deliberately shortened, test-only) server WriteTimeout to fully
// write must still arrive intact, not get cut mid-stream. This uses a REAL
// net/http.Server with a real WriteTimeout -- not a mocked timer -- because
// Go's WriteTimeout is enforced by the net/http server's own connection
// deadline machinery, which nothing short of an actual server exercises.
func TestProxyChat_StreamOutlivesServerWriteTimeout(t *testing.T) {
	const (
		writeTimeout = 150 * time.Millisecond
		chunkDelay   = 60 * time.Millisecond
		chunkCount   = 8 // 8 * 60ms = 480ms total, well past writeTimeout
	)

	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < chunkCount; i++ {
			time.Sleep(chunkDelay)
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
		}
	}))
	defer gw.Close()

	gin.SetMode(gin.TestMode)
	h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000")
	router := gin.New()
	router.POST("/v1/chat/completions", h.ProxyChat)

	srv := httptest.NewUnstartedServer(router)
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	defer srv.Close()

	reqBody := `{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`
	httpReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := srv.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		t.Fatalf("read stream: %v (elapsed %v, got %d lines: %v)", err, time.Since(start), len(lines), lines)
	}

	if elapsed := time.Since(start); elapsed < chunkCount*chunkDelay {
		t.Fatalf("stream finished suspiciously fast (%v) for %d chunks at %v apart -- test setup is not actually exercising the slow path", elapsed, chunkCount, chunkDelay)
	}
	if len(lines) != chunkCount {
		t.Fatalf("received %d chunks, want %d -- the %v server WriteTimeout cut the %v-long stream short: %v",
			len(lines), chunkCount, writeTimeout, chunkCount*chunkDelay, lines)
	}
	for i, line := range lines {
		want := fmt.Sprintf("data: chunk-%d", i)
		if line != want {
			t.Errorf("line %d = %q, want %q", i, line, want)
		}
	}
}

// TestProxyChat_RequestBodyReachesGatewayIntact pins the contract that the
// proxy rewrites "messages" and nothing else. It used to decode the body into a
// three-field struct and re-encode it, which silently dropped max_tokens,
// temperature, top_p and stop on every request -- and hid tools from the
// gateway, the one place that refuses it by name.
func TestProxyChat_RequestBodyReachesGatewayIntact(t *testing.T) {
	var seen map[string]json.RawMessage
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seen)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer gw.Close()

	gin.SetMode(gin.TestMode)
	h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000")
	r := gin.New()
	r.POST("/v1/chat/completions", h.ProxyChat)

	reqBody := `{"model":"m","max_tokens":64,"temperature":0.2,"top_p":0.9,"stop":["END"],` +
		`"tools":[{"type":"function","function":{"name":"f"}}],` +
		`"messages":[{"role":"user","name":"ada","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	for field, want := range map[string]string{
		"model":       `"m"`,
		"max_tokens":  `64`,
		"temperature": `0.2`,
		"top_p":       `0.9`,
		"stop":        `["END"]`,
		"tools":       `[{"type":"function","function":{"name":"f"}}]`,
	} {
		if got := string(seen[field]); got != want {
			t.Errorf("gateway saw %s = %s, want %s", field, got, want)
		}
	}

	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(seen["messages"], &messages); err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(messages) != 2 || string(messages[0]["role"]) != `"system"` {
		t.Fatalf("want the system prompt prepended to one user message, got %s", seen["messages"])
	}
	if got := string(messages[1]["name"]); got != `"ada"` {
		t.Errorf("per-message field name = %s, want \"ada\"", got)
	}
	if got := string(messages[1]["content"]); got != `[{"type":"text","text":"hi"}]` {
		t.Errorf("content blocks were rewritten: %s", got)
	}
}

// TestWithSystemPrompt_CallerSystemMessageWins covers the one case where the
// proxy leaves "messages" alone: the caller already leads with a system message.
func TestWithSystemPrompt_CallerSystemMessageWins(t *testing.T) {
	body := `{"model":"m","stream":true,"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}]}`
	modified, stream, err := withSystemPrompt([]byte(body))
	if err != nil {
		t.Fatalf("withSystemPrompt: %v", err)
	}
	if !stream {
		t.Error("stream = false, want true")
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(modified, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "be terse" {
		t.Fatalf("messages = %+v, want the caller's two messages unchanged", got.Messages)
	}

	if _, _, err := withSystemPrompt([]byte(`[]`)); err == nil {
		t.Error("a non-object body must be refused")
	}
}

// TestProxyChat_RetryAfterForwarded: a vendor's rate limit reaches the caller
// with its Retry-After, so an SDK backs off instead of retrying at once.
func TestProxyChat_RetryAfterForwarded(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error","code":"provider_rate_limited"}}`))
	}))
	defer gw.Close()

	gin.SetMode(gin.TestMode)
	h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000")
	r := gin.New()
	r.POST("/v1/chat/completions", h.ProxyChat)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want \"7\"", got)
	}
}
