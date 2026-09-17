package mock

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

const (
	providerName = "mock"
	modelID      = "mock-model"
	mockContent  = "This is a mock response from the local development environment."
)

type MockProvider struct{}

func New() *MockProvider {
	return &MockProvider{}
}

func (p *MockProvider) Name() string { return providerName }

func (p *MockProvider) Models() []string { return []string{modelID} }

func (p *MockProvider) Complete(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = modelID
	}
	return &provider.ChatResponse{
		ID:               fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano()),
		Model:            model,
		Content:          mockContent,
		PromptTokens:     10,
		CompletionTokens: 15,
		FinishReason:     "stop",
	}, nil
}

func (p *MockProvider) Stream(_ context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	model := req.Model
	if model == "" {
		model = modelID
	}
	id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano())

	chunk, err := provider.BuildStreamingChunkSSE(id, model, mockContent)
	if err != nil {
		return nil, fmt.Errorf("mock: building chunk: %w", err)
	}
	if _, err := w.Write(chunk); err != nil {
		return nil, fmt.Errorf("mock: writing chunk: %w", err)
	}

	stats := &provider.StreamStats{
		Model:            model,
		PromptTokens:     10,
		CompletionTokens: 15,
		FinishReason:     "stop",
	}

	final, err := provider.BuildFinalChunkSSE(id, model, "stop", stats)
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
