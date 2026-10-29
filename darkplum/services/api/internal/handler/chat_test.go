package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
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
		{http.StatusUnprocessableEntity, `{"error":{"message":"model needs Bedrock data retention raised","type":"invalid_request_error","code":"model_requires_data_retention","model":"fable-5","route":"smart","account_mode":"none","required_mode":"aws_review"}}`},
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
				h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000", nil)
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
			h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000", nil)
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
	h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000", nil)
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
//
// This is the SESSION path (the org system prompt is prepended); Bearer
// requests are forwarded untouched, see TestProxyChat_Bearer*.
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
	h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000", nil)
	r := gin.New()
	r.POST("/v1/chat/completions", h.ProxyChat)

	reqBody := `{"model":"m","max_tokens":64,"temperature":0.2,"top_p":0.9,"stop":["END"],` +
		`"tools":[{"type":"function","function":{"name":"f"}}],` +
		`"messages":[{"role":"user","name":"ada","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json") // no Authorization: session path
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
	h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000", nil)
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

// TestWithSystemPrompt_RejectsNonObjectAndEmptyMessages pins the two body
// shapes the chaos exercise found: a literal `null` used to panic on a nil
// map (500, empty body), and an empty messages array used to reach the
// gateway as a system-prompt-only conversation.
func TestWithSystemPrompt_RejectsNonObjectAndEmptyMessages(t *testing.T) {
	for _, body := range []string{`null`, `{"model":"m","messages":[]}`, `{"model":"m"}`, `[]`, `"x"`} {
		if _, _, err := withSystemPrompt([]byte(body)); err == nil {
			t.Errorf("body %s: expected an error, got none", body)
		}
	}
	if _, _, err := withSystemPrompt([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("valid body refused: %v", err)
	}
}

// bearerGateway is a gateway stub that records the raw body and Authorization
// header of the last request it saw.
type bearerGateway struct {
	*httptest.Server
	mu   sync.Mutex
	body []byte
	auth string
}

func newBearerGateway(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *bearerGateway {
	t.Helper()
	g := &bearerGateway{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.body, g.auth = b, r.Header.Get("Authorization")
		g.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *bearerGateway) seen() ([]byte, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.body, g.auth
}

func bearerRouter(gwURL string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewChatHandler(gwURL, "dp_test_service_key_000000000000000000000", nil)
	r := gin.New()
	r.POST("/v1/chat/completions", h.ProxyChat)
	return r
}

func okJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func postBearer(r http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dp_user_x")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestProxyChat_BearerBodyForwardedByteForByte: an API-key caller's body
// reaches the gateway exactly as sent. The body is deliberately non-canonical
// in every way a re-marshal would normalise: unsorted keys, whitespace, HTML
// characters, U+2028, a duplicate top-level key, 1.0, non-ASCII; it leads
// with a "developer" message and has no system message.
func TestProxyChat_BearerBodyForwardedByteForByte(t *testing.T) {
	gw := newBearerGateway(t, okJSON)
	r := bearerRouter(gw.URL)

	body := "{ \"temperature\" : 1.0 ,\"model\":\"m\",  \"zeta\":1,\"model\":\"m2\",\n" +
		"\"messages\" : [ {\"role\":\"developer\",\"content\":\"<tag> & x \u2028 \u00e9 caf\u00e9 \u2028\"} ,\n" +
		"  {\"role\":\"user\",\"content\":\"h\u00e9llo <b>&</b>\"} ] }"
	rec := postBearer(r, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got, auth := gw.seen()
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("gateway body differs from what the caller sent\n got: %q\nwant: %q", got, body)
	}
	if bytes.Contains(got, []byte(`"role":"system"`)) {
		t.Errorf("a system message was injected into an API-key request: %s", got)
	}
	if auth != "Bearer dp_user_x" {
		t.Errorf("gateway Authorization = %q, want the caller's own key", auth)
	}
}

// TestProxyChat_BearerSkipsOrgPrompt: the org prompt (built-in default here,
// custom when a database is available) never reaches an API-key request, and
// the same org prompt does reach a session request.
func TestProxyChat_BearerSkipsOrgPrompt(t *testing.T) {
	const custom = "ORG-PROMPT-SENTINEL-7f3a"
	var pool *pgxpool.Pool
	if os.Getenv("TEST_DATABASE_URL") != "" {
		pool = setupTestDB(t)
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO org_settings (key, value, updated_at) VALUES ($1, $2, now())
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, settingSystemPrompt, custom); err != nil {
			t.Fatalf("seed custom prompt: %v", err)
		}
		systemPromptCache.invalidate()
		t.Cleanup(systemPromptCache.invalidate)
	}
	gw := newBearerGateway(t, okJSON)
	gin.SetMode(gin.TestMode)
	h := NewChatHandler(gw.URL, "dp_test_service_key_000000000000000000000", pool)
	r := gin.New()
	r.POST("/v1/chat/completions", h.ProxyChat)

	want := resolvedSystemPrompt(systemPrompt)
	if pool != nil {
		want = custom
	}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if rec := postBearer(r, body); rec.Code != http.StatusOK {
		t.Fatalf("bearer status = %d; %s", rec.Code, rec.Body.String())
	}
	got, _ := gw.seen()
	if string(got) != body || strings.Contains(string(got), want) {
		t.Fatalf("bearer body carries the org prompt or was changed: %s", got)
	}

	// Control: the same handler on the session path does prepend it.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	got, _ = gw.seen()
	if rec.Code != http.StatusOK || !strings.Contains(string(got), `"role":"system"`) || !strings.Contains(string(got), want[:20]) {
		t.Fatalf("session path lost the org prompt (control failed): status %d body %s", rec.Code, got)
	}
}

func sseChunks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	f := w.(http.Flusher)
	for i := 0; i < 3; i++ {
		fmt.Fprintf(w, "data: chunk-%d\n\n", i)
		f.Flush()
		time.Sleep(30 * time.Millisecond)
	}
}

// serveStreamed runs the router on a real server and returns the arrival time
// of each chunk, to tell a per-chunk flush from a buffered response.
func serveStreamed(t *testing.T, r http.Handler, body string, bearer bool) (arrivals []time.Duration, got string) {
	t.Helper()
	srv := httptest.NewServer(r)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if bearer {
		req.Header.Set("Authorization", "Bearer dp_user_x")
	}
	start := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var sb strings.Builder
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			arrivals = append(arrivals, time.Since(start))
			sb.WriteString(line + "\n")
		}
	}
	return arrivals, sb.String()
}

func TestProxyChat_BearerStreamFlushesPerChunk(t *testing.T) {
	gw := newBearerGateway(t, sseChunks)
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	arrivals, got := serveStreamed(t, bearerRouter(gw.URL), body, true)
	if len(arrivals) != 3 || got != "data: chunk-0\ndata: chunk-1\ndata: chunk-2\n" {
		t.Fatalf("chunks = %q (%d)", got, len(arrivals))
	}
	if spread := arrivals[2] - arrivals[0]; spread < 40*time.Millisecond {
		t.Errorf("chunks arrived together (spread %v): the response was buffered, not flushed per chunk", spread)
	}
	if sent, _ := gw.seen(); string(sent) != body {
		t.Errorf("body changed: %s", sent)
	}
}

// An unparseable body yields stream=false from peekStream; a gateway answer
// with Content-Type text/event-stream must still be flushed per chunk.
func TestProxyChat_SSEContentTypeFlushesWithoutStreamFlag(t *testing.T) {
	gw := newBearerGateway(t, sseChunks)
	arrivals, got := serveStreamed(t, bearerRouter(gw.URL), `{not json`, true)
	if len(arrivals) != 3 || got != "data: chunk-0\ndata: chunk-1\ndata: chunk-2\n" {
		t.Fatalf("chunks = %q (%d)", got, len(arrivals))
	}
	if spread := arrivals[2] - arrivals[0]; spread < 40*time.Millisecond {
		t.Errorf("chunks arrived together (spread %v): SSE response was buffered", spread)
	}
}

// null and an empty messages array are the gateway's to refuse for an API-key
// caller: the proxy forwards them unchanged and passes the 400 through.
func TestProxyChat_BearerMalformedBodiesReachGateway(t *testing.T) {
	const refusal = `{"error":{"message":"nope","type":"invalid_request_error","code":"invalid_messages"}}`
	for _, body := range []string{`null`, `{"messages":[]}`} {
		t.Run(body, func(t *testing.T) {
			gw := newBearerGateway(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(refusal))
			})
			rec := postBearer(bearerRouter(gw.URL), body)
			if got, _ := gw.seen(); string(got) != body {
				t.Errorf("gateway saw %q, want %q", got, body)
			}
			if rec.Code != http.StatusBadRequest || rec.Body.String() != refusal {
				t.Errorf("response = %d %s, want the gateway's 400 passed through", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestProxyChat_BearerWithLibraryHeaderRefused(t *testing.T) {
	gw := newBearerGateway(t, okJSON)
	r := bearerRouter(gw.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dp_user_x")
	req.Header.Set(libraryItemsHeader, "00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "library_requires_session") {
		t.Fatalf("response = %d %s, want 400 library_requires_session", rec.Code, rec.Body.String())
	}
	if got, _ := gw.seen(); got != nil {
		t.Errorf("a refused request reached the gateway: %s", got)
	}
}
