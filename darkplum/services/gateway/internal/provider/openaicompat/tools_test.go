package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

func TestSupportsTools(t *testing.T) {
	p := newTestProvider(t, "http://127.0.0.1:1")
	if !provider.SupportsTools(p) {
		t.Fatal("openaicompat must report tool support")
	}
}

// captureServer records the request body and answers with respond.
func captureServer(t *testing.T, got *map[string]any, respond func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, got); err != nil {
			t.Errorf("bad request json: %v", err)
		}
		respond(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func weatherTool() provider.Tool {
	return provider.Tool{Type: "function", Function: provider.ToolFunction{
		Name: "get_weather", Description: "weather",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}
}

func TestBuildRequestBody_Tools_Table(t *testing.T) {
	call := provider.ToolCall{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Oslo"}`}}
	tests := []struct {
		name  string
		req   provider.ChatRequest
		check func(t *testing.T, body map[string]any)
	}{
		{
			name: "no tools leaves tool fields absent",
			req:  testRequest(),
			check: func(t *testing.T, b map[string]any) {
				for _, k := range []string{"tools", "tool_choice"} {
					if _, ok := b[k]; ok {
						t.Errorf("%s must be absent", k)
					}
				}
			},
		},
		{
			name: "tools and string tool_choice",
			req: provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}},
				Tools: []provider.Tool{weatherTool()}, ToolChoice: json.RawMessage(`"required"`)},
			check: func(t *testing.T, b map[string]any) {
				tools := b["tools"].([]any)
				fn := tools[0].(map[string]any)["function"].(map[string]any)
				if tools[0].(map[string]any)["type"] != "function" || fn["name"] != "get_weather" {
					t.Errorf("tools = %v", tools)
				}
				if fn["parameters"].(map[string]any)["type"] != "object" {
					t.Errorf("parameters not forwarded: %v", fn)
				}
				if b["tool_choice"] != "required" {
					t.Errorf("tool_choice = %v", b["tool_choice"])
				}
			},
		},
		{
			name: "named tool_choice forwarded raw",
			req: provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}},
				Tools: []provider.Tool{weatherTool()}, ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)},
			check: func(t *testing.T, b map[string]any) {
				want := map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
				if !reflect.DeepEqual(b["tool_choice"], want) {
					t.Errorf("tool_choice = %v", b["tool_choice"])
				}
			},
		},
		{
			name: "assistant tool_calls turn has null content, tool turn carries id and name",
			req: provider.ChatRequest{Model: "m", Tools: []provider.Tool{weatherTool()}, Messages: []provider.Message{
				{Role: "user", Content: "weather?"},
				{Role: "assistant", ToolCalls: []provider.ToolCall{call}},
				{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: `{"temp":3}`},
			}},
			check: func(t *testing.T, b map[string]any) {
				msgs := b["messages"].([]any)
				a := msgs[1].(map[string]any)
				if v, ok := a["content"]; !ok || v != nil {
					t.Errorf("assistant content = %v (present=%v), want explicit null", v, ok)
				}
				tc := a["tool_calls"].([]any)[0].(map[string]any)
				if tc["id"] != "call_1" || tc["function"].(map[string]any)["arguments"] != `{"city":"Oslo"}` {
					t.Errorf("tool_calls = %v", tc)
				}
				tm := msgs[2].(map[string]any)
				if tm["role"] != "tool" || tm["tool_call_id"] != "call_1" || tm["content"] != `{"temp":3}` || tm["name"] != "get_weather" {
					t.Errorf("tool turn = %v", tm)
				}
				if u := msgs[0].(map[string]any); u["content"] != "weather?" {
					t.Errorf("user turn = %v", u)
				}
			},
		},
		{
			name: "assistant turn with text and tool_calls keeps the text",
			req: provider.ChatRequest{Model: "m", Messages: []provider.Message{
				{Role: "assistant", Content: "checking", ToolCalls: []provider.ToolCall{call}},
			}},
			check: func(t *testing.T, b map[string]any) {
				if b["messages"].([]any)[0].(map[string]any)["content"] != "checking" {
					t.Errorf("content dropped: %v", b["messages"])
				}
			},
		},
		{
			name: "empty assistant text without tool_calls stays an empty string",
			req:  provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "assistant", Content: ""}}},
			check: func(t *testing.T, b map[string]any) {
				if c := b["messages"].([]any)[0].(map[string]any)["content"]; c != "" {
					t.Errorf("content = %v, want empty string", c)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got map[string]any
			srv := captureServer(t, &got, func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(buildMockCompletion("id", "m", "ok", 1, 1))
			})
			p := newTestProvider(t, srv.URL)
			if _, err := p.Complete(context.Background(), tt.req); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			tt.check(t, got)
		})
	}
}

func TestComplete_ToolCallsResponse(t *testing.T) {
	var got map[string]any
	srv := captureServer(t, &got, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}},
			{"id":"call_b","function":{"name":"get_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	})
	p := newTestProvider(t, srv.URL)
	resp, err := p.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.FinishReason != "tool_calls" || resp.Content != "" {
		t.Errorf("finish=%q content=%q", resp.FinishReason, resp.Content)
	}
	want := []provider.ToolCall{
		{ID: "call_a", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Oslo"}`}},
		{ID: "call_b", Type: "function", Function: provider.ToolCallFunction{Name: "get_time", Arguments: "{}"}},
	}
	if !reflect.DeepEqual(resp.ToolCalls, want) {
		t.Errorf("ToolCalls = %+v", resp.ToolCalls)
	}
}

// parsedStream is the client-visible view of an SSE body.
type parsedStream struct {
	calls  map[int]*provider.ToolCall
	order  []int // indexes in first-seen order
	text   string
	finish string
	done   bool
	frames []provider.ToolCallDelta
}

func parseSSE(t *testing.T, body string) parsedStream {
	t.Helper()
	ps := parsedStream{calls: map[int]*provider.ToolCall{}}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			ps.done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string                   `json:"content"`
					ToolCalls []provider.ToolCallDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad SSE payload %q: %v", payload, err)
		}
		for _, c := range chunk.Choices {
			ps.text += c.Delta.Content
			if c.FinishReason != nil {
				ps.finish = *c.FinishReason
			}
			for _, d := range c.Delta.ToolCalls {
				ps.frames = append(ps.frames, d)
				tc, ok := ps.calls[d.Index]
				if !ok {
					tc = &provider.ToolCall{}
					ps.calls[d.Index] = tc
					ps.order = append(ps.order, d.Index)
				}
				if d.ID != "" {
					tc.ID = d.ID
				}
				if d.Type != "" {
					tc.Type = d.Type
				}
				if d.Function != nil {
					tc.Function.Name += d.Function.Name
					tc.Function.Arguments += d.Function.Arguments
				}
			}
		}
	}
	return ps
}

func sseData(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

func TestStream_ToolCallRoundTrip(t *testing.T) {
	chunk := func(delta map[string]any, finish any) string {
		return sseData(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": "m",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}})
	}
	tc := func(idx int, id, name, args string) map[string]any {
		fn := map[string]any{"arguments": args}
		if name != "" {
			fn["name"] = name
		}
		d := map[string]any{"index": idx, "function": fn}
		if id != "" {
			d["id"] = id
			d["type"] = "function"
		}
		return d
	}
	upstream := chunk(map[string]any{"role": "assistant", "content": ""}, nil) +
		chunk(map[string]any{"tool_calls": []any{tc(0, "call_a", "get_weather", "")}}, nil) +
		chunk(map[string]any{"tool_calls": []any{tc(0, "", "", `{"ci`)}}, nil) +
		chunk(map[string]any{"tool_calls": []any{tc(1, "call_b", "get_time", "")}}, nil) +
		chunk(map[string]any{"tool_calls": []any{tc(0, "", "", `ty":"Oslo"}`)}}, nil) +
		chunk(map[string]any{"tool_calls": []any{tc(1, "", "", `{}`)}}, nil) +
		chunk(map[string]any{}, "tool_calls") +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstream)
	}))
	t.Cleanup(srv.Close)
	p := newTestProvider(t, srv.URL)

	var buf bytes.Buffer
	req := testRequest()
	req.Tools = []provider.Tool{weatherTool()}
	stats, err := p.Stream(context.Background(), req, &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if stats.FinishReason != "tool_calls" {
		t.Errorf("stats.FinishReason = %q, want tool_calls", stats.FinishReason)
	}
	if buf.String() != upstream {
		t.Errorf("relay must forward upstream bytes unchanged:\n got %q\nwant %q", buf.String(), upstream)
	}
	ps := parseSSE(t, buf.String())
	if !reflect.DeepEqual(ps.order, []int{0, 1}) || ps.finish != "tool_calls" || !ps.done {
		t.Errorf("order=%v finish=%q done=%v", ps.order, ps.finish, ps.done)
	}
	if got := ps.calls[0]; got.ID != "call_a" || got.Function.Name != "get_weather" || got.Function.Arguments != `{"city":"Oslo"}` {
		t.Errorf("call 0 = %+v", got)
	}
	if got := ps.calls[1]; got.ID != "call_b" || got.Function.Name != "get_time" || got.Function.Arguments != `{}` {
		t.Errorf("call 1 = %+v", got)
	}
}

func TestStream_NonStreamingUpstreamToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c9","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}},
			{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	}))
	t.Cleanup(srv.Close)
	p := newTestProvider(t, srv.URL)

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), testRequest(), &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if stats.FinishReason != "tool_calls" || stats.PromptTokens != 5 {
		t.Errorf("stats = %+v", stats)
	}
	ps := parseSSE(t, buf.String())
	if ps.text != "" {
		t.Errorf("tool-only turn must not emit text, got %q", ps.text)
	}
	// Exactly start + arguments fragment per call, in order.
	if len(ps.frames) != 4 || ps.frames[0].Index != 0 || ps.frames[0].ID != "call_a" || ps.frames[0].Function.Arguments != "" ||
		ps.frames[1].Index != 0 || ps.frames[1].ID != "" || ps.frames[2].Index != 1 || ps.frames[2].ID != "call_b" {
		t.Errorf("frames = %+v", ps.frames)
	}
	if ps.calls[0].Function.Arguments != `{"city":"Oslo"}` || ps.calls[1].Function.Name != "get_time" ||
		ps.finish != "tool_calls" || !ps.done {
		t.Errorf("reassembled = %+v / %+v finish=%q done=%v", ps.calls[0], ps.calls[1], ps.finish, ps.done)
	}
}

func TestToolChoiceNone_ForwardedWithToolsAndHistory(t *testing.T) {
	req := provider.ChatRequest{Model: "m", Tools: []provider.Tool{weatherTool()}, ToolChoice: json.RawMessage(`"none"`),
		Messages: []provider.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: ""}}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
		}}
	var b map[string]any
	srv := captureServer(t, &b, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildMockCompletion("id", "m", "ok", 1, 1))
	})
	if _, err := newTestProvider(t, srv.URL).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if tools, _ := b["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v, want definitions kept", b["tools"])
	}
	if b["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v, want none", b["tool_choice"])
	}
	tc := b["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["function"].(map[string]any)["arguments"] != "{}" {
		t.Errorf("empty arguments = %v, want {}", tc["function"])
	}
}
