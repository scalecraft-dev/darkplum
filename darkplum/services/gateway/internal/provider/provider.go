package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

// Message is a single turn in a conversation. Content can be a plain string
// or an array of content blocks (OpenAI multi-part format). UnmarshalJSON
// handles both and normalises to a string for downstream providers.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	type plain struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	m.Role = p.Role

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
	FinishReason     string
}

// StreamStats holds the token-count and finish metadata extracted from the
// final chunk of a streaming response. PromptTokens may be zero if the backend
// does not include usage in the stream (Bedrock's streaming path omits it).
type StreamStats struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
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
	Index        int            `json:"index"`
	Message      *choiceMessage `json:"message,omitempty"`
	Delta        *choiceDelta   `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type choiceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type choiceDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
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
					Role:    "assistant",
					Content: resp.Content,
				},
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

// StreamDoneSSE is the SSE stream terminator. Write this after the final chunk.
const StreamDoneSSE = "data: [DONE]\n\n"
