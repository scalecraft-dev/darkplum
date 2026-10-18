// Package bedrock implements the provider.Provider interface for AWS Bedrock
// using the model-agnostic Converse / ConverseStream API, so any Bedrock model
// that supports Converse (Anthropic, Meta Llama, Mistral, Amazon Nova,
// DeepSeek, ...) works through this provider — not just Anthropic Claude.
// Credentials are sourced from the default AWS credential chain — in
// production, IRSA via the gateway pod's service account token; in local dev,
// environment variables.
//
// There are no static credentials anywhere in this package. The IRSA binding is
// the IAM Layer 1 enforcement boundary; OPA in the gateway is Layer 2.
package bedrock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrock"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// BedrockProvider implements provider.Provider using the Bedrock Converse API.
// It is safe for concurrent use; the underlying SDK client manages its own
// connection pool.
type BedrockProvider struct {
	client        converseAPI
	controlClient controlAPI
	region        string
	retention     dataRetentionState
}

// New creates a BedrockProvider for the given AWS region, loading credentials
// from the default AWS config chain (IRSA → env → instance profile). An empty
// region is rejected immediately. Any config-load failure should be treated as
// fatal at startup — if we cannot obtain credentials the provider is unusable.
//
// ctx bounds the config load. With an explicit region the default chain does
// not normally touch the network here (credentials resolve lazily, per call),
// but the discovery loop calls New from a goroutine it waits on, so a load that
// did block must be cancellable rather than freezing discovery until restart.
func New(ctx context.Context, region string) (*BedrockProvider, error) {
	if region == "" {
		return nil, fmt.Errorf("bedrock: region must not be empty")
	}

	cfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("bedrock: load AWS config: %w", err)
	}

	return &BedrockProvider{
		client:        newRuntimeClient(bedrockruntime.NewFromConfig(cfg)),
		controlClient: bedrock.NewFromConfig(cfg),
		region:        region,
	}, nil
}

// Name implements provider.Provider.
func (p *BedrockProvider) Name() string { return "bedrock" }

// Region returns the AWS region this provider is configured for.
// Used by the gateway handler to populate provider_region in the recording
// middleware context without requiring a second config lookup.
func (p *BedrockProvider) Region() string { return p.region }

// DiscoverModels queries the Bedrock API for all foundation models available
// in this region and returns a set of model IDs. The gateway uses this at
// startup to build a routing table: if a requested model is in this set, route
// to Bedrock; otherwise route elsewhere. This replaces static prefix-based
// routing so customers can use whatever Bedrock models their IAM policy
// allows — Converse's model-agnostic wire format means this is no longer
// restricted to Anthropic Claude.
//
// ListInferenceProfiles is paginated: a customer with many system-defined
// profiles (e.g. one per model per supported region-pair) can exceed the
// 1000-item MaxResults in a single page, so every page is followed via
// NextToken until the API stops returning one.
func (p *BedrockProvider) DiscoverModels(ctx context.Context) (map[string]bool, error) {
	models := make(map[string]bool)

	// Inference profiles are the invocable IDs (e.g. "us.anthropic.claude-...").
	// Raw foundation model IDs (e.g. "anthropic.claude-...") cannot be used with
	// on-demand throughput and must be invoked via a profile. We only return
	// profile IDs so users see models they can actually call.
	var nextToken *string
	for {
		profileResp, err := p.controlClient.ListInferenceProfiles(ctx, &bedrock.ListInferenceProfilesInput{
			MaxResults: aws.Int32(1000),
			NextToken:  nextToken,
			TypeEquals: bedrocktypes.InferenceProfileTypeSystemDefined,
		})
		if err != nil {
			return nil, fmt.Errorf("bedrock: list inference profiles: %w", err)
		}

		for _, profile := range profileResp.InferenceProfileSummaries {
			if profile.InferenceProfileId != nil && profile.Status == bedrocktypes.InferenceProfileStatusActive {
				models[*profile.InferenceProfileId] = true
			}
		}

		if profileResp.NextToken == nil || *profileResp.NextToken == "" {
			break
		}
		nextToken = profileResp.NextToken
	}

	return models, nil
}

// Complete calls Converse (non-streaming) and returns the full response.
// req.Model must be a valid Bedrock model ID or inference profile ID.
func (p *BedrockProvider) Complete(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	system, messages, inferenceConfig, err := translateRequest(req)
	if err != nil {
		return nil, err
	}
	toolConfig, err := translateToolConfig(req)
	if err != nil {
		return nil, err
	}

	out, err := p.client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:         aws.String(req.Model),
		Messages:        messages,
		System:          system,
		InferenceConfig: inferenceConfig,
		ToolConfig:      toolConfig,
	})
	if err != nil {
		return nil, p.wrapError(req.Model, err, toolConfig != nil)
	}
	p.retention.setRefused(req.Model, false)

	return translateResponse(out, req.Model)
}

// Stream calls ConverseStream and writes OpenAI-compatible SSE chunks to w as
// tokens arrive. The caller must set appropriate response headers
// (Content-Type: text/event-stream, Cache-Control: no-cache, X-Accel-Buffering:
// no) before calling Stream, because w is written to as soon as the first chunk
// arrives.
//
// If w implements http.Flusher, it is flushed after each SSE chunk — matching
// the pattern established by the vLLM provider — so streaming clients see tokens
// as they arrive rather than when the response buffer fills.
//
// Token usage (PromptTokens/CompletionTokens) is populated from the trailing
// Metadata event Converse emits after MessageStop; this is new relative to the
// old InvokeModelWithResponseStream integration, which never populated
// streaming usage at all.
func (p *BedrockProvider) Stream(ctx context.Context, req provider.ChatRequest, w io.Writer) (*provider.StreamStats, error) {
	system, messages, inferenceConfig, err := translateRequest(req)
	if err != nil {
		return nil, err
	}
	toolConfig, err := translateToolConfig(req)
	if err != nil {
		return nil, err
	}

	out, err := p.client.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:         aws.String(req.Model),
		Messages:        messages,
		System:          system,
		InferenceConfig: inferenceConfig,
		ToolConfig:      toolConfig,
	})
	if err != nil {
		return nil, p.wrapError(req.Model, err, toolConfig != nil)
	}
	p.retention.setRefused(req.Model, false)
	defer out.Close()

	flusher, canFlush := w.(http.Flusher)

	stats := &provider.StreamStats{
		Model: req.Model,
	}

	// Bedrock does not issue a completion ID. We synthesise a stable one for
	// this stream using the stats pointer address — cheap, unique within the
	// process, not cryptographically random (which is fine for a correlation ID).
	completionID := fmt.Sprintf("bedrock-%p", stats)

	var toolState toolStreamState
	var streamErr error
loop:
	for {
		select {
		case <-ctx.Done():
			return stats, ctx.Err()
		case event, ok := <-out.Events():
			if !ok {
				break loop
			}

			// translateStreamEvent reports done=true on MessageStop, but the
			// loop deliberately keeps reading past it rather than breaking:
			// Converse sends the trailing Metadata event (token usage) *after*
			// MessageStop, and breaking early would silently drop streaming
			// usage. The loop instead exits when the events channel closes.
			_, err := translateStreamEvent(event, completionID, req.Model, stats, w, &toolState)
			if err != nil {
				slog.Error("bedrock: error translating stream event", "err", err)
				streamErr = err
				break loop
			}

			if canFlush {
				flusher.Flush()
			}
		}
	}

	// A mid-stream error (e.g. ModelStreamErrorException, ThrottlingException)
	// surfaces via Err() after the events channel closes, not as an event —
	// must not be swallowed.
	if err := out.Err(); err != nil {
		return stats, p.wrapError(req.Model, err, toolConfig != nil)
	}
	if streamErr != nil {
		return stats, fmt.Errorf("bedrock: translate error: %w", streamErr)
	}

	if _, err := fmt.Fprint(w, provider.StreamDoneSSE); err != nil {
		slog.Warn("bedrock: failed to write SSE done sentinel", "err", err)
	}
	if canFlush {
		flusher.Flush()
	}

	return stats, nil
}

// wrapError classifies the data-retention refusal (a typed, scrubbed error and
// a per-model flag for GET /v1/models availability) and otherwise defers to
// wrapBedrockError.
func (p *BedrockProvider) wrapError(model string, err error, hadTools bool) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && isDataRetentionValidation(apiErr.ErrorCode(), apiErr.ErrorMessage()) {
		mode, _ := p.retention.accountMode()
		p.retention.setRefused(model, true)
		slog.Warn("bedrock: model refused: account data-retention mode too low",
			"model", model, "account_mode", mode, "required_mode", provider.RequiredDataRetentionMode)
		return &DataRetentionError{Model: model, AccountMode: mode, RequiredMode: provider.RequiredDataRetentionMode, Cause: err}
	}
	if hadTools && errors.As(err, &apiErr) && isToolsUnsupportedValidation(apiErr.ErrorCode(), apiErr.ErrorMessage()) {
		slog.Warn("bedrock: model does not support tool calling", "model", model, "message", apiErr.ErrorMessage())
		statusCode, body := TranslateBedrockError("ValidationException",
			fmt.Sprintf("model %q does not support tool calling (tools / tool_choice); resend without tools or choose a tool-capable model", model))
		return &BedrockAPIError{StatusCode: statusCode, Body: body, Cause: err}
	}
	return wrapBedrockError(err)
}

// isToolsUnsupportedValidation reports whether a Converse error is the
// ValidationException a model without tool support returns for a request
// carrying toolConfig (wording varies by model: "This model doesn't support
// tool use", "...does not support tool_choice", ...). Pairing errors about
// toolUse/toolResult blocks do not mention support and are left alone.
func isToolsUnsupportedValidation(code, message string) bool {
	if code != "ValidationException" {
		return false
	}
	m := strings.ToLower(message)
	return strings.Contains(m, "tool") && (strings.Contains(m, "support") || strings.Contains(m, "not available"))
}

// SupportsTools implements provider.ToolCapable.
func (p *BedrockProvider) SupportsTools() bool { return true }

// wrapBedrockError converts a Bedrock SDK error into a BedrockAPIError that
// carries the HTTP status code and OpenAI-compatible body, so the gateway HTTP
// handler can write them directly without re-translating.
func wrapBedrockError(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		slog.Error("bedrock: API error",
			"code", apiErr.ErrorCode(),
			"message", apiErr.ErrorMessage(),
			"raw", err.Error())
		statusCode, body := TranslateBedrockError(apiErr.ErrorCode(), apiErr.ErrorMessage())
		return &BedrockAPIError{
			StatusCode: statusCode,
			Body:       body,
			Cause:      err,
		}
	}
	slog.Error("bedrock: non-API error", "err", err.Error())
	return fmt.Errorf("bedrock: %w", err)
}

// BedrockAPIError is returned by Complete and Stream when Bedrock returns a
// typed API error, or when request translation fails validation (e.g. no user
// messages). Gateway HTTP handlers should type-assert to this to write the
// correct status and body without a second translation step.
type BedrockAPIError struct {
	StatusCode int
	Body       []byte
	Cause      error
}

func (e *BedrockAPIError) Error() string {
	return fmt.Sprintf("bedrock API error %d: %s", e.StatusCode, e.Body)
}

func (e *BedrockAPIError) Unwrap() error { return e.Cause }

// HTTPStatus implements provider.APIError. Named distinctly from the
// StatusCode field (Go does not allow a method and a field to share a name
// on the same type) so existing callers that read e.StatusCode directly are
// unaffected by this addition.
func (e *BedrockAPIError) HTTPStatus() int { return e.StatusCode }

// ResponseBody implements provider.APIError, returning the same
// already-OpenAI-shaped bytes as the Body field (see the naming note on
// HTTPStatus above).
func (e *BedrockAPIError) ResponseBody() []byte { return e.Body }

// RetryAfterSeconds implements provider.APIError. Always 0: Bedrock's
// ThrottlingException does not carry a caller-usable retry-after value, so
// there is nothing to pass through on a 429.
func (e *BedrockAPIError) RetryAfterSeconds() int { return 0 }

// compile-time interface checks.
var _ provider.Provider = (*BedrockProvider)(nil)
var _ provider.APIError = (*BedrockAPIError)(nil)
var _ provider.Regional = (*BedrockProvider)(nil)
var _ provider.ToolCapable = (*BedrockProvider)(nil)
