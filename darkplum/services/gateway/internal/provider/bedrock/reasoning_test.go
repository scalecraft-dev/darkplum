package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Phase A3 of #3 on Bedrock: reasoning_effort on an Anthropic model becomes
// additionalModelRequestFields {"thinking": ...}; temperature stays unset;
// reasoningContent never reaches the caller.

func TestSupportsThinking_PerModel(t *testing.T) {
	yes := []string{
		"us.anthropic.claude-3-7-sonnet-20250219-v1:0",
		"anthropic.claude-3-7-sonnet-20250219-v1:0",
		"us.anthropic.claude-sonnet-4-20250514-v1:0",
		"global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"us.anthropic.claude-opus-4-1-20250805-v1:0",
		"us.anthropic.claude-haiku-4-5-20251001-v1:0",
		"eu.anthropic.claude-opus-5-0-v1:0",
	}
	no := []string{
		"us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		"us.anthropic.claude-3-5-haiku-20241022-v1:0",
		"anthropic.claude-3-haiku-20240307-v1:0",
		"anthropic.claude-3-opus-20240229-v1:0",
		"anthropic.claude-v2:1",
		"us.amazon.nova-lite-v1:0",
		"us.meta.llama3-1-70b-instruct-v1:0",
		"us.deepseek.r1-v1:0",
		"notanthropic.claude-sonnet-4-5",
		"",
	}
	for _, id := range yes {
		if !supportsThinking(id) {
			t.Errorf("%s: want thinking supported", id)
		}
	}
	for _, id := range no {
		if supportsThinking(id) {
			t.Errorf("%q: want thinking unsupported", id)
		}
	}
}

func TestPlanReasoning(t *testing.T) {
	p := newProviderWithFake(&fakeConverseAPI{})
	const claude = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	cases := []struct {
		model, effort string
		maxTokens     int
		mode          provider.ReasoningMode
		budget, max   int
	}{
		{claude, "low", 0, provider.ReasoningThinking, 1024, 4096 + 1024}, // default visible + budget
		{claude, "medium", 0, provider.ReasoningThinking, 4096, 8192},
		{claude, "high", 0, provider.ReasoningThinking, 16384, 20480},
		{claude, "high", 2000, provider.ReasoningThinking, 16384, 18384},
		{claude, "high", 10000, provider.ReasoningThinking, 16384, 26384}, // between budget and 2x: still visible+budget
		{claude, "high", 20000, provider.ReasoningThinking, 16384, 32000}, // capped at the ceiling
		{claude, "high", 32000, provider.ReasoningThinking, 16384, 32000}, // at the ceiling
		{claude, "high", 40000, provider.ReasoningThinking, 16384, 40000}, // above it: caller's own cap not lowered
		{"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-sonnet-4-5-20250929-v1:0", "low", 0, provider.ReasoningThinking, 1024, 5120},
		{"arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc123", "low", 0, provider.ReasoningNone, 0, 0},
		{"us.anthropic.claude-3-5-sonnet-20241022-v2:0", "high", 0, provider.ReasoningNone, 0, 0},
		{"us.amazon.nova-lite-v1:0", "low", 0, provider.ReasoningNone, 0, 0},
	}
	for _, tc := range cases {
		got := p.PlanReasoning(tc.model, tc.effort, tc.maxTokens)
		if got.Mode != tc.mode || got.Budget != tc.budget || got.MaxTokens != tc.max {
			t.Errorf("%s/%s/%d = %+v, want %v/%d/%d", tc.model, tc.effort, tc.maxTokens, got, tc.mode, tc.budget, tc.max)
		}
	}
}

func thinkingReq(budget, max int) provider.ChatRequest {
	temp := 0.3
	return provider.ChatRequest{
		Model:          "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Messages:       []provider.Message{{Role: "user", Content: "hi"}},
		MaxTokens:      max,
		ThinkingBudget: budget,
		Temperature:    &temp, // handler unsets it; provider must not send it either way
	}
}

func marshalDoc(t *testing.T, in *bedrockruntime.ConverseInput) map[string]any {
	t.Helper()
	if in.AdditionalModelRequestFields == nil {
		return nil
	}
	b, err := in.AdditionalModelRequestFields.MarshalSmithyDocument()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestComplete_ThinkingWireShape(t *testing.T) {
	fake := &fakeConverseAPI{converseOut: converseOutput("ok", types.StopReasonEndTurn, 1, 1)}
	if _, err := newProviderWithFake(fake).Complete(context.Background(), thinkingReq(4096, 8192)); err != nil {
		t.Fatal(err)
	}
	in := fake.lastConverseInput
	got := marshalDoc(t, in)
	want := map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(4096)}}
	if b1, b2 := mustMarshal(got), mustMarshal(want); b1 != b2 {
		t.Errorf("additionalModelRequestFields = %s, want %s", b1, b2)
	}
	if in.InferenceConfig.Temperature != nil {
		t.Errorf("temperature = %v, must be unset with thinking", *in.InferenceConfig.Temperature)
	}
	if mt := in.InferenceConfig.MaxTokens; mt == nil || *mt != 8192 {
		t.Errorf("maxTokens = %v", mt)
	}
}

func TestStream_ThinkingWireShape(t *testing.T) {
	fake := &fakeConverseAPI{stream: newFakeStream([]types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn}},
	}, nil)}
	if _, err := newProviderWithFake(fake).Stream(context.Background(), thinkingReq(1024, 4096), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	in := fake.lastConverseStreamInput
	if in.AdditionalModelRequestFields == nil {
		t.Fatal("no additionalModelRequestFields on ConverseStream")
	}
	b, _ := in.AdditionalModelRequestFields.MarshalSmithyDocument()
	if !strings.Contains(string(b), `"budget_tokens":1024`) || !strings.Contains(string(b), `"type":"enabled"`) {
		t.Errorf("fields = %s", b)
	}
	if in.InferenceConfig.Temperature != nil {
		t.Error("temperature set with thinking")
	}
}

func mustMarshal(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestThinkingOff_NoFieldsAndTemperatureFlows(t *testing.T) {
	fake := &fakeConverseAPI{converseOut: converseOutput("ok", types.StopReasonEndTurn, 1, 1)}
	req := thinkingReq(0, 0)
	if _, err := newProviderWithFake(fake).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	in := fake.lastConverseInput
	if in.AdditionalModelRequestFields != nil {
		t.Errorf("additionalModelRequestFields = %v without thinking", in.AdditionalModelRequestFields)
	}
	if in.InferenceConfig.Temperature == nil {
		t.Error("temperature lost without thinking")
	}
	// A budget that does not fit under maxTokens is not sent.
	if _, err := newProviderWithFake(fake).Complete(context.Background(), thinkingReq(4096, 4096)); err != nil {
		t.Fatal(err)
	}
	if fake.lastConverseInput.AdditionalModelRequestFields != nil {
		t.Error("budget >= maxTokens was sent")
	}
}

func TestReasoningContent_NeverReachesTheCaller(t *testing.T) {
	// Non-streaming.
	out := converseOutput("the answer", types.StopReasonEndTurn, 10, 900)
	msg := out.Output.(*types.ConverseOutputMemberMessage)
	msg.Value.Content = append([]types.ContentBlock{
		&types.ContentBlockMemberReasoningContent{Value: &types.ReasoningContentBlockMemberReasoningText{
			Value: types.ReasoningTextBlock{Text: strPtr("SECRET-REASONING"), Signature: strPtr("SIG-OPAQUE")},
		}},
		&types.ContentBlockMemberReasoningContent{Value: &types.ReasoningContentBlockMemberRedactedContent{Value: []byte("REDACTED-OPAQUE")}},
	}, msg.Value.Content...)
	fake := &fakeConverseAPI{converseOut: out}
	resp, err := newProviderWithFake(fake).Complete(context.Background(), thinkingReq(1024, 4096))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(resp)
	if resp.Content != "the answer" || strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "SIG-") || strings.Contains(string(b), "REDACTED") {
		t.Errorf("response = %s", b)
	}
	if resp.CompletionTokens != 900 {
		t.Errorf("CompletionTokens = %d, want the billed 900", resp.CompletionTokens)
	}

	// Streaming.
	idx := int32(0)
	idx2 := int32(1)
	events := []types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: &idx,
			Delta: &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberText{Value: "SECRET-REASONING"}}}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: &idx,
			Delta: &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberSignature{Value: "SIG-OPAQUE"}}}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: &idx,
			Delta: &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberRedactedContent{Value: []byte("REDACTED-OPAQUE")}}}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: &idx2,
			Delta: &types.ContentBlockDeltaMemberText{Value: "the answer"}}},
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn}},
	}
	sfake := &fakeConverseAPI{stream: newFakeStream(events, nil)}
	var buf bytes.Buffer
	if _, err := newProviderWithFake(sfake).Stream(context.Background(), thinkingReq(1024, 4096), &buf); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, "the answer") {
		t.Errorf("answer missing: %s", s)
	}
	for _, bad := range []string{"SECRET", "SIG-", "REDACTED", "reasoning"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q reached the stream: %s", bad, s)
		}
	}
}
