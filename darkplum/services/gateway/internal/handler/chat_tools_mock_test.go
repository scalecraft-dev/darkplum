package handler

// End-to-end tool-calling through the real mock provider: the shape a client
// sees for a tool call (non-stream and stream) and for the turn after it
// returns the tool's result.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/mock"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

func mockToolHandler(t *testing.T) *ChatHandler {
	t.Helper()
	return newTestHandler(
		fakeProviders{"mock": mock.New()},
		map[string]routing.Route{"mock-model": {ProviderKey: "mock", Kind: "mock", Class: routing.ClassSelfHosted}},
		policy.Snapshot{Enabled: map[string]bool{"mock-model": true}, Residency: policy.ResidencyAny},
		true,
	)
}

func TestToolCalling_MockNonStream(t *testing.T) {
	h := mockToolHandler(t)
	body := baseChatBody("mock-model")
	body["tools"] = []map[string]any{weatherTool}
	body["messages"] = []map[string]any{{"role": "user", "content": `call get_weather {"city":"Oslo"}`}}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Role      string              `json:"role"`
				Content   *string             `json:"content"`
				ToolCalls []provider.ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	ch := resp.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", ch.FinishReason)
	}
	if ch.Message.Content != nil {
		t.Errorf("content = %q, want null on a tool_calls turn", *ch.Message.Content)
	}
	if len(ch.Message.ToolCalls) != 1 || ch.Message.ToolCalls[0].Function.Name != "get_weather" || ch.Message.ToolCalls[0].Function.Arguments != `{"city":"Oslo"}` {
		t.Fatalf("tool_calls = %+v", ch.Message.ToolCalls)
	}

	// The follow-up turn carries the result back; the mock echoes it.
	body["messages"] = []map[string]any{
		{"role": "user", "content": `call get_weather {"city":"Oslo"}`},
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{"id": ch.Message.ToolCalls[0].ID, "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Oslo"}`}}}},
		{"role": "tool", "tool_call_id": ch.Message.ToolCalls[0].ID, "content": "12C and sunny"},
	}
	w, _ = doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("follow-up: expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "The tool answered: 12C and sunny") {
		t.Errorf("follow-up body = %s", w.Body.String())
	}
}

func TestToolCalling_MockStream(t *testing.T) {
	h := mockToolHandler(t)
	body := baseChatBody("mock-model")
	body["stream"] = true
	body["tools"] = []map[string]any{weatherTool}
	body["messages"] = []map[string]any{{"role": "user", "content": `call get_weather {"city":"Oslo"}`}}
	w, _ := doRawChat(t, h, body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	var (
		id, name, args string
		finish         string
	)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
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
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", line, err)
		}
		for _, d := range chunk.Choices[0].Delta.ToolCalls {
			if d.Index != 0 {
				t.Errorf("index = %d, want 0", d.Index)
			}
			if d.ID != "" {
				id = d.ID
			}
			if d.Function != nil {
				if d.Function.Name != "" {
					name = d.Function.Name
				}
				args += d.Function.Arguments
			}
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			finish = *fr
		}
	}
	if id == "" || name != "get_weather" || args != `{"city":"Oslo"}` || finish != "tool_calls" {
		t.Errorf("reassembled id=%q name=%q args=%q finish=%q\nstream:\n%s", id, name, args, finish, w.Body.String())
	}
}
