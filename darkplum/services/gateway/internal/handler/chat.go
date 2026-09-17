// Package handler contains the HTTP handlers for the gateway's OpenAI-compatible
// API surface. ChatHandler is the enforcement point for model routing: every
// caller (session or API key) passes through HandleChatCompletions, which
// resolves aliases, looks up the route table, and enforces the enabled and
// residency policy checks before dispatching to a backend provider.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/bedrock"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
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

// providerLookup is the subset of *provider.Registry the handler needs.
// Defined as an interface so tests can substitute a fake registry.
type providerLookup interface {
	Get(key string) (provider.Provider, bool)
}

// routeSnapshotter supplies the current route table. Satisfied by
// *routing.Manager; defined as an interface so tests can substitute a static
// table built with routing.NewTable without spinning up discovery.
type routeSnapshotter interface {
	Snapshot() *routing.Table
}

// ChatHandler is the model-routing enforcement point for
// POST /v1/chat/completions and GET /v1/models.
//
// Request order for HandleChatCompletions:
//  1. parse body; model required (400)
//  2. policy snapshot must be loaded, else fail closed (503 policy_unavailable)
//  3. alias resolve (admin intent wins over a same-named discovered model; no chains)
//  4. route lookup on the resolved id (404 model_not_found — no default fall-through, ever)
//  5. enabled check (403 model_not_enabled)
//  6. residency check (403 model_residency_violation)
//  7. dispatch
type ChatHandler struct {
	providers providerLookup
	routes    routeSnapshotter
	policy    policy.Store
}

// NewChatHandler constructs a ChatHandler.
func NewChatHandler(providers providerLookup, routes routeSnapshotter, pol policy.Store) *ChatHandler {
	return &ChatHandler{providers: providers, routes: routes, policy: pol}
}

// HandleChatCompletions handles POST /v1/chat/completions. See the ChatHandler
// doc comment for the exact enforcement order.
//
// Provider metadata (provider_name, provider_region), resolved_model,
// requested_model, and token counts are written to the Gin context so the
// Recording middleware can include them in the InferenceEvent without
// re-parsing the body. resolved_model/requested_model are set immediately
// after alias resolution (step 3) rather than after dispatch, so a request
// refused via an alias (404/403 below) is still attributed to the model the
// alias pointed at instead of recording model=<alias>/requested_model=NULL.
// Every refusal below also sets "denial_code" on the context and logs a
// structured line — inference_events has no column for the denial reason
// today, so this is the only audit trail until one is added.
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

	snap, loaded := h.policy.Current()
	if !loaded {
		c.Set("denial_code", "policy_unavailable")
		logDenial(c, "policy_unavailable", req.Model, "", "")
		respondPolicyUnavailable(c)
		return
	}

	// Alias resolution: admin intent is authoritative — an alias wins over a
	// same-named discovered model. No chains: the target is looked up as-is
	// below, never re-resolved through Aliases again (model_aliases
	// guarantees a target is never itself an alias).
	//
	// resolved_model/requested_model are written to the Gin context
	// immediately — before any of the checks below can refuse the request —
	// so that a denial reached through an alias (unknown target, disabled
	// target, residency violation) is still recorded against the model the
	// alias actually pointed at, not left with model=<alias> and
	// requested_model NULL.
	requestedModel := ""
	resolvedModel := req.Model
	if target, isAlias := snap.Aliases[req.Model]; isAlias {
		requestedModel = req.Model
		resolvedModel = target
	}
	c.Set("resolved_model", resolvedModel)
	c.Set("requested_model", requestedModel)

	route, ok := h.routes.Snapshot().Lookup(resolvedModel)
	if !ok {
		c.Set("denial_code", "model_not_found")
		logDenial(c, "model_not_found", req.Model, resolvedModel, "")
		respondModelNotFound(c, resolvedModel)
		return
	}

	if !snap.Enabled[resolvedModel] {
		c.Set("denial_code", "model_not_enabled")
		logDenial(c, "model_not_enabled", req.Model, resolvedModel, route.Class)
		respondModelNotEnabled(c, resolvedModel)
		return
	}

	if snap.Residency == policy.ResidencySelfHostedOnly && route.Class == routing.ClassCloudManaged {
		c.Set("denial_code", "model_residency_violation")
		logDenial(c, "model_residency_violation", req.Model, resolvedModel, route.Class)
		respondResidencyViolation(c, resolvedModel)
		return
	}

	prov, ok := h.providers.Get(route.ProviderKey)
	if !ok {
		// Should never happen: a model only appears in the route table once
		// its source has successfully discovered it, and every source
		// registers its provider before Discover can return that model id.
		// Surface loudly rather than silently egressing anywhere.
		c.JSON(http.StatusInternalServerError, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("model %q resolved to provider %q which is not registered", resolvedModel, route.ProviderKey),
				Type:    "internal_error",
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

	providerRegion := ""
	if bp, ok := prov.(*bedrock.BedrockProvider); ok {
		providerRegion = bp.Region()
	}
	c.Set("provider_region", providerRegion)

	chatReq := provider.ChatRequest{
		Model:       resolvedModel,
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

// HandleListModels returns every model currently in the route table,
// regardless of enabled status — the api's SyncModels needs the full
// discovered set, not just what's currently enabled. Each entry reports
// owned_by (the backend kind) and backend_class (self_hosted/cloud_managed)
// straight from the route, so this handler never needs a provider lookup.
func (h *ChatHandler) HandleListModels(c *gin.Context) {
	type modelEntry struct {
		ID           string `json:"id"`
		Object       string `json:"object"`
		Created      int64  `json:"created"`
		OwnedBy      string `json:"owned_by"`
		BackendClass string `json:"backend_class"`
	}
	type modelsResponse struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	now := time.Now().Unix()
	table := h.routes.Snapshot()
	ids := table.IDs() // already sorted

	models := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		route, ok := table.Lookup(id)
		if !ok {
			// Defensive: IDs() is always derived from the same table snapshot.
			continue
		}
		models = append(models, modelEntry{
			ID:           id,
			Object:       "model",
			Created:      now,
			OwnedBy:      route.Kind,
			BackendClass: route.Class,
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

// respondModelNotFound writes the 404 for a model with no route table entry.
// There is no default-provider fall-through: an unrecognised model id is
// always refused rather than silently sent to whichever backend happens to be
// primary.
func respondModelNotFound(c *gin.Context, model string) {
	c.JSON(http.StatusNotFound, openAIError{
		Error: openAIErrorBody{
			Message: fmt.Sprintf("model %q is not available on this gateway", model),
			Type:    "invalid_request_error",
			Code:    "model_not_found",
		},
	})
}

// respondModelNotEnabled writes the 403 for a model that has a route but is
// not enabled in policy (models.enabled).
func respondModelNotEnabled(c *gin.Context, model string) {
	c.JSON(http.StatusForbidden, openAIError{
		Error: openAIErrorBody{
			Message: fmt.Sprintf("model %q is not enabled on this gateway", model),
			Type:    "permission_error",
			Code:    "model_not_enabled",
		},
	})
}

// respondResidencyViolation writes the 403 for a cloud_managed route refused
// by a self_hosted_only residency policy.
func respondResidencyViolation(c *gin.Context, model string) {
	c.JSON(http.StatusForbidden, openAIError{
		Error: openAIErrorBody{
			Message: fmt.Sprintf("model %q is cloud-managed, which the current residency policy (self-hosted only) does not permit", model),
			Type:    "permission_error",
			Code:    "model_residency_violation",
		},
	})
}

// logDenial emits one structured log line for a refused chat completion so an
// auditor can see WHY a request was denied without cross-referencing
// inference_events, which today has no column to carry the denial reason
// (model=<alias> with requested_model NULL would otherwise be the only trace
// of an alias-routed 404/403). code is one of model_not_found,
// model_not_enabled, model_residency_violation, policy_unavailable.
// requestedModel is always the raw value the caller sent in the request body
// (regardless of whether it was an alias); resolvedModel and class are empty
// when not yet known (e.g. policy_unavailable, or model_not_found which by
// definition has no route to read a class from).
func logDenial(c *gin.Context, code, requestedModel, resolvedModel, class string) {
	slog.Warn("chat completion denied",
		"request_id", c.GetHeader("X-Request-Id"),
		"key_id", c.GetString(middleware.ContextKeyID),
		"user_id", c.GetString(middleware.ContextUserID),
		"requested_model", requestedModel,
		"resolved_model", resolvedModel,
		"backend_class", class,
		"code", code,
	)
}

// respondPolicyUnavailable writes the 503 for the fail-closed case: no policy
// snapshot has ever loaded successfully.
func respondPolicyUnavailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, openAIError{
		Error: openAIErrorBody{
			Message: "routing policy is not yet available; try again shortly",
			Type:    "service_unavailable",
			Code:    "policy_unavailable",
		},
	})
}

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
