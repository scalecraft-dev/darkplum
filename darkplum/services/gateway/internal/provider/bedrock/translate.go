// Package bedrock handles all format translation between the OpenAI-compatible
// ChatRequest/ChatResponse types and the Bedrock Anthropic Messages API wire
// format. No HTTP or AWS SDK logic lives here — bedrock.go owns that layer.
// All translation functions are pure so they can be tested without any AWS
// SDK involvement.
package bedrock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// bedrockRequest is the JSON body sent to InvokeModel /
// InvokeModelWithResponseStream for Anthropic Claude models on Bedrock.
type bedrockRequest struct {
	AnthropicVersion string           `json:"anthropic_version"`
	MaxTokens        int              `json:"max_tokens"`
	System           string           `json:"system,omitempty"`
	Temperature      *float64         `json:"temperature,omitempty"`
	Messages         []bedrockMessage `json:"messages"`
}

type bedrockMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// bedrockResponse is the non-streaming JSON body returned by InvokeModel.
type bedrockResponse struct {
	Content    []bedrockContentBlock `json:"content"`
	StopReason string                `json:"stop_reason"`
	Usage      bedrockUsage          `json:"usage"`
}

type bedrockContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type bedrockUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// streamEvent is the discriminated union for all Bedrock streaming event
// payloads. All event types arrive as raw JSON bytes inside
// ResponseStreamMemberChunk.Value.Bytes; we decode into this struct and switch
// on Type.
type streamEvent struct {
	Type string `json:"type"`

	// message_start: carries input token count.
	Message *struct {
		Usage bedrockUsage `json:"usage"`
	} `json:"message,omitempty"`

	// content_block_delta: carries the token text.
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`        // present when type == "text_delta"
		StopReason string `json:"stop_reason"` // present in message_delta
	} `json:"delta,omitempty"`

	// message_delta: carries output token count alongside delta.
	Usage *bedrockUsage `json:"usage,omitempty"`
}

// translateRequest converts a provider.ChatRequest to the Bedrock Anthropic
// Messages API wire format.
//
// Translation rules:
//   - A message with role "system" is extracted and placed in the top-level
//     system field; multiple system messages are concatenated with "\n".
//   - Only "user" and "assistant" roles appear in the messages array.
//   - MaxTokens of 0 defaults to 4096 (Bedrock requires max_tokens).
//   - Temperature is forwarded only when non-nil.
func translateRequest(req provider.ChatRequest) ([]byte, error) {
	br := bedrockRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        req.MaxTokens,
		Temperature:      req.Temperature,
	}

	if br.MaxTokens == 0 {
		br.MaxTokens = 4096
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			// Bedrock takes exactly one system prompt as a top-level field.
			// Concatenate multiple system messages with a newline rather than
			// silently discarding all but the first.
			if br.System == "" {
				br.System = m.Content
			} else {
				br.System += "\n" + m.Content
			}
		case "user", "assistant":
			br.Messages = append(br.Messages, bedrockMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		default:
			// Unknown roles are dropped — Bedrock only accepts "user" and
			// "assistant" in the messages array.
		}
	}

	// If the caller sends only system messages (degenerate input), we let
	// Bedrock return the ValidationException rather than masking it here.

	return json.Marshal(br)
}

// translateResponse converts the raw Bedrock InvokeModel response body to a
// provider.ChatResponse. modelID must be passed in because Bedrock does not
// echo it back in the response body.
func translateResponse(body []byte, modelID string) (*provider.ChatResponse, error) {
	var br bedrockResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return nil, fmt.Errorf("bedrock: unmarshal response: %w", err)
	}

	// Collect all text content blocks. In practice Claude returns exactly one
	// for a basic completion, but the spec allows multiples.
	var content string
	for _, block := range br.Content {
		if block.Type == "text" {
			content += block.Text
		}
	}

	return &provider.ChatResponse{
		// Bedrock does not return a completion ID; the gateway assigns one at a
		// higher layer if needed.
		ID:               "",
		Model:            modelID,
		Content:          content,
		PromptTokens:     br.Usage.InputTokens,
		CompletionTokens: br.Usage.OutputTokens,
		FinishReason:     convertFinishReason(br.StopReason),
	}, nil
}

// translateStreamEvent processes one raw event JSON payload from the Bedrock
// response stream. It writes zero or more SSE chunks to w using the shared
// provider SSE helpers (provider.BuildStreamingChunkSSE,
// provider.BuildFinalChunkSSE) and updates stats in place.
//
// Returns done=true when a "message_stop" event is received, signalling the
// caller should exit the event loop.
func translateStreamEvent(
	rawEvent []byte,
	completionID string,
	modelID string,
	stats *provider.StreamStats,
	w io.Writer,
) (done bool, err error) {
	var ev streamEvent
	if err := json.Unmarshal(rawEvent, &ev); err != nil {
		return false, fmt.Errorf("bedrock: unmarshal stream event: %w", err)
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			stats.PromptTokens = ev.Message.Usage.InputTokens
		}

	case "content_block_start":
		// Marks the opening of a content block. No SSE output needed.

	case "content_block_delta":
		if ev.Delta != nil && ev.Delta.Type == "text_delta" {
			chunk, err := provider.BuildStreamingChunkSSE(completionID, modelID, ev.Delta.Text)
			if err != nil {
				return false, fmt.Errorf("bedrock: build SSE chunk: %w", err)
			}
			if _, err := w.Write(chunk); err != nil {
				return false, fmt.Errorf("bedrock: write SSE chunk: %w", err)
			}
		}

	case "content_block_stop":
		// Marks the end of a content block. No SSE output needed.

	case "message_delta":
		if ev.Delta != nil {
			stats.FinishReason = convertFinishReason(ev.Delta.StopReason)
		}
		if ev.Usage != nil {
			stats.CompletionTokens = ev.Usage.OutputTokens
		}
		// Emit the terminal chunk with finish_reason and accumulated usage.
		finalChunk, err := provider.BuildFinalChunkSSE(completionID, modelID, stats.FinishReason, stats)
		if err != nil {
			return false, fmt.Errorf("bedrock: build final SSE chunk: %w", err)
		}
		if _, err := w.Write(finalChunk); err != nil {
			return false, fmt.Errorf("bedrock: write final SSE chunk: %w", err)
		}

	case "message_stop":
		return true, nil
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
			Message: "Model access denied - check IAM permissions",
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
	case "ModelNotReadyException":
		statusCode = http.StatusServiceUnavailable
		detail = errDetail{
			Message: "Model not available",
			Type:    "server_error",
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
	default:
		statusCode = http.StatusInternalServerError
		detail = errDetail{
			Message: "Model service error",
			Type:    "server_error",
		}
	}

	body, _ = json.Marshal(errEnvelope{Error: detail})
	return statusCode, body
}

// convertFinishReason maps Bedrock stop reasons to OpenAI finish_reason values.
// Bedrock uses "end_turn", "max_tokens", etc.; OpenAI clients expect "stop",
// "length", "tool_calls".
func convertFinishReason(reason string) string {
	if reason == "" {
		return ""
	}
	mapping := map[string]string{
		"end_turn":         "stop",
		"stop_sequence":    "stop",
		"finished":         "stop",
		"complete":         "stop",
		"max_tokens":       "length",
		"tool_use":         "tool_calls",
		"content_filtered": "content_filter",
	}
	if mapped, ok := mapping[strings.ToLower(reason)]; ok {
		return mapped
	}
	return strings.ToLower(reason)
}
