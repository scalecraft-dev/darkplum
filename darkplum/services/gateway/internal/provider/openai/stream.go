package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Stream implements provider.Provider. Rendered ONLY through
// provider.BuildStreamingChunkSSE / BuildFinalChunkSSE / StreamDoneSSE, per
// the shared OpenAI-wire contract every provider uses — the vendor's own SSE
// bytes are never piped through directly (unlike openaicompat, which forwards
// an already-OpenAI-shaped self-hosted upstream's bytes almost verbatim):
// this package re-renders every delta so no vendor-specific field
// (system_fingerprint, service_tier, the vendor's own id) ever reaches the
// client.
//
// The 400 always precedes any byte written to w: the adaptive
// sampling-parameter retry (see detectUnsupportedParam) happens entirely
// before the SSE loop below starts, using the vendor's initial response
// headers/status, never a byte of the (would-be) stream body.
//
// Both attempts (and the eventual streaming read) are made under idleCtx —
// a single context.WithCancelCause child of ctx created up front — rather
// than the caller's ctx directly: the idle watchdog constructed below can
// only actually abort an in-flight resp.Body.Read() by cancelling the SAME
// context the underlying *http.Request was built with (cancelling an
// unrelated child context does nothing to a request already in flight on
// its parent). Building the request with idleCtx from the start costs
// nothing before the watchdog exists (nothing cancels idleCtx early), and
// means the watchdog set up after a successful response actually works.
func (p *Provider) Stream(ctx context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	key, _, err := p.creds.Key()
	if err != nil {
		return nil, translateCredentialError(err)
	}

	idleCtx, idleCancel := context.WithCancelCause(ctx)
	defer idleCancel(nil)

	drop := droppedParams{}
	resp, err := p.doStream(idleCtx, key, req, drop)
	if err != nil {
		return nil, translateTransportError(err)
	}

	if resp.StatusCode == http.StatusBadRequest {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		retry, ok := detectUnsupportedParam(errBody, drop)
		if !ok {
			return nil, translateVendor400(nil, p.cfg.Status, errBody)
		}

		slog.Info("openai: retrying stream without unsupported sampling parameter", "model", req.Model)
		drop = retry
		resp, err = p.doStream(idleCtx, key, req, drop)
		if err != nil {
			return nil, translateTransportError(err)
		}
	}

	if resp.StatusCode != http.StatusOK {
		// A second failure (including a second 400) is returned as-is —
		// never retried again.
		header := resp.Header
		if resp.StatusCode == http.StatusBadRequest {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, translateVendor400(header, p.cfg.Status, errBody)
		}
		resp.Body.Close()
		return nil, translateVendorError(resp.StatusCode, header, p.cfg.Status)
	}
	defer resp.Body.Close()

	// Nothing has been written to w until this point.
	trackedBody := provider.NewIdleTimeoutReader(resp.Body, p.streamIdleTimeout, idleCancel)

	flusher, canFlush := w.(http.Flusher)

	scanner := bufio.NewScanner(trackedBody)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	stats := &provider.StreamStats{Model: req.Model}
	completionID := ""

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Malformed chunk — log-worthy but not fatal; keep streaming,
			// matching openaicompat's identical tolerance.
			continue
		}

		if chunk.Error != nil {
			return nil, translateVendorError(0, nil, p.cfg.Status)
		}
		if chunk.ID != "" {
			completionID = chunk.ID
		}
		if chunk.Model != "" {
			stats.Model = chunk.Model
		}
		if chunk.Usage != nil {
			stats.PromptTokens = chunk.Usage.PromptTokens
			stats.CompletionTokens = chunk.Usage.CompletionTokens
			stats.CacheReadTokens = chunk.Usage.cachedTokens()
		}

		if len(chunk.Choices) == 0 {
			// The terminal usage-only chunk (stream_options.include_usage)
			// carries no choice to render.
			continue
		}
		choice := chunk.Choices[0]

		content := ""
		if choice.Delta != nil {
			content = choice.Delta.Content
			if choice.Delta.Refusal != "" {
				content = choice.Delta.Refusal
				stats.FinishReason = "content_filter"
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" && stats.FinishReason == "" {
			stats.FinishReason = mapFinishReason(*choice.FinishReason)
		}

		var toolDeltas []provider.ToolCallDelta
		if choice.Delta != nil {
			for _, d := range choice.Delta.ToolCalls {
				if d.ID == "" && (d.Function == nil || (d.Function.Name == "" && d.Function.Arguments == "")) {
					continue // nothing to render
				}
				if d.ID != "" && d.Type == "" {
					d.Type = "function"
				}
				toolDeltas = append(toolDeltas, d)
			}
		}

		if content == "" && len(toolDeltas) == 0 {
			// Skip empty deltas and role-only chunks — there is nothing to
			// render.
			continue
		}

		var out [][]byte
		if content != "" {
			sseChunk, buildErr := provider.BuildStreamingChunkSSE(completionID, stats.Model, content)
			if buildErr != nil {
				return nil, fmt.Errorf("openai: building streaming chunk: %w", buildErr)
			}
			out = append(out, sseChunk)
		}
		// Tool-call fragments keep the vendor's index and arrival order, so
		// parallel calls reassemble correctly on the client.
		for _, d := range toolDeltas {
			sseChunk, buildErr := provider.BuildToolCallDeltaSSE(completionID, stats.Model, d)
			if buildErr != nil {
				return nil, fmt.Errorf("openai: building tool-call chunk: %w", buildErr)
			}
			out = append(out, sseChunk)
		}
		for _, sseChunk := range out {
			if _, writeErr := w.Write(sseChunk); writeErr != nil {
				return nil, fmt.Errorf("openai: writing streaming chunk: %w", writeErr)
			}
		}
		if canFlush {
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		if cause := context.Cause(idleCtx); cause != nil {
			return nil, provider.NewVendorAPIError("openai", http.StatusGatewayTimeout, "upstream_error", "upstream_timeout",
				"the model provider timed out", 0, cause)
		}
		return nil, translateTransportError(err)
	}

	if stats.FinishReason == "" {
		stats.FinishReason = "stop"
	}

	finalChunk, err := provider.BuildFinalChunksSSE(completionID, stats.Model, stats.FinishReason, stats, req.IncludeUsage)
	if err != nil {
		return nil, fmt.Errorf("openai: building final chunk: %w", err)
	}
	if _, err := w.Write(finalChunk); err != nil {
		return nil, fmt.Errorf("openai: writing final chunk: %w", err)
	}
	if _, err := io.WriteString(w, provider.StreamDoneSSE); err != nil {
		return nil, fmt.Errorf("openai: writing stream terminator: %w", err)
	}
	if canFlush {
		flusher.Flush()
	}

	return stats, nil
}

// doStream sends a streaming (stream:true) request and returns the raw
// response as soon as headers/status arrive — the caller is responsible for
// closing resp.Body in every path, including a non-200 status.
func (p *Provider) doStream(ctx context.Context, key string, req provider.ChatRequest, drop droppedParams) (*http.Response, error) {
	body, err := buildRequestBody(req, true, drop)
	if err != nil {
		return nil, err
	}
	httpReq, err := p.newRequest(ctx, key, body)
	if err != nil {
		return nil, err
	}
	return p.client.Do(httpReq)
}
