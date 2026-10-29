package handler

// Dropped parameters (GitHub #3, Phase B): the catch-all rule for unknown
// keys in both modes, the header decided before the first streamed byte, the
// `user` that could not be pseudonymised, the first-drop audit notification,
// and that a dropped key lands in BOTH the response header and
// inference_events.dropped_params.

import (
	"context"
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

	"github.com/scalecraft-dev/darkplum/gateway/internal/chatparams"
	"github.com/scalecraft-dev/darkplum/gateway/internal/middleware"
	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/recorder"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

func anthropicBackend() backendPersona {
	for _, b := range allBackends() {
		if b.name == "anthropic" {
			return b
		}
	}
	panic("no anthropic persona")
}

func TestUnknownKeys_DroppedAndRecordedInDefaultModeRefusedInStrict(t *testing.T) {
	for _, be := range allBackends() {
		t.Run(be.name, func(t *testing.T) {
			h, prov := contractHandler(t, be, false)
			body := baseChatBody("m")
			body["x_vendor_flag"] = true
			body["best_of"] = 3
			w, c := doRawChat(t, h, body)
			if w.Code != http.StatusOK {
				t.Fatalf("lenient: %d %s", w.Code, w.Body.String())
			}
			got := entriesOf(w)
			if !contains(got, "x_vendor_flag") || !contains(got, "best_of") {
				t.Errorf("header = %v, want both unknown keys named", got)
			}
			if v, _ := c.Get(middleware.ContextDroppedParams); !contains(asStrings(v), "best_of") {
				t.Errorf("usage-log entries = %v", v)
			}
			if strings.Contains(marshalReq(prov.lastReq), "x_vendor_flag") {
				t.Error("an unknown key reached the provider request")
			}

			hs, provS := contractHandler(t, be, true)
			w, _ = doRawChat(t, hs, body)
			// First unknown key in sorted order is named.
			assertParamRefused(t, w.Code, w.Body.Bytes(), &provS.capturingProvider, "best_of")
		})
	}
}

func TestUnknownKeys_NullIsSilentInBothModes(t *testing.T) {
	for _, strict := range []bool{false, true} {
		h, _ := contractHandler(t, anthropicBackend(), strict)
		body := baseChatBody("m")
		body["x_vendor_flag"] = nil
		body["stream_options"] = nil
		body["response_format"] = map[string]any{"type": "text", "weird": nil}
		w, _ := doRawChat(t, h, body)
		if w.Code != http.StatusOK || len(entriesOf(w)) != 0 {
			t.Errorf("strict=%v: %d %v", strict, w.Code, entriesOf(w))
		}
	}
}

func TestUnknownKeys_NestedKeysFollowTheSameRule(t *testing.T) {
	cases := map[string]map[string]any{
		"stream_options.future_flag": {"stream": true, "stream_options": map[string]any{"include_usage": true, "future_flag": 1}},
		"response_format.future_key": {"response_format": map[string]any{"type": "text", "future_key": "x"}},
	}
	for want, extra := range cases {
		for _, strict := range []bool{false, true} {
			h, prov := contractHandler(t, anthropicBackend(), strict)
			body := baseChatBody("m")
			for k, v := range extra {
				body[k] = v
			}
			w, _ := doRawChat(t, h, body)
			if strict {
				assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, want)
				continue
			}
			if w.Code != http.StatusOK || !contains(entriesOf(w), want) {
				t.Errorf("%s lenient: %d %v", want, w.Code, entriesOf(w))
			}
		}
	}
}

func TestUnknownKeys_NamesAreSanitisedAndBounded(t *testing.T) {
	h, _ := contractHandler(t, anthropicBackend(), false)
	body := baseChatBody("m")
	body["evil\r\nX-Injected: 1"] = 1
	for i := 0; i < 30; i++ {
		body[fmt.Sprintf("extra_%02d", i)] = i
	}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	hv := w.Header().Get(droppedParamsHeader)
	if strings.ContainsAny(hv, "\r\n") || w.Header().Get("X-Injected") != "" {
		t.Fatalf("header injection: %q", hv)
	}
	entries := entriesOf(w)
	if len(entries) != maxUnknownKeysRecorded+1 || !contains(entries, "unknown_keys=more_11") {
		t.Errorf("want %d named + a count of the rest, got %d entries: %v", maxUnknownKeysRecorded, len(entries), entries)
	}
}

// Go would match "Temperature" to temperature; the gateway must not forward
// it AND call it unknown, so it is neither: it is dropped and recorded.
func TestUnknownKeys_CaseVariantOfAKnownKeyIsNotHonoured(t *testing.T) {
	h, prov := contractHandler(t, allBackends()[0], false)
	body := baseChatBody("m")
	body["Temperature"] = 0.9
	body["SEED"] = 7
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if prov.lastReq.Temperature != nil || prov.lastReq.Seed != nil {
		t.Errorf("a mis-cased key was honoured: %s", marshalReq(prov.lastReq))
	}
	if got := entriesOf(w); !contains(got, "Temperature") || !contains(got, "SEED") {
		t.Errorf("header = %v", got)
	}
}

func TestParamsMode_LenientValueEqualsDefault(t *testing.T) {
	h, _ := contractHandler(t, anthropicBackend(), false)
	h.policy = &fakePolicyStore{snap: policy.Snapshot{Enabled: map[string]bool{"m": true}, Residency: policy.ResidencyAny, ParamsMode: "lenient"}, loaded: true}
	body := baseChatBody("m")
	body["seed"] = 1
	if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK {
		t.Fatalf("explicit lenient refused: %d", w.Code)
	}
}

// streamingProvider writes a chunk, as a real backend does, so the response
// is committed.
type streamingProvider struct{ reasoningProvider }

func (p *streamingProvider) Stream(_ context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	p.lastReq = req
	p.calls++
	_, _ = w.Write([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	return &provider.StreamStats{Model: req.Model, FinishReason: "stop"}, nil
}

func TestDroppedHeader_IsOnTheWireBeforeTheFirstStreamedByte(t *testing.T) {
	be := anthropicBackend()
	caps := be.caps
	sp := &streamingProvider{reasoningProvider{toolCapturingProvider: toolCapturingProvider{capturingProvider{name: "a", kind: be.kind, caps: &caps}},
		plan: func(string, string, int) provider.ReasoningPlan { return provider.ReasoningPlan{} }}}
	h := newTestHandler(fakeProviders{"a": sp},
		map[string]routing.Route{"m": {ProviderKey: "a", Kind: be.kind, Class: be.class}},
		policy.Snapshot{Enabled: map[string]bool{"m": true}, Residency: policy.ResidencyAny}, true)
	body := baseChatBody("m")
	body["stream"] = true
	body["seed"] = 5
	w, _ := doRawChat(t, h, body)
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("stream did not run: %s", w.Body.String())
	}
	// Result() snapshots the headers as they were when the first byte was written.
	if got := w.Result().Header.Get(droppedParamsHeader); got != "seed" {
		t.Errorf("header at first byte = %q, want seed", got)
	}
}

func TestUser_WithoutSecretIsDroppedAndRecordedNeverSentRaw(t *testing.T) {
	for _, be := range allBackends() {
		if be.name == "vllm" {
			continue
		}
		t.Run(be.name, func(t *testing.T) {
			h, prov := contractHandler(t, be, false)
			h.SetUserPseudonymizer(nil)
			body := baseChatBody("m")
			body["user"] = "alice@example.com"
			w, c := doRawChat(t, h, body)
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if prov.lastReq.User != "" || strings.Contains(marshalReq(prov.lastReq), "alice") {
				t.Errorf("raw user reached the provider: %q", prov.lastReq.User)
			}
			if !contains(entriesOf(w), "user") {
				t.Errorf("header = %v, want user", entriesOf(w))
			}
			if v, _ := c.Get(middleware.ContextDroppedParams); !contains(asStrings(v), "user") {
				t.Errorf("usage-log entries = %v", v)
			}

			hs, provS := contractHandler(t, be, true)
			hs.SetUserPseudonymizer(nil)
			w, _ = doRawChat(t, hs, body)
			assertParamRefused(t, w.Code, w.Body.Bytes(), &provS.capturingProvider, "user")
		})
	}
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeNotifier) NoteDrops(kind, id, backend, model string, params []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s|%s|%s|%s|%s", kind, id, backend, model, strings.Join(params, ",")))
}

func TestDropNotifier_KeyedByKeyOrByUserForSessionTraffic(t *testing.T) {
	const userID = "11111111-1111-1111-1111-111111111111"
	const keyID = "22222222-2222-2222-2222-222222222222"
	run := func(keyType, headerUser string) string {
		h, _ := contractHandler(t, anthropicBackend(), false)
		n := &fakeNotifier{}
		h.SetDropNotifier(n)
		body := baseChatBody("m")
		body["seed"] = 1
		body["reasoning_effort"] = "low"
		body["max_tokens"] = 50
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = buildRawChatRequest(t, body)
		if headerUser != "" {
			c.Request.Header.Set("X-Darkplum-User-Id", headerUser)
		}
		c.Set(middleware.ContextKeyID, keyID)
		c.Set(middleware.ContextKeyType, keyType)
		h.HandleChatCompletions(c)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if len(n.calls) != 1 {
			t.Fatalf("notifier calls = %v", n.calls)
		}
		return n.calls[0]
	}
	// A user key: keyed by the key. Only dropped names, not the max_tokens raise.
	if got := run(middleware.KeyTypeUser, ""); got != "api_key|"+keyID+"|anthropic|m|seed" {
		t.Errorf("user key: %s", got)
	}
	// Webchat session traffic arrives under the service key with the user id.
	if got := run(middleware.KeyTypeService, userID); got != "user|"+userID+"|anthropic|m|seed" {
		t.Errorf("session traffic: %s", got)
	}
	// A user key cannot attribute its traffic to someone else via the header.
	if got := run(middleware.KeyTypeUser, userID); !strings.HasPrefix(got, "api_key|"+keyID) {
		t.Errorf("user key honoured X-Darkplum-User-Id: %s", got)
	}
	// Not a UUID: fall back to the service key itself.
	if got := run(middleware.KeyTypeService, "not-a-uuid"); !strings.HasPrefix(got, "service_key|"+keyID) {
		t.Errorf("bad header: %s", got)
	}
}

// ---------------------------------------------------------------------------
// A dropped key is in the header AND in inference_events.dropped_params:
// handler -> Recording middleware -> recorder -> Postgres.
// ---------------------------------------------------------------------------

func TestDroppedParams_HeaderAndDatabaseColumnAgree(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("dropped_e2e_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	cfg, _ := pgxpool.ParseConfig(url)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TABLE inference_events (
		id UUID NOT NULL DEFAULT gen_random_uuid(), request_id TEXT NOT NULL, api_key_id UUID, user_id UUID,
		model TEXT NOT NULL, requested_model TEXT, status_code INT NOT NULL, latency_ms INT NOT NULL,
		prompt_tokens INT, completion_tokens INT, stream BOOLEAN NOT NULL DEFAULT false, ip_address INET,
		provider TEXT, region TEXT, denial_code TEXT, purpose TEXT NOT NULL DEFAULT 'inference',
		route_name TEXT, router_reason TEXT, router_latency_ms INT, backend_class TEXT,
		cache_read_tokens INT, cache_creation_tokens INT, dropped_params TEXT[],
		created_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}

	rec := recorder.New(pool)
	h, _ := contractHandler(t, anthropicBackend(), false)
	r := gin.New()
	r.Use(middleware.Recording(rec))
	r.POST("/v1/chat/completions", h.HandleChatCompletions)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7,"brand_new_key":1,"reasoning_effort":"low","max_tokens":50}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Request-Id", "req-dropped-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	header := entriesOf(w)
	for _, want := range []string{"seed", "brand_new_key", "max_tokens=raised"} {
		if !contains(header, want) {
			t.Errorf("header %v missing %s", header, want)
		}
	}

	var col []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `SELECT dropped_params FROM inference_events WHERE request_id = 'req-dropped-1'`).Scan(&col); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if strings.Join(col, ",") != strings.Join(header, ",") {
		t.Errorf("dropped_params column = %v, header = %v; they must be the same entries", col, header)
	}
	for _, e := range col {
		if strings.Contains(e, "7") && e != "max_tokens=raised" {
			t.Errorf("entry %q looks like it carries a value", e)
		}
	}
}

// Every field the request struct decodes is a registry parameter: a field
// added to chatCompletionRequest without a row would be forwarded (or acted
// on) while the contract called it unknown.
func TestRequestStructFieldsAreAllRegistryParameters(t *testing.T) {
	for name := range requestKeys {
		if !chatparams.TopLevel(name) {
			t.Errorf("chatCompletionRequest decodes %q but the registry has no row for it", name)
		}
	}
}

// A case variant with a value of the wrong type must be an unknown key, not a
// typed-decode 400.
func TestUnknownKeys_CaseVariantWithWrongTypeIsUnknownNotDecodeError(t *testing.T) {
	h, _ := contractHandler(t, allBackends()[0], false)
	body := baseChatBody("m")
	body["Temperature"] = "hot"
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK || !contains(entriesOf(w), "Temperature") {
		t.Fatalf("lenient: %d %v %s", w.Code, entriesOf(w), w.Body.String())
	}
	hs, prov := contractHandler(t, allBackends()[0], true)
	w, _ = doRawChat(t, hs, body)
	assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, "Temperature")
}
