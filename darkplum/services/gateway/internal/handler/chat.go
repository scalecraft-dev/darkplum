// Package handler contains the HTTP handlers for the gateway's OpenAI-compatible
// API surface. ChatHandler is the routing layer that dispatches chat completion
// requests to the appropriate backend provider (vLLM, LM Studio, or Bedrock)
// based on the model field in the request body.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/bedrock"
)

// openAIError is the wire format for error responses on the OpenAI-compatible surface.
// Clients (including the darkplum-api webchat) parse this shape for error display.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// chatCompletionRequest is the subset of the OpenAI chat/completions request
// body that the gateway needs to inspect for routing and recording. Fields we
// do not need to inspect (top_p, n, stop, etc.) are round-tripped transparently
// by the provider implementations.
type chatCompletionRequest struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	Stream      bool               `json:"stream"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
}

// modelInfo holds the model ID and the instance name of the provider that
// serves it. Used by HandleListModels to emit the correct owned_by kind.
type modelInfo struct {
	id           string
	instanceName string // key into providers map
}

// ChatHandler routes POST /v1/chat/completions to the correct backend provider
// based on the model field in the request. Routing precedence:
//  1. bedrockModels set (discovered at startup) -> "bedrock"
//  2. modelRoute map (model-id -> instance name, built from upstream discovery) -> that instance
//  3. defaultProvider (instance name fallback)
type ChatHandler struct {
	providers       map[string]provider.Provider
	bedrockModels   map[string]bool
	// openaiModels is the ordered list of {model-id, instance-name} pairs for
	// all openai-compatible upstreams, used by HandleListModels.
	openaiModels    []modelInfo
	modelRoute      map[string]string // model-id -> instance name
	defaultProvider string            // instance name
}

// NewChatHandler constructs a ChatHandler.
//
//   - defaultProv must not be nil; it is used when no model-specific route matches.
//   - bedrockProv may be nil (Bedrock disabled).
//   - bedrockModels is the set of model IDs discovered from Bedrock at startup.
//   - openaiProviders is a slice of all openai-compatible provider instances
//     (vllm-default, lmstudio-dev, etc.) in registration order.
//   - modelRoute maps each discovered model-id to the instance name that serves it.
func NewChatHandler(
	defaultProv provider.Provider,
	bedrockProv provider.Provider,
	bedrockModels map[string]bool,
	openaiProviders []provider.Provider,
	modelRoute map[string]string,
) *ChatHandler {
	h := &ChatHandler{
		providers:       make(map[string]provider.Provider),
		bedrockModels:   bedrockModels,
		modelRoute:      modelRoute,
		defaultProvider: defaultProv.Name(),
	}

	// Register default provider first.
	h.providers[defaultProv.Name()] = defaultProv

	// Register all openai-compatible instances and record their model lists
	// for HandleListModels. Default provider is already registered above; skip
	// re-registering it if it appears in openaiProviders.
	for _, p := range openaiProviders {
		if _, exists := h.providers[p.Name()]; !exists {
			h.providers[p.Name()] = p
		}
	}

	// Build openaiModels from modelRoute (model-id -> instance-name).
	// Iterate in a stable order (sorted model IDs) so HandleListModels output
	// is deterministic.
	sortedModelIDs := make([]string, 0, len(modelRoute))
	for id := range modelRoute {
		sortedModelIDs = append(sortedModelIDs, id)
	}
	sort.Strings(sortedModelIDs)
	for _, id := range sortedModelIDs {
		h.openaiModels = append(h.openaiModels, modelInfo{
			id:           id,
			instanceName: modelRoute[id],
		})
	}

	if bedrockProv != nil {
		h.providers[bedrockProv.Name()] = bedrockProv
	}
	return h
}

// HandleChatCompletions handles POST /v1/chat/completions. It parses the
// request body, selects the appropriate provider based on the discovered
// Bedrock model set and the model-route table, then calls either Stream or Complete.
//
// Provider metadata (provider_name, provider_region, resolved_model) and token
// counts (prompt_tokens, completion_tokens) are written to the Gin context so
// the Recording middleware can include them in the InferenceEvent without
// re-parsing the body.
//
// provider_name is set to the backend KIND ("vllm", "lmstudio", "bedrock"),
// not the instance name, so that audit/billing aggregation is not fragmented
// across instance renames or multiple instances of the same kind.
func (h *ChatHandler) HandleChatCompletions(c *gin.Context) {
	var req chatCompletionRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("invalid request body: %s", err.Error()),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	if req.Model == "" {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: "model is required",
				Type:    "invalid_request_error",
				Code:    "model_required",
			},
		})
		return
	}

	// Route: (1) Bedrock model set; (2) per-model route; (3) default provider.
	providerKey := h.resolveProviderKey(req.Model)

	prov, ok := h.providers[providerKey]
	if !ok {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("model %q requires the %q provider which is not enabled on this gateway", req.Model, providerKey),
				Type:    "invalid_request_error",
				Code:    "provider_not_enabled",
			},
		})
		return
	}

	// Write provider KIND to Gin context for the Recording middleware.
	// Using Kind() when available ensures audit records store "vllm"/"lmstudio"
	// rather than instance names like "vllm-default", which would fragment
	// billing aggregation if instances are renamed or multiplied.
	// Bedrock and mock implement Name() only — Name() already returns their kind.
	if kp, ok := prov.(interface{ Kind() string }); ok {
		c.Set("provider_name", kp.Kind())
	} else {
		c.Set("provider_name", prov.Name())
	}

	c.Set("resolved_model", req.Model)
	providerRegion := ""
	if bp, ok := prov.(*bedrock.BedrockProvider); ok {
		providerRegion = bp.Region()
	}
	c.Set("provider_region", providerRegion)

	chatReq := provider.ChatRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}

	if req.Stream {
		h.handleStream(c, prov, chatReq)
	} else {
		h.handleComplete(c, prov, chatReq)
	}
}

// handleStream sets SSE response headers then delegates to provider.Stream.
// Token counts are written to the Gin context after streaming completes so the
// Recording middleware can pick them up even though headers are already flushed.
//
// Header commitment strategy: gin buffers response headers until the first
// Write() to the underlying ResponseWriter. provider.Stream() writes nothing
// to c.Writer until the upstream HTTP connection succeeds and the first SSE
// byte arrives. This means that if the provider returns an error before writing
// anything (e.g. dial timeout from a NetworkPolicy-blocked SYN), we can still
// serve a clean JSON error response — the SSE Content-Type header has NOT been
// committed to the wire yet.
//
// After the first write, we are committed to the SSE stream. Any subsequent
// error is written as an SSE error event so partial-stream clients can surface
// the failure without corrupting the stream with a raw JSON body.
func (h *ChatHandler) handleStream(c *gin.Context, prov provider.Provider, chatReq provider.ChatRequest) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// gin.ResponseWriter implements both io.Writer and http.Flusher. The
	// provider implementations check for http.Flusher and flush after each SSE
	// chunk, so clients see tokens as they arrive.
	stats, err := prov.Stream(c.Request.Context(), chatReq, c.Writer)
	if err != nil {
		if !c.Writer.Written() {
			// Nothing has been written yet — SSE headers are buffered but not
			// committed. We can still serve a clean JSON error response here
			// (e.g. for dial timeouts, NetworkPolicy blocks, or non-200 upstream
			// responses that fail before the first token). Override the
			// Content-Type set above and use the standard provider error handler.
			c.Header("Content-Type", "application/json")
			handleProviderError(c, err)
		} else {
			// Headers are committed (partial SSE stream was written). We cannot
			// send a JSON error body — that would corrupt the stream. Write an
			// SSE error event so clients that handle "event: error" frames can
			// surface the failure gracefully.
			_ = writeStreamError(c.Writer, err)
		}
		return
	}

	if stats != nil {
		c.Set("prompt_tokens", stats.PromptTokens)
		c.Set("completion_tokens", stats.CompletionTokens)
	}
}

// handleComplete calls provider.Complete, serialises the result with
// BuildCompletionJSON, and writes the full response body.
func (h *ChatHandler) handleComplete(c *gin.Context, prov provider.Provider, chatReq provider.ChatRequest) {
	resp, err := prov.Complete(c.Request.Context(), chatReq)
	if err != nil {
		handleProviderError(c, err)
		return
	}

	body, err := provider.BuildCompletionJSON(resp)
	if err != nil {
		c.JSON(http.StatusInternalServerError, openAIError{
			Error: openAIErrorBody{
				Message: "failed to serialise completion response",
				Type:    "internal_server_error",
			},
		})
		return
	}

	c.Set("prompt_tokens", resp.PromptTokens)
	c.Set("completion_tokens", resp.CompletionTokens)

	c.Data(http.StatusOK, "application/json", body)
}

// resolveProviderKey returns the provider key (instance name or "bedrock") for
// the given model ID using the routing precedence documented on ChatHandler:
//  1. bedrockModels set  -> "bedrock"
//  2. modelRoute entry   -> that instance name
//  3. defaultProvider    -> fallback
//
// This is a pure helper extracted so that routing logic can be unit-tested
// independently of the full HTTP handler.
func (h *ChatHandler) resolveProviderKey(model string) string {
	if h.bedrockModels[model] {
		return "bedrock"
	}
	if instanceName, ok := h.modelRoute[model]; ok {
		return instanceName
	}
	return h.defaultProvider
}

// HandleListModels returns all available models from openai-compatible upstreams
// and Bedrock in OpenAI-compatible format. OpenAI-compatible models come from
// the modelRoute table populated at startup; Bedrock models come from the
// bedrockModels set discovered via the AWS API. Both sets are sorted for
// deterministic output.
//
// The owned_by field for openai-compatible models is set to the KIND of the
// serving instance ("vllm" or "lmstudio"), not the instance name, so that
// clients can group models by backend type.
func (h *ChatHandler) HandleListModels(c *gin.Context) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	type modelsResponse struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	now := time.Now().Unix()
	var models []modelEntry

	// OpenAI-compatible models — fetched from upstreams at startup.
	// openaiModels is already sorted by model ID (done in NewChatHandler).
	for _, m := range h.openaiModels {
		ownedBy := m.instanceName // fallback: use instance name
		if prov, ok := h.providers[m.instanceName]; ok {
			if kp, ok := prov.(interface{ Kind() string }); ok {
				ownedBy = kp.Kind()
			}
		}
		models = append(models, modelEntry{
			ID:      m.id,
			Object:  "model",
			Created: now,
			OwnedBy: ownedBy,
		})
	}

	// Bedrock models — discovered from the customer's AWS account at startup.
	bedrockIDs := make([]string, 0, len(h.bedrockModels))
	for id := range h.bedrockModels {
		bedrockIDs = append(bedrockIDs, id)
	}
	sort.Strings(bedrockIDs)
	for _, id := range bedrockIDs {
		models = append(models, modelEntry{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "bedrock",
		})
	}

	c.JSON(http.StatusOK, modelsResponse{
		Object: "list",
		Data:   models,
	})
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

// handleProviderError maps provider errors to HTTP responses. BedrockAPIError
// carries a pre-translated status code and body; all other errors become 502.
func handleProviderError(c *gin.Context, err error) {
	var bedrockErr *bedrock.BedrockAPIError
	if errors.As(err, &bedrockErr) {
		// BedrockAPIError.Body is already an OpenAI-compatible JSON error body,
		// produced by bedrock.TranslateBedrockError. Write status + body directly.
		c.Data(bedrockErr.StatusCode, "application/json", bedrockErr.Body)
		return
	}

	c.JSON(http.StatusBadGateway, openAIError{
		Error: openAIErrorBody{
			Message: fmt.Sprintf("upstream provider error: %s", err.Error()),
			Type:    "upstream_error",
		},
	})
}

// writeStreamError attempts to write an SSE error event to w. This is
// best-effort: if headers have already been sent and partial SSE written, we
// cannot guarantee the client will parse this correctly, but some clients do
// handle "event: error" frames.
func writeStreamError(w io.Writer, err error) error {
	msg := fmt.Sprintf("data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\"}}\n\n", err.Error())
	_, writeErr := io.WriteString(w, msg)
	return writeErr
}
