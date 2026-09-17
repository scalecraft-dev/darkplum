package bedrock

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// ---------------------------------------------------------------------------
// translateRequest tests
// ---------------------------------------------------------------------------

func TestTranslateRequest_SystemMessageExtracted(t *testing.T) {
	req := provider.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Hello"},
		},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal bedrockRequest: %v", err)
	}

	if br.System != "You are a helpful assistant." {
		t.Errorf("expected system=%q, got %q", "You are a helpful assistant.", br.System)
	}

	if len(br.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(br.Messages))
	}
	if br.Messages[0].Role != "user" || br.Messages[0].Content != "Hello" {
		t.Errorf("unexpected message: %+v", br.Messages[0])
	}
}

func TestTranslateRequest_MultipleSystemMessagesConcatenated(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "First system instruction."},
			{Role: "system", Content: "Second system instruction."},
			{Role: "user", Content: "Go"},
		},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := "First system instruction.\nSecond system instruction."
	if br.System != want {
		t.Errorf("expected system=%q, got %q", want, br.System)
	}
}

func TestTranslateRequest_NoSystemMessage(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "user", Content: "Hi"},
			{Role: "assistant", Content: "Hello"},
		},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if br.System != "" {
		t.Errorf("expected empty system field, got %q", br.System)
	}
	if len(br.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(br.Messages))
	}
}

func TestTranslateRequest_UnknownRolesDropped(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "tool", Content: "tool result"},
			{Role: "user", Content: "What do you think?"},
		},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(br.Messages) != 1 {
		t.Fatalf("expected 1 message (tool role dropped), got %d", len(br.Messages))
	}
	if br.Messages[0].Role != "user" {
		t.Errorf("expected user role, got %q", br.Messages[0].Role)
	}
}

// ---------------------------------------------------------------------------
// MaxTokens defaulting
// ---------------------------------------------------------------------------

func TestTranslateRequest_MaxTokensDefaultsTo4096(t *testing.T) {
	req := provider.ChatRequest{
		MaxTokens: 0,
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if br.MaxTokens != 4096 {
		t.Errorf("expected MaxTokens=4096, got %d", br.MaxTokens)
	}
}

func TestTranslateRequest_MaxTokensExplicitValuePreserved(t *testing.T) {
	req := provider.ChatRequest{
		MaxTokens: 512,
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if br.MaxTokens != 512 {
		t.Errorf("expected MaxTokens=512, got %d", br.MaxTokens)
	}
}

// ---------------------------------------------------------------------------
// Temperature forwarding
// ---------------------------------------------------------------------------

func TestTranslateRequest_TemperatureNilOmitted(t *testing.T) {
	req := provider.ChatRequest{
		Temperature: nil,
		Messages:    []provider.Message{{Role: "user", Content: "hi"}},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	// json.Unmarshal into a map so we can check key presence.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	if _, ok := m["temperature"]; ok {
		t.Error("temperature key should be absent when nil, but it is present")
	}
}

func TestTranslateRequest_TemperatureNonNilForwarded(t *testing.T) {
	temp := 0.7
	req := provider.ChatRequest{
		Temperature: &temp,
		Messages:    []provider.Message{{Role: "user", Content: "hi"}},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if br.Temperature == nil {
		t.Fatal("expected temperature to be set, got nil")
	}
	if *br.Temperature != 0.7 {
		t.Errorf("expected temperature=0.7, got %f", *br.Temperature)
	}
}

// ---------------------------------------------------------------------------
// AnthropicVersion
// ---------------------------------------------------------------------------

func TestTranslateRequest_AnthropicVersionSet(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}

	raw, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	var br bedrockRequest
	if err := json.Unmarshal(raw, &br); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if br.AnthropicVersion != "bedrock-2023-05-31" {
		t.Errorf("expected anthropic_version=bedrock-2023-05-31, got %q", br.AnthropicVersion)
	}
}

// ---------------------------------------------------------------------------
// translateResponse tests
// ---------------------------------------------------------------------------

func TestTranslateResponse_BasicMapping(t *testing.T) {
	responseJSON := `{
		"content": [{"type": "text", "text": "Hello, world!"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 5}
	}`

	resp, err := translateResponse([]byte(responseJSON), "anthropic.claude-3-haiku-20240307-v1:0")
	if err != nil {
		t.Fatalf("translateResponse returned error: %v", err)
	}

	if resp.Content != "Hello, world!" {
		t.Errorf("expected content=%q, got %q", "Hello, world!", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("expected finish_reason=stop (mapped from end_turn), got %q", resp.FinishReason)
	}
	if resp.PromptTokens != 10 {
		t.Errorf("expected prompt_tokens=10, got %d", resp.PromptTokens)
	}
	if resp.CompletionTokens != 5 {
		t.Errorf("expected completion_tokens=5, got %d", resp.CompletionTokens)
	}
	if resp.Model != "anthropic.claude-3-haiku-20240307-v1:0" {
		t.Errorf("expected model passed through, got %q", resp.Model)
	}
}

func TestTranslateResponse_MultipleTextBlocksConcatenated(t *testing.T) {
	responseJSON := `{
		"content": [
			{"type": "text", "text": "Part one. "},
			{"type": "text", "text": "Part two."}
		],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 5, "output_tokens": 10}
	}`

	resp, err := translateResponse([]byte(responseJSON), "model")
	if err != nil {
		t.Fatalf("translateResponse returned error: %v", err)
	}

	if resp.Content != "Part one. Part two." {
		t.Errorf("expected concatenated content, got %q", resp.Content)
	}
}

func TestTranslateResponse_NonTextBlocksIgnored(t *testing.T) {
	responseJSON := `{
		"content": [
			{"type": "tool_use", "id": "123"},
			{"type": "text", "text": "Only this."}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 5, "output_tokens": 3}
	}`

	resp, err := translateResponse([]byte(responseJSON), "model")
	if err != nil {
		t.Fatalf("translateResponse returned error: %v", err)
	}

	if resp.Content != "Only this." {
		t.Errorf("expected only text block content, got %q", resp.Content)
	}
}

func TestTranslateResponse_InvalidJSONReturnsError(t *testing.T) {
	_, err := translateResponse([]byte("{bad json"), "model")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// ---------------------------------------------------------------------------
// translateStreamEvent / SSE chunk format tests
// ---------------------------------------------------------------------------

// TestTranslateStreamEvent_ContentBlockDelta verifies that a content_block_delta
// event produces an OpenAI SSE chunk with the correct format.
func TestTranslateStreamEvent_ContentBlockDelta(t *testing.T) {
	raw := []byte(`{
		"type": "content_block_delta",
		"delta": {"type": "text_delta", "text": "Hello"}
	}`)

	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(raw, "test-id", "model-x", stats, &buf)
	if err != nil {
		t.Fatalf("translateStreamEvent returned error: %v", err)
	}
	if done {
		t.Fatal("expected done=false for content_block_delta")
	}

	output := buf.String()
	if !strings.HasPrefix(output, "data: ") {
		t.Errorf("SSE chunk must start with 'data: ', got: %q", output)
	}
	if !strings.HasSuffix(output, "\n\n") {
		t.Errorf("SSE chunk must end with '\\n\\n', got: %q", output)
	}

	// Verify the JSON payload is valid and contains the token.
	payload := strings.TrimPrefix(output, "data: ")
	payload = strings.TrimSuffix(payload, "\n\n")

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("SSE chunk JSON is not valid: %v", err)
	}

	// Drill into choices[0].delta.content.
	var choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(parsed["choices"], &choices); err != nil {
		t.Fatalf("parse choices: %v", err)
	}
	if len(choices) == 0 {
		t.Fatal("expected at least one choice in SSE chunk")
	}
	if choices[0].Delta.Content != "Hello" {
		t.Errorf("expected delta content=%q, got %q", "Hello", choices[0].Delta.Content)
	}
}

// TestTranslateStreamEvent_MessageStart verifies input token accumulation.
func TestTranslateStreamEvent_MessageStart(t *testing.T) {
	raw := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":42,"output_tokens":0}}}`)
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	_, err := translateStreamEvent(raw, "id", "model", stats, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no SSE output for message_start, got: %q", buf.String())
	}
	if stats.PromptTokens != 42 {
		t.Errorf("expected prompt_tokens=42, got %d", stats.PromptTokens)
	}
}

// TestTranslateStreamEvent_MessageDelta verifies finish_reason and output token
// accumulation, and that a final SSE chunk is emitted.
func TestTranslateStreamEvent_MessageDelta(t *testing.T) {
	raw := []byte(`{
		"type": "message_delta",
		"delta": {"stop_reason": "end_turn"},
		"usage": {"output_tokens": 17}
	}`)
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(raw, "id", "model", stats, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("message_delta should not set done=true; message_stop does")
	}
	if stats.FinishReason != "stop" {
		t.Errorf("expected finish_reason=stop (mapped from end_turn), got %q", stats.FinishReason)
	}
	if stats.CompletionTokens != 17 {
		t.Errorf("expected completion_tokens=17, got %d", stats.CompletionTokens)
	}
	if buf.Len() == 0 {
		t.Error("expected final SSE chunk to be written on message_delta, got empty buffer")
	}

	// The final chunk must carry a non-nil finish_reason.
	output := buf.String()
	payload := strings.TrimPrefix(strings.TrimSuffix(output, "\n\n"), "data: ")
	var parsed struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("parse final chunk: %v", err)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].FinishReason == nil {
		t.Error("final SSE chunk must have a non-nil finish_reason")
	}
}

// TestTranslateStreamEvent_MessageStop verifies done=true is returned and
// nothing is written to the buffer.
func TestTranslateStreamEvent_MessageStop(t *testing.T) {
	raw := []byte(`{"type":"message_stop"}`)
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(raw, "id", "model", stats, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected done=true for message_stop")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no SSE output for message_stop, got: %q", buf.String())
	}
}

// TestTranslateStreamEvent_UnknownTypeIsNoOp ensures future unknown event types
// don't produce output or errors.
func TestTranslateStreamEvent_UnknownTypeIsNoOp(t *testing.T) {
	raw := []byte(`{"type":"ping"}`)
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(raw, "id", "model", stats, &buf)
	if err != nil {
		t.Fatalf("unexpected error for unknown event type: %v", err)
	}
	if done {
		t.Fatal("unknown event type should not signal done")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no SSE output for unknown event type")
	}
}

func TestTranslateStreamEvent_InvalidJSONReturnsError(t *testing.T) {
	_, err := translateStreamEvent([]byte("{bad"), "id", "model", &provider.StreamStats{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for invalid JSON stream event")
	}
}

// ---------------------------------------------------------------------------
// TranslateBedrockError tests
// ---------------------------------------------------------------------------

func TestTranslateBedrockError_Throttling(t *testing.T) {
	code, body := TranslateBedrockError("ThrottlingException", "")
	assertErrorResponse(t, code, body, http.StatusTooManyRequests, "rate_limit_error")
}

func TestTranslateBedrockError_AccessDenied(t *testing.T) {
	code, body := TranslateBedrockError("AccessDeniedException", "")
	assertErrorResponse(t, code, body, http.StatusForbidden, "permission_error")
	if !strings.Contains(string(body), "IAM permissions") {
		t.Errorf("access denied error should mention IAM permissions; got: %s", body)
	}
}

func TestTranslateBedrockError_Validation(t *testing.T) {
	code, body := TranslateBedrockError("ValidationException", "messages must not be empty")
	assertErrorResponse(t, code, body, http.StatusBadRequest, "invalid_request_error")
	if !strings.Contains(string(body), "messages must not be empty") {
		t.Errorf("validation error should include raw message; got: %s", body)
	}
}

func TestTranslateBedrockError_ValidationEmptyMessageDefaulted(t *testing.T) {
	code, body := TranslateBedrockError("ValidationException", "")
	assertErrorResponse(t, code, body, http.StatusBadRequest, "invalid_request_error")
	// Should not produce empty message field.
	if strings.Contains(string(body), `"message":""`) {
		t.Errorf("empty rawMessage should be replaced with default; got: %s", body)
	}
}

func TestTranslateBedrockError_ModelNotReady(t *testing.T) {
	code, body := TranslateBedrockError("ModelNotReadyException", "")
	assertErrorResponse(t, code, body, http.StatusServiceUnavailable, "server_error")
}

func TestTranslateBedrockError_UnknownDefaultsTo500(t *testing.T) {
	code, body := TranslateBedrockError("SomeOtherException", "weird error")
	assertErrorResponse(t, code, body, http.StatusInternalServerError, "server_error")
}

// ---------------------------------------------------------------------------
// convertFinishReason tests
// ---------------------------------------------------------------------------

func TestConvertFinishReason(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"content_filtered", "content_filter"},
		{"finished", "stop"},
		{"complete", "stop"},
		{"END_TURN", "stop"},
		{"", ""},
		{"unknown_reason", "unknown_reason"},
	}
	for _, tc := range cases {
		got := convertFinishReason(tc.input)
		if got != tc.want {
			t.Errorf("convertFinishReason(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// assertErrorResponse is a test helper that checks the status code and that
// the JSON body contains the expected error type string.
func assertErrorResponse(t *testing.T, code int, body []byte, wantCode int, wantType string) {
	t.Helper()

	if code != wantCode {
		t.Errorf("expected status %d, got %d", wantCode, code)
	}

	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("error body is not valid JSON: %v — body: %s", err, body)
	}
	if envelope.Error.Type != wantType {
		t.Errorf("expected error.type=%q, got %q", wantType, envelope.Error.Type)
	}
	if envelope.Error.Message == "" {
		t.Error("error.message must not be empty")
	}
}
