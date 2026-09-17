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
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
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
	// streamIdleTimeout is the maximum duration between consecutive SSE chunks
	// before the stream is considered wedged and is cancelled. This handles the
	// case where the TCP connection stays open but the upstream stops sending
	// tokens (e.g. OOM-killed model process). Legitimate slow generation is not
	// affected because the timer resets on every received chunk.
	streamIdleTimeout = 45 * time.Second
)

// OpenAICompatProvider forwards chat requests to an upstream server that
// speaks the OpenAI-compatible HTTP API (e.g. vLLM, LM Studio).
type OpenAICompatProvider struct {
	name       string // instance name, e.g. "vllm-default", "lmstudio-dev"
	kind       string // backend kind, e.g. "vllm", "lmstudio"
	upstream   *url.URL
	httpClient *http.Client
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

type compatRequest struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	Stream      bool               `json:"stream"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type compatResponse struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Model   string          `json:"model"`
	Choices []compatChoice  `json:"choices"`
	Usage   *compatUsage    `json:"usage"`
	Error   *compatError    `json:"error"`
}

type compatChoice struct {
	Index        int            `json:"index"`
	Message      *compatMessage `json:"message"`
	Delta        *compatDelta   `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type compatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type compatDelta struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type compatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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
	if choice.Message != nil {
		content = choice.Message.Content
	}

	finishReason := ""
	if choice.FinishReason != nil {
		finishReason = *choice.FinishReason
	}

	chatResp := &provider.ChatResponse{
		ID:           cr.ID,
		Model:        cr.Model,
		Content:      content,
		FinishReason: finishReason,
	}
	if cr.Usage != nil {
		chatResp.PromptTokens = cr.Usage.PromptTokens
		chatResp.CompletionTokens = cr.Usage.CompletionTokens
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
// An idle-read watchdog (streamIdleTimeout) cancels the stream if no bytes
// arrive for longer than that window. The timer resets on every received chunk,
// so legitimate slow generation is not penalised. If the dial/connect itself
// fails, Stream returns an error before writing any bytes to w, allowing the
// caller to serve a clean JSON error response instead of a committed SSE stream.
func (p *OpenAICompatProvider) Stream(ctx context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	body, err := p.buildRequestBody(req, true)
	if err != nil {
		return nil, err
	}

	httpReq, err := p.newUpstreamRequest(ctx, body)
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

	// Wrap the response body with an idle-timeout reader. If no bytes arrive
	// within streamIdleTimeout between consecutive reads, the context is
	// cancelled and the scanner receives an error, cleanly terminating the loop.
	idleCtx, idleCancel := context.WithCancelCause(ctx)
	defer idleCancel(nil)
	trackedBody := newIdleTimeoutReader(resp.Body, streamIdleTimeout, idleCancel)

	flusher, canFlush := w.(http.Flusher)

	stats := &provider.StreamStats{
		Model: req.Model,
	}

	scanner := bufio.NewScanner(trackedBody)
	// Increase scanner buffer for large chunks (some models emit large deltas).
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		// Check whether the idle watchdog fired (context cancelled).
		if err := idleCtx.Err(); err != nil {
			cause := context.Cause(idleCtx)
			return nil, fmt.Errorf("openaicompat(%s): stream idle timeout (%s): %w", p.name, streamIdleTimeout, cause)
		}

		line := scanner.Text()

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
		}
	}

	if err := scanner.Err(); err != nil {
		// If the error is due to the idle watchdog, unwrap to a clear message.
		if idleCtx.Err() != nil {
			cause := context.Cause(idleCtx)
			return nil, fmt.Errorf("openaicompat(%s): stream idle timeout (%s): %w", p.name, streamIdleTimeout, cause)
		}
		return nil, fmt.Errorf("openaicompat(%s): reading stream: %w", p.name, err)
	}

	// Final flush after the stream ends.
	if canFlush {
		flusher.Flush()
	}

	return stats, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (p *OpenAICompatProvider) buildRequestBody(req provider.ChatRequest, stream bool) ([]byte, error) {
	cr := compatRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Stream:      stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
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

// ---------------------------------------------------------------------------
// idleTimeoutReader — idle-stream watchdog
// ---------------------------------------------------------------------------

// idleTimeoutReader wraps an io.Reader and cancels a context if no bytes are
// received within the configured idle window. The timer is reset on every
// successful Read call that returns at least one byte, so a slow but active
// stream (e.g. long generation) is never killed. Only a stream that wedges
// completely (no bytes for idleTimeout) triggers the cancellation.
//
// The cancel function is called with an explanatory error as its cause so that
// callers can distinguish idle-timeout errors from upstream errors via
// context.Cause.
type idleTimeoutReader struct {
	r           io.Reader
	idleTimeout time.Duration
	cancelCause context.CancelCauseFunc

	mu    sync.Mutex
	timer *time.Timer
}

// newIdleTimeoutReader wraps r with an idle-timeout watchdog. If no bytes
// arrive within idleTimeout, cancelCause is called with a descriptive error.
// The caller is responsible for calling the returned CancelCauseFunc when done
// (to stop the internal timer and avoid a goroutine leak).
func newIdleTimeoutReader(r io.Reader, idleTimeout time.Duration, cancelCause context.CancelCauseFunc) *idleTimeoutReader {
	itr := &idleTimeoutReader{
		r:           r,
		idleTimeout: idleTimeout,
		cancelCause: cancelCause,
	}
	itr.timer = time.AfterFunc(idleTimeout, func() {
		cancelCause(fmt.Errorf("no bytes received from upstream for %s", idleTimeout))
	})
	return itr
}

// Read implements io.Reader. Each call that returns at least one byte resets
// the idle timer. If the underlying reader returns io.EOF or an error the
// timer is stopped to avoid a spurious cancellation after the stream ends.
func (r *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > 0 {
		// Activity — reset the watchdog.
		r.timer.Reset(r.idleTimeout)
	}
	if err != nil {
		// Stream ended or errored — stop the timer so the cancelCause
		// is not called after the caller has already handled the EOF/error.
		r.timer.Stop()
	}
	return n, err
}

// compile-time interface check.
var _ provider.Provider = (*OpenAICompatProvider)(nil)
