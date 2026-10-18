package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

func TestSupportsTools(t *testing.T) {
	if !provider.SupportsTools(New(&fakeCreds{key: "k", version: 1}, Config{})) {
		t.Fatal("openai must report tool support")
	}
}

func toolsFixture() provider.Tool {
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
		check func(t *testing.T, b map[string]any)
	}{
		{
			name: "no tools leaves tool fields absent",
			req:  basicRequest("gpt-5-mini"),
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
				Tools: []provider.Tool{toolsFixture()}, ToolChoice: json.RawMessage(`"auto"`)},
			check: func(t *testing.T, b map[string]any) {
				tools := b["tools"].([]any)
				fn := tools[0].(map[string]any)["function"].(map[string]any)
				if tools[0].(map[string]any)["type"] != "function" || fn["name"] != "get_weather" || fn["parameters"].(map[string]any)["type"] != "object" {
					t.Errorf("tools = %v", tools)
				}
				if b["tool_choice"] != "auto" {
					t.Errorf("tool_choice = %v", b["tool_choice"])
				}
			},
		},
		{
			name: "named tool_choice forwarded raw",
			req: provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}},
				Tools: []provider.Tool{toolsFixture()}, ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)},
			check: func(t *testing.T, b map[string]any) {
				want := map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
				if !reflect.DeepEqual(b["tool_choice"], want) {
					t.Errorf("tool_choice = %v", b["tool_choice"])
				}
			},
		},
		{
			name: "assistant tool_calls turn has null content, tool turn carries id and name",
			req: provider.ChatRequest{Model: "m", Tools: []provider.Tool{toolsFixture()}, Messages: []provider.Message{
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
				if tc["id"] != "call_1" || tc["type"] != "function" || tc["function"].(map[string]any)["arguments"] != `{"city":"Oslo"}` {
					t.Errorf("tool_calls = %v", tc)
				}
				tm := msgs[2].(map[string]any)
				if tm["role"] != "tool" || tm["tool_call_id"] != "call_1" || tm["content"] != `{"temp":3}` || tm["name"] != "get_weather" {
					t.Errorf("tool turn = %v", tm)
				}
				if msgs[0].(map[string]any)["content"] != "weather?" {
					t.Errorf("user turn = %v", msgs[0])
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
			raw, err := buildRequestBody(tt.req, false, droppedParams{})
			if err != nil {
				t.Fatal(err)
			}
			var b map[string]any
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatal(err)
			}
			tt.check(t, b)
		})
	}
}

func TestMapFinishReason_ToolCallsPassesThrough(t *testing.T) {
	for in, want := range map[string]string{"tool_calls": "tool_calls", "function_call": "stop", "stop": "stop", "length": "length"} {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComplete_ToolCallsResponse(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","model":"gpt-5-mini","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}},
				{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})
	req := basicRequest("gpt-5-mini")
	req.Tools = []provider.Tool{toolsFixture()}
	resp, err := p.Complete(context.Background(), req)
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
	if got := srv.requestAt(0).body["tools"]; got == nil {
		t.Error("tools not forwarded upstream")
	}
}

// streamedToolCalls reassembles tool calls from the SSE the gateway wrote.
type streamedToolCalls struct {
	calls  map[int]*provider.ToolCall
	order  []int
	frames []provider.ToolCallDelta
	finish string
	done   bool
}

func parseToolSSE(t *testing.T, body string) streamedToolCalls {
	t.Helper()
	out := streamedToolCalls{calls: map[int]*provider.ToolCall{}}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			out.done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []provider.ToolCallDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad SSE payload %q: %v", payload, err)
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != nil {
				out.finish = *c.FinishReason
			}
			for _, d := range c.Delta.ToolCalls {
				out.frames = append(out.frames, d)
				tc, ok := out.calls[d.Index]
				if !ok {
					tc = &provider.ToolCall{}
					out.calls[d.Index] = tc
					out.order = append(out.order, d.Index)
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
	return out
}

func toolChunk(w io.Writer, delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl_t1", "object": "chat.completion.chunk", "model": "gpt-5-mini",
		"system_fingerprint": "fp_x",
		"choices":            []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
}

func tcDelta(idx int, id, name, args string) map[string]any {
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

func TestStream_ToolCallRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		send     func(w io.Writer)
		wantIdx  []int
		wantArgs map[int]string
		wantName map[int]string
		wantIDs  map[int]string
		frames   int
	}{
		{
			name: "single call",
			send: func(w io.Writer) {
				toolChunk(w, map[string]any{"role": "assistant", "content": nil}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(0, "call_a", "get_weather", "")}}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(0, "", "", `{"ci`)}}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(0, "", "", `ty":"Oslo"}`)}}, nil)
				toolChunk(w, map[string]any{}, "tool_calls")
			},
			wantIdx:  []int{0},
			wantArgs: map[int]string{0: `{"city":"Oslo"}`},
			wantName: map[int]string{0: "get_weather"},
			wantIDs:  map[int]string{0: "call_a"},
			frames:   3,
		},
		{
			name: "parallel calls interleaved across chunks",
			send: func(w io.Writer) {
				toolChunk(w, map[string]any{"role": "assistant"}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(0, "call_a", "get_weather", "")}}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(0, "", "", `{"ci`)}}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(1, "call_b", "get_time", "")}}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(0, "", "", `ty":"Oslo"}`)}}, nil)
				// Two fragments for different indexes in a single chunk.
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(1, "", "", `{"tz":`), tcDelta(0, "", "", ` `)}}, nil)
				toolChunk(w, map[string]any{"tool_calls": []any{tcDelta(1, "", "", `"UTC"}`)}}, nil)
				toolChunk(w, map[string]any{}, "tool_calls")
			},
			wantIdx:  []int{0, 1},
			wantArgs: map[int]string{0: `{"city":"Oslo"} `, 1: `{"tz":"UTC"}`},
			wantName: map[int]string{0: "get_weather", 1: "get_time"},
			wantIDs:  map[int]string{0: "call_a", 1: "call_b"},
			frames:   7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFakeOpenAIServer(t)
			srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
				func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					tt.send(w)
					_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_t1\",\"model\":\"gpt-5-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6}}\n\n")
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				},
			}
			p := newTestProvider(t, srv, nil, Config{})
			req := basicRequest("gpt-5-mini")
			req.Tools = []provider.Tool{toolsFixture()}

			var buf bytes.Buffer
			stats, err := p.Stream(context.Background(), req, &buf)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if stats.FinishReason != "tool_calls" || stats.PromptTokens != 4 || stats.CompletionTokens != 6 {
				t.Errorf("stats = %+v", stats)
			}
			out := buf.String()
			if strings.Contains(out, "system_fingerprint") {
				t.Error("vendor fields must not reach the client")
			}
			ps := parseToolSSE(t, out)
			if !reflect.DeepEqual(ps.order, tt.wantIdx) {
				t.Errorf("index order = %v, want %v", ps.order, tt.wantIdx)
			}
			if len(ps.frames) != tt.frames {
				t.Errorf("frames = %d, want %d: %+v", len(ps.frames), tt.frames, ps.frames)
			}
			for idx, c := range ps.calls {
				if c.ID != tt.wantIDs[idx] || c.Type != "function" || c.Function.Name != tt.wantName[idx] || c.Function.Arguments != tt.wantArgs[idx] {
					t.Errorf("call %d = %+v", idx, c)
				}
			}
			// The first fragment of each call carries id/type/name; later ones
			// only arguments.
			seen := map[int]bool{}
			for _, f := range ps.frames {
				if !seen[f.Index] && (f.ID == "" || f.Function == nil || f.Function.Name == "") {
					t.Errorf("first fragment for index %d lacks id/name: %+v", f.Index, f)
				}
				if seen[f.Index] && f.ID != "" {
					t.Errorf("later fragment for index %d repeats id: %+v", f.Index, f)
				}
				seen[f.Index] = true
			}
			if ps.finish != "tool_calls" || !ps.done || !strings.HasSuffix(out, provider.StreamDoneSSE) {
				t.Errorf("finish=%q done=%v", ps.finish, ps.done)
			}
		})
	}
}

func TestToolChoiceNone_ForwardedWithToolsAndHistory(t *testing.T) {
	req := provider.ChatRequest{Model: "m", Tools: []provider.Tool{toolsFixture()}, ToolChoice: json.RawMessage(`"none"`),
		Messages: []provider.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: ""}}}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
		}}
	raw, err := buildRequestBody(req, false, droppedParams{})
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	if err := json.Unmarshal(raw, &b); err != nil {
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
