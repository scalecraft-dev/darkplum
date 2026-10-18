package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

func docJSON(t *testing.T, d document.Interface) string {
	t.Helper()
	raw, err := d.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	// Normalise key order and whitespace; numbers stay exact.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func weatherTool() provider.Tool {
	return provider.Tool{Type: "function", Function: provider.ToolFunction{
		Name:        "get_weather",
		Description: "Look up weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"n":{"type":"integer","maximum":9007199254740993}}}`),
	}}
}

func noArgTool() provider.Tool {
	return provider.Tool{Type: "function", Function: provider.ToolFunction{Name: "ping"}}
}

func TestSupportsTools(t *testing.T) {
	if !provider.SupportsTools(&BedrockProvider{}) {
		t.Fatal("bedrock provider must report tool support")
	}
}

func TestTranslateToolConfig(t *testing.T) {
	cases := []struct {
		name       string
		tools      []provider.Tool
		choice     string
		wantNil    bool
		wantChoice string // "", "auto", "any", "tool:<name>"
		wantErr    bool
	}{
		{name: "no tools", wantNil: true},
		{name: "tools without choice", tools: []provider.Tool{weatherTool()}},
		{name: "auto", tools: []provider.Tool{weatherTool()}, choice: `"auto"`, wantChoice: "auto"},
		{name: "required", tools: []provider.Tool{weatherTool()}, choice: `"required"`, wantChoice: "any"},
		{name: "named", tools: []provider.Tool{weatherTool()}, choice: `{"type":"function","function":{"name":"get_weather"}}`, wantChoice: "tool:get_weather"},
		{name: "none keeps definitions with automatic choice", tools: []provider.Tool{weatherTool()}, choice: `"none"`, wantChoice: ""},
		{name: "invalid choice", tools: []provider.Tool{weatherTool()}, choice: `"sometimes"`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := translateToolConfig(provider.ChatRequest{Tools: tc.tools, ToolChoice: json.RawMessage(tc.choice)})
			if tc.wantErr {
				var apiErr *BedrockAPIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
					t.Fatalf("want 400 BedrockAPIError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if cfg != nil {
					t.Fatalf("want nil ToolConfiguration, got %+v", cfg)
				}
				return
			}
			if len(cfg.Tools) != len(tc.tools) {
				t.Fatalf("tools: got %d", len(cfg.Tools))
			}
			got := ""
			switch c := cfg.ToolChoice.(type) {
			case nil:
			case *types.ToolChoiceMemberAuto:
				got = "auto"
			case *types.ToolChoiceMemberAny:
				got = "any"
			case *types.ToolChoiceMemberTool:
				got = "tool:" + aws.ToString(c.Value.Name)
			default:
				t.Fatalf("unexpected choice %T", c)
			}
			if got != tc.wantChoice {
				t.Errorf("tool choice: got %q want %q", got, tc.wantChoice)
			}
		})
	}
}

func TestTranslateToolConfig_SpecAndSchema(t *testing.T) {
	cfg, err := translateToolConfig(provider.ChatRequest{Tools: []provider.Tool{weatherTool(), noArgTool()}})
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.Tools[0].(*types.ToolMemberToolSpec).Value
	if aws.ToString(w.Name) != "get_weather" || aws.ToString(w.Description) != "Look up weather" {
		t.Errorf("spec: %+v", w)
	}
	// Large integers must survive exactly (no float64 round trip).
	want := `{"properties":{"city":{"type":"string"},"n":{"maximum":9007199254740993,"type":"integer"}},"type":"object"}`
	if got := docJSON(t, w.InputSchema.(*types.ToolInputSchemaMemberJson).Value); got != want {
		t.Errorf("schema:\n got %s\nwant %s", got, want)
	}
	p := cfg.Tools[1].(*types.ToolMemberToolSpec).Value
	if p.Description != nil {
		t.Errorf("empty description must be omitted, got %q", *p.Description)
	}
	if got := docJSON(t, p.InputSchema.(*types.ToolInputSchemaMemberJson).Value); got != `{"properties":{},"type":"object"}` {
		t.Errorf("default schema: %s", got)
	}
	// An explicit JSON null behaves like absent.
	nullTool := provider.Tool{Type: "function", Function: provider.ToolFunction{Name: "x", Parameters: json.RawMessage("null")}}
	cfg, err = translateToolConfig(provider.ChatRequest{Tools: []provider.Tool{nullTool}})
	if err != nil {
		t.Fatal(err)
	}
	if got := docJSON(t, cfg.Tools[0].(*types.ToolMemberToolSpec).Value.InputSchema.(*types.ToolInputSchemaMemberJson).Value); got != `{"properties":{},"type":"object"}` {
		t.Errorf("null schema: %s", got)
	}
}

// blockDesc renders message content compactly for table assertions:
// T:<text>, U:<id>/<name>/<input>, R:<id>/<text>.
func blockDesc(t *testing.T, m types.Message) []string {
	t.Helper()
	out := []string{string(m.Role)}
	for _, b := range m.Content {
		switch v := b.(type) {
		case *types.ContentBlockMemberText:
			out = append(out, "T:"+v.Value)
		case *types.ContentBlockMemberToolUse:
			out = append(out, "U:"+aws.ToString(v.Value.ToolUseId)+"/"+aws.ToString(v.Value.Name)+"/"+docJSON(t, v.Value.Input))
		case *types.ContentBlockMemberToolResult:
			txt := ""
			for _, c := range v.Value.Content {
				txt += c.(*types.ToolResultContentBlockMemberText).Value
			}
			out = append(out, "R:"+aws.ToString(v.Value.ToolUseId)+"/"+txt)
		default:
			t.Fatalf("unexpected block %T", b)
		}
	}
	return out
}

func call(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: args}}
}

func TestTranslateRequest_ToolTurns(t *testing.T) {
	user := provider.Message{Role: "user", Content: "weather in Paris and Rome?"}
	cases := []struct {
		name string
		msgs []provider.Message
		want [][]string
	}{
		{
			name: "assistant tool_calls with text then single result",
			msgs: []provider.Message{user,
				{Role: "assistant", Content: "Checking.", ToolCalls: []provider.ToolCall{call("c1", "get_weather", `{"city":"Paris"}`)}},
				{Role: "tool", ToolCallID: "c1", Content: "18C"},
			},
			want: [][]string{
				{"user", "T:weather in Paris and Rome?"},
				{"assistant", "T:Checking.", `U:c1/get_weather/{"city":"Paris"}`},
				{"user", "R:c1/18C"},
			},
		},
		{
			name: "tool_calls only, empty arguments become {}",
			msgs: []provider.Message{user,
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "ping", "")}},
				{Role: "tool", ToolCallID: "c1", Content: "pong"},
			},
			want: [][]string{
				{"user", "T:weather in Paris and Rome?"},
				{"assistant", "U:c1/ping/{}"},
				{"user", "R:c1/pong"},
			},
		},
		{
			name: "consecutive tool results merge into one user message",
			msgs: []provider.Message{user,
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "get_weather", `{"city":"Paris"}`), call("c2", "get_weather", `{"city":"Rome"}`)}},
				{Role: "tool", ToolCallID: "c1", Content: "18C"},
				{Role: "tool", ToolCallID: "c2", Content: "25C"},
			},
			want: [][]string{
				{"user", "T:weather in Paris and Rome?"},
				{"assistant", `U:c1/get_weather/{"city":"Paris"}`, `U:c2/get_weather/{"city":"Rome"}`},
				{"user", "R:c1/18C", "R:c2/25C"},
			},
		},
		{
			name: "tool results lead a following user message",
			msgs: []provider.Message{user,
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "ping", "{}")}},
				{Role: "tool", ToolCallID: "c1", Content: "pong"},
				{Role: "user", Content: "thanks, and?"},
			},
			want: [][]string{
				{"user", "T:weather in Paris and Rome?"},
				{"assistant", "U:c1/ping/{}"},
				{"user", "R:c1/pong", "T:thanks, and?"},
			},
		},
		{
			name: "empty tool output gets a placeholder",
			msgs: []provider.Message{user,
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "ping", "{}")}},
				{Role: "tool", ToolCallID: "c1", Content: "  "},
			},
			want: [][]string{
				{"user", "T:weather in Paris and Rome?"},
				{"assistant", "U:c1/ping/{}"},
				{"user", "R:c1/" + emptyToolResultText},
			},
		},
		{
			name: "orphan results of a dropped leading assistant turn go with it",
			msgs: []provider.Message{
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "ping", "{}")}},
				{Role: "tool", ToolCallID: "c1", Content: "pong"},
				{Role: "user", Content: "hi"},
			},
			want: [][]string{{"user", "T:hi"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, msgs, _, err := translateRequest(provider.ChatRequest{Messages: tc.msgs})
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != len(tc.want) {
				t.Fatalf("got %d messages, want %d", len(msgs), len(tc.want))
			}
			for i := range msgs {
				got := blockDesc(t, msgs[i])
				if strings.Join(got, "|") != strings.Join(tc.want[i], "|") {
					t.Errorf("message %d:\n got %q\nwant %q", i, got, tc.want[i])
				}
			}
		})
	}
}

func TestTranslateRequest_MalformedToolArguments(t *testing.T) {
	for name, args := range map[string]string{
		"not json":        `{"city":`,
		"array":           `[1,2]`,
		"scalar":          `"x"`,
		"trailing data":   `{"a":1} {"b":2}`,
		"literal null":    `null`,
		"truncated value": `{"a":tru`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := translateRequest(provider.ChatRequest{Messages: []provider.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "get_weather", args)}},
			}})
			var apiErr *BedrockAPIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("want 400 BedrockAPIError, got %v", err)
			}
			if !strings.Contains(string(apiErr.Body), "messages[1].tool_calls[0].function.arguments") {
				t.Errorf("body should name the parameter: %s", apiErr.Body)
			}
		})
	}
}

func TestTranslateRequest_ToolTurnWithoutIDRejected(t *testing.T) {
	_, _, _, err := translateRequest(provider.ChatRequest{Messages: []provider.Message{
		{Role: "user", Content: "hi"}, {Role: "tool", Content: "x"},
	}})
	var apiErr *BedrockAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %v", err)
	}
}

func TestComplete_SendsToolConfigAndReturnsToolCalls(t *testing.T) {
	in := document.NewLazyDocument(map[string]any{"city": "Paris", "n": 2})
	fake := &fakeConverseAPI{converseOut: &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{Value: types.Message{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "Looking."},
				&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{ToolUseId: aws.String("tu_1"), Name: aws.String("get_weather"), Input: in}},
				&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{ToolUseId: aws.String("tu_2"), Name: aws.String("ping")}},
			},
		}},
		StopReason: types.StopReasonToolUse,
	}}
	p := newProviderWithFake(fake)
	resp, err := p.Complete(context.Background(), provider.ChatRequest{
		Model:      "m",
		Messages:   []provider.Message{{Role: "user", Content: "hi"}},
		Tools:      []provider.Tool{weatherTool()},
		ToolChoice: json.RawMessage(`"required"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.lastConverseInput.ToolConfig == nil || len(fake.lastConverseInput.ToolConfig.Tools) != 1 {
		t.Fatalf("ToolConfig not sent: %+v", fake.lastConverseInput.ToolConfig)
	}
	if _, ok := fake.lastConverseInput.ToolConfig.ToolChoice.(*types.ToolChoiceMemberAny); !ok {
		t.Errorf("tool choice: %T", fake.lastConverseInput.ToolConfig.ToolChoice)
	}
	if resp.FinishReason != "tool_calls" || resp.Content != "Looking." {
		t.Errorf("finish=%q content=%q", resp.FinishReason, resp.Content)
	}
	want := []provider.ToolCall{
		call("tu_1", "get_weather", `{"city":"Paris","n":2}`),
		call("tu_2", "ping", `{}`),
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0] != want[0] || resp.ToolCalls[1] != want[1] {
		t.Errorf("tool calls:\n got %+v\nwant %+v", resp.ToolCalls, want)
	}
}

func TestStream_NoToolsSendsNoToolConfig(t *testing.T) {
	fake := &fakeConverseAPI{stream: newFakeStream(nil, nil)}
	p := newProviderWithFake(fake)
	var buf bytes.Buffer
	if _, err := p.Stream(context.Background(), provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, &buf); err != nil {
		t.Fatal(err)
	}
	if fake.lastConverseStreamInput.ToolConfig != nil {
		t.Errorf("ToolConfig must be nil without tools")
	}
}

func TestToolsUnsupportedModelIs400OnlyWhenToolsSent(t *testing.T) {
	msg := "This model doesn't support tool use."
	withTools := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}, Tools: []provider.Tool{weatherTool()}}
	noTools := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}

	apiErr := &types.ValidationException{Message: aws.String(msg)}

	p := newProviderWithFake(&fakeConverseAPI{converseErr: apiErr})
	_, err := p.Complete(context.Background(), withTools)
	var be *BedrockAPIError
	if !errors.As(err, &be) || be.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %v", err)
	}
	if !strings.Contains(string(be.Body), "does not support tool calling") || strings.Contains(string(be.Body), "502") {
		t.Errorf("body: %s", be.Body)
	}

	// Streaming path maps the same way.
	p = newProviderWithFake(&fakeConverseAPI{streamErr: apiErr})
	_, err = p.Stream(context.Background(), withTools, &bytes.Buffer{})
	if !errors.As(err, &be) || !strings.Contains(string(be.Body), "does not support tool calling") {
		t.Fatalf("stream: %v", err)
	}

	// Without tools the existing mapping (Bedrock's own message) is untouched.
	p = newProviderWithFake(&fakeConverseAPI{converseErr: apiErr})
	_, err = p.Complete(context.Background(), noTools)
	if !errors.As(err, &be) || !strings.Contains(string(be.Body), msg) {
		t.Fatalf("no-tools mapping changed: %v", err)
	}

	// An unrelated tool validation error with tools sent keeps its own message.
	other := &types.ValidationException{Message: aws.String("Expected toolResult blocks at messages.2.content")}
	p = newProviderWithFake(&fakeConverseAPI{converseErr: other})
	_, err = p.Complete(context.Background(), withTools)
	if !errors.As(err, &be) || !strings.Contains(string(be.Body), "Expected toolResult") {
		t.Fatalf("pairing error should pass through: %v", err)
	}
}

func startEvent(idx int32, id, name string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{
		ContentBlockIndex: &idx,
		Start:             &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{ToolUseId: aws.String(id), Name: aws.String(name)}},
	}}
}

func argsEvent(idx int32, frag string) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: &idx,
		Delta:             &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: aws.String(frag)}},
	}}
}

func stopEvent(idx int32) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: &idx}}
}

// choiceOf decodes the single choice of one SSE data payload.
func toolDeltaOf(t *testing.T, payload string) (idx int, id, typ, name, args string, finish *string) {
	t.Helper()
	var c struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		t.Fatalf("payload %s: %v", payload, err)
	}
	ch := c.Choices[0]
	if len(ch.Delta.ToolCalls) != 1 {
		return 0, "", "", "", "", ch.FinishReason
	}
	tc := ch.Delta.ToolCalls[0]
	return tc.Index, tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments, ch.FinishReason
}

func TestTranslateStreamEvent_ParallelToolCalls(t *testing.T) {
	// Block 0 is text; the tool_use blocks are Bedrock blocks 1 and 2 but must
	// stream as OpenAI tool indexes 0 and 1. Block 2 interleaves with block 1's
	// arguments to prove the index is keyed on the block, not the arrival order.
	events := []types.ConverseStreamOutput{
		startEvent(1, "tu_a", "get_weather"),
		argsEvent(1, `{"city":`),
		startEvent(2, "tu_b", "get_weather"),
		argsEvent(2, `{"city":"Rome"}`),
		argsEvent(1, `"Paris"}`),
		stopEvent(1),
		stopEvent(2),
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
	}
	stats := &provider.StreamStats{}
	var ts toolStreamState
	var buf bytes.Buffer
	for _, ev := range events {
		if _, err := translateStreamEvent(ev, "cid", "mdl", stats, &buf, &ts); err != nil {
			t.Fatal(err)
		}
	}
	payloads := extractDataPayloads(t, buf.String())
	type frag struct {
		idx                int
		id, typ, name, arg string
	}
	want := []frag{
		{0, "tu_a", "function", "get_weather", ""},
		{0, "", "", "", `{"city":`},
		{1, "tu_b", "function", "get_weather", ""},
		{1, "", "", "", `{"city":"Rome"}`},
		{0, "", "", "", `"Paris"}`},
	}
	if len(payloads) != len(want)+1 {
		t.Fatalf("got %d payloads, want %d: %v", len(payloads), len(want)+1, payloads)
	}
	for i, w := range want {
		idx, id, typ, name, args, fin := toolDeltaOf(t, payloads[i])
		if (frag{idx, id, typ, name, args}) != w || fin != nil {
			t.Errorf("fragment %d: got %+v finish=%v, want %+v\n%s", i, frag{idx, id, typ, name, args}, fin, w, payloads[i])
		}
	}
	// Exact wire shape of the start fragment: arguments is "" not omitted.
	if !strings.Contains(payloads[0], `"tool_calls":[{"index":0,"id":"tu_a","type":"function","function":{"name":"get_weather","arguments":""}}]`) {
		t.Errorf("start fragment shape: %s", payloads[0])
	}
	if !strings.Contains(payloads[1], `"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]`) {
		t.Errorf("args fragment shape: %s", payloads[1])
	}
	_, _, _, _, _, fin := toolDeltaOf(t, payloads[len(payloads)-1])
	if fin == nil || *fin != "tool_calls" || stats.FinishReason != "tool_calls" {
		t.Errorf("final finish_reason=%v stats=%q", fin, stats.FinishReason)
	}
}

func TestTranslateStreamEvent_NoArgToolGetsEmptyObject(t *testing.T) {
	var ts toolStreamState
	var buf bytes.Buffer
	stats := &provider.StreamStats{}
	for _, ev := range []types.ConverseStreamOutput{startEvent(0, "tu_a", "ping"), argsEvent(0, ""), stopEvent(0)} {
		if _, err := translateStreamEvent(ev, "cid", "mdl", stats, &buf, &ts); err != nil {
			t.Fatal(err)
		}
	}
	payloads := extractDataPayloads(t, buf.String())
	if len(payloads) != 2 {
		t.Fatalf("want start + {} fragments, got %v", payloads)
	}
	if _, _, _, _, args, _ := toolDeltaOf(t, payloads[1]); args != "{}" {
		t.Errorf("arguments: %q", args)
	}
}

func TestTranslateStreamEvent_ToolDeltaWithoutStartIgnored(t *testing.T) {
	var buf bytes.Buffer
	if _, err := translateStreamEvent(argsEvent(3, `{"a":1}`), "cid", "mdl", &provider.StreamStats{}, &buf, &toolStreamState{}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestStream_ToolUseEndToEnd(t *testing.T) {
	in, out := int32(10), int32(5)
	txt := int32(0)
	events := []types.ConverseStreamOutput{
		&types.ConverseStreamOutputMemberMessageStart{Value: types.MessageStartEvent{Role: types.ConversationRoleAssistant}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: &txt, Delta: &types.ContentBlockDeltaMemberText{Value: "ok"}}},
		stopEvent(0),
		startEvent(1, "tu_a", "get_weather"),
		argsEvent(1, `{"city":"Paris"}`),
		stopEvent(1),
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
		&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{Usage: &types.TokenUsage{InputTokens: &in, OutputTokens: &out}}},
	}
	fake := &fakeConverseAPI{stream: newFakeStream(events, nil)}
	p := newProviderWithFake(fake)
	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Tools:    []provider.Tool{weatherTool()},
	}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if fake.lastConverseStreamInput.ToolConfig == nil {
		t.Fatal("ToolConfig not sent on stream")
	}
	if stats.FinishReason != "tool_calls" || stats.PromptTokens != 10 || stats.CompletionTokens != 5 {
		t.Errorf("stats: %+v", stats)
	}
	payloads := extractDataPayloads(t, buf.String())
	if len(payloads) != 4 { // text, start, args, final
		t.Fatalf("payloads: %v", payloads)
	}
	if idx, id, _, name, _, _ := toolDeltaOf(t, payloads[1]); idx != 0 || id != "tu_a" || name != "get_weather" {
		t.Errorf("start: %s", payloads[1])
	}
	if !strings.HasSuffix(buf.String(), provider.StreamDoneSSE) {
		t.Errorf("missing [DONE]")
	}
}

func TestTranslateToolConfig_NoneKeepsToolsForToolHistory(t *testing.T) {
	req := provider.ChatRequest{
		Tools:      []provider.Tool{weatherTool()},
		ToolChoice: json.RawMessage(`"none"`),
		Messages: []provider.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{call("c1", "get_weather", `{"city":"Paris"}`)}},
			{Role: "tool", ToolCallID: "c1", Content: "sunny"},
		},
	}
	if _, _, _, err := translateRequest(req); err != nil {
		t.Fatal(err)
	}
	cfg, err := translateToolConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || len(cfg.Tools) != 1 {
		t.Fatalf("ToolConfiguration must keep the tools, got %+v", cfg)
	}
	if cfg.ToolChoice != nil {
		t.Errorf("ToolChoice = %T, want omitted (automatic)", cfg.ToolChoice)
	}
}
