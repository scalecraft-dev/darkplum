package provider

import (
	"encoding/json"
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
