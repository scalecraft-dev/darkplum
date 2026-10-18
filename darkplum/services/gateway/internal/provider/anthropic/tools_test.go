package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

func call(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: args}}
}

// wireBody builds the request through the same path Complete uses and returns
// the JSON the SDK would put on the wire.
func wireBody(t *testing.T, req provider.ChatRequest) map[string]any {
	t.Helper()
	norm, err := normalizeMessages(req.Messages)
	if err != nil {
		t.Fatalf("normalizeMessages: %v", err)
	}
	raw, err := json.Marshal(buildParams(req, norm, 100, true))
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	return body
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test json %q: %v", s, err)
	}
	return v
}

func TestSupportsTools(t *testing.T) {
	if !provider.SupportsTools(New(&fakeCreds{}, Config{})) {
		t.Fatal("anthropic provider must be tool capable")
	}
}

func TestToolsTranslation(t *testing.T) {
	weather := provider.Tool{Type: "function", Function: provider.ToolFunction{
		Name:        "get_weather",
		Description: "Look up weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
	}}
	noArgs := provider.Tool{Type: "function", Function: provider.ToolFunction{Name: "ping"}}
	nullArgs := provider.Tool{Type: "function", Function: provider.ToolFunction{Name: "pong", Parameters: json.RawMessage("null")}}

	cases := []struct {
		name       string
		tools      []provider.Tool
		choice     string
		wantTools  string // JSON array, "" = tools omitted
		wantChoice string // JSON, "" = tool_choice omitted
	}{
		{
			name:       "no tool_choice omits the field",
			tools:      []provider.Tool{weather},
			wantTools:  `[{"name":"get_weather","description":"Look up weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}]`,
			wantChoice: "",
		},
		{
			name:       "auto",
			tools:      []provider.Tool{noArgs},
			choice:     `"auto"`,
			wantTools:  `[{"name":"ping","input_schema":{"type":"object","properties":{}}}]`,
			wantChoice: `{"type":"auto"}`,
		},
		{
			name:       "required maps to any, null parameters get empty object schema",
			tools:      []provider.Tool{nullArgs},
			choice:     `"required"`,
			wantTools:  `[{"name":"pong","input_schema":{"type":"object","properties":{}}}]`,
			wantChoice: `{"type":"any"}`,
		},
		{
			name:       "named function maps to tool",
			tools:      []provider.Tool{weather, noArgs},
			choice:     `{"type":"function","function":{"name":"ping"}}`,
			wantTools:  `[{"name":"get_weather","description":"Look up weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}},{"name":"ping","input_schema":{"type":"object","properties":{}}}]`,
			wantChoice: `{"type":"tool","name":"ping"}`,
		},
		{
			name:       "none keeps the definitions and renders type none",
			tools:      []provider.Tool{weather},
			choice:     `"none"`,
			wantTools:  `[{"name":"get_weather","description":"Look up weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}}]`,
			wantChoice: `{"type":"none"}`,
		},
		{
			name: "no tools, nothing sent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := basicRequest("claude-haiku-4-5")
			req.Tools = tc.tools
			if tc.choice != "" {
				req.ToolChoice = json.RawMessage(tc.choice)
			}
			body := wireBody(t, req)

			if tc.wantTools == "" {
				if _, ok := body["tools"]; ok {
					t.Errorf("tools present, want omitted: %v", body["tools"])
				}
			} else if got, want := body["tools"], mustJSON(t, tc.wantTools); !reflect.DeepEqual(got, want) {
				t.Errorf("tools = %v, want %v", got, want)
			}
			if tc.wantChoice == "" {
				if _, ok := body["tool_choice"]; ok {
					t.Errorf("tool_choice present, want omitted: %v", body["tool_choice"])
				}
			} else if got, want := body["tool_choice"], mustJSON(t, tc.wantChoice); !reflect.DeepEqual(got, want) {
				t.Errorf("tool_choice = %v, want %v", got, want)
			}
		})
	}
}

func TestToolHistoryTranslation(t *testing.T) {
	cases := []struct {
		name     string
		messages []provider.Message
		want     string // JSON array of Anthropic messages
	}{
		{
			name: "assistant tool_calls with text and two calls",
			messages: []provider.Message{
				msg("user", "weather?"),
				{Role: "assistant", Content: "Checking.", ToolCalls: []provider.ToolCall{
					call("call_1", "get_weather", `{"city":"Paris"}`),
					call("call_2", "ping", ``),
				}},
				{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
				{Role: "tool", ToolCallID: "call_2", Content: "pong"},
			},
			want: `[
{"role":"user","content":[{"type":"text","text":"weather?"}]},
{"role":"assistant","content":[
  {"type":"text","text":"Checking."},
  {"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}},
  {"type":"tool_use","id":"call_2","name":"ping","input":{}}]},
{"role":"user","content":[
  {"type":"tool_result","is_error":false,"tool_use_id":"call_1","content":[{"type":"text","text":"sunny"}]},
  {"type":"tool_result","is_error":false,"tool_use_id":"call_2","content":[{"type":"text","text":"pong"}]}]}
]`,
		},
		{
			name: "empty-content tool_calls turn is kept, single result then user text merge with result first",
			messages: []provider.Message{
				msg("user", "weather?"),
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("call_1", "get_weather", `{"city":"Paris"}`)}},
				{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
				msg("user", "and tomorrow?"),
			},
			want: `[
{"role":"user","content":[{"type":"text","text":"weather?"}]},
{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}}]},
{"role":"user","content":[
  {"type":"tool_result","is_error":false,"tool_use_id":"call_1","content":[{"type":"text","text":"sunny"}]},
  {"type":"text","text":"and tomorrow?"}]}
]`,
		},
		{
			name: "user text before a tool result is moved after it",
			messages: []provider.Message{
				msg("user", "weather?"),
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("call_1", "ping", `{}`)}},
				msg("user", "hurry"),
				{Role: "tool", ToolCallID: "call_1", Content: "pong"},
			},
			want: `[
{"role":"user","content":[{"type":"text","text":"weather?"}]},
{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"ping","input":{}}]},
{"role":"user","content":[
  {"type":"tool_result","is_error":false,"tool_use_id":"call_1","content":[{"type":"text","text":"pong"}]},
  {"type":"text","text":"hurry"}]}
]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := basicRequest("claude-haiku-4-5")
			req.Messages = tc.messages
			body := wireBody(t, req)
			if got, want := body["messages"], mustJSON(t, tc.want); !reflect.DeepEqual(got, want) {
				gj, _ := json.MarshalIndent(got, "", " ")
				t.Errorf("messages =\n%s\nwant\n%s", gj, tc.want)
			}
		})
	}
}

func TestNormalizeMessages_ToolTurns(t *testing.T) {
	t.Run("alternation holds with tool turns", func(t *testing.T) {
		got, err := normalizeMessages([]provider.Message{
			msg("user", "a"),
			{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "f", `{}`)}},
			{Role: "tool", ToolCallID: "c1", Content: "r"},
			{Role: "assistant", Content: "done"},
			msg("user", "b"),
		})
		if err != nil {
			t.Fatal(err)
		}
		var roles []string
		for _, tr := range got.Turns {
			roles = append(roles, tr.Role)
		}
		if want := []string{"user", "assistant", "user", "assistant", "user"}; !reflect.DeepEqual(roles, want) {
			t.Errorf("roles = %v, want %v", roles, want)
		}
	})

	t.Run("conversation ending in a tool result is valid", func(t *testing.T) {
		if _, err := normalizeMessages([]provider.Message{
			msg("user", "a"),
			{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "f", `{}`)}},
			{Role: "tool", ToolCallID: "c1", Content: "r"},
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("trailing assistant tool_calls turn is still rejected", func(t *testing.T) {
		_, err := normalizeMessages([]provider.Message{
			msg("user", "a"),
			{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "f", `{}`)}},
		})
		if err == nil || !strings.Contains(err.Error(), "must not end with an assistant") {
			t.Fatalf("err = %v", err)
		}
	})

	for _, args := range []string{`{"city":`, `[1,2]`, `"str"`, `null`, `42`} {
		t.Run("malformed arguments "+args, func(t *testing.T) {
			_, err := normalizeMessages([]provider.Message{
				msg("user", "a"),
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "f", args)}},
				{Role: "tool", ToolCallID: "c1", Content: "r"},
			})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want *APIError, got %T: %v", err, err)
			}
			if apiErr.HTTPStatus() != 400 {
				t.Errorf("status = %d, want 400", apiErr.HTTPStatus())
			}
			if !strings.Contains(err.Error(), "messages[1].tool_calls[0].function.arguments") {
				t.Errorf("error does not locate the bad argument: %v", err)
			}
		})
	}
}

func TestComplete_MalformedToolArgumentsNeverReachVendor(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
	req := basicRequest("claude-haiku-4-5")
	req.Messages = []provider.Message{
		msg("user", "a"),
		{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "f", `{bad`)}},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
	}
	for name, run := range map[string]func() error{
		"complete": func() error { _, err := p.Complete(context.Background(), req); return err },
		"stream":   func() error { _, err := p.Stream(context.Background(), req, io.Discard); return err },
	} {
		var apiErr *APIError
		if err := run(); !errors.As(err, &apiErr) || apiErr.HTTPStatus() != 400 {
			t.Errorf("%s: want 400 APIError, got %v", name, err)
		}
	}
	if n := srv.callCount(); n != 0 {
		t.Errorf("vendor called %d times, want 0", n)
	}
}

func TestComplete_ToolUseResponse(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_t1","type":"message","role":"assistant","model":"claude-haiku-4-5",
"content":[
 {"type":"text","text":"Let me check."},
 {"type":"tool_use","id":"toolu_1","name":"get_weather","input":{ "city": "Paris" }},
 {"type":"tool_use","id":"toolu_2","name":"ping","input":{}}],
"stop_reason":"tool_use","stop_sequence":null,
"usage":{"input_tokens":12,"output_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
	req := basicRequest("claude-haiku-4-5")
	req.Tools = []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "get_weather"}}}

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if resp.Content != "Let me check." {
		t.Errorf("Content = %q", resp.Content)
	}
	want := []provider.ToolCall{
		call("toolu_1", "get_weather", `{"city":"Paris"}`),
		call("toolu_2", "ping", `{}`),
	}
	if !reflect.DeepEqual(resp.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v, want %+v", resp.ToolCalls, want)
	}
	if got := srv.requestAt(0).body["tools"]; got == nil {
		t.Error("tools were not sent upstream")
	}
}

func TestMapFinishReason_ToolUse(t *testing.T) {
	if got := mapFinishReason("tool_use"); got != "tool_calls" {
		t.Errorf("got %q", got)
	}
}

// sseEvents writes raw Anthropic SSE events.
func sseEvents(w http.ResponseWriter, events ...map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		b, _ := json.Marshal(e)
		_, _ = io.WriteString(w, "event: "+e["type"].(string)+"\ndata: "+string(b)+"\n\n")
	}
}

func evStart() map[string]any {
	return map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_s1", "type": "message", "role": "assistant", "model": "claude-haiku-4-5",
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 9, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	}}
}

func evBlock(i int, block map[string]any) map[string]any {
	return map[string]any{"type": "content_block_start", "index": i, "content_block": block}
}

func evDelta(i int, delta map[string]any) map[string]any {
	return map[string]any{"type": "content_block_delta", "index": i, "delta": delta}
}

func evStop(i int) map[string]any { return map[string]any{"type": "content_block_stop", "index": i} }

func evEnd(reason string) []map[string]any {
	return []map[string]any{
		{"type": "message_delta", "delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 21}},
		{"type": "message_stop"},
	}
}

// sseFragments returns the delta object and finish_reason of each gateway SSE
// data line, with the (time-dependent) created field dropped.
func gatewayChunks(t *testing.T, out string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, line := range strings.Split(out, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("decode %q: %v", payload, err)
		}
		choice := c["choices"].([]any)[0].(map[string]any)
		chunks = append(chunks, map[string]any{"delta": choice["delta"], "finish_reason": choice["finish_reason"]})
	}
	return chunks
}

func TestStream_ToolCalls(t *testing.T) {
	tool := func(id, name string) map[string]any {
		return map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}
	}
	partial := func(s string) map[string]any { return map[string]any{"type": "input_json_delta", "partial_json": s} }
	text := func(s string) map[string]any { return map[string]any{"type": "text_delta", "text": s} }

	cases := []struct {
		name   string
		events []map[string]any
		want   []string // JSON of {"delta":..,"finish_reason":..} per gateway chunk
	}{
		{
			name: "text then one tool call",
			events: append([]map[string]any{
				evStart(),
				evBlock(0, map[string]any{"type": "text", "text": ""}),
				evDelta(0, text("Checking.")),
				evStop(0),
				evBlock(1, tool("toolu_1", "get_weather")),
				evDelta(1, partial("")),
				evDelta(1, partial(`{"city":`)),
				evDelta(1, partial(`"Paris"}`)),
				evStop(1),
			}, evEnd("tool_use")...),
			want: []string{
				`{"delta":{"role":"assistant","content":"Checking."},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}`,
				`{"delta":{},"finish_reason":"tool_calls"}`,
			},
		},
		{
			name: "two parallel tool calls use tool indexes, not block indexes",
			events: append([]map[string]any{
				evStart(),
				evBlock(0, map[string]any{"type": "text", "text": ""}),
				evDelta(0, text("Both.")),
				evStop(0),
				evBlock(1, tool("toolu_1", "get_weather")),
				evDelta(1, partial(`{"city":"Paris"}`)),
				evStop(1),
				evBlock(2, tool("toolu_2", "ping")),
				evDelta(2, partial(`{"host":`)),
				evDelta(2, partial(`"a"}`)),
				evStop(2),
			}, evEnd("tool_use")...),
			want: []string{
				`{"delta":{"role":"assistant","content":"Both."},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":1,"id":"toolu_2","type":"function","function":{"name":"ping","arguments":""}}]},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"host\":"}}]},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"a\"}"}}]},"finish_reason":null}`,
				`{"delta":{},"finish_reason":"tool_calls"}`,
			},
		},
		{
			name: "tool with no argument stream gets empty object arguments",
			events: append([]map[string]any{
				evStart(),
				evBlock(0, tool("toolu_9", "ping")),
				evStop(0),
			}, evEnd("tool_use")...),
			want: []string{
				`{"delta":{"tool_calls":[{"index":0,"id":"toolu_9","type":"function","function":{"name":"ping","arguments":""}}]},"finish_reason":null}`,
				`{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}`,
				`{"delta":{},"finish_reason":"tool_calls"}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeAnthropicServer(t)
			srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
				func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { sseEvents(w, tc.events...) },
			}
			p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
			req := basicRequest("claude-haiku-4-5")
			req.Tools = []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "get_weather"}}}

			var buf bytes.Buffer
			stats, err := p.Stream(context.Background(), req, &buf)
			if err != nil {
				t.Fatal(err)
			}
			if stats.FinishReason != "tool_calls" {
				t.Errorf("stats.FinishReason = %q, want tool_calls", stats.FinishReason)
			}
			if stats.PromptTokens != 9 || stats.CompletionTokens != 21 {
				t.Errorf("tokens = %d/%d, want 9/21", stats.PromptTokens, stats.CompletionTokens)
			}
			if !strings.HasSuffix(buf.String(), provider.StreamDoneSSE) {
				t.Errorf("missing DONE terminator: %q", buf.String())
			}
			got := gatewayChunks(t, buf.String())
			if len(got) != len(tc.want) {
				gj, _ := json.Marshal(got)
				t.Fatalf("got %d chunks, want %d: %s", len(got), len(tc.want), gj)
			}
			for i := range got {
				if want := mustJSON(t, tc.want[i]); !reflect.DeepEqual(got[i], want) {
					gj, _ := json.Marshal(got[i])
					t.Errorf("chunk %d = %s, want %s", i, gj, tc.want[i])
				}
			}
		})
	}
}

func TestToolChoiceNone_WithToolHistoryKeepsDefinitions(t *testing.T) {
	req := basicRequest("claude-haiku-4-5")
	req.Messages = []provider.Message{
		msg("user", "weather?"),
		{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "get_weather", `{"city":"Paris"}`)}},
		{Role: "tool", ToolCallID: "c1", Content: "sunny"},
	}
	req.Tools = []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "get_weather"}}}
	req.ToolChoice = json.RawMessage(`"none"`)
	body := wireBody(t, req)
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tools = %v", body["tools"])
	}
	if !reflect.DeepEqual(body["tool_choice"], mustJSON(t, `{"type":"none"}`)) {
		t.Errorf("tool_choice = %v", body["tool_choice"])
	}
	if len(body["messages"].([]any)) != 3 {
		t.Errorf("history not rendered: %v", body["messages"])
	}
}

func TestEmptyToolResultGetsPlaceholder(t *testing.T) {
	for _, content := range []string{"", "  \n"} {
		req := basicRequest("claude-haiku-4-5")
		req.Messages = []provider.Message{
			msg("user", "go"),
			{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "ping", `{}`)}},
			{Role: "tool", ToolCallID: "c1", Content: content},
		}
		body := wireBody(t, req)
		msgs := body["messages"].([]any)
		blk := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
		want := mustJSON(t, `[{"type":"text","text":"(empty result)"}]`)
		if !reflect.DeepEqual(blk["content"], want) {
			t.Errorf("content %q: tool_result content = %v", content, blk["content"])
		}
	}
}
