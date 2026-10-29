// Package openaicompat implements the provider.Provider interface for any
// upstream that speaks the OpenAI-compatible HTTP API (vLLM, LM Studio, etc.).
// Complete() and Stream() are thin wrappers that handle auth stripping,
// timeouts, and token-stat extraction.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

const (
	// dialTimeout is the TCP connect timeout. 5s is generous for in-cluster
	// traffic; if the SYN is dropped (e.g. by a NetworkPolicy) we fail fast
	// instead of waiting for the OS default (~120s).
	dialTimeout           = 5 * time.Second
	dialKeepAlive         = 30 * time.Second
	responseHeaderTimeout = 120 * time.Second
	idleConnTimeout       = 90 * time.Second
	// maxIdleConnsPerHost caps idle connections per upstream. 4 is sufficient
	// for typical single-node in-cluster deployments.
	maxIdleConnsPerHost = 4
	// defaultStreamIdleTimeout is the maximum duration between consecutive SSE
	// chunks before the stream is considered wedged and is cancelled. This
	// handles the case where the TCP connection stays open but the upstream
	// stops sending tokens (e.g. OOM-killed model process). Legitimate slow
	// generation is not affected because the timer resets on every received
	// chunk. See OpenAICompatProvider.StreamIdleTimeout for the (test-only)
	// override knob — production code never sets it, so this constant is
	// always what a real deployment uses.
	defaultStreamIdleTimeout = 45 * time.Second
	// maxNonStreamAdaptBodyBytes bounds how much of a non-SSE body is buffered
	// when a stream=true request is answered with a plain JSON chat.completion
	// (observed with LM Studio and some OpenAI-compatible proxies that ignore
	// the stream flag). See adaptNonStreamingResponse.
	maxNonStreamAdaptBodyBytes = 4 * 1024 * 1024
)

// OpenAICompatProvider forwards chat requests to an upstream server that
// speaks the OpenAI-compatible HTTP API (e.g. vLLM, LM Studio).
type OpenAICompatProvider struct {
	name       string // instance name, e.g. "vllm-default", "lmstudio-dev"
	kind       string // backend kind, e.g. "vllm", "lmstudio"
	upstream   *url.URL
	httpClient *http.Client

	// nonStreamWarnOnce ensures the "upstream does not stream" warning is
	// logged at most once per provider instance rather than once per request.
	nonStreamWarnOnce sync.Once

	// StreamIdleTimeout overrides defaultStreamIdleTimeout (45s) for Stream's
	// idle-read watchdog. Zero (the default returned by New) means "use
	// defaultStreamIdleTimeout" — production code (cmd/main.go) never sets
	// this; it exists solely so tests can shorten the idle window instead of
	// waiting out the real 45s default.
	StreamIdleTimeout time.Duration
}

// New creates an OpenAICompatProvider targeting the given upstream URL.
// name is the unique instance name (used as the routing-map key).
// kind is the backend kind ("vllm" or "lmstudio") written to audit records.
// Returns an error if upstream is not a valid URL with scheme and host.
func New(name, kind, upstream string) (*OpenAICompatProvider, error) {
	if name == "" {
		return nil, fmt.Errorf("openaicompat: name must not be empty")
	}
	if kind == "" {
		return nil, fmt.Errorf("openaicompat: kind must not be empty")
	}

	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: invalid upstream URL %q: %w", upstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("openaicompat: upstream URL %q must include scheme and host", upstream)
	}

	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
	return &OpenAICompatProvider{
		name:     name,
		kind:     kind,
		upstream: u,
		httpClient: &http.Client{
			Transport: &http.Transport{
				// DialContext bounds the TCP connect phase. Without this, a dropped
				// SYN (e.g. NetworkPolicy blocking egress) causes the OS TCP connect
				// timeout to fire — typically ~120s on Linux — which manifests as a
				// silent hang before the gateway can surface an error to the client.
				DialContext: dialer.DialContext,
				// Long completion responses mean we need a generous header timeout.
				// This is the time from a connected socket to first response byte,
				// NOT the total response time, so 120s is appropriate for slow-starting
				// models without penalising legitimate long generation.
				ResponseHeaderTimeout: responseHeaderTimeout,
				IdleConnTimeout:       idleConnTimeout,
				MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			},
			// No client-level Timeout: streaming responses can run for minutes.
			// The ResponseHeaderTimeout above protects against hung upstreams
			// at the connection layer; the DialContext timeout handles blocked SYNs.
		},
	}, nil
}

// Name returns the unique instance name (e.g. "vllm-default", "lmstudio-dev").
// This is the routing-map key and is used in log lines.
func (p *OpenAICompatProvider) Name() string { return p.name }

// Kind returns the backend kind ("vllm" or "lmstudio").
// This is what gets written to audit records — callers should use Kind()
// rather than Name() for audit/billing aggregation.
func (p *OpenAICompatProvider) Kind() string { return p.kind }

// ---------------------------------------------------------------------------
// Request/response shapes (OpenAI-compatible subset)
// ---------------------------------------------------------------------------

// compatWireMessage is one request turn on the wire. Content is a pointer so
// an assistant turn that only carries tool_calls renders "content": null.
type compatWireMessage struct {
	Role       string              `json:"role"`
	Content    *string             `json:"content"`
	ToolCalls  []provider.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	Name       string              `json:"name,omitempty"`
}

type compatRequest struct {
	Model       string              `json:"model"`
	Messages    []compatWireMessage `json:"messages"`
	Stream      bool                `json:"stream"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
	TopP        *float64            `json:"top_p,omitempty"`
	Stop        []string            `json:"stop,omitempty"`
	Tools       []provider.Tool     `json:"tools,omitempty"`
	ToolChoice  json.RawMessage     `json:"tool_choice,omitempty"`
	// ResponseFormat is sent only for json_object; StreamOptions only when
	// the caller asked for stream_options.include_usage, so an upstream that
	// would otherwise add a usage chunk to a stream nobody asked it of does not.
	ResponseFormat *compatResponseFormat `json:"response_format,omitempty"`
	StreamOptions  *compatStreamOptions  `json:"stream_options,omitempty"`
}

type compatResponseFormat struct {
	Type string `json:"type"`
}

type compatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type compatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []compatChoice `json:"choices"`
	Usage   *compatUsage   `json:"usage"`
	Error   *compatError   `json:"error"`
}

type compatChoice struct {
	Index        int            `json:"index"`
	Message      *compatMessage `json:"message"`
	Delta        *compatDelta   `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type compatMessage struct {
	Role      string              `json:"role"`
	Content   string              `json:"content"`
	ToolCalls []provider.ToolCall `json:"tool_calls"`
}

type compatDelta struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type compatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails.CachedTokens is passed through when the upstream
	// sends it (cache-read share of PromptTokens); 0 otherwise.
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

func (u *compatUsage) cachedTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

type compatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// ---------------------------------------------------------------------------
// Complete — non-streaming path
// ---------------------------------------------------------------------------

// Complete sends a non-streaming request to the upstream and returns a ChatResponse.
// The Authorization and X-Darkplum-User-Id headers are stripped before the
// request is forwarded.
func (p *OpenAICompatProvider) Complete(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	body, err := p.buildRequestBody(req, false)
	if err != nil {
		return nil, err
	}

	httpReq, err := p.newUpstreamRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openaicompat(%s): upstream request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat(%s): reading response body: %w", p.name, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, p.upstreamError(resp.StatusCode, respBytes)
	}

	var cr compatResponse
	if err := json.Unmarshal(respBytes, &cr); err != nil {
		return nil, fmt.Errorf("openaicompat(%s): decoding response: %w", p.name, err)
	}

	if cr.Error != nil {
		return nil, fmt.Errorf("openaicompat(%s): upstream error: %s (%s)", p.name, cr.Error.Message, cr.Error.Type)
	}

	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("openaicompat(%s): response contained no choices", p.name)
	}

	choice := cr.Choices[0]
	content := ""
	var toolCalls []provider.ToolCall
	if choice.Message != nil {
		content = choice.Message.Content
		toolCalls = normalizeToolCalls(choice.Message.ToolCalls)
	}

	finishReason := ""
	if choice.FinishReason != nil {
		finishReason = *choice.FinishReason
	}

	chatResp := &provider.ChatResponse{
		ID:           cr.ID,
		Model:        cr.Model,
		Content:      content,
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}
	if cr.Usage != nil {
		chatResp.PromptTokens = cr.Usage.PromptTokens
		chatResp.CompletionTokens = cr.Usage.CompletionTokens
		chatResp.CacheReadTokens = cr.Usage.cachedTokens()
	}

	return chatResp, nil
}

// ---------------------------------------------------------------------------
// Stream — streaming SSE path
// ---------------------------------------------------------------------------

// Stream sends a streaming request to the upstream. The upstream already
// speaks OpenAI SSE format so the response bytes are piped line-by-line
// directly to w. Each line is inspected as it passes through; the final data
// chunk (the one that carries finish_reason) is parsed to populate StreamStats.
// The "data: [DONE]\n\n" terminator is forwarded as-is.
//
// If w implements http.Flusher, it is flushed after each line so streaming
// clients receive tokens as they are generated rather than buffered.
//
// An idle-read watchdog (StreamIdleTimeout, defaulting to
// defaultStreamIdleTimeout) cancels the stream if no bytes arrive for longer
// than that window. The timer resets on every received chunk, so legitimate
// slow generation is not penalised. If the dial/connect itself fails, Stream
// returns an error before writing any bytes to w, allowing the caller to
// serve a clean JSON error response instead of a committed SSE stream.
//
// idleCtx (a context.WithCancelCause child of ctx) is created BEFORE the
// upstream request is built, and the request itself is built and issued
// under idleCtx — not ctx directly. net/http only aborts an in-flight
// request by cancelling the EXACT context.Context the request was built
// with (http.NewRequestWithContext); cancelling an unrelated context
// derived from it does nothing to a request already in flight. Building the
// request with idleCtx from the start costs nothing before the watchdog
// exists below (nothing cancels idleCtx early), and means the watchdog
// constructed once headers arrive can actually abort a blocked
// resp.Body.Read() instead of leaking the connection and the goroutine
// blocked on it.
func (p *OpenAICompatProvider) Stream(ctx context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	body, err := p.buildRequestBody(req, true)
	if err != nil {
		return nil, err
	}

	idleCtx, idleCancel := context.WithCancelCause(ctx)
	defer idleCancel(nil)

	httpReq, err := p.newUpstreamRequest(idleCtx, body)
	if err != nil {
		return nil, err
	}

	// httpClient.Do completes once response headers arrive (or fails on dial/connect
	// error). If the dial is blocked by a dropped SYN, the DialContext timeout
	// (dialTimeout) fires here — before any bytes are written to w — so the
	// caller can still serve a clean JSON error.
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openaicompat(%s): upstream stream request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBytes, _ := io.ReadAll(resp.Body)
		return nil, p.upstreamError(resp.StatusCode, errBytes)
	}

	// Some OpenAI-compatible upstreams (LM Studio, some proxies) ignore the
	// stream flag and reply with a plain JSON chat.completion body instead of
	// SSE. Detect that here and adapt rather than piping the JSON bytes
	// through as if they were SSE frames, which would leave the client with a
	// silent empty reply on a 200. Any Content-Type other than
	// text/event-stream (including a missing header) is treated as
	// non-streaming.
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "text/event-stream") {
		return p.adaptNonStreamingResponse(resp, w, req.IncludeUsage)
	}

	// Wrap the response body with an idle-timeout reader. If no bytes arrive
	// within the idle timeout between consecutive reads, idleCancel fires —
	// which, because the request above was built with idleCtx, actually
	// aborts the blocked Read() on trackedBody (see this function's doc
	// comment) — and the scanner receives an error, cleanly terminating the
	// loop.
	idleTimeout := p.StreamIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamIdleTimeout
	}
	trackedBody := provider.NewIdleTimeoutReader(resp.Body, idleTimeout, idleCancel)

	flusher, canFlush := w.(http.Flusher)

	stats := &provider.StreamStats{
		Model: req.Model,
	}

	completionID := ""
	sawUsageChunk := false

	scanner := bufio.NewScanner(trackedBody)
	// Increase scanner buffer for large chunks (some models emit large deltas).
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		// Check whether the idle watchdog fired (context cancelled).
		if err := idleCtx.Err(); err != nil {
			cause := context.Cause(idleCtx)
			return nil, fmt.Errorf("openaicompat(%s): stream idle timeout (%s): %w", p.name, idleTimeout, cause)
		}

		line := scanner.Text()

		// An upstream that ignored stream_options.include_usage would leave
		// the caller without the usage-only chunk it asked for; synthesize
		// it just before the terminator, from whatever usage the upstream
		// did report (zero counts when it reported none, as Bedrock does),
		// so include_usage is never silently unanswered.
		if req.IncludeUsage && !sawUsageChunk && line == "data: [DONE]" {
			if completionID == "" {
				completionID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
			}
			usageChunk, err := provider.BuildUsageChunkSSE(completionID, stats.Model, stats)
			if err != nil {
				return nil, fmt.Errorf("openaicompat(%s): building usage chunk: %w", p.name, err)
			}
			if _, err := w.Write(usageChunk); err != nil {
				return nil, fmt.Errorf("openaicompat(%s): writing usage chunk: %w", p.name, err)
			}
		}

		// Write the line plus the newline that the scanner stripped.
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			return nil, fmt.Errorf("openaicompat(%s): writing stream chunk: %w", p.name, err)
		}

		// SSE uses blank lines as event separators. After each blank line
		// (which terminates an event) we flush so the client sees the chunk.
		if line == "" {
			if canFlush {
				flusher.Flush()
			}
			continue
		}

		// Parse data lines for stats extraction. We skip comment lines (": ..."),
		// event-type lines ("event: ..."), and the [DONE] sentinel.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}

		// Parse every chunk to capture the most recent finish_reason and usage.
		// Most chunks will have neither; we overwrite stats on the ones that do.
		var chunk compatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Malformed chunk — log-worthy but not fatal; keep streaming.
			continue
		}

		if chunk.Model != "" {
			stats.Model = chunk.Model
		}
		if chunk.ID != "" {
			completionID = chunk.ID
		}
		if len(chunk.Choices) == 0 && chunk.Usage != nil {
			sawUsageChunk = true
		}

		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			if c.FinishReason != nil && *c.FinishReason != "" {
				stats.FinishReason = *c.FinishReason
			}
		}

		// OpenAI-compatible upstreams include usage in the final streaming chunk
		// when stream_options.include_usage is set. Capture it if present.
		if chunk.Usage != nil {
			stats.PromptTokens = chunk.Usage.PromptTokens
			stats.CompletionTokens = chunk.Usage.CompletionTokens
			stats.CacheReadTokens = chunk.Usage.cachedTokens()
		}
	}

	if err := scanner.Err(); err != nil {
		// If the error is due to the idle watchdog, unwrap to a clear message.
		if idleCtx.Err() != nil {
			cause := context.Cause(idleCtx)
			return nil, fmt.Errorf("openaicompat(%s): stream idle timeout (%s): %w", p.name, idleTimeout, cause)
		}
		return nil, fmt.Errorf("openaicompat(%s): reading stream: %w", p.name, err)
	}

	// Final flush after the stream ends.
	if canFlush {
		flusher.Flush()
	}

	return stats, nil
}

// adaptNonStreamingResponse handles a stream=true request that was answered
// with a plain JSON chat.completion body instead of SSE. It buffers the body
// (bounded by maxNonStreamAdaptBodyBytes), decodes it, and — only once that
// has succeeded — synthesizes an equivalent SSE sequence (one delta chunk,
// one final chunk carrying finish_reason/usage, then the [DONE] terminator)
// to w. Because decoding happens before anything is written to w, a decode
// failure surfaces as a normal error and the caller can still serve a clean
// JSON error response instead of a corrupted stream.
func (p *OpenAICompatProvider) adaptNonStreamingResponse(resp *http.Response, w io.Writer, includeUsage bool) (*provider.StreamStats, error) {
	p.nonStreamWarnOnce.Do(func() {
		slog.Warn("openaicompat: upstream does not support streaming, adapting JSON response to SSE",
			"provider", p.name, "kind", p.kind, "content_type", resp.Header.Get("Content-Type"))
	})

	limited := io.LimitReader(resp.Body, maxNonStreamAdaptBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("openaicompat(%s): reading non-streaming response body: %w", p.name, err)
	}
	if len(body) > maxNonStreamAdaptBodyBytes {
		return nil, fmt.Errorf("openaicompat(%s): non-streaming response body exceeds %d bytes", p.name, maxNonStreamAdaptBodyBytes)
	}

	var cr compatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("openaicompat(%s): decoding non-streaming response as chat.completion: %w", p.name, err)
	}
	if cr.Error != nil {
		return nil, fmt.Errorf("openaicompat(%s): upstream error: %s (%s)", p.name, cr.Error.Message, cr.Error.Type)
	}
	if len(cr.Choices) == 0 || cr.Choices[0].Message == nil {
		return nil, fmt.Errorf("openaicompat(%s): non-streaming response contained no message choices", p.name)
	}

	choice := cr.Choices[0]
	content := choice.Message.Content
	finishReason := "stop"
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		finishReason = *choice.FinishReason
	}

	stats := &provider.StreamStats{
		Model:        cr.Model,
		FinishReason: finishReason,
	}
	if cr.Usage != nil {
		stats.PromptTokens = cr.Usage.PromptTokens
		stats.CompletionTokens = cr.Usage.CompletionTokens
		stats.CacheReadTokens = cr.Usage.cachedTokens()
	}

	// Content chunk first (always, as before, unless the turn is only tool
	// calls), then each call as a start fragment plus an arguments fragment.
	var frames [][]byte
	toolCalls := normalizeToolCalls(choice.Message.ToolCalls)
	if content != "" || len(toolCalls) == 0 {
		deltaChunk, err := provider.BuildStreamingChunkSSE(cr.ID, cr.Model, content)
		if err != nil {
			return nil, fmt.Errorf("openaicompat(%s): building synthesized delta chunk: %w", p.name, err)
		}
		frames = append(frames, deltaChunk)
	}
	for i, tc := range toolCalls {
		for _, d := range []provider.ToolCallDelta{
			provider.NewToolCallStartDelta(i, tc.ID, tc.Function.Name),
			provider.NewToolCallArgumentsDelta(i, tc.Function.Arguments),
		} {
			frame, err := provider.BuildToolCallDeltaSSE(cr.ID, cr.Model, d)
			if err != nil {
				return nil, fmt.Errorf("openaicompat(%s): building synthesized tool-call chunk: %w", p.name, err)
			}
			frames = append(frames, frame)
		}
	}
	finalChunk, err := provider.BuildFinalChunksSSE(cr.ID, cr.Model, finishReason, stats, includeUsage)
	if err != nil {
		return nil, fmt.Errorf("openaicompat(%s): building synthesized final chunk: %w", p.name, err)
	}

	// Nothing has been written to w until this point, so every error path
	// above leaves w untouched.
	for _, frame := range frames {
		if _, err := w.Write(frame); err != nil {
			return nil, fmt.Errorf("openaicompat(%s): writing synthesized delta chunk: %w", p.name, err)
		}
	}
	if _, err := w.Write(finalChunk); err != nil {
		return nil, fmt.Errorf("openaicompat(%s): writing synthesized final chunk: %w", p.name, err)
	}
	if _, err := io.WriteString(w, provider.StreamDoneSSE); err != nil {
		return nil, fmt.Errorf("openaicompat(%s): writing stream terminator: %w", p.name, err)
	}

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	return stats, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (p *OpenAICompatProvider) buildRequestBody(req provider.ChatRequest, stream bool) ([]byte, error) {
	messages := make([]compatWireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		wm := compatWireMessage{Role: m.Role, ToolCallID: m.ToolCallID, Name: m.Name}
		if m.Content == "" && len(m.ToolCalls) > 0 {
			wm.Content = nil // renders "content": null
		} else {
			c := m.Content
			wm.Content = &c
		}
		for _, tc := range m.ToolCalls {
			if tc.Type == "" {
				tc.Type = "function"
			}
			if strings.TrimSpace(tc.Function.Arguments) == "" {
				tc.Function.Arguments = "{}"
			}
			wm.ToolCalls = append(wm.ToolCalls, tc)
		}
		messages = append(messages, wm)
	}
	cr := compatRequest{
		Model:       req.Model,
		Messages:    messages,
		Stream:      stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	if len(req.Tools) > 0 {
		cr.Tools = req.Tools
	}
	if kind, _, _ := provider.ToolChoiceKind(req.ToolChoice); kind != "" {
		cr.ToolChoice = req.ToolChoice
	}
	if req.JSONMode {
		cr.ResponseFormat = &compatResponseFormat{Type: "json_object"}
	}
	if stream && req.IncludeUsage {
		cr.StreamOptions = &compatStreamOptions{IncludeUsage: true}
	}
	b, err := json.Marshal(cr)
	if err != nil {
		return nil, fmt.Errorf("openaicompat(%s): marshalling request: %w", p.name, err)
	}
	return b, nil
}

func (p *OpenAICompatProvider) newUpstreamRequest(ctx context.Context, body []byte) (*http.Request, error) {
	endpoint := p.upstream.String() + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openaicompat(%s): building request: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Authorization and X-Darkplum-User-Id are gateway-internal headers.
	// The upstream has no auth layer inside the cluster and must never see them.
	httpReq.Header.Del("Authorization")
	httpReq.Header.Del("X-Darkplum-User-Id")
	return httpReq, nil
}

func (p *OpenAICompatProvider) upstreamError(statusCode int, body []byte) error {
	var errResp struct {
		Error *compatError `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != nil {
		return fmt.Errorf("openaicompat(%s): upstream returned %d: %s (%s)",
			p.name, statusCode, errResp.Error.Message, errResp.Error.Type)
	}
	return fmt.Errorf("openaicompat(%s): upstream returned %d: %s", p.name, statusCode, string(body))
}

// SupportsTools makes this a tool-capable backend (provider.ToolCapable).
func (p *OpenAICompatProvider) SupportsTools() bool { return true }

// ParamCaps implements provider.ParamCapable. vLLM takes json_object, stop and
// tool_choice required/named; it ignores parallel_tool_calls:false, and
// logprobs/logit_bias are not put on the wire yet, so those are refused.
func (p *OpenAICompatProvider) ParamCaps() provider.ParamCaps {
	return provider.ParamCaps{JSONMode: true, Stop: true, ForcedToolChoice: true}
}

// normalizeToolCalls defaults a missing type to "function" and drops nothing
// else: ids, names and argument strings are relayed exactly as the upstream
// sent them.
func normalizeToolCalls(in []provider.ToolCall) []provider.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, len(in))
	for i, tc := range in {
		if tc.Type == "" {
			tc.Type = "function"
		}
		out[i] = tc
	}
	return out
}

// compile-time interface check.
var _ provider.Provider = (*OpenAICompatProvider)(nil)
