package openai

import (
	"encoding/json"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// ---------------------------------------------------------------------------
// Request shape (POST /v1/chat/completions)
// ---------------------------------------------------------------------------

// chatMessage is one request turn. Content is a pointer so an assistant turn
// that only carries tool_calls renders "content": null (what OpenAI's own SDKs
// send); every other turn renders its string, "" included.
type chatMessage struct {
	Role       string              `json:"role"`
	Content    *string             `json:"content"`
	ToolCalls  []provider.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	Name       string              `json:"name,omitempty"`
}

// chatRequest is this package's own request builder — there is no
// requirement to use an OpenAI SDK, and this wire shape is already the
// gateway's native OpenAI-compatible surface (see
// internal/provider/openaicompat, which speaks the identical format to a
// self-hosted upstream).
//
// MaxCompletionTokens is ALWAYS sent as max_completion_tokens, never
// max_tokens: the reasoning/gpt-5-family models 400 on max_tokens, and every
// current chat-completions model accepts max_completion_tokens instead.
// Temperature/TopP/Stop are omitted entirely unless the caller set them (or
// this is the one-time adaptive retry that has just dropped one of them —
// see droppedParams).
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	Tools               []provider.Tool `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ---------------------------------------------------------------------------
// Non-streaming response shape
// ---------------------------------------------------------------------------

type chatCompletionResponse struct {
	ID      string           `json:"id"`
	Model   string           `json:"model"`
	Choices []completionChoi `json:"choices"`
	Usage   *completionUsage `json:"usage"`
}

type completionChoi struct {
	Message      *completionMessage `json:"message"`
	FinishReason *string            `json:"finish_reason"`
}

// completionMessage.Refusal carries the model's refusal text when the
// vendor's own safety classifier declines to answer — surfaced as content
// with finish_reason content_filter (see toChatResponse), rather than
// dropped or treated as an error, matching the streaming path's identical
// handling of a refusal delta.
type completionMessage struct {
	Content   string              `json:"content"`
	Refusal   string              `json:"refusal"`
	ToolCalls []provider.ToolCall `json:"tool_calls"`
}

type completionUsage struct {
	PromptTokens        int                 `json:"prompt_tokens"`
	CompletionTokens    int                 `json:"completion_tokens"`
	PromptTokensDetails *promptTokenDetails `json:"prompt_tokens_details,omitempty"`
}

// promptTokenDetails carries usage.prompt_tokens_details; cached_tokens is
// the cache-read share of prompt_tokens (OpenAI has no cache-creation charge).
type promptTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

func (u *completionUsage) cachedTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// ---------------------------------------------------------------------------
// Streaming (SSE) chunk shape
// ---------------------------------------------------------------------------

// streamChunk is one `data: {...}` SSE event. Choices is empty ([]) on the
// terminal usage-only chunk stream_options.include_usage adds — Usage is
// populated there and nowhere else.
type streamChunk struct {
	ID      string           `json:"id"`
	Model   string           `json:"model"`
	Choices []streamChoice   `json:"choices"`
	Usage   *completionUsage `json:"usage"`
	Error   *streamError     `json:"error"`
}

type streamChoice struct {
	Delta        *streamDelta `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type streamDelta struct {
	Content string `json:"content"`
	Refusal string `json:"refusal"`
	// ToolCalls are the streamed tool-call fragments (index, id, type,
	// function.name, function.arguments), re-emitted by Stream.
	ToolCalls []provider.ToolCallDelta `json:"tool_calls"`
}

// streamError is a mid-stream vendor error event: the connection stays open
// (200 already sent) but the vendor reports a failure inline instead of a
// further content delta.
type streamError struct {
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Models list (GET /v1/models) — id only; see discover.go.
// ---------------------------------------------------------------------------

type modelsListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ---------------------------------------------------------------------------
// finish_reason mapping
// ---------------------------------------------------------------------------

// mapFinishReason maps a Chat Completions API finish_reason to the
// OpenAI-compatible finish_reason vocabulary this gateway's own surface
// promises: stop|length|content_filter|tool_calls. tool_calls passes through
// (client-executed tool calling, see provider/tools.go); the legacy
// function_call and any other unrecognised value (present or future) collapse
// to stop, the same generic fallback internal/provider/anthropic uses.
func mapFinishReason(reason string) string {
	switch reason {
	case "length":
		return "length"
	case "content_filter":
		return "content_filter"
	case "tool_calls":
		return "tool_calls"
	default:
		return "stop"
	}
}
