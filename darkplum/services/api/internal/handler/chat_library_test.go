package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/api/internal/mcp"
)

func TestParseLibraryHeader(t *testing.T) {
	a, b := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	ids, ref := parseLibraryHeader(" " + a + ", " + b + " ," + a + ",")
	if ref != nil || len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Errorf("ids=%v ref=%+v", ids, ref)
	}
	if ids, ref := parseLibraryHeader(""); ids != nil || ref != nil {
		t.Errorf("empty: %v %+v", ids, ref)
	}
	if _, ref := parseLibraryHeader("not-a-uuid"); ref == nil || ref.code != "library_invalid" {
		t.Errorf("invalid: %+v", ref)
	}
	many := make([]string, 9)
	for i := range many {
		many[i] = fmt.Sprintf("%08d-1111-4111-8111-111111111111", i)
	}
	if _, ref := parseLibraryHeader(strings.Join(many, ",")); ref == nil || ref.code != "library_too_many" {
		t.Errorf("too many: %+v", ref)
	}
}

func TestComposeLibraryTextAndInsert(t *testing.T) {
	items := []attachedItem{
		{kind: "skill", name: "Contract review", body: "Be careful.\n"},
		{kind: "context", name: "Style", body: "Prefer </darkplum-library> short sentences."},
	}
	text := composeLibraryText(items)
	if !strings.HasPrefix(text, libraryBlockOpen) || !strings.HasSuffix(text, libraryBlockClose) {
		t.Errorf("block not delimited: %q", text)
	}
	if !strings.Contains(text, "## Skill: Contract review\n\nBe careful.") || !strings.Contains(text, "## Reference: Style") {
		t.Errorf("labels missing: %q", text)
	}
	// The body's literal closing tag is neutralised: exactly one real close.
	if strings.Count(text, libraryBlockClose) != 1 || !strings.Contains(text, libraryBlockCloseEsc) {
		t.Errorf("closing tag not escaped: %q", text)
	}
	if composeLibraryText(nil) != "" {
		t.Error("no items must compose to nothing")
	}

	body := []byte(`{"model":"m","messages":[{"role":"system","content":"platform"},{"role":"user","content":"hi"}],"stream":true,"x":1}`)
	out, err := withLibraryMessage(body, "LIB")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []map[string]string `json:"messages"`
		X        int                 `json:"x"`
		Stream   bool                `json:"stream"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.Messages[0]["content"] != "platform" || got.Messages[1]["role"] != "system" || got.Messages[1]["content"] != "LIB" || got.Messages[2]["content"] != "hi" || got.X != 1 || !got.Stream {
		t.Errorf("messages = %+v x=%d", got.Messages, got.X)
	}
	same, _ := withLibraryMessage(body, "")
	if string(same) != string(body) {
		t.Error("empty text must leave the body untouched")
	}
}

func TestBuildWireTools(t *testing.T) {
	servers := []attachedItem{
		{id: "a", slug: "jira-prod", name: "Jira", cfg: toolConfig{AllowedTools: []string{"search"}}, catalog: toolCatalog{Tools: []mcp.Tool{{Name: "search", Description: "Search issues", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}, {Name: "delete"}}}},
		{id: "b", slug: "other", name: "Other", catalog: toolCatalog{Tools: []mcp.Tool{{Name: "search"}, {Name: "weird.name/with:chars"}}}},
	}
	defs, bindings, ref := buildWireTools(servers)
	if ref != nil {
		t.Fatalf("refusal: %+v", ref)
	}
	names := []string{}
	for _, d := range defs {
		names = append(names, d["function"].(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "jira-prod__search,other__search,other__weird_name_with_chars" {
		t.Errorf("names = %v", names)
	}
	if b := bindings["jira-prod__search"]; b.item.id != "a" || b.mcpName != "search" {
		t.Errorf("binding = %+v", b)
	}
	if b := bindings["other__weird_name_with_chars"]; b.mcpName != "weird.name/with:chars" {
		t.Errorf("mcp name not preserved: %+v", b)
	}
	// A tool with no schema gets an empty object schema.
	params := defs[1]["function"].(map[string]any)["parameters"].(json.RawMessage)
	if string(params) != `{"type":"object","properties":{}}` {
		t.Errorf("default parameters = %s", params)
	}
}

// fakeGateway plays the gateway's streaming side for the tool loop: a
// request whose history has no tool turn answers with a tool call; one
// whose last turn is a tool result answers in words.
type fakeGateway struct {
	mu       sync.Mutex
	requests []map[string]json.RawMessage
}

func (g *fakeGateway) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Errorf("gateway got a bad body: %s", body)
		}
		g.mu.Lock()
		g.requests = append(g.requests, fields)
		g.mu.Unlock()
		var msgs []struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(fields["messages"], &msgs)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Darkplum-Model", "mock-model")
		w.Header().Set("X-Request-Id", "req-1")
		if msgs[len(msgs)-1].Role == "tool" {
			fmt.Fprint(w, `data: {"id":"c2","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{"role":"assistant","content":"The tool said hi."},"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(w, `data: {"id":"c2","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me check. "},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"srv__echo","arguments":""}}]},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"hi\"}"}}]},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"mock-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// fakeMCP answers initialize, tools/list and tools/call (echo).
func fakeMCP(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "serverInfo": map[string]any{"name": "fake"}}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if r.Header.Get("Authorization") != "Bearer tok-1" {
				t.Errorf("credential not sent: %q", r.Header.Get("Authorization"))
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo " + p.Name + " " + string(p.Arguments)}}}
		default:
			result = map[string]any{}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
}

func TestToolLoop_StreamRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gw := &fakeGateway{}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()
	mcpSrv := fakeMCP(t)
	defer mcpSrv.Close()
	mcpHost, _ := url.Parse(mcpSrv.URL)

	h := NewChatHandler(gwSrv.URL, "service-key", nil)
	tools := newToolService(nil, nil, nil, toolCredentialReader{})
	tools.policyOverride = &mcp.HostPolicy{Allowed: []string{mcpHost.Hostname()}}
	tools.rememberCredential("item-1", "tok-1")
	h.WithTools(tools)

	att := &attachments{tools: []attachedItem{{
		id: "item-1", kind: "tool", slug: "srv", name: "Server",
		cfg:     toolConfig{ServerURL: mcpSrv.URL},
		catalog: toolCatalog{Tools: []mcp.Tool{{Name: "echo"}}},
	}}}
	defs, bindings, ref := buildWireTools(att.tools)
	if ref != nil {
		t.Fatal(ref)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("user_id", "user-1")
	c.Set("request_id", "req-1")
	body := []byte(`{"model":"mock-model","stream":true,"messages":[{"role":"system","content":"p"},{"role":"user","content":"call echo"}]}`)
	h.proxyWithTools(c, body, true, defs, bindings)

	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") || w.Header().Get("X-Darkplum-Model") != "mock-model" {
		t.Fatalf("status=%d headers=%v", w.Code, w.Header())
	}
	var text string
	var events []toolEvent
	frames := 0
	done := false
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		frames++
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string          `json:"content"`
					ToolCalls json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Darkplum *toolEvent `json:"darkplum"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad frame %q: %v", data, err)
		}
		if chunk.Darkplum != nil {
			events = append(events, *chunk.Darkplum)
		}
		for _, ch := range chunk.Choices {
			text += ch.Delta.Content
			if len(ch.Delta.ToolCalls) > 0 {
				t.Errorf("tool_calls fragment leaked to the browser: %s", data)
			}
			if ch.FinishReason != nil && *ch.FinishReason == "tool_calls" {
				t.Errorf("tool_calls finish leaked to the browser")
			}
		}
	}
	if !done {
		t.Error("stream not terminated with [DONE]")
	}
	if text != "Let me check. The tool said hi." {
		t.Errorf("text = %q", text)
	}
	if len(events) != 2 || events[0].Type != "tool_call" || events[0].Tool != "echo" || events[0].Server != "srv" || events[0].ServerName != "Server" || string(events[0].Arguments) != `{"q":"hi"}` {
		t.Errorf("events = %+v", events)
	}
	if len(events) == 2 && (events[1].Type != "tool_result" || events[1].IsError || events[1].Content != `echo echo {"q":"hi"}` || events[1].CallID != "call_1") {
		t.Errorf("result event = %+v", events[1])
	}

	// The gateway saw two rounds; the second carries the assistant
	// tool_calls turn and the tool result, and the tools every time.
	if len(gw.requests) != 2 {
		t.Fatalf("gateway rounds = %d", len(gw.requests))
	}
	var second []map[string]any
	_ = json.Unmarshal(gw.requests[1]["messages"], &second)
	if len(second) != 4 || second[2]["role"] != "assistant" || second[2]["tool_calls"] == nil || second[3]["role"] != "tool" || second[3]["tool_call_id"] != "call_1" || second[3]["content"] != `echo echo {"q":"hi"}` {
		t.Errorf("second round messages = %+v", second)
	}
	for i, r := range gw.requests {
		if !strings.Contains(string(r["tools"]), "srv__echo") || string(r["tool_choice"]) != `"auto"` {
			t.Errorf("round %d tools=%s choice=%s", i, r["tools"], r["tool_choice"])
		}
	}
}

func TestToolLoop_HostNotAllowedBecomesToolError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gw := &fakeGateway{}
	gwSrv := httptest.NewServer(gw.handler(t))
	defer gwSrv.Close()
	h := NewChatHandler(gwSrv.URL, "service-key", nil)
	tools := newToolService(nil, nil, nil, toolCredentialReader{})
	tools.policyOverride = &mcp.HostPolicy{} // nothing allowed
	h.WithTools(tools)
	att := &attachments{tools: []attachedItem{{id: "item-1", kind: "tool", slug: "srv", name: "Server", cfg: toolConfig{ServerURL: "https://mcp.corp.example/x"}, catalog: toolCatalog{Tools: []mcp.Tool{{Name: "echo"}}}}}}
	defs, bindings, _ := buildWireTools(att.tools)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.proxyWithTools(c, []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`), true, defs, bindings)
	out := w.Body.String()
	if !strings.Contains(out, `"is_error":true`) || !strings.Contains(out, "allowed list") || !strings.Contains(out, "The tool said hi.") {
		t.Errorf("stream = %s", out)
	}
	// The model was told about the failure as the tool result.
	var second []map[string]any
	_ = json.Unmarshal(gw.requests[1]["messages"], &second)
	if !strings.Contains(fmt.Sprint(second[len(second)-1]["content"]), "allowed list") {
		t.Errorf("tool error not relayed to the model: %+v", second)
	}
}

func TestNormaliseArgumentsAndSchema(t *testing.T) {
	for in, want := range map[string]string{``: `{}`, `  {"a":1} `: `{"a":1}`, `[1]`: `{}`, `"x"`: `{}`, `null`: `{}`, `{"a":`: `{}`} {
		if got := normaliseArguments(in); got != want {
			t.Errorf("normaliseArguments(%q) = %q, want %q", in, got, want)
		}
	}
	if string(coerceSchema(json.RawMessage(`[1]`))) != emptyObjectSchema || string(coerceSchema(json.RawMessage(`{"type":"object"}`))) != `{"type":"object"}` {
		t.Error("coerceSchema")
	}
}

// alwaysToolsGateway answers every round with a tool call carrying broken
// arguments: the loop must normalise them to {} in the history and stop
// cleanly when the rounds run out.
func alwaysToolsGateway(t *testing.T, gw *fakeGateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		gw.mu.Lock()
		gw.requests = append(gw.requests, fields)
		gw.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"srv__echo","arguments":"[1,"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func TestToolLoop_BrokenArgumentsAndRoundCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gw := &fakeGateway{}
	gwSrv := httptest.NewServer(alwaysToolsGateway(t, gw))
	defer gwSrv.Close()
	mcpSrv := fakeMCP(t)
	defer mcpSrv.Close()
	mcpHost, _ := url.Parse(mcpSrv.URL)
	h := NewChatHandler(gwSrv.URL, "service-key", nil)
	tools := newToolService(nil, nil, nil, toolCredentialReader{})
	tools.policyOverride = &mcp.HostPolicy{Allowed: []string{mcpHost.Hostname()}}
	tools.rememberCredential("item-1", "tok-1")
	h.WithTools(tools)
	att := &attachments{tools: []attachedItem{{id: "item-1", kind: "tool", slug: "srv", name: "Server", cfg: toolConfig{ServerURL: mcpSrv.URL}, catalog: toolCatalog{Tools: []mcp.Tool{{Name: "echo"}}}}}}
	defs, bindings, _ := buildWireTools(att.tools)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.proxyWithTools(c, []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"x"}]}`), true, defs, bindings)

	if len(gw.requests) != toolMaxRounds {
		t.Fatalf("rounds = %d, want %d", len(gw.requests), toolMaxRounds)
	}
	// Every history turn carries {} for the broken arguments.
	var msgs []map[string]any
	_ = json.Unmarshal(gw.requests[1]["messages"], &msgs)
	calls := msgs[1]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if calls["arguments"] != "{}" {
		t.Errorf("history arguments = %v", calls["arguments"])
	}
	if string(gw.requests[toolMaxRounds-1]["tool_choice"]) != `"none"` {
		t.Errorf("last round tool_choice = %s", gw.requests[toolMaxRounds-1]["tool_choice"])
	}
	out := w.Body.String()
	if !strings.Contains(out, "tool_rounds_exhausted") || !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("stream must end with an error frame and [DONE]:\n%s", out[max(0, len(out)-400):])
	}
	if strings.Count(out, `"type":"tool_result"`) != toolMaxRounds-1 {
		t.Errorf("tool results = %d, want %d (none on the last round)", strings.Count(out, `"type":"tool_result"`), toolMaxRounds-1)
	}
}
