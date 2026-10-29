// Package openai implements provider.Provider for the direct OpenAI Chat
// Completions API. There is no requirement to use an OpenAI SDK here: this
// package builds and parses the wire format itself with raw net/http, since
// the chat-completions shape is already this gateway's own native
// OpenAI-compatible surface (see internal/provider/openaicompat, which
// speaks the identical format to a self-hosted upstream). Credentials come
// from a file-backed credsource.Source (a Kubernetes secret volume mount) —
// never from an env var, and never logged. IAM-style enforcement does not
// exist at this boundary (a vendor API key is an org-wide bearer credential,
// not a scoped cloud role); Layer 1 for a vendor route is whatever scope the
// operator gave the key on the vendor's own side (see the plan's Layer 1
// honesty note, and internal/provider/anthropic's identical package doc) —
// OPA in front of this provider is the only enforcement Darkplum itself
// contributes for external_api routes.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
)

const (
	// streamIdleTimeout mirrors openaicompat's and anthropic's inter-event
	// watchdog: if no stream bytes arrive for this long, the stream is
	// considered wedged and cancelled.
	streamIdleTimeout = 45 * time.Second
	// discoveryTimeout bounds a single DiscoverModels call independently of
	// whatever timeout routing.Manager's caller applies.
	discoveryTimeout = 15 * time.Second
	// defaultBaseURL is used when Config.BaseURL is empty (OPENAI_BASE_URL
	// unset) — production traffic goes here.
	defaultBaseURL = "https://api.openai.com"
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
	// BaseURL overrides the production endpoint (OPENAI_BASE_URL) — tests
	// and dev proxies only. Empty means defaultBaseURL.
	BaseURL string
	// Status, if non-nil, receives a Report call whenever a request hits a
	// 401/403 — in addition to the periodic Report calls
	// routing.NewOpenAISource makes on every discovery tick.
	Status authFailureReporter
}

// Provider implements provider.Provider for the direct OpenAI Chat
// Completions API.
type Provider struct {
	creds  credentialSource
	cfg    Config
	client *http.Client

	// streamIdleTimeout defaults to the package-level streamIdleTimeout
	// constant; overridable (same-package tests only) so the idle-watchdog
	// test doesn't need to wait 45 real seconds.
	streamIdleTimeout time.Duration
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
	}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "openai" }

// Kind matches Name — present for symmetry with providers whose Name is an
// instance identifier distinct from their audit Kind (e.g. openaicompat).
func (p *Provider) Kind() string { return "openai" }

func (p *Provider) baseURL() string {
	if p.cfg.BaseURL != "" {
		return p.cfg.BaseURL
	}
	return defaultBaseURL
}

// newRequest builds a POST /v1/chat/completions request carrying body, with
// the bearer key attached. The key is passed in (rather than read here)
// so every call site reads it fresh from creds exactly once per attempt.
func (p *Provider) newRequest(ctx context.Context, key string, body []byte) (*http.Request, error) {
	endpoint := p.baseURL() + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	return req, nil
}

// buildRequestBody translates req into this package's own wire shape. drop
// controls which caller-set sampling parameters are omitted even though the
// caller set them — non-zero only on the one-time adaptive retry after the
// vendor has named that exact parameter unsupported for the model (see
// detectUnsupportedParam). max_completion_tokens is sent (NEVER max_tokens)
// whenever the caller set MaxTokens>0; temperature/top_p/stop are otherwise
// forwarded whenever the caller set them, matching every other provider's
// request-shape honesty. stream_options.include_usage is set whenever
// streaming, so the final SSE chunk carries usage even though choices is
// empty on it (see Stream).
func buildRequestBody(req provider.ChatRequest, stream bool, drop droppedParams) ([]byte, error) {
	messages := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		cm := chatMessage{Role: m.Role, ToolCallID: m.ToolCallID, Name: m.Name}
		if m.Content == "" && len(m.ToolCalls) > 0 {
			cm.Content = nil // renders "content": null
		} else {
			c := m.Content
			cm.Content = &c
		}
		for _, tc := range m.ToolCalls {
			if tc.Type == "" {
				tc.Type = "function"
			}
			if strings.TrimSpace(tc.Function.Arguments) == "" {
				tc.Function.Arguments = "{}"
			}
			cm.ToolCalls = append(cm.ToolCalls, tc)
		}
		messages = append(messages, cm)
	}

	body := chatRequest{
		Model:    req.Model,
		Messages: messages,
	}
	if len(req.Tools) > 0 {
		body.Tools = req.Tools
	}
	if kind, _, _ := provider.ToolChoiceKind(req.ToolChoice); kind != "" {
		body.ToolChoice = req.ToolChoice
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		body.MaxCompletionTokens = &mt
	}
	if req.Temperature != nil && !drop.temperature {
		body.Temperature = req.Temperature
	}
	if req.TopP != nil && !drop.topP {
		body.TopP = req.TopP
	}
	if len(req.Stop) > 0 && !drop.stop {
		body.Stop = req.Stop
	}
	if req.JSONMode {
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	// Non-default sampling/behaviour parameters go on the wire as sent; they
	// are never part of droppedParams, so the adaptive retry cannot swallow
	// them (a vendor 400 on one comes back naming it, see translateVendor400).
	body.Seed = req.Seed
	body.FrequencyPenalty = req.FrequencyPenalty
	body.PresencePenalty = req.PresencePenalty
	if req.Logprobs {
		t := true
		body.Logprobs = &t
		body.TopLogprobs = req.TopLogprobs
	}
	if len(req.LogitBias) > 0 {
		body.LogitBias = req.LogitBias
	}
	// OpenAI refuses parallel_tool_calls on a request without tools.
	if req.ParallelToolCalls != nil && len(req.Tools) > 0 {
		body.ParallelToolCalls = req.ParallelToolCalls
	}
	if req.NoStore {
		f := false
		body.Store = &f
	}
	body.User = req.User
	if stream {
		body.Stream = true
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: marshalling request: %w", err)
	}
	return b, nil
}

// doNonStream sends body and reads the full response, returning the status
// code and body bytes regardless of status (the caller decides how to
// classify a non-200).
func (p *Provider) doNonStream(ctx context.Context, key string, body []byte) (statusCode int, header http.Header, respBody []byte, err error) {
	httpReq, err := p.newRequest(ctx, key, body)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("openai: reading response body: %w", err)
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// Complete implements provider.Provider. The 400 always precedes any byte
// being written by the caller — Complete either returns a fully-formed
// provider.ChatResponse or an error; it never writes partial output.
func (p *Provider) Complete(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	key, _, err := p.creds.Key()
	if err != nil {
		return nil, translateCredentialError(err)
	}

	drop := droppedParams{}
	body, err := buildRequestBody(req, false, drop)
	if err != nil {
		return nil, err
	}

	statusCode, header, respBody, err := p.doNonStream(ctx, key, body)
	if err != nil {
		return nil, translateTransportError(err)
	}

	if statusCode == http.StatusBadRequest {
		if retry, ok := detectUnsupportedParam(respBody, drop); ok {
			slog.Info("openai: retrying without unsupported sampling parameter", "model", req.Model)
			drop = retry
			body, err = buildRequestBody(req, false, drop)
			if err != nil {
				return nil, err
			}
			statusCode, header, respBody, err = p.doNonStream(ctx, key, body)
			if err != nil {
				return nil, translateTransportError(err)
			}
		}
		// A second 400 (or a 400 that names nothing retriable) falls through
		// to the generic status check below and is returned as-is — never
		// retried again.
	}

	if statusCode == http.StatusBadRequest {
		return nil, translateVendor400(header, p.cfg.Status, respBody)
	}
	if statusCode != http.StatusOK {
		return nil, translateVendorError(statusCode, header, p.cfg.Status)
	}

	var cr chatCompletionResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return nil, fmt.Errorf("openai: decoding response: %w", err)
	}
	return toChatResponse(&cr)
}

// toChatResponse converts a complete (non-streaming) response into
// Darkplum's provider.ChatResponse. A refusal is surfaced as content with
// finish_reason content_filter — callers still get a well-formed response
// with text rather than an error — matching the streaming path's identical
// handling (see Stream).
func toChatResponse(cr *chatCompletionResponse) (*provider.ChatResponse, error) {
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("openai: response contained no choices")
	}
	choice := cr.Choices[0]

	content := ""
	finishReason := ""
	if choice.FinishReason != nil {
		finishReason = *choice.FinishReason
	}
	if choice.Message != nil {
		content = choice.Message.Content
		if choice.Message.Refusal != "" {
			content = choice.Message.Refusal
			finishReason = "content_filter"
		}
	}

	resp := &provider.ChatResponse{
		ID:           cr.ID,
		Model:        cr.Model,
		Content:      content,
		FinishReason: mapFinishReason(finishReason),
		Logprobs:     provider.NormalizeLogprobs(choice.Logprobs),
	}
	if choice.Message != nil {
		for _, tc := range choice.Message.ToolCalls {
			if tc.Type == "" {
				tc.Type = "function"
			}
			resp.ToolCalls = append(resp.ToolCalls, tc)
		}
	}
	if cr.Usage != nil {
		resp.PromptTokens = cr.Usage.PromptTokens
		resp.CompletionTokens = cr.Usage.CompletionTokens
		resp.CacheReadTokens = cr.Usage.cachedTokens()
	}
	return resp, nil
}

// SupportsTools makes this a tool-capable backend (provider.ToolCapable).
func (p *Provider) SupportsTools() bool { return true }

// ParamCaps implements provider.ParamCapable. Everything OpenAI's Chat
// Completions API takes that the gateway exposes is forwarded: json_object,
// stop, forced tool_choice, parallel_tool_calls:false, logprobs (relayed back
// in OpenAI's shape on both paths) and logit_bias. `user` goes as the HMAC
// pseudonym, never raw: OpenAI is a third party outside the customer's boundary.
func (p *Provider) ParamCaps() provider.ParamCaps {
	return provider.ParamCaps{
		JSONMode: true, Stop: true, ForcedToolChoice: true,
		Logprobs: true, LogitBias: true, SerialToolCalls: true,
		UserID: provider.UserIDHMAC,
	}
}

var _ provider.Provider = (*Provider)(nil)
