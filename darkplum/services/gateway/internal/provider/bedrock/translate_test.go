package bedrock

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// ---------------------------------------------------------------------------
// translateRequest tests
// ---------------------------------------------------------------------------

func textOf(t *testing.T, block types.ContentBlock) string {
	t.Helper()
	tb, ok := block.(*types.ContentBlockMemberText)
	if !ok {
		t.Fatalf("expected a text content block, got %T", block)
	}
	return tb.Value
}

func TestTranslateRequest_SystemMessageExtracted(t *testing.T) {
	req := provider.ChatRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Hello"},
		},
	}

	system, messages, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	if len(system) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(system))
	}
	sb, ok := system[0].(*types.SystemContentBlockMemberText)
	if !ok {
		t.Fatalf("expected text system block, got %T", system[0])
	}
	if sb.Value != "You are a helpful assistant." {
		t.Errorf("expected system=%q, got %q", "You are a helpful assistant.", sb.Value)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Role != types.ConversationRoleUser || textOf(t, messages[0].Content[0]) != "Hello" {
		t.Errorf("unexpected message: %+v", messages[0])
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

	system, _, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	want := "First system instruction.\nSecond system instruction."
	sb := system[0].(*types.SystemContentBlockMemberText)
	if sb.Value != want {
		t.Errorf("expected system=%q, got %q", want, sb.Value)
	}
}

func TestTranslateRequest_NoSystemMessage(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "user", Content: "Hi"},
			{Role: "assistant", Content: "Hello"},
		},
	}

	system, messages, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	if len(system) != 0 {
		t.Errorf("expected no system blocks, got %d", len(system))
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
}

func TestTranslateRequest_UnknownRolesDropped(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "function", Content: "legacy function result"},
			{Role: "user", Content: "What do you think?"},
		},
	}

	_, messages, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message (unknown role dropped), got %d", len(messages))
	}
	if messages[0].Role != types.ConversationRoleUser {
		t.Errorf("expected user role, got %q", messages[0].Role)
	}
}

func TestTranslateRequest_ConsecutiveSameRoleMerged(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "user", Content: "Part one."},
			{Role: "user", Content: "Part two."},
			{Role: "assistant", Content: "Ack one."},
			{Role: "assistant", Content: "Ack two."},
		},
	}

	_, messages, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	if len(messages) != 2 {
		t.Fatalf("expected 2 merged messages, got %d", len(messages))
	}
	if messages[0].Role != types.ConversationRoleUser || textOf(t, messages[0].Content[0]) != "Part one.\nPart two." {
		t.Errorf("unexpected merged user message: role=%q content=%+v", messages[0].Role, messages[0].Content)
	}
	if messages[1].Role != types.ConversationRoleAssistant || textOf(t, messages[1].Content[0]) != "Ack one.\nAck two." {
		t.Errorf("unexpected merged assistant message: role=%q content=%+v", messages[1].Role, messages[1].Content)
	}
}

func TestTranslateRequest_LeadingAssistantDropped(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "assistant", Content: "Hi, how can I help?"},
			{Role: "user", Content: "What's the weather?"},
		},
	}

	_, messages, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected leading assistant message dropped, got %d messages", len(messages))
	}
	if messages[0].Role != types.ConversationRoleUser {
		t.Errorf("expected first message to be user after dropping leading assistant, got %q", messages[0].Role)
	}
}

func TestTranslateRequest_NoUserMessagesReturnsBedrockAPIError(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "assistant", Content: "Only an assistant turn."},
		},
	}

	_, _, _, err := translateRequest(req)
	if err == nil {
		t.Fatal("expected an error when no user messages remain")
	}
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *BedrockAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", apiErr.StatusCode)
	}
}

func TestTranslateRequest_OnlySystemMessagesReturnsBedrockAPIError(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "system", Content: "You are a helpful assistant."},
		},
	}

	_, _, _, err := translateRequest(req)
	if err == nil {
		t.Fatal("expected an error when the request has no user messages")
	}
}

func TestTranslateRequest_EmptyContentMessagesDropped(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "user", Content: "   "},
			{Role: "user", Content: "Real content"},
		},
	}

	_, messages, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected empty-content message to be dropped, got %d messages", len(messages))
	}
	if textOf(t, messages[0].Content[0]) != "Real content" {
		t.Errorf("unexpected surviving message content: %+v", messages[0])
	}
}

func TestTranslateRequest_EmptySystemMessageDropped(t *testing.T) {
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: "system", Content: ""},
			{Role: "user", Content: "hi"},
		},
	}

	system, _, _, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if len(system) != 0 {
		t.Errorf("expected no system blocks for empty system message, got %d", len(system))
	}
}

// ---------------------------------------------------------------------------
// MaxTokens / Temperature (InferenceConfiguration)
// ---------------------------------------------------------------------------

func TestTranslateRequest_MaxTokensDefaultsWhenUnset(t *testing.T) {
	req := provider.ChatRequest{
		MaxTokens: 0,
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if cfg.MaxTokens == nil || *cfg.MaxTokens != defaultMaxTokens {
		t.Errorf("expected MaxTokens=%d, got %v", defaultMaxTokens, cfg.MaxTokens)
	}
}

func TestTranslateRequest_MaxTokensExplicitValuePreserved(t *testing.T) {
	req := provider.ChatRequest{
		MaxTokens: 512,
		Messages:  []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if cfg.MaxTokens == nil || *cfg.MaxTokens != 512 {
		t.Errorf("expected MaxTokens=512, got %v", cfg.MaxTokens)
	}
}

func TestTranslateRequest_TemperatureNilOmitted(t *testing.T) {
	req := provider.ChatRequest{
		Temperature: nil,
		Messages:    []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if cfg.Temperature != nil {
		t.Errorf("expected nil temperature, got %v", *cfg.Temperature)
	}
}

func TestTranslateRequest_TemperatureNonNilForwarded(t *testing.T) {
	temp := 0.7
	req := provider.ChatRequest{
		Temperature: &temp,
		Messages:    []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if cfg.Temperature == nil {
		t.Fatal("expected temperature to be set, got nil")
	}
	if *cfg.Temperature != 0.7 {
		t.Errorf("expected temperature=0.7, got %f", *cfg.Temperature)
	}
}

func TestTranslateRequest_TopPNilOmitted(t *testing.T) {
	req := provider.ChatRequest{
		TopP:     nil,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if cfg.TopP != nil {
		t.Errorf("expected nil top_p, got %v", *cfg.TopP)
	}
}

func TestTranslateRequest_TopPNonNilForwarded(t *testing.T) {
	topP := 0.9
	req := provider.ChatRequest{
		TopP:     &topP,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if cfg.TopP == nil || *cfg.TopP != 0.9 {
		t.Errorf("expected top_p=0.9, got %v", cfg.TopP)
	}
}

func TestTranslateRequest_StopEmptyOmitted(t *testing.T) {
	req := provider.ChatRequest{
		Stop:     nil,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if cfg.StopSequences != nil {
		t.Errorf("expected nil stop sequences, got %v", cfg.StopSequences)
	}
}

func TestTranslateRequest_StopNonEmptyForwarded(t *testing.T) {
	req := provider.ChatRequest{
		Stop:     []string{"STOP", "END"},
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}

	_, _, cfg, err := translateRequest(req)
	if err != nil {
		t.Fatalf("translateRequest returned error: %v", err)
	}
	if len(cfg.StopSequences) != 2 || cfg.StopSequences[0] != "STOP" || cfg.StopSequences[1] != "END" {
		t.Errorf("expected stop sequences [STOP END], got %v", cfg.StopSequences)
	}
}

// ---------------------------------------------------------------------------
// translateResponse tests
// ---------------------------------------------------------------------------

func converseOutput(text string, stopReason types.StopReason, inputTokens, outputTokens int32) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: text}},
			},
		},
		StopReason: stopReason,
		Usage: &types.TokenUsage{
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
		},
	}
}

func TestTranslateResponse_BasicMapping(t *testing.T) {
	out := converseOutput("Hello, world!", types.StopReasonEndTurn, 10, 5)

	resp, err := translateResponse(out, "anthropic.claude-3-haiku-20240307-v1:0")
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
	out := &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role: types.ConversationRoleAssistant,
				Content: []types.ContentBlock{
					&types.ContentBlockMemberText{Value: "Part one. "},
					&types.ContentBlockMemberText{Value: "Part two."},
				},
			},
		},
		StopReason: types.StopReasonEndTurn,
		Usage:      &types.TokenUsage{},
	}

	resp, err := translateResponse(out, "model")
	if err != nil {
		t.Fatalf("translateResponse returned error: %v", err)
	}

	if resp.Content != "Part one. Part two." {
		t.Errorf("expected concatenated content, got %q", resp.Content)
	}
}

func TestTranslateResponse_NonTextBlocksIgnored(t *testing.T) {
	out := &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role: types.ConversationRoleAssistant,
				Content: []types.ContentBlock{
					&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{ToolUseId: strPtr("123")}},
					&types.ContentBlockMemberText{Value: "Only this."},
				},
			},
		},
		StopReason: types.StopReasonToolUse,
		Usage:      &types.TokenUsage{},
	}

	resp, err := translateResponse(out, "model")
	if err != nil {
		t.Fatalf("translateResponse returned error: %v", err)
	}

	if resp.Content != "Only this." {
		t.Errorf("expected only text block content, got %q", resp.Content)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason=tool_calls, got %q", resp.FinishReason)
	}
}

func TestTranslateResponse_NilOutputReturnsError(t *testing.T) {
	if _, err := translateResponse(nil, "model"); err == nil {
		t.Fatal("expected error for nil Converse output")
	}
}

func TestTranslateResponse_UsageNilDefaultsToZero(t *testing.T) {
	out := &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{
			Value: types.Message{
				Role:    types.ConversationRoleAssistant,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "hi"}},
			},
		},
		StopReason: types.StopReasonEndTurn,
	}

	resp, err := translateResponse(out, "model")
	if err != nil {
		t.Fatalf("translateResponse returned error: %v", err)
	}
	if resp.PromptTokens != 0 || resp.CompletionTokens != 0 {
		t.Errorf("expected zero token counts when Usage is nil, got prompt=%d completion=%d", resp.PromptTokens, resp.CompletionTokens)
	}
}

// ---------------------------------------------------------------------------
// translateStreamEvent tests
// ---------------------------------------------------------------------------

func TestTranslateStreamEvent_ContentBlockDelta(t *testing.T) {
	event := &types.ConverseStreamOutputMemberContentBlockDelta{
		Value: types.ContentBlockDeltaEvent{
			Delta: &types.ContentBlockDeltaMemberText{Value: "Hello"},
		},
	}

	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(event, "test-id", "model-x", stats, &buf, nil)
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

	payload := strings.TrimSuffix(strings.TrimPrefix(output, "data: "), "\n\n")

	var parsed struct {
		Choices []struct {
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("SSE chunk JSON is not valid: %v", err)
	}
	if len(parsed.Choices) == 0 {
		t.Fatal("expected at least one choice in SSE chunk")
	}
	if parsed.Choices[0].Delta.Content != "Hello" {
		t.Errorf("expected delta content=%q, got %q", "Hello", parsed.Choices[0].Delta.Content)
	}
	// Every content chunk already carries role="assistant" (via
	// provider.BuildStreamingChunkSSE) — this matches the pre-Converse
	// behaviour exactly, so no separate initial role-only chunk is emitted.
	if parsed.Choices[0].Delta.Role != "assistant" {
		t.Errorf("expected delta role=assistant, got %q", parsed.Choices[0].Delta.Role)
	}
}

func TestTranslateStreamEvent_ContentBlockDeltaEmptyTextIsNoOp(t *testing.T) {
	event := &types.ConverseStreamOutputMemberContentBlockDelta{
		Value: types.ContentBlockDeltaEvent{
			Delta: &types.ContentBlockDeltaMemberText{Value: ""},
		},
	}
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(event, "id", "model", stats, &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected done=false")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no SSE output for an empty text delta, got: %q", buf.String())
	}
}

func TestTranslateStreamEvent_MessageStart(t *testing.T) {
	event := &types.ConverseStreamOutputMemberMessageStart{
		Value: types.MessageStartEvent{Role: types.ConversationRoleAssistant},
	}
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	_, err := translateStreamEvent(event, "id", "model", stats, &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no SSE output for message_start, got: %q", buf.String())
	}
}

func TestTranslateStreamEvent_MessageStop(t *testing.T) {
	event := &types.ConverseStreamOutputMemberMessageStop{
		Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn},
	}
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(event, "id", "model", stats, &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected done=true for message_stop")
	}
	if stats.FinishReason != "stop" {
		t.Errorf("expected finish_reason=stop (mapped from end_turn), got %q", stats.FinishReason)
	}
	if buf.Len() == 0 {
		t.Error("expected a final SSE chunk to be written on message_stop, got empty buffer")
	}

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

func TestTranslateStreamEvent_MetadataCapturesUsage(t *testing.T) {
	input := int32(42)
	output := int32(17)
	event := &types.ConverseStreamOutputMemberMetadata{
		Value: types.ConverseStreamMetadataEvent{
			Usage: &types.TokenUsage{InputTokens: &input, OutputTokens: &output},
		},
	}
	stats := &provider.StreamStats{}
	var buf bytes.Buffer

	done, err := translateStreamEvent(event, "id", "model", stats, &buf, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("metadata event should not signal done")
	}
	if buf.Len() != 0 {
		t.Errorf("expected no SSE output for metadata event, got: %q", buf.String())
	}
	if stats.PromptTokens != 42 {
		t.Errorf("expected prompt_tokens=42, got %d", stats.PromptTokens)
	}
	if stats.CompletionTokens != 17 {
		t.Errorf("expected completion_tokens=17, got %d", stats.CompletionTokens)
	}
}

func TestTranslateStreamEvent_ContentBlockStartStopAreNoOps(t *testing.T) {
	stats := &provider.StreamStats{}

	startEvent := &types.ConverseStreamOutputMemberContentBlockStart{
		Value: types.ContentBlockStartEvent{},
	}
	var buf1 bytes.Buffer
	if _, err := translateStreamEvent(startEvent, "id", "model", stats, &buf1, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf1.Len() != 0 {
		t.Errorf("expected no SSE output for content_block_start, got: %q", buf1.String())
	}

	stopEvent := &types.ConverseStreamOutputMemberContentBlockStop{
		Value: types.ContentBlockStopEvent{},
	}
	var buf2 bytes.Buffer
	if _, err := translateStreamEvent(stopEvent, "id", "model", stats, &buf2, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf2.Len() != 0 {
		t.Errorf("expected no SSE output for content_block_stop, got: %q", buf2.String())
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
	if !strings.Contains(string(body), "bedrock:InvokeModel") {
		t.Errorf("access denied error should hint at the bedrock:InvokeModel IAM allowlist; got: %s", body)
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
	if strings.Contains(string(body), `"message":""`) {
		t.Errorf("empty rawMessage should be replaced with default; got: %s", body)
	}
}

func TestTranslateBedrockError_ModelNotReady(t *testing.T) {
	code, body := TranslateBedrockError("ModelNotReadyException", "")
	assertErrorResponse(t, code, body, http.StatusServiceUnavailable, "server_error")
}

func TestTranslateBedrockError_ServiceUnavailable(t *testing.T) {
	code, body := TranslateBedrockError("ServiceUnavailableException", "")
	assertErrorResponse(t, code, body, http.StatusServiceUnavailable, "server_error")
}

func TestTranslateBedrockError_ResourceNotFound(t *testing.T) {
	code, body := TranslateBedrockError("ResourceNotFoundException", "")
	assertErrorResponse(t, code, body, http.StatusNotFound, "invalid_request_error")
}

func TestTranslateBedrockError_ModelTimeout(t *testing.T) {
	code, body := TranslateBedrockError("ModelTimeoutException", "")
	assertErrorResponse(t, code, body, http.StatusGatewayTimeout, "server_error")
}

func TestTranslateBedrockError_UnknownDefaultsTo502(t *testing.T) {
	code, body := TranslateBedrockError("SomeOtherException", "weird error")
	assertErrorResponse(t, code, body, http.StatusBadGateway, "server_error")
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
		{"guardrail_intervened", "content_filter"},
		{"finished", "stop"},
		{"complete", "stop"},
		{"END_TURN", "stop"},
		{"", ""},
		// Unknown reasons (including other real Converse StopReason values we
		// don't special-case, like model_context_window_exceeded) must not leak
		// a Bedrock-specific string to an OpenAI-compatible client.
		{"model_context_window_exceeded", "stop"},
		{"unknown_reason", "stop"},
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

func strPtr(s string) *string { return &s }

func TestTranslateResponse_CacheTokens(t *testing.T) {
	out := converseOutput("hi", types.StopReasonEndTurn, 10, 5)
	read, write := int32(70), int32(20)
	out.Usage.CacheReadInputTokens = &read
	out.Usage.CacheWriteInputTokens = &write

	resp, err := translateResponse(out, "m")
	if err != nil {
		t.Fatal(err)
	}
	if resp.PromptTokens != 100 || resp.CacheReadTokens != 70 || resp.CacheCreationTokens != 20 {
		t.Errorf("prompt=%d read=%d create=%d, want 100/70/20 (input+read+write)", resp.PromptTokens, resp.CacheReadTokens, resp.CacheCreationTokens)
	}

	resp, _ = translateResponse(converseOutput("hi", types.StopReasonEndTurn, 10, 5), "m")
	if resp.CacheReadTokens != 0 || resp.CacheCreationTokens != 0 {
		t.Errorf("absent cache fields must be 0, got %d/%d", resp.CacheReadTokens, resp.CacheCreationTokens)
	}
}

func TestTranslateStreamEvent_MetadataCapturesCacheTokens(t *testing.T) {
	input, output, read, write := int32(42), int32(17), int32(30), int32(8)
	event := &types.ConverseStreamOutputMemberMetadata{
		Value: types.ConverseStreamMetadataEvent{
			Usage: &types.TokenUsage{InputTokens: &input, OutputTokens: &output, CacheReadInputTokens: &read, CacheWriteInputTokens: &write},
		},
	}
	stats := &provider.StreamStats{}
	if _, err := translateStreamEvent(event, "id", "model", stats, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	if stats.PromptTokens != 80 || stats.CacheReadTokens != 30 || stats.CacheCreationTokens != 8 {
		t.Errorf("prompt=%d read=%d create=%d, want 80/30/8 (input+read+write)", stats.PromptTokens, stats.CacheReadTokens, stats.CacheCreationTokens)
	}
}
