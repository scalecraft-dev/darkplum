// Package bedrock handles all format translation between the OpenAI-compatible
// ChatRequest/ChatResponse types and the Bedrock Converse / ConverseStream API.
// No HTTP or AWS SDK client-construction logic lives here — bedrock.go owns
// that layer. All translation functions are pure so they can be tested without
// any AWS SDK involvement (only the Converse* input/output/types structs are
// used, never a live client).
package bedrock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// defaultMaxTokens is used when the caller does not specify max_tokens.
// Converse requires an explicit value for some models; this matches the
// default used by the previous InvokeModel-based implementation.
const defaultMaxTokens = 4096

// translateRequest converts a provider.ChatRequest into the fields needed for
// a Converse/ConverseStream call: the system blocks, the message list, and the
// inference config. It is shared by Complete and Stream since both APIs take
// the same shape.
//
// Translation rules:
//   - A message with role "system" is extracted into a System content block;
//     multiple system messages are concatenated with "\n" into a single block
//     (matching the previous InvokeModel behaviour of one system field).
//   - Only "user" and "assistant" roles enter the Messages list; other roles
//     are dropped, as before.
//   - Messages with empty (or whitespace-only) content are dropped entirely —
//     Converse rejects empty text content blocks.
//   - Consecutive messages with the same role are merged into a single
//     message (content joined with "\n"), because Converse requires roles to
//     strictly alternate.
//   - Converse also requires the first message to have role "user". Leading
//     assistant messages (before the first user message) have no addressed
//     user turn to respond to and are dropped rather than rejected outright —
//     this tolerates a caller that opens a conversation with a canned
//     assistant greeting before any user input. If no user message remains
//     after this filtering, translateRequest returns a 400-style
//     BedrockAPIError rather than sending Converse a request guaranteed to
//     fail validation.
//   - MaxTokens of 0 defaults to defaultMaxTokens (Converse's per-model
//     default is not always what we want as a floor).
//   - Temperature is forwarded only when non-nil.
func translateRequest(req provider.ChatRequest) (system []types.SystemContentBlock, messages []types.Message, inferenceConfig *types.InferenceConfiguration, err error) {
	var systemText strings.Builder
	type turn struct {
		role    types.ConversationRole
		content string
	}
	var turns []turn

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			if systemText.Len() > 0 {
				systemText.WriteString("\n")
			}
			systemText.WriteString(m.Content)
		case "user", "assistant":
			if strings.TrimSpace(m.Content) == "" {
				// Empty-content messages must not produce empty text blocks —
				// Converse rejects them. Drop rather than send a blank turn.
				continue
			}
			role := types.ConversationRoleUser
			if m.Role == "assistant" {
				role = types.ConversationRoleAssistant
			}
			if len(turns) > 0 && turns[len(turns)-1].role == role {
				// Merge consecutive same-role turns — Converse requires strict
				// alternation.
				turns[len(turns)-1].content += "\n" + m.Content
			} else {
				turns = append(turns, turn{role: role, content: m.Content})
			}
		default:
			// Unknown roles are dropped — Converse only accepts "user" and
			// "assistant" turns.
		}
	}

	// Drop any leading assistant turn(s). After the merge above there can be at
	// most one leading assistant turn (its neighbour, if any, is a different
	// role by construction).
	for len(turns) > 0 && turns[0].role == types.ConversationRoleAssistant {
		turns = turns[1:]
	}

	if len(turns) == 0 {
		statusCode, body := TranslateBedrockError("ValidationException", "at least one user message is required")
		return nil, nil, nil, &BedrockAPIError{StatusCode: statusCode, Body: body}
	}

	for _, t := range turns {
		messages = append(messages, types.Message{
			Role:    t.role,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: t.content}},
		})
	}

	if systemText.Len() > 0 {
		system = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: systemText.String()},
		}
	}

	maxTokens := int32(req.MaxTokens)
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	inferenceConfig = &types.InferenceConfiguration{
		MaxTokens: int32Ptr(maxTokens),
	}
	if req.Temperature != nil {
		temp := float32(*req.Temperature)
		inferenceConfig.Temperature = &temp
	}

	return system, messages, inferenceConfig, nil
}

// int32Ptr is a tiny local helper (avoids importing the aws package just for
// aws.Int32 in this file, which otherwise deals only in SDK "types").
func int32Ptr(v int32) *int32 { return &v }

// translateResponse converts a Converse response into a provider.ChatResponse.
// modelID must be passed in because Converse does not echo the model ID back
// on the response.
func translateResponse(out *bedrockruntime.ConverseOutput, modelID string) (*provider.ChatResponse, error) {
	if out == nil {
		return nil, fmt.Errorf("bedrock: nil Converse output")
	}

	var content string
	if msgOutput, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
		for _, block := range msgOutput.Value.Content {
			if textBlock, ok := block.(*types.ContentBlockMemberText); ok {
				content += textBlock.Value
			}
		}
	}

	var promptTokens, completionTokens int
	if out.Usage != nil {
		if out.Usage.InputTokens != nil {
			promptTokens = int(*out.Usage.InputTokens)
		}
		if out.Usage.OutputTokens != nil {
			completionTokens = int(*out.Usage.OutputTokens)
		}
	}

	return &provider.ChatResponse{
		// Converse does not return a completion ID; the gateway assigns one at
		// a higher layer if needed.
		ID:               "",
		Model:            modelID,
		Content:          content,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		FinishReason:     convertFinishReason(string(out.StopReason)),
	}, nil
}

// translateStreamEvent processes one decoded ConverseStream event. It writes
// zero or more SSE chunks to w using the shared provider SSE helpers
// (provider.BuildStreamingChunkSSE, provider.BuildFinalChunkSSE) and updates
// stats in place.
//
// Returns done=true when a MessageStop event is received: the final SSE chunk
// carrying finish_reason is written here. done is informational only — the
// caller (BedrockProvider.Stream) does NOT exit its loop on done=true, because
// Converse sends the trailing Metadata event (token usage) *after*
// MessageStop; the caller instead exits when the events channel closes, so
// that event is not missed and streaming usage is not silently dropped.
func translateStreamEvent(
	event types.ConverseStreamOutput,
	completionID string,
	modelID string,
	stats *provider.StreamStats,
	w io.Writer,
) (done bool, err error) {
	switch ev := event.(type) {
	case *types.ConverseStreamOutputMemberContentBlockDelta:
		textDelta, ok := ev.Value.Delta.(*types.ContentBlockDeltaMemberText)
		if !ok || textDelta.Value == "" {
			return false, nil
		}
		chunk, err := provider.BuildStreamingChunkSSE(completionID, modelID, textDelta.Value)
		if err != nil {
			return false, fmt.Errorf("bedrock: build SSE chunk: %w", err)
		}
		if _, err := w.Write(chunk); err != nil {
			return false, fmt.Errorf("bedrock: write SSE chunk: %w", err)
		}

	case *types.ConverseStreamOutputMemberMessageStop:
		stats.FinishReason = convertFinishReason(string(ev.Value.StopReason))
		finalChunk, err := provider.BuildFinalChunkSSE(completionID, modelID, stats.FinishReason, stats)
		if err != nil {
			return false, fmt.Errorf("bedrock: build final SSE chunk: %w", err)
		}
		if _, err := w.Write(finalChunk); err != nil {
			return false, fmt.Errorf("bedrock: write final SSE chunk: %w", err)
		}
		return true, nil

	case *types.ConverseStreamOutputMemberMetadata:
		if ev.Value.Usage != nil {
			if ev.Value.Usage.InputTokens != nil {
				stats.PromptTokens = int(*ev.Value.Usage.InputTokens)
			}
			if ev.Value.Usage.OutputTokens != nil {
				stats.CompletionTokens = int(*ev.Value.Usage.OutputTokens)
			}
		}

	case *types.ConverseStreamOutputMemberMessageStart,
		*types.ConverseStreamOutputMemberContentBlockStart,
		*types.ConverseStreamOutputMemberContentBlockStop:
		// No SSE output needed for these markers.

	default:
		// Forward-compatible: future event types (e.g. tool-use deltas) are
		// silently ignored rather than erroring the whole stream.
	}

	return false, nil
}

// TranslateBedrockError converts a Bedrock SDK error code to an HTTP status
// and an OpenAI-compatible JSON error body. The errorCode parameter is the
// smithy fault type name (e.g. "ThrottlingException"). rawMessage is forwarded
// as-is for ValidationException so the caller sees Bedrock's validation detail.
func TranslateBedrockError(errorCode string, rawMessage string) (statusCode int, body []byte) {
	type errDetail struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	type errEnvelope struct {
		Error errDetail `json:"error"`
	}

	var detail errDetail
	switch errorCode {
	case "ThrottlingException":
		statusCode = http.StatusTooManyRequests
		detail = errDetail{
			Message: "Rate limit exceeded",
			Type:    "rate_limit_error",
		}
	case "AccessDeniedException":
		statusCode = http.StatusForbidden
		detail = errDetail{
			Message: "Access denied invoking this model — the gateway's IAM role bedrock:InvokeModel allowlist may not include this model",
			Type:    "permission_error",
		}
	case "ValidationException":
		statusCode = http.StatusBadRequest
		msg := rawMessage
		if msg == "" {
			msg = "Invalid request"
		}
		detail = errDetail{
			Message: msg,
			Type:    "invalid_request_error",
		}
	case "ResourceNotFoundException":
		statusCode = http.StatusNotFound
		msg := rawMessage
		if msg == "" {
			msg = "Model not found or no longer available"
		}
		detail = errDetail{
			Message: msg,
			Type:    "invalid_request_error",
		}
	case "ModelNotReadyException", "ServiceUnavailableException":
		statusCode = http.StatusServiceUnavailable
		detail = errDetail{
			Message: "Model not available",
			Type:    "server_error",
		}
	case "ModelTimeoutException":
		statusCode = http.StatusGatewayTimeout
		detail = errDetail{
			Message: "Model timed out generating a response",
			Type:    "server_error",
		}
	default:
		statusCode = http.StatusBadGateway
		detail = errDetail{
			Message: "Model service error",
			Type:    "server_error",
		}
	}

	body, _ = json.Marshal(errEnvelope{Error: detail})
	return statusCode, body
}

// convertFinishReason maps Bedrock/Converse stop reasons to OpenAI
// finish_reason values.
func convertFinishReason(reason string) string {
	if reason == "" {
		return ""
	}
	mapping := map[string]string{
		"end_turn":             "stop",
		"stop_sequence":        "stop",
		"finished":             "stop",
		"complete":             "stop",
		"max_tokens":           "length",
		"tool_use":             "tool_calls",
		"content_filtered":     "content_filter",
		"guardrail_intervened": "content_filter",
	}
	lower := strings.ToLower(reason)
	if mapped, ok := mapping[lower]; ok {
		return mapped
	}
	// Unknown stop reasons (including malformed_model_output,
	// malformed_tool_use, model_context_window_exceeded) map to "stop" rather
	// than leaking a Bedrock-specific string to an OpenAI-compatible client.
	return "stop"
}
