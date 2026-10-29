package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Provider is the interface every backend (vLLM, Bedrock, Vertex, Azure OpenAI)
// must implement. The gateway routes requests through this interface so that all
// enforcement, recording, and SSE formatting logic is backend-agnostic.
//
// Both Complete and Stream accept a ChatRequest in Darkplum's internal format and
// return structured results. Callers are responsible for rendering those results
// into the OpenAI-compatible HTTP response using the helpers in this package.
type Provider interface {
	// Complete sends a non-streaming chat request and returns the full response.
	Complete(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// Stream sends a streaming chat request. It writes raw OpenAI SSE bytes to w
	// (including the final "data: [DONE]\n\n" terminator) and returns token-count
	// statistics extracted from the response stream.
	Stream(ctx context.Context, req ChatRequest, w io.Writer) (*StreamStats, error)

	// Name returns a short identifier for the backend (e.g. "vllm", "bedrock").
	// Used in log lines and audit events.
	Name() string
}

// ChatRequest is the backend-agnostic representation of an OpenAI-compatible
// chat completion request. Providers translate this into their native wire format.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Stream      bool
	MaxTokens   int
	Temperature *float64
	// TopP is forwarded to every provider when the caller set it (nil
	// otherwise). Unlike Temperature, no provider today needs to retry
	// without it — this is a plain passthrough field.
	TopP *float64
	// Stop is forwarded to every provider when the caller set it. Empty
	// (nil/zero-length) means the caller did not set `stop`.
	Stop []string
	// Tools and ToolChoice are client-executed tool calling (see tools.go).
	// Both are validated by the handler and only ever reach a provider that
	// reports ToolCapable. ToolChoice is the raw OpenAI value ("auto",
	// "none", "required" or a named function) or empty when the caller sent
	// none/null. Tools are ALWAYS forwarded when present, "none" included:
	// a history that already carries tool turns is only valid to every
	// backend with the tool definitions alongside it. A provider renders
	// "none" in its backend's own way (OpenAI-shaped backends take it
	// verbatim; Anthropic has tool_choice {type:"none"}; Bedrock Converse
	// has no equivalent and keeps the definitions with automatic choice,
	// which is the closest it offers -- see each provider).
	Tools      []Tool
	ToolChoice json.RawMessage
	// JSONMode is response_format {"type":"json_object"}. Only ever true for
	// a provider whose ParamCaps().JSONMode is set; the handler refuses it
	// for every other backend.
	JSONMode bool
	// IncludeUsage is stream_options.include_usage on a stream: the provider
	// ends the stream with OpenAI's separate usage chunk (choices: []). When
	// false a stream keeps usage on the finish_reason chunk, as before.
	IncludeUsage bool

	// The fields below are set by the handler only for a request the resolved
	// backend can honour them on (see ParamCaps), and only when the caller
	// sent a non-default value; a provider forwards what it is given.

	// Seed is the OpenAI `seed`.
	Seed *int64
	// FrequencyPenalty and PresencePenalty are non-zero values only.
	FrequencyPenalty *float64
	PresencePenalty  *float64
	// Logprobs is logprobs:true; TopLogprobs is the caller's top_logprobs
	// (only meaningful with Logprobs). The provider returns the logprobs in
	// ChatResponse.Logprobs / on the stream chunks, in OpenAI's shape.
	Logprobs    bool
	TopLogprobs *int
	// LogitBias is a non-empty logit_bias (token id -> bias).
	LogitBias map[string]float64
	// ParallelToolCalls is non-nil only for parallel_tool_calls:false on a
	// request that carries tools.
	ParallelToolCalls *bool
	// NoStore is store:false (store:true is refused by the handler).
	NoStore bool
	// User is the `user` value in the form this backend is to receive it:
	// raw, or already pseudonymised, per ParamCaps().UserID. Empty means
	// send nothing.
	User string
	// ReasoningEffort is the OpenAI reasoning_effort, set only for a
	// provider whose PlanReasoning said ReasoningForward (OpenAI).
	ReasoningEffort string
	// ThinkingBudget > 0 turns Anthropic extended thinking on with that
	// budget_tokens, set only after PlanReasoning said ReasoningThinking.
	// MaxTokens already exceeds it, and the handler has unset Temperature
	// (thinking forbids it). Thinking output is never returned to the caller.
	ThinkingBudget int
	// VLLM carries the vLLM sampling extensions; only the openaicompat
	// provider reads it.
	VLLM VLLMExtensions
}

// VLLMExtensions are request fields outside the OpenAI schema that vLLM's
// OpenAI-compatible server accepts. Forwarded by the openaicompat provider
// only; every other backend ignores them, as it always has.
type VLLMExtensions struct {
	TopK              *int
	MinP              *float64
	RepetitionPenalty *float64
	// GuidedJSON is the JSON schema (or schema string) for guided decoding,
	// relayed as sent.
	GuidedJSON json.RawMessage
	// ChatTemplateKwargs is a JSON object handed to the chat template.
	ChatTemplateKwargs json.RawMessage
}

// Message is a single turn in a conversation. Content can be a plain string
// or an array of content blocks (OpenAI multi-part format). UnmarshalJSON
// handles both and normalises to a string for downstream providers.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is set on an assistant turn that asked for tools to be run
	// (request history). Content is usually empty on such a turn.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on a role:tool turn and names the assistant
	// tool_calls entry this turn is the result of.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name is the optional OpenAI participant/function name on a turn.
	Name string `json:"name,omitempty"`
	// HasNonTextContent is true when the original content was an array of
	// content blocks containing at least one non-"text" block (e.g.
	// image_url). Content itself already drops non-text blocks when
	// flattening to a plain string (see UnmarshalJSON); this is the one
	// signal that flattening would otherwise lose, needed by a routed
	// alias's builtin:multimodal rules signal (see internal/router). Always
	// false for a plain string content body.
	HasNonTextContent bool `json:"-"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	type plain struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	m.Role = p.Role
	m.ToolCalls = p.ToolCalls
	m.ToolCallID = p.ToolCallID
	m.Name = p.Name

	// Absent or null content (an assistant tool_calls turn is sent with
	// "content": null by every OpenAI SDK) is empty, not the literal text
	// "null".
	if trimmed := strings.TrimSpace(string(p.Content)); trimmed == "" || trimmed == "null" {
		m.Content = ""
		return nil
	}

	// Try string first.
	var s string
	if err := json.Unmarshal(p.Content, &s); err == nil {
		m.Content = s
		return nil
	}

	// Array of content blocks: [{"type":"text","text":"..."},...]
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(p.Content, &blocks); err == nil {
		var combined string
		for _, b := range blocks {
			if b.Type == "text" {
				combined += b.Text
			} else {
				m.HasNonTextContent = true
			}
		}
		m.Content = combined
		return nil
	}

	m.Content = string(p.Content)
	return nil
}

// ChatResponse holds the result of a non-streaming completion.
type ChatResponse struct {
	ID               string
	Model            string
	Content          string
	PromptTokens     int
	CompletionTokens int
	// Invariant: prompt_tokens includes cache tokens; cache_* are its split
	// (priced differently per vendor); 0 when the backend reports none.
	CacheReadTokens     int
	CacheCreationTokens int
	FinishReason        string
	// ToolCalls is what the model asked the caller to run; FinishReason is
	// "tool_calls" when non-empty.
	ToolCalls []ToolCall
	// Logprobs is the choice's logprobs object ({"content":[...],
	// "refusal":...}) relayed as the backend sent it; nil when the caller
	// did not ask for logprobs or the backend sent none.
	Logprobs json.RawMessage
}

// StreamStats holds the token-count and finish metadata extracted from the
// final chunk of a streaming response. PromptTokens may be zero if the backend
// does not include usage in the stream (Bedrock's streaming path omits it).
type StreamStats struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	// Invariant: prompt_tokens includes cache tokens; cache_* are its split.
	CacheReadTokens     int
	CacheCreationTokens int
	FinishReason        string
}

// ---------------------------------------------------------------------------
// OpenAI-format response helpers
//
// Both vLLM and Bedrock must emit an OpenAI-compatible surface. These helpers
// build the wire-format JSON so each provider doesn't reimplement serialization.
// ---------------------------------------------------------------------------

// openAICompletion is the full non-streaming response shape.
type openAICompletion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usageInfo          `json:"usage"`
}

type completionChoice struct {
	Index        int             `json:"index"`
	Message      *choiceMessage  `json:"message,omitempty"`
	Delta        *choiceDelta    `json:"delta,omitempty"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
	FinishReason *string         `json:"finish_reason"`
}

type choiceMessage struct {
	Role string `json:"role"`
	// Content is null, not "", on a turn that only carries tool_calls --
	// the shape OpenAI SDKs expect.
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type choiceDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

type usageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// BuildCompletionJSON serialises a ChatResponse into a full OpenAI
// chat.completion JSON body. The caller should set Content-Type:
// application/json before writing this to the HTTP response.
func BuildCompletionJSON(resp *ChatResponse) ([]byte, error) {
	finishReason := resp.FinishReason
	completion := openAICompletion{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []completionChoice{
			{
				Index: 0,
				Message: &choiceMessage{
					Role:      "assistant",
					Content:   completionContent(resp),
					ToolCalls: resp.ToolCalls,
				},
				Logprobs:     NormalizeLogprobs(resp.Logprobs),
				FinishReason: &finishReason,
			},
		},
		Usage: usageInfo{
			PromptTokens:     resp.PromptTokens,
			CompletionTokens: resp.CompletionTokens,
			TotalTokens:      resp.PromptTokens + resp.CompletionTokens,
		},
	}
	return json.Marshal(completion)
}

// completionContent is the message content for a non-streaming response:
// null when the turn is only tool calls, the text otherwise ("" included).
func completionContent(resp *ChatResponse) *string {
	if resp.Content == "" && len(resp.ToolCalls) > 0 {
		return nil
	}
	c := resp.Content
	return &c
}

// BuildToolCallDeltaSSE builds a single SSE data line carrying one tool-call
// fragment in the OpenAI chat.completion.chunk shape (delta.tool_calls with
// index/id/type/function). No role is set: an SDK stream accumulator
// concatenates string fields across deltas, and OpenAI itself sends the role
// only on a stream's first chunk.
func BuildToolCallDeltaSSE(id, model string, delta ToolCallDelta) ([]byte, error) {
	return BuildToolCallDeltaLogprobsSSE(id, model, delta, nil)
}

// BuildToolCallDeltaLogprobsSSE is BuildToolCallDeltaSSE for a chunk that also
// carries the choice's logprobs.
func BuildToolCallDeltaLogprobsSSE(id, model string, delta ToolCallDelta, logprobs json.RawMessage) ([]byte, error) {
	noReason := (*string)(nil)
	chunk := openAICompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{
			{
				Index:        0,
				Delta:        &choiceDelta{ToolCalls: []ToolCallDelta{delta}},
				Logprobs:     NormalizeLogprobs(logprobs),
				FinishReason: noReason,
			},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "data: %s\n\n", data), nil
}

// NormalizeLogprobs returns raw, or nil when it is absent or the JSON null a
// backend sends for "no logprobs on this choice", so the field is omitted
// rather than relayed as an empty object.
func NormalizeLogprobs(raw json.RawMessage) json.RawMessage {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return nil
	}
	return raw
}

// BuildStreamingChunkLogprobsSSE is BuildStreamingChunkSSE for a delta that
// also carries the choice's logprobs (OpenAI places them beside delta, on the
// choice). With empty logprobs it is identical to BuildStreamingChunkSSE.
func BuildStreamingChunkLogprobsSSE(id, model, contentDelta string, logprobs json.RawMessage) ([]byte, error) {
	noReason := (*string)(nil)
	chunk := openAICompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{
			{
				Index:        0,
				Delta:        &choiceDelta{Role: "assistant", Content: contentDelta},
				Logprobs:     NormalizeLogprobs(logprobs),
				FinishReason: noReason,
			},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "data: %s\n\n", data), nil
}

// BuildStreamingChunkSSE builds a single SSE data line carrying a content
// delta in the OpenAI chat.completion.chunk format. The returned bytes include
// the "data: " prefix and "\n\n" suffix so callers can write them directly to
// the response body.
//
// id and model must match the values used in BuildFinalChunkSSE so the client
// sees a consistent stream.
func BuildStreamingChunkSSE(id, model, contentDelta string) ([]byte, error) {
	noReason := (*string)(nil)
	chunk := openAICompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{
			{
				Index: 0,
				Delta: &choiceDelta{
					Role:    "assistant",
					Content: contentDelta,
				},
				FinishReason: noReason,
			},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "data: %s\n\n", data), nil
}

// BuildFinalChunkSSE builds the terminal SSE chunk that carries finish_reason
// and usage. Follows the OpenAI streaming convention where the last non-DONE
// chunk has an empty delta and usage populated (some clients rely on this for
// token accounting).
func BuildFinalChunkSSE(id, model, finishReason string, stats *StreamStats) ([]byte, error) {
	fr := finishReason
	chunk := openAICompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{
			{
				Index:        0,
				Delta:        &choiceDelta{},
				FinishReason: &fr,
			},
		},
		Usage: usageInfo{
			PromptTokens:     stats.PromptTokens,
			CompletionTokens: stats.CompletionTokens,
			TotalTokens:      stats.PromptTokens + stats.CompletionTokens,
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "data: %s\n\n", data), nil
}

// BuildFinishChunkSSE builds the finish_reason chunk of a stream whose caller
// asked for stream_options.include_usage: OpenAI's shape, with usage null on
// it (the usage follows in its own chunk, see BuildUsageChunkSSE).
func BuildFinishChunkSSE(id, model, finishReason string) ([]byte, error) {
	fr := finishReason
	return marshalChunk(usageChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{{Index: 0, Delta: &choiceDelta{}, FinishReason: &fr}},
	})
}

// BuildUsageChunkSSE builds the usage-only chunk stream_options.include_usage
// adds after the finish_reason chunk: choices is an empty array and usage is
// set, exactly what OpenAI's own SDK stream accumulators expect.
func BuildUsageChunkSSE(id, model string, stats *StreamStats) ([]byte, error) {
	return marshalChunk(usageChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []completionChoice{},
		Usage: &usageInfo{
			PromptTokens:     stats.PromptTokens,
			CompletionTokens: stats.CompletionTokens,
			TotalTokens:      stats.PromptTokens + stats.CompletionTokens,
		},
	})
}

// BuildFinalChunksSSE is the stream's terminal chunk(s) for a provider that
// knows its usage by the time it finishes. Without include_usage it is
// BuildFinalChunkSSE unchanged (usage on the finish_reason chunk, which
// callers that never asked for stream_options already receive and may rely
// on); with it, the finish_reason chunk carries usage null and the usage
// follows in its own choices: [] chunk.
func BuildFinalChunksSSE(id, model, finishReason string, stats *StreamStats, includeUsage bool) ([]byte, error) {
	if !includeUsage {
		return BuildFinalChunkSSE(id, model, finishReason, stats)
	}
	finish, err := BuildFinishChunkSSE(id, model, finishReason)
	if err != nil {
		return nil, err
	}
	usage, err := BuildUsageChunkSSE(id, model, stats)
	if err != nil {
		return nil, err
	}
	return append(finish, usage...), nil
}

// usageChunk is a chat.completion.chunk whose usage is a pointer, so it
// renders null (finish chunk) or an object (usage chunk) rather than the
// always-present zero object openAICompletion carries.
type usageChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *usageInfo         `json:"usage"`
}

func marshalChunk(c usageChunk) ([]byte, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "data: %s\n\n", data), nil
}

// StreamDoneSSE is the SSE stream terminator. Write this after the final chunk.
const StreamDoneSSE = "data: [DONE]\n\n"

// ---------------------------------------------------------------------------
// Cross-provider error/metadata contracts
// ---------------------------------------------------------------------------

// Regional is implemented by a provider whose backend is bound to a single
// cloud region (Bedrock today; a future GCP Vertex provider would too).
// internal/handler's Recording integration reads Region() to populate
// inference_events.region without a second per-request lookup or a
// provider-package-specific type assertion.
type Regional interface {
	Region() string
}

// APIError is implemented by a provider's own error type when it wants to
// report a structured status/body/retry-after rather than have the gateway
// treat it as an opaque upstream failure. bedrock.BedrockAPIError implements
// this (see its HTTPStatus/ResponseBody/RetryAfterSeconds methods, added
// alongside its existing StatusCode/Body/Cause fields, which callers that
// already type-assert *bedrock.BedrockAPIError directly keep working
// unchanged); future vendor providers (anthropic, openai) will too, mapping
// their own HTTP statuses into the wire contract's closed set (429 with
// Retry-After, 502 provider_auth_failed, 502 upstream_error, 504
// upstream_timeout).
//
// handler.handleProviderError and handler.writeStreamError use this
// interface — never err.Error() — to decide what a caller sees, so an
// upstream's raw error text (which can carry internal hostnames, vendor
// request ids, or account detail) never reaches the client on any path,
// including the SSE partial-stream error path, which historically echoed it.
type APIError interface {
	error
	// HTTPStatus is the status code to write to the client.
	HTTPStatus() int
	// ResponseBody is the fully-formed OpenAI-compatible JSON error body to
	// write verbatim (or to embed in an SSE "data:" frame for a mid-stream
	// failure).
	ResponseBody() []byte
	// RetryAfterSeconds is the value to echo on a 429 response's Retry-After
	// header, or 0 if the provider did not report one / the error is not a
	// rate limit.
	RetryAfterSeconds() int
}
