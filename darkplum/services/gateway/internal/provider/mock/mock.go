package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

const (
	providerName = "mock"
	modelID      = "mock-model"
	mockContent  = "This is a mock response from the local development environment."
)

// MockProvider is the local-development backend. It also stands in for a
// tool-capable backend so the tool-calling loop can be exercised end to end
// without a real model:
//
//   - when the request carries tools and the last user turn starts with
//     "call <tool name>" (the rest of the line, if any, is the JSON
//     arguments), the reply is a single tool call to that tool;
//   - when the last turn is a tool result, the reply echoes it as text.
//
// Everything else answers the fixed mockContent, as before.
type MockProvider struct{}

func New() *MockProvider {
	return &MockProvider{}
}

func (p *MockProvider) Name() string { return providerName }

func (p *MockProvider) Models() []string { return []string{modelID} }

// ParamCaps keeps dev and demo flows unblocked: the mock accepts json_object,
// stop and forced tool_choice (its reply is canned, so it ignores them). It
// is excluded from the parameter contract.
func (p *MockProvider) ParamCaps() provider.ParamCaps {
	return provider.ParamCaps{JSONMode: true, Stop: true, ForcedToolChoice: true}
}

// SupportsTools makes the mock a tool-capable backend (provider.ToolCapable).
func (p *MockProvider) SupportsTools() bool { return true }

// plan decides what the mock answers: a tool call, or text.
func plan(req provider.ChatRequest) (text string, call *provider.ToolCall) {
	if len(req.Messages) == 0 {
		return mockContent, nil
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		return fmt.Sprintf("The tool answered: %s", last.Content), nil
	}
	if choice, _, _ := provider.ToolChoiceKind(req.ToolChoice); choice == "none" {
		return mockContent, nil
	}
	if len(req.Tools) > 0 && last.Role == "user" {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(last.Content), "call "); ok {
			name, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
			args = strings.TrimSpace(args)
			if args == "" || !json.Valid([]byte(args)) {
				args = "{}"
			}
			for _, t := range req.Tools {
				if t.Function.Name == name {
					return "", &provider.ToolCall{
						ID:       fmt.Sprintf("call_mock_%d", time.Now().UnixNano()),
						Type:     "function",
						Function: provider.ToolCallFunction{Name: name, Arguments: args},
					}
				}
			}
		}
	}
	return mockContent, nil
}

func (p *MockProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = modelID
	}
	text, call := plan(req)
	resp := &provider.ChatResponse{
		ID:               fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
		Model:            model,
		Content:          text,
		PromptTokens:     10,
		CompletionTokens: 15,
		FinishReason:     "stop",
	}
	if call != nil {
		resp.ToolCalls = []provider.ToolCall{*call}
		resp.FinishReason = "tool_calls"
	}
	return resp, nil
}

func (p *MockProvider) Stream(_ context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	model := req.Model
	if model == "" {
		model = modelID
	}
	id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano())
	text, call := plan(req)
	finish := "stop"

	if call != nil {
		finish = "tool_calls"
		// Two fragments, as a real backend streams them: the call's identity
		// first, then its arguments.
		for _, delta := range []provider.ToolCallDelta{
			provider.NewToolCallStartDelta(0, call.ID, call.Function.Name),
			provider.NewToolCallArgumentsDelta(0, call.Function.Arguments),
		} {
			chunk, err := provider.BuildToolCallDeltaSSE(id, model, delta)
			if err != nil {
				return nil, fmt.Errorf("mock: building tool chunk: %w", err)
			}
			if _, err := w.Write(chunk); err != nil {
				return nil, fmt.Errorf("mock: writing tool chunk: %w", err)
			}
		}
	} else {
		chunk, err := provider.BuildStreamingChunkSSE(id, model, text)
		if err != nil {
			return nil, fmt.Errorf("mock: building chunk: %w", err)
		}
		if _, err := w.Write(chunk); err != nil {
			return nil, fmt.Errorf("mock: writing chunk: %w", err)
		}
	}

	stats := &provider.StreamStats{
		Model:            model,
		PromptTokens:     10,
		CompletionTokens: 15,
		FinishReason:     finish,
	}

	final, err := provider.BuildFinalChunksSSE(id, model, finish, stats, req.IncludeUsage)
	if err != nil {
		return nil, fmt.Errorf("mock: building final chunk: %w", err)
	}
	if _, err := w.Write(final); err != nil {
		return nil, fmt.Errorf("mock: writing final chunk: %w", err)
	}

	if _, err := io.WriteString(w, provider.StreamDoneSSE); err != nil {
		return nil, fmt.Errorf("mock: writing done: %w", err)
	}

	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}

	return stats, nil
}
