// Package bedrock handles all format translation between the OpenAI-compatible
// ChatRequest/ChatResponse types and the Bedrock Converse / ConverseStream API.
// No HTTP or AWS SDK client-construction logic lives here — bedrock.go owns
// that layer. All translation functions are pure so they can be tested without
// any AWS SDK involvement (only the Converse* input/output/types structs are
// used, never a live client).
package bedrock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
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
//   - Only "user", "assistant" and "tool" roles enter the Messages list; other
//     roles are dropped. An assistant turn's tool_calls become toolUse blocks
//     after its text; a "tool" turn becomes a toolResult block in a USER
//     message, and consecutive tool turns share one user message with the
//     results first (tool_calls with unparseable arguments are a 400).
//     Tools and tool_choice are translated separately by translateToolConfig.
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
//   - Temperature and TopP are forwarded only when non-nil; Stop is forwarded
//     only when non-empty.
func translateRequest(req provider.ChatRequest) (system []types.SystemContentBlock, messages []types.Message, inferenceConfig *types.InferenceConfiguration, err error) {
	var systemText strings.Builder
	var turns []*turn

	for mi, m := range req.Messages {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			if systemText.Len() > 0 {
				systemText.WriteString("\n")
			}
			systemText.WriteString(m.Content)
		case "user":
			if strings.TrimSpace(m.Content) == "" {
				// Empty-content messages must not produce empty text blocks —
				// Converse rejects them. Drop rather than send a blank turn.
				continue
			}
			turnFor(&turns, types.ConversationRoleUser).addText(m.Content)
		case "assistant":
			hasText := strings.TrimSpace(m.Content) != ""
			if !hasText && len(m.ToolCalls) == 0 {
				continue
			}
			// Build the tool_use blocks first so a malformed call rejects the
			// request before the turn list is touched.
			uses := make([]types.ContentBlock, 0, len(m.ToolCalls))
			for ci, tc := range m.ToolCalls {
				input, perr := parseToolArguments(tc.Function.Arguments)
				if perr != nil {
					return nil, nil, nil, validationError(fmt.Sprintf("messages[%d].tool_calls[%d].function.arguments: %v", mi, ci, perr))
				}
				uses = append(uses, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String(tc.ID),
					Name:      aws.String(tc.Function.Name),
					Input:     input,
				}})
			}
			t := turnFor(&turns, types.ConversationRoleAssistant)
			if hasText {
				t.addText(m.Content)
			}
			t.blocks = append(t.blocks, uses...)
		case "tool":
			if m.ToolCallID == "" {
				return nil, nil, nil, validationError(fmt.Sprintf("messages[%d].tool_call_id: required on a tool turn", mi))
			}
			result := m.Content
			if strings.TrimSpace(result) == "" {
				// Converse rejects an empty tool-result text block; a tool that
				// legitimately returned nothing still has to answer its call.
				result = emptyToolResultText
			}
			t := turnFor(&turns, types.ConversationRoleUser)
			t.results = append(t.results, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
				ToolUseId: aws.String(m.ToolCallID),
				Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: result}},
			}})
		default:
			// Unknown roles are dropped — Converse only accepts "user" and
			// "assistant" turns.
		}
	}

	// Drop any leading assistant turn(s). After the merge above there can be at
	// most one leading assistant turn (its neighbour, if any, is a different
	// role by construction). Tool results answering a dropped turn's tool_use
	// blocks would be orphans Converse rejects, so they are dropped with it, and
	// a user turn left empty by that goes too.
	for len(turns) > 0 {
		if turns[0].role == types.ConversationRoleAssistant {
			dropped := turns[0].toolUseIDs()
			turns = turns[1:]
			if len(turns) > 0 {
				turns[0].dropResults(dropped)
			}
			continue
		}
		if turns[0].empty() {
			turns = turns[1:]
			continue
		}
		break
	}

	if len(turns) == 0 {
		return nil, nil, nil, validationError("at least one user message is required")
	}

	for _, t := range turns {
		messages = append(messages, types.Message{Role: t.role, Content: t.content()})
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
	if req.TopP != nil {
		topP := float32(*req.TopP)
		inferenceConfig.TopP = &topP
	}
	if len(req.Stop) > 0 {
		inferenceConfig.StopSequences = req.Stop
	}

	return system, messages, inferenceConfig, nil
}

// emptyToolResultText stands in for a role:tool turn with no content.
const emptyToolResultText = "(empty result)"

// turn is one Converse message under construction. Tool results live apart
// from the other blocks so they can be emitted first: Converse requires a
// user message's toolResult blocks to lead.
type turn struct {
	role    types.ConversationRole
	results []types.ContentBlock
	blocks  []types.ContentBlock
}

// turnFor returns the turn a new message of role belongs to: the last turn
// when it has the same role (Converse requires strict alternation, so
// consecutive same-role messages merge), otherwise a fresh one.
func turnFor(turns *[]*turn, role types.ConversationRole) *turn {
	if n := len(*turns); n > 0 && (*turns)[n-1].role == role {
		return (*turns)[n-1]
	}
	t := &turn{role: role}
	*turns = append(*turns, t)
	return t
}

// addText appends text, joining onto a trailing text block with "\n".
func (t *turn) addText(s string) {
	if n := len(t.blocks); n > 0 {
		if tb, ok := t.blocks[n-1].(*types.ContentBlockMemberText); ok {
			tb.Value += "\n" + s
			return
		}
	}
	t.blocks = append(t.blocks, &types.ContentBlockMemberText{Value: s})
}

func (t *turn) content() []types.ContentBlock {
	out := make([]types.ContentBlock, 0, len(t.results)+len(t.blocks))
	out = append(out, t.results...)
	return append(out, t.blocks...)
}

func (t *turn) empty() bool { return len(t.results) == 0 && len(t.blocks) == 0 }

func (t *turn) toolUseIDs() map[string]bool {
	ids := map[string]bool{}
	for _, b := range t.blocks {
		if tu, ok := b.(*types.ContentBlockMemberToolUse); ok && tu.Value.ToolUseId != nil {
			ids[*tu.Value.ToolUseId] = true
		}
	}
	return ids
}

func (t *turn) dropResults(ids map[string]bool) {
	if len(ids) == 0 {
		return
	}
	kept := t.results[:0]
	for _, b := range t.results {
		if tr, ok := b.(*types.ContentBlockMemberToolResult); ok && tr.Value.ToolUseId != nil && ids[*tr.Value.ToolUseId] {
			continue
		}
		kept = append(kept, b)
	}
	t.results = kept
}

// validationError is the 400 BedrockAPIError for a request the gateway can
// tell Converse would refuse (or that is malformed), without a round trip.
func validationError(msg string) *BedrockAPIError {
	statusCode, body := TranslateBedrockError("ValidationException", msg)
	return &BedrockAPIError{StatusCode: statusCode, Body: body}
}

// decodeJSONDocument parses raw JSON for the SDK's document encoder. Numbers
// are decoded via json.Number and then converted to int64 when integral (else
// float64): the encoder writes a json.Number as a JSON string, and a plain
// float64 decode would round integers above 2^53.
func decodeJSONDocument(raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after JSON value")
	}
	return convertNumbers(v)
}

func convertNumbers(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		return x.Float64()
	case map[string]any:
		for k, e := range x {
			c, err := convertNumbers(e)
			if err != nil {
				return nil, err
			}
			x[k] = c
		}
	case []any:
		for i, e := range x {
			c, err := convertNumbers(e)
			if err != nil {
				return nil, err
			}
			x[i] = c
		}
	}
	return v, nil
}

// parseToolArguments turns an OpenAI arguments string into a toolUse input
// document. Empty arguments mean a no-argument call ({}); anything else must
// be a JSON object.
func parseToolArguments(args string) (document.Interface, error) {
	if strings.TrimSpace(args) == "" {
		return document.NewLazyDocument(map[string]any{}), nil
	}
	v, err := decodeJSONDocument(args)
	if err != nil {
		return nil, fmt.Errorf("must be a JSON object string: %v", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be a JSON object")
	}
	return document.NewLazyDocument(obj), nil
}

// translateToolConfig builds the Converse ToolConfiguration from the request's
// tools and tool_choice. It returns nil only when there are no tools.
//
// tool_choice "none" DIVERGES from the OpenAI contract: Converse has no
// "none" choice, and a history carrying toolUse/toolResult blocks is rejected
// without a toolConfig. So "none" keeps the full ToolConfiguration and omits
// ToolChoice (automatic), the closest Converse offers; the model may still
// call a tool on such a turn.
func translateToolConfig(req provider.ChatRequest) (*types.ToolConfiguration, error) {
	kind, name, ok := provider.ToolChoiceKind(req.ToolChoice)
	if !ok {
		return nil, validationError(`tool_choice: must be "none", "auto", "required" or {"type":"function","function":{"name":...}}`)
	}
	if len(req.Tools) == 0 {
		return nil, nil
	}

	cfg := &types.ToolConfiguration{}
	for i, t := range req.Tools {
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if p := strings.TrimSpace(string(t.Function.Parameters)); p != "" && p != "null" {
			v, err := decodeJSONDocument(p)
			if err != nil {
				return nil, validationError(fmt.Sprintf("tools[%d].function.parameters: must be a JSON Schema object", i))
			}
			if _, isObj := v.(map[string]any); !isObj {
				return nil, validationError(fmt.Sprintf("tools[%d].function.parameters: must be a JSON Schema object", i))
			}
			schema = v
		}
		spec := types.ToolSpecification{
			Name:        aws.String(t.Function.Name),
			InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)},
		}
		if t.Function.Description != "" {
			spec.Description = aws.String(t.Function.Description)
		}
		cfg.Tools = append(cfg.Tools, &types.ToolMemberToolSpec{Value: spec})
	}

	switch kind {
	case "auto":
		cfg.ToolChoice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
	case "required":
		cfg.ToolChoice = &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
	case "function":
		cfg.ToolChoice = &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: aws.String(name)}}
	}
	return cfg, nil
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
	var toolCalls []provider.ToolCall
	if msgOutput, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
		for _, block := range msgOutput.Value.Content {
			switch b := block.(type) {
			case *types.ContentBlockMemberText:
				content += b.Value
			case *types.ContentBlockMemberToolUse:
				args, err := marshalToolInput(b.Value.Input)
				if err != nil {
					return nil, fmt.Errorf("bedrock: decode toolUse input: %w", err)
				}
				toolCalls = append(toolCalls, provider.ToolCall{
					ID:   aws.ToString(b.Value.ToolUseId),
					Type: "function",
					Function: provider.ToolCallFunction{
						Name:      aws.ToString(b.Value.Name),
						Arguments: args,
					},
				})
			}
		}
	}

	var promptTokens, completionTokens, cacheRead, cacheWrite int
	if out.Usage != nil {
		if out.Usage.CacheReadInputTokens != nil {
			cacheRead = int(*out.Usage.CacheReadInputTokens)
		}
		if out.Usage.CacheWriteInputTokens != nil {
			cacheWrite = int(*out.Usage.CacheWriteInputTokens)
		}
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
		ID:                  "",
		Model:               modelID,
		Content:             content,
		PromptTokens:        promptTokens + cacheRead + cacheWrite,
		CompletionTokens:    completionTokens,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheWrite,
		FinishReason:        convertFinishReason(string(out.StopReason)),
		ToolCalls:           toolCalls,
	}, nil
}

// marshalToolInput renders a toolUse input document as the compact JSON
// arguments string OpenAI carries. A missing input is an empty object.
func marshalToolInput(doc document.Interface) (string, error) {
	if doc == nil {
		return "{}", nil
	}
	// MarshalSmithyDocument rather than UnmarshalSmithyDocument into an
	// interface{}: for a response document both yield the same JSON (numbers
	// stay exact), but it also works for the marshaler-backed documents the
	// package's own tests build.
	raw, err := doc.MarshalSmithyDocument()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	if buf.String() == "null" {
		return "{}", nil
	}
	return buf.String(), nil
}

// toolStreamState tracks the tool_use blocks of one ConverseStream. Bedrock's
// contentBlockIndex counts every block (text included); the OpenAI
// tool_calls[].index counts tool calls only, so each tool_use block gets the
// next tool index when its start event arrives.
type toolStreamState struct {
	next   int
	blocks map[int32]*toolStreamBlock
}

type toolStreamBlock struct {
	index   int
	sawArgs bool
}

func blockIndex(i *int32) int32 {
	if i == nil {
		return 0
	}
	return *i
}

func writeToolDelta(w io.Writer, completionID, modelID string, d provider.ToolCallDelta) error {
	chunk, err := provider.BuildToolCallDeltaSSE(completionID, modelID, d)
	if err != nil {
		return fmt.Errorf("bedrock: build tool-call SSE chunk: %w", err)
	}
	if _, err := w.Write(chunk); err != nil {
		return fmt.Errorf("bedrock: write tool-call SSE chunk: %w", err)
	}
	return nil
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
	ts *toolStreamState,
) (done bool, err error) {
	if ts == nil {
		ts = &toolStreamState{}
	}
	switch ev := event.(type) {
	case *types.ConverseStreamOutputMemberContentBlockStart:
		toolStart, ok := ev.Value.Start.(*types.ContentBlockStartMemberToolUse)
		if !ok {
			return false, nil
		}
		if ts.blocks == nil {
			ts.blocks = map[int32]*toolStreamBlock{}
		}
		blk := &toolStreamBlock{index: ts.next}
		ts.next++
		ts.blocks[blockIndex(ev.Value.ContentBlockIndex)] = blk
		d := provider.NewToolCallStartDelta(blk.index, aws.ToString(toolStart.Value.ToolUseId), aws.ToString(toolStart.Value.Name))
		if err := writeToolDelta(w, completionID, modelID, d); err != nil {
			return false, err
		}

	case *types.ConverseStreamOutputMemberContentBlockStop:
		// A tool_use block that streamed no input fragments is a no-argument
		// call; give the client "{}" so its accumulated arguments parse.
		key := blockIndex(ev.Value.ContentBlockIndex)
		if blk, ok := ts.blocks[key]; ok {
			delete(ts.blocks, key)
			if !blk.sawArgs {
				if err := writeToolDelta(w, completionID, modelID, provider.NewToolCallArgumentsDelta(blk.index, "{}")); err != nil {
					return false, err
				}
			}
		}

	case *types.ConverseStreamOutputMemberContentBlockDelta:
		if toolDelta, ok := ev.Value.Delta.(*types.ContentBlockDeltaMemberToolUse); ok {
			blk, tracked := ts.blocks[blockIndex(ev.Value.ContentBlockIndex)]
			frag := aws.ToString(toolDelta.Value.Input)
			if !tracked || frag == "" {
				return false, nil
			}
			blk.sawArgs = true
			if err := writeToolDelta(w, completionID, modelID, provider.NewToolCallArgumentsDelta(blk.index, frag)); err != nil {
				return false, err
			}
			return false, nil
		}
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
			if ev.Value.Usage.CacheReadInputTokens != nil {
				stats.CacheReadTokens = int(*ev.Value.Usage.CacheReadInputTokens)
			}
			if ev.Value.Usage.CacheWriteInputTokens != nil {
				stats.CacheCreationTokens = int(*ev.Value.Usage.CacheWriteInputTokens)
			}
			// AWS InputTokens excludes cache tokens; fold them in so
			// prompt_tokens = all billed input, as for every provider.
			var input int
			if ev.Value.Usage.InputTokens != nil {
				input = int(*ev.Value.Usage.InputTokens)
			}
			if ev.Value.Usage.InputTokens != nil || stats.CacheReadTokens != 0 || stats.CacheCreationTokens != 0 {
				stats.PromptTokens = input + stats.CacheReadTokens + stats.CacheCreationTokens
			}
			if ev.Value.Usage.OutputTokens != nil {
				stats.CompletionTokens = int(*ev.Value.Usage.OutputTokens)
			}
		}

	case *types.ConverseStreamOutputMemberMessageStart:
		// No SSE output needed for these markers.

	default:
		// Forward-compatible: future event types (e.g. reasoning deltas) are
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
