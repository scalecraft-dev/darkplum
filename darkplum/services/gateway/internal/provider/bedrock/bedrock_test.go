package bedrock

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithy "github.com/aws/smithy-go"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// ---------------------------------------------------------------------------
// fakeConverseAPI — implements converseAPI for Complete/Stream tests.
// ---------------------------------------------------------------------------

type fakeConverseAPI struct {
	converseOut *bedrockruntime.ConverseOutput
	converseErr error

	stream    converseStream
	streamErr error
	// capture the last request passed in, for assertions on translation.
	lastConverseInput       *bedrockruntime.ConverseInput
	lastConverseStreamInput *bedrockruntime.ConverseStreamInput
}

func (f *fakeConverseAPI) Converse(_ context.Context, params *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.lastConverseInput = params
	if f.converseErr != nil {
		return nil, f.converseErr
	}
	return f.converseOut, nil
}

func (f *fakeConverseAPI) ConverseStream(_ context.Context, params *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (converseStream, error) {
	f.lastConverseStreamInput = params
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	if f.stream == nil {
		return nil, errors.New("fakeConverseAPI: no stream configured")
	}
	return f.stream, nil
}

// ---------------------------------------------------------------------------
// fakeStream — implements converseStream, feeding a canned event sequence.
// ---------------------------------------------------------------------------

type fakeStream struct {
	events    []types.ConverseStreamOutput
	err       error // returned by Err() once the channel is drained
	closed    bool
	closeOnce sync.Once
	ch        chan types.ConverseStreamOutput
}

func newFakeStream(events []types.ConverseStreamOutput, err error) *fakeStream {
	ch := make(chan types.ConverseStreamOutput, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return &fakeStream{events: events, err: err, ch: ch}
}

func (f *fakeStream) Events() <-chan types.ConverseStreamOutput { return f.ch }
func (f *fakeStream) Err() error                                { return f.err }
func (f *fakeStream) Close() error {
	f.closeOnce.Do(func() { f.closed = true })
	return nil
}

// blockingFakeStream never sends events and blocks until Close is called —
// used to prove ctx cancellation stops Stream and always closes the stream.
type blockingFakeStream struct {
	closed chan struct{}
	ch     chan types.ConverseStreamOutput
}

func newBlockingFakeStream() *blockingFakeStream {
	return &blockingFakeStream{
		closed: make(chan struct{}),
		ch:     make(chan types.ConverseStreamOutput),
	}
}

func (b *blockingFakeStream) Events() <-chan types.ConverseStreamOutput { return b.ch }
func (b *blockingFakeStream) Err() error                                { return nil }
func (b *blockingFakeStream) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func newProviderWithFake(api converseAPI) *BedrockProvider {
	return &BedrockProvider{client: api, region: "us-east-1"}
}

// ---------------------------------------------------------------------------
// Complete
// ---------------------------------------------------------------------------

func TestComplete_HappyPath(t *testing.T) {
	fake := &fakeConverseAPI{converseOut: converseOutput("Hello!", types.StopReasonEndTurn, 3, 2)}
	p := newProviderWithFake(fake)

	resp, err := p.Complete(context.Background(), provider.ChatRequest{
		Model:    "us.amazon.nova-lite-v1:0",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Content != "Hello!" {
		t.Errorf("expected content=%q, got %q", "Hello!", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("expected finish_reason=stop, got %q", resp.FinishReason)
	}
	if resp.PromptTokens != 3 || resp.CompletionTokens != 2 {
		t.Errorf("unexpected token counts: %+v", resp)
	}
	if fake.lastConverseInput == nil || *fake.lastConverseInput.ModelId != "us.amazon.nova-lite-v1:0" {
		t.Errorf("expected ModelId to be forwarded, got %+v", fake.lastConverseInput)
	}
}

func TestComplete_NoUserMessagesReturnsBedrockAPIErrorWithoutCallingConverse(t *testing.T) {
	fake := &fakeConverseAPI{converseOut: converseOutput("unused", types.StopReasonEndTurn, 0, 0)}
	p := newProviderWithFake(fake)

	_, err := p.Complete(context.Background(), provider.ChatRequest{
		Model:    "model",
		Messages: []provider.Message{{Role: "assistant", Content: "no user turn"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *BedrockAPIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", apiErr.StatusCode)
	}
	if fake.lastConverseInput != nil {
		t.Error("expected Converse not to be called when translation fails validation")
	}
}

func TestComplete_ErrorMapping(t *testing.T) {
	fake := &fakeConverseAPI{converseErr: &types.AccessDeniedException{Message: strPtr("nope")}}
	p := newProviderWithFake(fake)

	_, err := p.Complete(context.Background(), provider.ChatRequest{
		Model:    "model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *BedrockAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", apiErr.StatusCode)
	}
}

func TestComplete_ThrottlingMapsTo429(t *testing.T) {
	fake := &fakeConverseAPI{converseErr: &types.ThrottlingException{Message: strPtr("slow down")}}
	p := newProviderWithFake(fake)

	_, err := p.Complete(context.Background(), provider.ChatRequest{
		Model:    "model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *BedrockAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", apiErr.StatusCode)
	}
}

func TestComplete_NonAPIErrorIsWrappedButNotBedrockAPIError(t *testing.T) {
	fake := &fakeConverseAPI{converseErr: errors.New("boom: network reset")}
	p := newProviderWithFake(fake)

	_, err := p.Complete(context.Background(), provider.ChatRequest{
		Model:    "model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *BedrockAPIError
	if errors.As(err, &apiErr) {
		t.Fatalf("non-smithy error should not become a BedrockAPIError, got one: %+v", apiErr)
	}
}

// smithy.APIError compile-time sanity: confirm the types we use in tests
// actually satisfy the interface wrapBedrockError type-asserts against.
var _ smithy.APIError = (*types.AccessDeniedException)(nil)

// ---------------------------------------------------------------------------
// Stream
// ---------------------------------------------------------------------------

func TestStream_DeltasThenFinalChunkThenDone(t *testing.T) {
	input, output := int32(7), int32(4)
	events := []types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberMessageStart{Value: types.MessageStartEvent{Role: types.ConversationRoleAssistant}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{Delta: &types.ContentBlockDeltaMemberText{Value: "Hel"}}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{Delta: &types.ContentBlockDeltaMemberText{Value: "lo"}}},
		&types.ConverseStreamOutputMemberContentBlockStop{},
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn}},
		&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{Usage: &types.TokenUsage{InputTokens: &input, OutputTokens: &output}}},
	}
	fake := &fakeConverseAPI{stream: newFakeStream(events, nil)}
	p := newProviderWithFake(fake)

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), provider.ChatRequest{
		Model:    "meta.llama3-70b-instruct-v1:0",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, &buf)
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}

	out := buf.String()
	dataLines := extractDataPayloads(t, out)
	if len(dataLines) < 3 {
		t.Fatalf("expected at least 3 data chunks (2 deltas + final), got %d: %v", len(dataLines), dataLines)
	}

	// First two content chunks carry the deltas in order.
	if !strings.Contains(dataLines[0], `"content":"Hel"`) {
		t.Errorf("expected first chunk to carry delta %q, got %s", "Hel", dataLines[0])
	}
	if !strings.Contains(dataLines[1], `"content":"lo"`) {
		t.Errorf("expected second chunk to carry delta %q, got %s", "lo", dataLines[1])
	}
	// Final chunk carries the finish_reason.
	if !strings.Contains(dataLines[2], `"finish_reason":"stop"`) {
		t.Errorf("expected final chunk to carry finish_reason=stop, got %s", dataLines[2])
	}

	if !strings.HasSuffix(out, provider.StreamDoneSSE) {
		t.Errorf("expected stream to end with %q, got suffix %q", provider.StreamDoneSSE, out[max(0, len(out)-30):])
	}

	if stats.PromptTokens != 7 || stats.CompletionTokens != 4 {
		t.Errorf("expected usage from metadata event to be captured, got %+v", stats)
	}
	if stats.FinishReason != "stop" {
		t.Errorf("expected FinishReason=stop, got %q", stats.FinishReason)
	}

	fs := fake.stream.(*fakeStream)
	if !fs.closed {
		t.Error("expected the stream to be Closed")
	}
}

func TestStream_MidStreamErrorReturnedNotSwallowed(t *testing.T) {
	events := []types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{Delta: &types.ContentBlockDeltaMemberText{Value: "partial"}}},
	}
	streamErr := &types.ModelStreamErrorException{Message: strPtr("upstream reset mid-generation")}
	fake := &fakeConverseAPI{stream: newFakeStream(events, streamErr)}
	p := newProviderWithFake(fake)

	var buf bytes.Buffer
	_, err := p.Stream(context.Background(), provider.ChatRequest{
		Model:    "model",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}, &buf)
	if err == nil {
		t.Fatal("expected the mid-stream error surfaced via Err() to be returned, not swallowed")
	}
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *BedrockAPIError, got %T: %v", err, err)
	}

	fs := fake.stream.(*fakeStream)
	if !fs.closed {
		t.Error("expected the stream to be Closed even on mid-stream error")
	}

	// The [DONE] sentinel must not be written when the stream ended in error.
	if strings.Contains(buf.String(), "[DONE]") {
		t.Error("must not write the [DONE] sentinel after a mid-stream error")
	}
}

func TestStream_CtxCancelClosesStream(t *testing.T) {
	blocking := newBlockingFakeStream()
	fake := &fakeConverseAPI{stream: blocking}
	p := newProviderWithFake(fake)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var streamErr error
	go func() {
		_, streamErr = p.Stream(ctx, provider.ChatRequest{
			Model:    "model",
			Messages: []provider.Message{{Role: "user", Content: "hi"}},
		}, &bytes.Buffer{})
		close(done)
	}()

	// Give the goroutine a moment to reach the select/blocking read, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after ctx cancellation")
	}

	if streamErr == nil {
		t.Error("expected an error from Stream after ctx cancellation")
	}
	select {
	case <-blocking.closed:
	default:
		t.Error("expected the stream to be Closed after ctx cancellation")
	}
}

func TestStream_NoUserMessagesReturnsBedrockAPIErrorWithoutCallingConverseStream(t *testing.T) {
	fake := &fakeConverseAPI{stream: newFakeStream(nil, nil)}
	p := newProviderWithFake(fake)

	_, err := p.Stream(context.Background(), provider.ChatRequest{
		Model:    "model",
		Messages: []provider.Message{{Role: "assistant", Content: "no user turn"}},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *BedrockAPIError, got %T", err)
	}
	if fake.lastConverseStreamInput != nil {
		t.Error("expected ConverseStream not to be called when translation fails validation")
	}
}

// extractDataPayloads pulls every "data: ..." line (excluding [DONE]) out of
// an SSE byte stream, in order.
func extractDataPayloads(t *testing.T, sse string) []string {
	t.Helper()
	var payloads []string
	for _, chunk := range strings.Split(sse, "\n\n") {
		if !strings.HasPrefix(chunk, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(chunk, "data: ")
		if payload == "[DONE]" || payload == "" {
			continue
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
