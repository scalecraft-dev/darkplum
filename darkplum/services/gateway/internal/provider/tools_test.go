package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateTools(t *testing.T) {
	ok := []Tool{{Type: "function", Function: ToolFunction{Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	for _, tc := range []struct {
		name      string
		tools     []Tool
		choice    string
		messages  []Message
		wantParam string
	}{
		{"fine", ok, `"auto"`, nil, ""},
		{"fine named", ok, `{"type":"function","function":{"name":"f"}}`, nil, ""},
		{"fine none without tools", nil, `"none"`, nil, ""},
		{"bad type", []Tool{{Type: "x", Function: ToolFunction{Name: "f"}}}, "", nil, "tools"},
		{"bad name", []Tool{{Type: "function", Function: ToolFunction{Name: "a b"}}}, "", nil, "tools"},
		{"dup", append(append([]Tool{}, ok...), ok...), "", nil, "tools"},
		{"bad params", []Tool{{Type: "function", Function: ToolFunction{Name: "f", Parameters: json.RawMessage(`"s"`)}}}, "", nil, "tools"},
		{"choice without tools", nil, `"auto"`, nil, "tool_choice"},
		{"choice unknown", ok, `"maybe"`, nil, "tool_choice"},
		{"choice missing fn", ok, `{"type":"function","function":{"name":"g"}}`, nil, "tool_choice"},
		{"orphan result", ok, "", []Message{{Role: "tool", ToolCallID: "c1"}}, "messages"},
		{"paired result", ok, "", []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f"}}}}, {Role: "tool", ToolCallID: "c1"}}, ""},
		{"two calls both answered", ok, "", []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}, {ID: "c2", Function: ToolCallFunction{Name: "f", Arguments: `{"a":1}`}}}},
			{Role: "tool", ToolCallID: "c2"}, {Role: "tool", ToolCallID: "c1"}, {Role: "user", Content: "thanks"},
		}, ""},
		{"unanswered call before next turn", ok, "", []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f"}}, {ID: "c2", Function: ToolCallFunction{Name: "f"}}}},
			{Role: "tool", ToolCallID: "c1"}, {Role: "user", Content: "?"},
		}, "messages"},
		{"trailing unanswered call", ok, "", []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f"}}}},
		}, "messages"},
		{"answered twice", ok, "", []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f"}}}},
			{Role: "tool", ToolCallID: "c1"}, {Role: "tool", ToolCallID: "c1"},
		}, "messages"},
		{"duplicate call id across turns", ok, "", []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f"}}}},
			{Role: "tool", ToolCallID: "c1"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f"}}}},
			{Role: "tool", ToolCallID: "c1"},
		}, "messages"},
		{"arguments not an object", ok, "", []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f", Arguments: "[1]"}}}},
			{Role: "tool", ToolCallID: "c1"},
		}, "messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			param, msg := ValidateTools(tc.tools, json.RawMessage(tc.choice), tc.messages)
			if param != tc.wantParam {
				t.Errorf("param = %q (msg %q), want %q", param, msg, tc.wantParam)
			}
		})
	}
}

func TestMessageUnmarshal_ToolTurns(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "c1" {
		t.Errorf("got %+v", m)
	}
	if err := json.Unmarshal([]byte(`{"role":"tool","tool_call_id":"c1","content":"out"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != "tool" || m.ToolCallID != "c1" || m.Content != "out" || len(m.ToolCalls) != 0 {
		t.Errorf("got %+v", m)
	}
}

func TestBuildCompletionJSON_ToolCalls(t *testing.T) {
	b, err := BuildCompletionJSON(&ChatResponse{ID: "x", Model: "m", FinishReason: "tool_calls", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"content":null`) || !strings.Contains(s, `"tool_calls":[{"id":"c1"`) || !strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Errorf("body = %s", s)
	}
	b, _ = BuildCompletionJSON(&ChatResponse{ID: "x", Model: "m", Content: "", FinishReason: "stop"})
	if !strings.Contains(string(b), `"content":""`) {
		t.Errorf("plain empty content must stay a string: %s", b)
	}
}

func TestBuildToolCallDeltaSSE(t *testing.T) {
	b, err := BuildToolCallDeltaSSE("id", "m", NewToolCallStartDelta(1, "c1", "f"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "data: ") || !strings.HasSuffix(s, "\n\n") || !strings.Contains(s, `"tool_calls":[{"index":1,"id":"c1","type":"function","function":{"name":"f","arguments":""}}]`) {
		t.Errorf("sse = %q", s)
	}
}

func TestHasToolTurns(t *testing.T) {
	if HasToolTurns([]Message{{Role: "user", Content: "x"}, {Role: "assistant", Content: "y"}}) {
		t.Error("plain history reported as tool turns")
	}
	if !HasToolTurns([]Message{{Role: "tool", ToolCallID: "c"}}) || !HasToolTurns([]Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "c"}}}}) {
		t.Error("tool turns not detected")
	}
}

func TestBuildToolCallDeltaSSE_NoRole(t *testing.T) {
	b, _ := BuildToolCallDeltaSSE("id", "m", NewToolCallArgumentsDelta(0, "{"))
	if strings.Contains(string(b), `"role"`) {
		t.Errorf("tool delta must not carry a role: %s", b)
	}
}
