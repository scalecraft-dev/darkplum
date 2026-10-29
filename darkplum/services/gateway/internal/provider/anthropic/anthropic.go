// Package anthropic implements provider.Provider for the direct Anthropic
// Messages API, using the official Go SDK
// (github.com/anthropics/anthropic-sdk-go). Credentials come from a
// file-backed credsource.Source (a Kubernetes secret volume mount) — never
// from an env var, and never logged. IAM-style enforcement does not exist at
// this boundary (a vendor API key is an org-wide bearer credential, not a
// scoped cloud role); Layer 1 for a vendor route is whatever scope the
// operator gave the key on the vendor's own side (see the plan's Layer 1
// honesty note) — OPA in front of this provider is the only enforcement
// Darkplum itself contributes for external_api routes.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
)

const (
	// streamIdleTimeout mirrors openaicompat's inter-event watchdog: if no
	// stream event arrives for this long, the stream is considered wedged
	// and cancelled. Reset on every event received, so legitimate slow
	// generation is never penalised.
	streamIdleTimeout = 45 * time.Second
	// discoveryTimeout bounds a single DiscoverModels call independently of
	// whatever timeout routing.Manager's caller applies, so a call made
	// directly against this package (bypassing Manager) still cannot hang
	// forever, and so ListAutoPaging's sequential page fetches cannot
	// individually exceed this budget.
	discoveryTimeout = 15 * time.Second
	// sdkMaxRetries is kept low so a 429 (with its Retry-After) or a
	// transient 5xx surfaces to the caller quickly rather than being
	// silently absorbed by the SDK's own retry loop for many seconds.
	sdkMaxRetries = 1
)

// credentialSource is the subset of *credsource.Source this package depends
// on. Defined as an interface so tests can substitute a fake without
// touching the filesystem.
type credentialSource interface {
	Key() (key string, version uint64, err error)
}

var _ credentialSource = (*credsource.Source)(nil)

// Config holds the vendor-specific knobs a Provider needs beyond its
// credential source.
type Config struct {
	// DefaultMaxTokens is used when a caller's request does not set
	// max_tokens (ANTHROPIC_DEFAULT_MAX_TOKENS).
	DefaultMaxTokens int
	// BaseURL overrides the SDK's production endpoint (ANTHROPIC_BASE_URL) —
	// tests and dev proxies only. Empty means use the SDK default.
	BaseURL string
	// Status, if non-nil, receives a Report call whenever a request hits a
	// 401/403 (see translateError) — in addition to the periodic Report
	// calls routing.NewAnthropicSource makes on every discovery tick.
	Status authFailureReporter
	// WorkspaceID returns the effective Anthropic workspace id, "" for none.
	// Evaluated per request; when non-empty every request carries an
	// anthropic-workspace-id header (a per-request option, so the
	// version-cached SDK client is never rebuilt for it). nil means none.
	WorkspaceID func() string
}

// Provider implements provider.Provider for the direct Anthropic API.
type Provider struct {
	creds  credentialSource
	cfg    Config
	client *http.Client

	// streamIdleTimeout defaults to the package-level streamIdleTimeout
	// constant; overridable (same-package tests only) so the idle-watchdog
	// test doesn't need to wait 45 real seconds.
	streamIdleTimeout time.Duration

	mu           sync.Mutex
	sdkClient    *sdk.Client
	sdkVersion   uint64
	sdkConfigErr error

	modelsMu        sync.RWMutex
	maxOutputTokens map[string]int64
}

// New builds a Provider. creds supplies the API key (see credsource.Source);
// it is read on every request/discovery call, not just at startup, so a key
// rotation takes effect within creds' own recheck interval with no gateway
// restart.
func New(creds credentialSource, cfg Config) *Provider {
	return &Provider{
		creds:             creds,
		cfg:               cfg,
		client:            provider.NewVendorHTTPClient(),
		streamIdleTimeout: streamIdleTimeout,
		maxOutputTokens:   make(map[string]int64),
	}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "anthropic" }

// Kind matches Name — present for symmetry with providers whose Name is an
// instance identifier distinct from their audit Kind (e.g. openaicompat).
func (p *Provider) Kind() string { return "anthropic" }

// sdkClientFor resolves the current credential and returns an SDK client for
// it, rebuilding the client only when the credential's version has changed
// (see credsource.Source.Key). SDK debug logging is never enabled — no
// option in this construction ever turns it on, and none of it is
// conditioned on any environment variable, so it cannot be turned on by one
// either.
func (p *Provider) sdkClientFor() (*sdk.Client, error) {
	key, version, err := p.creds.Key()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.sdkClient != nil && p.sdkVersion == version {
		return p.sdkClient, nil
	}

	opts := []option.RequestOption{
		// This package does its own credential resolution (credsource); skip
		// the SDK's env/profile autoload entirely so an operator's stray
		// ANTHROPIC_API_KEY in the pod's environment can never silently
		// shadow the file-backed key this gateway was actually configured
		// with.
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(key),
		option.WithHTTPClient(p.client),
		option.WithMaxRetries(sdkMaxRetries),
	}
	if p.cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.cfg.BaseURL))
	}

	client := sdk.NewClient(opts...)
	p.sdkClient = &client
	p.sdkVersion = version
	return p.sdkClient, nil
}

// requestOptions returns the per-request SDK options for the effective
// workspace id: the anthropic-workspace-id header when one is in force.
func (p *Provider) requestOptions(ctx context.Context) []option.RequestOption {
	if id, ok := provider.WorkspaceOverrideFromContext(ctx); ok {
		return workspaceOptions(id)
	}
	if p.cfg.WorkspaceID == nil {
		return nil
	}
	return workspaceOptions(p.cfg.WorkspaceID())
}

func workspaceOptions(workspaceID string) []option.RequestOption {
	if workspaceID == "" {
		return nil
	}
	return []option.RequestOption{option.WithHeader("anthropic-workspace-id", workspaceID)}
}

// resolveMaxTokens picks the max_tokens value to send: the caller's value if
// set, else Config.DefaultMaxTokens, clamped to the model's own max output
// tokens if discovery has learned it (see DiscoverModels).
func (p *Provider) resolveMaxTokens(model string, requested int) int64 {
	max := int64(requested)
	if max <= 0 {
		max = int64(p.cfg.DefaultMaxTokens)
	}

	p.modelsMu.RLock()
	limit, ok := p.maxOutputTokens[model]
	p.modelsMu.RUnlock()

	if ok && limit > 0 && max > limit {
		max = limit
	}
	return max
}

// Complete implements provider.Provider.
func (p *Provider) Complete(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	norm, err := normalizeMessages(req.Messages)
	if err != nil {
		return nil, err
	}

	client, err := p.sdkClientFor()
	if err != nil {
		return nil, translateCredentialError(err)
	}

	maxTokens := p.resolveMaxTokens(req.Model, req.MaxTokens)
	params := buildParams(req, norm, maxTokens, true)

	reqOpts := p.requestOptions(ctx)
	msg, err := client.Messages.New(ctx, params, reqOpts...)
	if err != nil && isSamplingParamUnsupportedError(err) {
		slog.Info("anthropic: retrying without sampling parameters (temperature, top_p)", "model", req.Model)
		params = buildParams(req, norm, maxTokens, false)
		msg, err = client.Messages.New(ctx, params, reqOpts...)
	}
	if err != nil {
		return nil, translateError(err, p.cfg.Status, p.Name())
	}

	return toChatResponse(msg), nil
}

// Stream implements provider.Provider. Rendered ONLY through
// provider.BuildStreamingChunkSSE / BuildFinalChunkSSE / StreamDoneSSE, per
// the shared OpenAI-wire contract every provider uses.
func (p *Provider) Stream(ctx context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	norm, err := normalizeMessages(req.Messages)
	if err != nil {
		return nil, err
	}

	client, err := p.sdkClientFor()
	if err != nil {
		return nil, translateCredentialError(err)
	}

	maxTokens := p.resolveMaxTokens(req.Model, req.MaxTokens)
	params := buildParams(req, norm, maxTokens, true)

	// idleCtx bounds each individual stream.Next() call: the Messages
	// streaming API has no per-chunk deadline of its own, so a connection
	// that stays open but stops sending events would otherwise hang forever.
	// This is a ctx-cancel timer around stream.Next() rather than an
	// io.Reader wrap (unlike openaicompat/NewIdleTimeoutReader) because the
	// SDK's ssestream.Stream does not expose its underlying reader.
	idleCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	reqOpts := p.requestOptions(ctx)
	stream := client.Messages.NewStreaming(idleCtx, params, reqOpts...)
	if stream.Err() != nil && isSamplingParamUnsupportedError(stream.Err()) {
		slog.Info("anthropic: retrying stream without sampling parameters (temperature, top_p)", "model", req.Model)
		_ = stream.Close()
		params = buildParams(req, norm, maxTokens, false)
		stream = client.Messages.NewStreaming(idleCtx, params, reqOpts...)
	}
	if stream.Err() != nil {
		// Nothing has been written to w yet at this point: NewStreaming's
		// initial request/response round-trip has already completed (an
		// error here reflects a non-2xx response, not a mid-stream failure),
		// so the caller can still serve a clean JSON error.
		return nil, translateError(stream.Err(), p.cfg.Status, p.Name())
	}
	defer stream.Close()

	idleTimeout := p.streamIdleTimeout
	idleTimer := time.AfterFunc(idleTimeout, func() {
		cancel(&idleTimeoutError{timeout: idleTimeout.String()})
	})
	defer idleTimer.Stop()

	flusher, canFlush := w.(http.Flusher)

	acc := sdk.Message{}
	completionID := ""
	model := req.Model
	// Anthropic's content-block index counts every block (text included); the
	// OpenAI tool_calls index counts tool calls only.
	toolIndexByBlock := map[int64]int{}
	toolArgsSent := map[int]bool{}

	writeChunk := func(chunk []byte, buildErr error) error {
		if buildErr != nil {
			return fmt.Errorf("anthropic: building streaming chunk: %w", buildErr)
		}
		if _, writeErr := w.Write(chunk); writeErr != nil {
			return fmt.Errorf("anthropic: writing streaming chunk: %w", writeErr)
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	}

	for stream.Next() {
		idleTimer.Reset(idleTimeout)
		ev := stream.Current()
		if err := acc.Accumulate(ev); err != nil {
			return nil, translateError(fmt.Errorf("anthropic: accumulating stream event: %w", err), p.cfg.Status, p.Name())
		}

		switch e := ev.AsAny().(type) {
		case sdk.MessageStartEvent:
			completionID = e.Message.ID
			if string(e.Message.Model) != "" {
				model = string(e.Message.Model)
			}
		case sdk.ContentBlockStartEvent:
			if e.ContentBlock.Type != "tool_use" {
				continue
			}
			idx := len(toolIndexByBlock)
			toolIndexByBlock[e.Index] = idx
			chunk, buildErr := provider.BuildToolCallDeltaSSE(completionID, model,
				provider.NewToolCallStartDelta(idx, e.ContentBlock.ID, e.ContentBlock.Name))
			if err := writeChunk(chunk, buildErr); err != nil {
				return nil, err
			}
		case sdk.ContentBlockStopEvent:
			// A tool with no arguments may stream no input_json_delta at all;
			// OpenAI clients would then parse "" as the arguments. Send {}.
			if idx, ok := toolIndexByBlock[e.Index]; ok && !toolArgsSent[idx] {
				toolArgsSent[idx] = true
				chunk, buildErr := provider.BuildToolCallDeltaSSE(completionID, model,
					provider.NewToolCallArgumentsDelta(idx, "{}"))
				if err := writeChunk(chunk, buildErr); err != nil {
					return nil, err
				}
			}
		case sdk.ContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case sdk.InputJSONDelta:
				idx, ok := toolIndexByBlock[e.Index]
				if !ok || d.PartialJSON == "" {
					continue
				}
				toolArgsSent[idx] = true
				chunk, buildErr := provider.BuildToolCallDeltaSSE(completionID, model,
					provider.NewToolCallArgumentsDelta(idx, d.PartialJSON))
				if err := writeChunk(chunk, buildErr); err != nil {
					return nil, err
				}
			case sdk.TextDelta:
				if d.Text == "" {
					continue
				}
				chunk, buildErr := provider.BuildStreamingChunkSSE(completionID, model, d.Text)
				if buildErr != nil {
					return nil, fmt.Errorf("anthropic: building streaming chunk: %w", buildErr)
				}
				if _, writeErr := w.Write(chunk); writeErr != nil {
					return nil, fmt.Errorf("anthropic: writing streaming chunk: %w", writeErr)
				}
				if canFlush {
					flusher.Flush()
				}
			}
			// Every other delta type (thinking_delta,
			// signature_delta, citations_delta) carries nothing the
			// OpenAI-compatible surface renders; skipped by omission.
		}
	}
	idleTimer.Stop()

	if err := stream.Err(); err != nil {
		if cause := context.Cause(idleCtx); cause != nil {
			var idleErr *idleTimeoutError
			if errors.As(cause, &idleErr) {
				return nil, translateError(idleErr, p.cfg.Status, p.Name())
			}
		}
		return nil, translateError(err, p.cfg.Status, p.Name())
	}

	finishReason := mapFinishReason(acc.StopReason)
	stats := &provider.StreamStats{
		Model:               model,
		PromptTokens:        int(acc.Usage.InputTokens + acc.Usage.CacheCreationInputTokens + acc.Usage.CacheReadInputTokens),
		CompletionTokens:    int(acc.Usage.OutputTokens),
		CacheReadTokens:     int(acc.Usage.CacheReadInputTokens),
		CacheCreationTokens: int(acc.Usage.CacheCreationInputTokens),
		FinishReason:        finishReason,
	}

	finalChunk, err := provider.BuildFinalChunksSSE(completionID, model, finishReason, stats, req.IncludeUsage)
	if err != nil {
		return nil, fmt.Errorf("anthropic: building final chunk: %w", err)
	}
	if _, err := w.Write(finalChunk); err != nil {
		return nil, fmt.Errorf("anthropic: writing final chunk: %w", err)
	}
	if _, err := io.WriteString(w, provider.StreamDoneSSE); err != nil {
		return nil, fmt.Errorf("anthropic: writing stream terminator: %w", err)
	}
	if canFlush {
		flusher.Flush()
	}

	return stats, nil
}

// buildParams translates a normalized request into SDK params. includeSampling
// controls whether temperature/top_p are sent at all — false on the
// one-time retry after the vendor rejects them for the requested model (see
// isSamplingParamUnsupportedError). stop and top_p are otherwise forwarded
// whenever the caller set them, matching every other provider's
// request-shape honesty.
func buildParams(req provider.ChatRequest, norm normalizedRequest, maxTokens int64, includeSampling bool) sdk.MessageNewParams {
	params := sdk.MessageNewParams{
		Model:     sdk.Model(req.Model),
		MaxTokens: maxTokens,
		Messages:  toSDKMessages(norm.Turns),
	}
	if norm.System != "" {
		params.System = []sdk.TextBlockParam{{Text: norm.System}}
	}
	params.Tools, params.ToolChoice = toSDKTools(req)
	// req.User is already the org-keyed HMAC pseudonym (ParamCaps().UserID is
	// UserIDHMAC), never the caller's raw value; empty when no secret is
	// installed, in which case nothing is sent.
	if req.User != "" {
		params.Metadata = sdk.MetadataParam{UserID: sdk.String(req.User)}
	}
	if len(req.Stop) > 0 {
		params.StopSequences = req.Stop
	}
	if includeSampling {
		if req.Temperature != nil {
			params.Temperature = sdk.Float(*req.Temperature)
		}
		if req.TopP != nil {
			params.TopP = sdk.Float(*req.TopP)
		}
	}
	return params
}

func toSDKMessages(turns []turn) []sdk.MessageParam {
	out := make([]sdk.MessageParam, 0, len(turns))
	for _, t := range turns {
		var blocks []sdk.ContentBlockParamUnion
		if t.Role == "assistant" {
			if strings.TrimSpace(t.Content) != "" {
				blocks = append(blocks, sdk.NewTextBlock(t.Content))
			}
			for _, u := range t.ToolUses {
				blocks = append(blocks, sdk.NewToolUseBlock(u.ID, u.Input, u.Name))
			}
			out = append(out, sdk.NewAssistantMessage(blocks...))
			continue
		}
		// tool_result blocks must lead the user message.
		for _, r := range t.ToolResults {
			content := r.Content
			if strings.TrimSpace(content) == "" {
				// Anthropic rejects an empty tool_result text block.
				content = emptyToolResultText
			}
			blocks = append(blocks, sdk.NewToolResultBlock(r.ToolUseID, content, false))
		}
		if strings.TrimSpace(t.Content) != "" {
			blocks = append(blocks, sdk.NewTextBlock(t.Content))
		}
		out = append(out, sdk.NewUserMessage(blocks...))
	}
	return out
}

// emptyToolResultText stands in for a role:tool turn with no content
// (identical to the bedrock provider).
const emptyToolResultText = "(empty result)"

// SupportsTools implements provider.ToolCapable.
func (p *Provider) SupportsTools() bool { return true }

// ParamCaps implements provider.ParamCapable. The Messages API has no JSON
// mode, logprobs or logit_bias. tool_choice any/tool is enforced natively, and
// parallel_tool_calls:false is translated to disable_parallel_tool_use on the
// tool_choice. `user` becomes metadata.user_id as an HMAC pseudonym.
func (p *Provider) ParamCaps() provider.ParamCaps {
	return provider.ParamCaps{
		Stop: true, ForcedToolChoice: true, SerialToolCalls: true,
		UserID: provider.UserIDHMAC,
	}
}

// toSDKTools translates the request's tools and tool_choice. tool_choice
// "none" still sends the tool definitions (a history carrying tool_use /
// tool_result blocks is rejected without them) and renders as tool_choice
// {"type":"none"}. Absent tool_choice omits the field.
func toSDKTools(req provider.ChatRequest) ([]sdk.ToolUnionParam, sdk.ToolChoiceUnionParam) {
	var choice sdk.ToolChoiceUnionParam
	if len(req.Tools) == 0 {
		return nil, choice
	}
	kind, name, _ := provider.ToolChoiceKind(req.ToolChoice)
	switch kind {
	case "none":
		choice = sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}
	case "auto":
		choice = sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{}}
	case "required":
		choice = sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{}}
	case "function":
		choice = sdk.ToolChoiceUnionParam{OfTool: &sdk.ToolChoiceToolParam{Name: name}}
	}

	// parallel_tool_calls:false -> disable_parallel_tool_use:true on whatever
	// tool_choice applies. Absent tool_choice is auto, which is where the
	// flag has to live. "none" calls no tool at all, so there is nothing to
	// serialise and the flag is a no-op there.
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		disable := sdk.Bool(true)
		switch kind {
		case "", "auto":
			choice = sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{DisableParallelToolUse: disable}}
		case "required":
			choice = sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{DisableParallelToolUse: disable}}
		case "function":
			choice = sdk.ToolChoiceUnionParam{OfTool: &sdk.ToolChoiceToolParam{Name: name, DisableParallelToolUse: disable}}
		}
	}

	tools := make([]sdk.ToolUnionParam, 0, len(req.Tools))
	for _, t := range req.Tools {
		tp := sdk.ToolParam{Name: t.Function.Name, InputSchema: toInputSchema(t.Function.Parameters)}
		if t.Function.Description != "" {
			tp.Description = sdk.String(t.Function.Description)
		}
		tools = append(tools, sdk.ToolUnionParam{OfTool: &tp})
	}
	return tools, choice
}

// toInputSchema maps a raw JSON Schema object onto the SDK's input_schema,
// keeping every key (e.g. $defs, additionalProperties) as an extra field. A
// missing/null schema becomes {"type":"object","properties":{}}.
func toInputSchema(raw json.RawMessage) sdk.ToolInputSchemaParam {
	schema := sdk.ToolInputSchemaParam{Properties: map[string]any{}}
	var obj map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return schema
	}
	extras := map[string]any{}
	for k, v := range obj {
		switch k {
		case "type":
		case "properties":
			if v != nil {
				schema.Properties = v
			}
		case "required":
			if list, ok := v.([]any); ok {
				for _, r := range list {
					if s, ok := r.(string); ok {
						schema.Required = append(schema.Required, s)
					}
				}
			}
		default:
			extras[k] = v
		}
	}
	if len(extras) > 0 {
		schema.ExtraFields = extras
	}
	return schema
}

// toChatResponse converts a complete (non-streaming) SDK Message into
// Darkplum's provider.ChatResponse. Prompt tokens reported to Darkplum are
// input + cache_read + cache_creation, matching the Messages API's own
// documented total ("Total input tokens ... is the summation of
// input_tokens, cache_creation_input_tokens, and cache_read_input_tokens").
func toChatResponse(msg *sdk.Message) *provider.ChatResponse {
	var content strings.Builder
	var toolCalls []provider.ToolCall
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, provider.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      block.Name,
					Arguments: compactToolInput(block.Input),
				},
			})
		}
	}

	usage := msg.Usage
	return &provider.ChatResponse{
		ID:                  msg.ID,
		Model:               string(msg.Model),
		Content:             content.String(),
		PromptTokens:        int(usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens),
		CompletionTokens:    int(usage.OutputTokens),
		CacheReadTokens:     int(usage.CacheReadInputTokens),
		CacheCreationTokens: int(usage.CacheCreationInputTokens),
		FinishReason:        mapFinishReason(msg.StopReason),
		ToolCalls:           toolCalls,
	}
}

// compactToolInput renders a tool_use input as compact JSON; an absent or
// invalid input is the empty object.
func compactToolInput(input json.RawMessage) string {
	var buf bytes.Buffer
	if len(input) == 0 || json.Compact(&buf, input) != nil || buf.String() == "null" {
		return "{}"
	}
	return buf.String()
}

// mapFinishReason maps a Messages API stop_reason to the OpenAI-compatible
// finish_reason vocabulary. A refusal (streaming classifiers intervening on a
// potential policy violation) maps to content_filter even when it carries no
// text — callers still get a well-formed response with empty content rather
// than an error.
func mapFinishReason(reason sdk.StopReason) string {
	switch reason {
	case sdk.StopReasonMaxTokens:
		return "length"
	case sdk.StopReasonRefusal:
		return "content_filter"
	case sdk.StopReasonToolUse:
		return "tool_calls"
	case sdk.StopReasonEndTurn, sdk.StopReasonStopSequence:
		return "stop"
	default:
		// pause_turn, model_context_window_exceeded, and any future value all
		// map to the same generic "stop" — none of them has a closer
		// OpenAI-compatible equivalent.
		return "stop"
	}
}

// translateCredentialError wraps a credsource.Source.Key() failure that
// surfaced at request time (rather than at a routing discovery tick, where
// it is handled directly) into a generic scrubbed upstream error. This
// should not normally be reachable in production: a model only appears in
// the route table once discovery has already proven a key is present and
// working, but a key that is rotated OUT from under a still-routable model
// (before the next discovery tick expires it) can hit this path.
func translateCredentialError(err error) error {
	return newAPIError(http.StatusBadGateway, "upstream_error", "upstream_error",
		"the model provider is not currently configured", 0, err)
}

var _ provider.Provider = (*Provider)(nil)
