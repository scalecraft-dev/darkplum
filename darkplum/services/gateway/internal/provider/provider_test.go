package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageUnmarshalJSON_PlainStringContent(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hello"}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "hello" || m.HasNonTextContent {
		t.Errorf("got %+v", m)
	}
}

func TestMessageUnmarshalJSON_TextOnlyBlocksNotFlaggedMultimodal(t *testing.T) {
	var m Message
	body := `{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}`
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "hello world" || m.HasNonTextContent {
		t.Errorf("got %+v", m)
	}
}

// TestMessageUnmarshalJSON_NonTextBlockSetsFlag is the wiring point for the
// routed-alias builtin:multimodal rules signal: an image_url block (or any
// non-"text" block) must set HasNonTextContent even though its content is
// dropped from the flattened Content string.
func TestMessageUnmarshalJSON_NonTextBlockSetsFlag(t *testing.T) {
	var m Message
	body := `{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}`
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Content != "what is this?" {
		t.Errorf("Content = %q, want the text block only", m.Content)
	}
	if !m.HasNonTextContent {
		t.Error("expected HasNonTextContent to be true for a message with an image_url block")
	}
}

func TestBuildFinalChunksSSE_UsageShapes(t *testing.T) {
	stats := &StreamStats{PromptTokens: 7, CompletionTokens: 3}
	decode := func(t *testing.T, b []byte) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, frame := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &m); err != nil {
				t.Fatalf("decode %q: %v", frame, err)
			}
			out = append(out, m)
		}
		return out
	}

	// Without include_usage: one chunk, usage on the finish_reason chunk,
	// exactly what BuildFinalChunkSSE has always produced.
	b, err := BuildFinalChunksSSE("id", "m", "stop", stats, false)
	if err != nil {
		t.Fatal(err)
	}
	frames := decode(t, b)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0]["usage"].(map[string]any)["total_tokens"].(float64) != 10 {
		t.Errorf("usage = %v, want total 10 on the finish chunk", frames[0]["usage"])
	}

	// With include_usage: finish chunk has usage null, then a usage-only
	// chunk with choices [] (an empty array, not omitted).
	b, err = BuildFinalChunksSSE("id", "m", "stop", stats, true)
	if err != nil {
		t.Fatal(err)
	}
	frames = decode(t, b)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[0]["usage"] != nil {
		t.Errorf("finish chunk usage = %v, want null", frames[0]["usage"])
	}
	if ch := frames[0]["choices"].([]any); len(ch) != 1 || ch[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish chunk choices = %v", frames[0]["choices"])
	}
	if ch, ok := frames[1]["choices"].([]any); !ok || len(ch) != 0 {
		t.Errorf("usage chunk choices = %v, want []", frames[1]["choices"])
	}
	u := frames[1]["usage"].(map[string]any)
	if u["prompt_tokens"].(float64) != 7 || u["completion_tokens"].(float64) != 3 || u["total_tokens"].(float64) != 10 {
		t.Errorf("usage chunk usage = %v", u)
	}
	if frames[1]["object"] != "chat.completion.chunk" || frames[1]["id"] != "id" || frames[1]["model"] != "m" {
		t.Errorf("usage chunk envelope = %v", frames[1])
	}
}

func TestVendorAPIError_WithParam(t *testing.T) {
	e := NewVendorAPIError("openai", 400, "invalid_request_error", "invalid_request", "x", 0, nil).WithParam("response_format")
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(e.ResponseBody(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Param != "response_format" || body.Error.Code != "invalid_request" || e.HTTPStatus() != 400 {
		t.Errorf("body = %+v status=%d", body, e.HTTPStatus())
	}
}
