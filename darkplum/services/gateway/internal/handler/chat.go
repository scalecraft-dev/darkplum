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
	"regexp"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/bedrock"
	"github.com/scalecraft/darkplum/gateway/internal/router"
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

// noModelRecorded is written to the "resolved_model" context key (and used as
// the requested_model value in the structured denial log) for the two 400
// paths that never reach model resolution: an unparseable body has no model
// field to read at all, and a missing `model` field is indistinguishable from
// one that was never sent. Recording.go documents why this literal is used
// instead of the empty string for the recorded inference_events.model column.
const noModelRecorded = "(none)"

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
// POST /v1/chat/completions, GET /v1/models, POST /v1/routing/dryrun, and
// GET /v1/policy/effective.
//
// Request order for HandleChatCompletions:
//  1. parse body; if `model` is omitted and the caller used a user API key,
//     try the caller's effective default_model first (see the
//     default-model-resolution block below) — otherwise, and if that lookup
//     found nothing, model is required (400)
//  2. policy snapshot must be loaded, else fail closed (503 policy_unavailable)
//  3. alias resolve (admin intent wins over a same-named discovered model; no
//     chains). A routed alias additionally classifies the request here (step
//     3a below) to pick which of its routes' targets to resolve to; a static
//     alias resolves straight to its target exactly as before.
//  4. route lookup on the resolved id (404 model_not_found — no default fall-through, ever)
//  5. enabled check (403 model_not_enabled)
//  6. residency check (403 model_residency_violation) — against the
//     PRINCIPAL'S effective (strictest) residency, not just the org's
//  7. model_not_allowed check (403) — the resolved id must be in the
//     principal's effective allowlist
//  8. dispatch
//
// Steps 4-7 run unchanged regardless of how the target was resolved: a route
// a routed alias's classifier picked goes through exactly the same
// enforcement as a plain model id or a static alias's target. A forbidden
// pick is refused (see respondModelNotFound/respondModelNotEnabled/
// respondResidencyViolation/respondModelNotAllowed's routeContext parameter)
// — never silently swapped for default_route or any other route.
type ChatHandler struct {
	providers providerLookup
	routes    routeSnapshotter
	policy    policy.Store

	// rulesClassifier and semanticClassifier are the two router.Classifier
	// implementations dispatched to by a routed alias's classifier_kind (see
	// classifierFor). Both are always non-nil.
	rulesClassifier    router.Classifier
	semanticClassifier router.Classifier
}

// NewChatHandler constructs a ChatHandler.
func NewChatHandler(providers providerLookup, routes routeSnapshotter, pol policy.Store, rulesClassifier, semanticClassifier router.Classifier) *ChatHandler {
	return &ChatHandler{
		providers:          providers,
		routes:             routes,
		policy:             pol,
		rulesClassifier:    rulesClassifier,
		semanticClassifier: semanticClassifier,
	}
}

// classifierFor returns the router.Classifier a routed alias's
// classifier_kind selects. Defaults to rules for any unrecognised kind —
// this covers both an unreachable value given the api's classifier_kind
// CHECK constraint, and the removed 'model' kind (policy.buildAliases
// already coerces that to ClassifierKindRules on load, so this default
// branch is what actually serves it) — a routed alias must always resolve
// to SOME classifier rather than panic or leave a nil interface.
func (h *ChatHandler) classifierFor(kind string) router.Classifier {
	switch kind {
	case policy.ClassifierKindSemanticRouter:
		return h.semanticClassifier
	default:
		return h.rulesClassifier
	}
}

// routeContext names the route and alias a routed alias's classifier chose,
// for the custom forbidden-pick error message respondModelNotFound/
// respondModelNotEnabled/respondResidencyViolation produce when non-nil. nil
// for a plain model id or a static alias, preserving their original
// messages unchanged.
type routeContext struct {
	Alias string
	Route string
}

// attributedUserID applies the same X-Darkplum-User-Id trust rule as
// middleware.Recording's resolveAttributedUserID: the header is honoured
// only for a service-key caller (the api proxying a session user's
// request); a user-key caller is always attributed to its own key-level
// user_id. Duplicated here (rather than exporting the middleware package's
// version) because it is the same three-line rule, not shared state, and
// pulling in a cross-package export for it would be more coupling than the
// rule is worth.
func attributedUserID(c *gin.Context) string {
	if c.GetString(middleware.ContextKeyType) == middleware.KeyTypeService {
		if h := c.GetHeader("X-Darkplum-User-Id"); h != "" {
			return h
		}
	}
	return c.GetString(middleware.ContextUserID)
}

// lastUserTurn returns the last message with Role == "user", or a zero
// Message if there is none. This is the text a routed alias's classifier
// evaluates — the system prompt (and any other non-user turn) is
// deliberately excluded.
func lastUserTurn(messages []provider.Message) provider.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i]
		}
	}
	return provider.Message{}
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
		c.Set("denial_code", "invalid_request")
		c.Set("resolved_model", noModelRecorded)
		c.Header("X-Darkplum-Model", noModelRecorded)
		logDenial(c, "invalid_request", noModelRecorded, "", "")
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("invalid request body: %s", err.Error()),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	// A user-key caller that omits `model` gets its effective default_model,
	// if one is set anywhere in its org -> user -> api_key chain. This is a
	// best-effort lookup against whatever policy snapshot happens to be
	// available right now (never blocking, never itself a source of 503): if
	// no snapshot has loaded yet, or the caller isn't a user key, or no
	// default is set, req.Model stays "" and the existing 400 below fires
	// exactly as before. If a default IS found, req.Model is filled in and
	// the ENTIRE normal chain below still runs against it — including
	// model_not_allowed, in case a later policy tightening orphaned the
	// stored default (see DESIGN4.md's "Omitted model" note).
	if req.Model == "" && c.GetString(middleware.ContextKeyType) == middleware.KeyTypeUser {
		if snap, loaded := h.policy.Current(); loaded {
			eff := snap.EffectiveFor(attributedUserID(c), c.GetString(middleware.ContextKeyID), middleware.KeyTypeUser)
			if eff.DefaultModel != nil && *eff.DefaultModel != "" {
				req.Model = *eff.DefaultModel
			}
		}
	}

	if req.Model == "" {
		c.Set("denial_code", "invalid_request")
		c.Set("resolved_model", noModelRecorded)
		c.Header("X-Darkplum-Model", noModelRecorded)
		logDenial(c, "invalid_request", noModelRecorded, "", "")
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
	// resolved_model/requested_model/route_name/router_reason are written to
	// the Gin context (and, for resolved_model/route_name/router_reason, the
	// X-Darkplum-* response headers) immediately — before any of the checks
	// below can refuse the request — so that a denial reached through an
	// alias (unknown target, disabled target, residency violation), including
	// a routed alias's forbidden pick, is still recorded/reported against the
	// model (and, for a routed alias, the route) actually selected, not left
	// with model=<alias> and requested_model NULL.
	requestedModel := ""
	resolvedModel := req.Model
	routeName := ""
	routerReason := ""
	var routerLatencyMS *int

	alias, isAlias := snap.Aliases[req.Model]
	switch {
	case isAlias && alias.Kind == policy.AliasKindRouted:
		requestedModel = req.Model

		turn := lastUserTurn(req.Messages)
		in := router.Input{
			Text:        router.Truncate(turn.Content, router.DefaultTruncateChars),
			OriginalLen: len([]rune(turn.Content)),
			HasNonText:  turn.HasNonTextContent,
		}

		start := time.Now()
		decision, err := h.classifierFor(alias.ClassifierKind).Classify(c.Request.Context(), alias, in)
		latency := int(time.Since(start).Milliseconds())
		routerLatencyMS = &latency

		if err != nil {
			// ErrAborted (ctx canceled): never dispatch, never fall back to
			// default_route or any route.
			c.Set("denial_code", "request_canceled")
			logDenial(c, "request_canceled", req.Model, "", "")
			respondRequestCanceled(c)
			return
		}

		routeName = decision.RouteName
		routerReason = decision.Reason

		rd, ok := alias.Route(routeName)
		if !ok {
			// Unreachable for any alias actually present in a loaded
			// Snapshot (policy.buildAliases drops a routed alias whose
			// default_route doesn't resolve, and every Classifier only ever
			// names alias.DefaultRoute or one of alias.Routes) — but fail
			// closed rather than dispatch to an unresolved target if this
			// invariant is ever violated.
			c.Set("denial_code", "model_not_found")
			logDenial(c, "model_not_found", req.Model, "", "")
			respondModelNotFound(c, req.Model, &routeContext{Alias: requestedModel, Route: routeName})
			return
		}
		resolvedModel = rd.Target
	case isAlias:
		// Static alias: unchanged behaviour.
		requestedModel = req.Model
		resolvedModel = alias.Target
	}

	c.Set("resolved_model", resolvedModel)
	c.Set("requested_model", requestedModel)
	c.Header("X-Darkplum-Model", resolvedModel)

	var routeCtx *routeContext
	if routeName != "" {
		c.Set("route_name", routeName)
		c.Set("router_reason", routerReason)
		c.Header("X-Darkplum-Route", routeName)
		c.Header("X-Darkplum-Router-Reason", routerReason)
		routeCtx = &routeContext{Alias: requestedModel, Route: routeName}
	}
	if routerLatencyMS != nil {
		c.Set("router_latency_ms", *routerLatencyMS)
	}

	route, ok := h.routes.Snapshot().Lookup(resolvedModel)
	if !ok {
		c.Set("denial_code", "model_not_found")
		logDenial(c, "model_not_found", req.Model, resolvedModel, "")
		respondModelNotFound(c, resolvedModel, routeCtx)
		return
	}

	if !snap.Enabled[resolvedModel] {
		c.Set("denial_code", "model_not_enabled")
		logDenial(c, "model_not_enabled", req.Model, resolvedModel, route.Class)
		respondModelNotEnabled(c, resolvedModel, routeCtx)
		return
	}

	// eff is the calling principal's effective policy: for a user key, its
	// own key row folded through its owner's user row and the org row; for a
	// service key, the X-Darkplum-User-Id it presents (attributedUserID),
	// with no key-level row ever consulted (see policy.Snapshot.EffectiveFor
	// and its KeyTypeService branch). The residency check below uses this
	// STRICTEST effective value rather than snap.Residency directly, so a
	// tighter user/key residency setting is enforced even when the org
	// itself allows cloud-managed models.
	eff := snap.EffectiveFor(attributedUserID(c), c.GetString(middleware.ContextKeyID), c.GetString(middleware.ContextKeyType))

	if eff.Residency == policy.ResidencySelfHostedOnly && route.Class == routing.ClassCloudManaged {
		c.Set("denial_code", "model_residency_violation")
		logDenial(c, "model_residency_violation", req.Model, resolvedModel, route.Class)
		respondResidencyViolation(c, resolvedModel, routeCtx)
		return
	}

	// model_not_allowed: the check is always on resolvedModel — the SELECTED
	// target, including a routed alias's classifier pick. A route to a
	// disallowed model is refused here, never silently swapped for
	// default_route or any other route.
	if !eff.Allows(resolvedModel) {
		c.Set("denial_code", "model_not_allowed")
		logDenial(c, "model_not_allowed", req.Model, resolvedModel, route.Class)
		respondModelNotAllowed(c, resolvedModel, routeCtx)
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
//
// Filtering by effective routing policy applies ONLY to a user API key
// (ContextKeyType == middleware.KeyTypeUser): the caller's own allowlist
// hides models it could never successfully call anyway. A SERVICE key's
// response is always the full, unfiltered list — the api's SyncModels is the
// one caller of this endpoint using a service key, and it needs the
// complete discovered set (including models no principal is currently
// allowed to use) to keep its own models table in sync; filtering that away
// would silently stop syncing models the moment an admin tightened policy.
func (h *ChatHandler) HandleListModels(c *gin.Context) {
	type modelEntry struct {
		ID           string `json:"id"`
		Object       string `json:"object"`
		Created      int64  `json:"created"`
		OwnedBy      string `json:"owned_by"`
		BackendClass string `json:"backend_class"`
	}
	// DefaultModel is the caller's effective default_model (what a request
	// that names no model will run on) — set only for a user key, where the
	// list is also filtered; nil for the service key's unfiltered view. The
	// api relays it to Bearer callers of its own /v1/models so the two lists
	// agree on both membership and default.
	type modelsResponse struct {
		Object       string       `json:"object"`
		Data         []modelEntry `json:"data"`
		DefaultModel *string      `json:"default_model,omitempty"`
	}

	now := time.Now().Unix()
	table := h.routes.Snapshot()
	ids := table.IDs() // already sorted

	var eff policy.EffectivePolicy
	filterByPolicy := false
	if c.GetString(middleware.ContextKeyType) == middleware.KeyTypeUser {
		if snap, loaded := h.policy.Current(); loaded {
			eff = snap.EffectiveFor(attributedUserID(c), c.GetString(middleware.ContextKeyID), middleware.KeyTypeUser)
			filterByPolicy = true
		}
		// If policy hasn't loaded yet, fall through unfiltered rather than
		// 503 — this endpoint is metadata, not an enforcement point; a
		// request that actually tries to USE a filtered-out model still goes
		// through HandleChatCompletions, which fails closed on its own.
	}

	models := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		route, ok := table.Lookup(id)
		if !ok {
			// Defensive: IDs() is always derived from the same table snapshot.
			continue
		}
		if filterByPolicy && !eff.Allows(id) {
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

	resp := modelsResponse{Object: "list", Data: models}
	if filterByPolicy {
		resp.DefaultModel = eff.DefaultModel
	}
	c.JSON(http.StatusOK, resp)
}

// uuidPattern matches a canonical 8-4-4-4-12 hex UUID (any version/variant —
// the gateway only needs to reject obviously malformed input before using
// the value as a map key, not validate RFC 4122 conformance).
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

// effectivePolicyResponse is the body of GET /v1/policy/effective, and (with
// Keys additionally populated in some cases — see HandleEffectivePolicyOverride)
// the body of POST /v1/policy/effective. Keys is omitted from the wire
// response entirely when empty/unset (both for GET, which never sets it, and
// for a POST call where it wasn't applicable), so GET's response shape is
// byte-for-byte unchanged by POST's existence.
type effectivePolicyResponse struct {
	policy.EffectivePolicy
	ReachableModels []string                  `json:"reachable_models"`
	Keys            []effectivePolicyKeyEntry `json:"keys,omitempty"`
}

// effectivePolicyKeyEntry is one entry of effectivePolicyResponse.Keys — a
// key owned by the user_id under preview, and what ITS effective policy
// would be under the same hypothetical override. Name is deliberately absent
// (see HandleEffectivePolicyOverride's doc comment): the gateway has no
// api_keys.name column to read, only ownership (Snapshot.KeyOwner/UserKeys)
// and policy rows — the api fills in the name from its own database when it
// builds keys_affected.
type effectivePolicyKeyEntry struct {
	APIKeyID  string                  `json:"api_key_id"`
	Effective effectivePolicyResponse `json:"effective"`
}

// HandleEffectivePolicy handles GET /v1/policy/effective?user_id=<uuid>[&api_key_id=<uuid>],
// the single merge implementation's read surface for the api: every
// "effective policy" view the api renders (org card, per-user/per-key
// drawers, /me) proxies this endpoint with the service key rather than
// re-implementing Effective() on the api side.
//
// Service-key only: a user or service caller impersonating another
// principal's policy view is exactly the kind of cross-tenant read the api
// must never be able to trigger from a session-scoped credential, so this
// refuses (403) for anything but the shared service key — the api always
// holds one, every session-authenticated request already proxies through it.
func (h *ChatHandler) HandleEffectivePolicy(c *gin.Context) {
	if c.GetString(middleware.ContextKeyType) != middleware.KeyTypeService {
		c.JSON(http.StatusForbidden, openAIError{
			Error: openAIErrorBody{
				Message: "GET /v1/policy/effective is only available to the service key",
				Type:    "permission_error",
				Code:    "service_key_required",
			},
		})
		return
	}

	userID := c.Query("user_id")
	if !isUUID(userID) {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: "user_id is required and must be a UUID",
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	keyID := c.Query("api_key_id")
	if keyID != "" && !isUUID(keyID) {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: "api_key_id must be a UUID",
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	snap, loaded := h.policy.Current()
	if !loaded {
		respondPolicyUnavailable(c)
		return
	}

	// keyType is deliberately middleware.KeyTypeUser here regardless of the
	// CALLING credential's own type (always the service key, checked above):
	// this parameter tells EffectiveFor whether to consult KeyPolicies for
	// keyID, which is exactly what we want to evaluate for the user/key pair
	// under inspection — never KeyTypeService, which would (correctly, for a
	// REAL service-key request) skip key-level rows entirely.
	eff := snap.EffectiveFor(userID, keyID, middleware.KeyTypeUser)

	c.JSON(http.StatusOK, effectivePolicyResponse{
		EffectivePolicy: eff,
		ReachableModels: reachableModels(eff, snap, h.routes.Snapshot()),
	})
}

// effectivePolicyOverrideRequest is the body of POST /v1/policy/effective.
type effectivePolicyOverrideRequest struct {
	UserID   string                   `json:"user_id"`
	APIKeyID string                   `json:"api_key_id"`
	Override *effectivePolicyOverride `json:"override"`
}

// effectivePolicyOverride names the one chain level (policy.PrincipalType*)
// to hypothetically substitute, and what to substitute it with. Policy nil
// means "inherit": remove whatever row this level currently has, exactly
// like DELETE .../routing-policy does today.
type effectivePolicyOverride struct {
	Level  string                `json:"level"`
	Policy *effectivePolicyRowIn `json:"policy"`
}

// effectivePolicyRowIn is the wire shape of one level's policy — the same
// shape the api's PUT .../routing-policy accepts, so the api can pass the
// proposed body straight through as the override with no translation.
type effectivePolicyRowIn struct {
	Residency     *string   `json:"residency"`
	AllowedModels *[]string `json:"allowed_models"`
	DefaultModel  *string   `json:"default_model"`
}

// HandleEffectivePolicyOverride handles POST /v1/policy/effective, the
// preview counterpart of GET /v1/policy/effective: it builds the same
// principal chain (Snapshot.ChainFor) and runs the SAME Effective() merge as
// every other consumer of this package, but with one chain level
// hypothetically substituted (or removed, when override.policy is null =
// "inherit") — this is what lets the api's impact-preview endpoints (POST
// .../users/:id/routing-policy/preview and .../api-keys/:id/routing-policy/preview)
// answer "what would this look like" before writing anything, without a
// second, divergent merge implementation living on the api side. See
// DESIGN4.md's "Effective(chain) — ONE implementation, in the gateway".
//
// Service-key only, identical rule to GET (see its doc comment) — a user or
// service caller impersonating another principal's policy preview is exactly
// the cross-tenant read the api must never be able to trigger from a
// session-scoped credential.
func (h *ChatHandler) HandleEffectivePolicyOverride(c *gin.Context) {
	if c.GetString(middleware.ContextKeyType) != middleware.KeyTypeService {
		c.JSON(http.StatusForbidden, openAIError{
			Error: openAIErrorBody{
				Message: "POST /v1/policy/effective is only available to the service key",
				Type:    "permission_error",
				Code:    "service_key_required",
			},
		})
		return
	}

	var req effectivePolicyOverrideRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("invalid request body: %s", err.Error()),
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	if !isUUID(req.UserID) {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: "user_id is required and must be a UUID",
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}
	if req.APIKeyID != "" && !isUUID(req.APIKeyID) {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: "api_key_id must be a UUID",
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	var override *policy.PolicyOverride
	if req.Override != nil {
		switch req.Override.Level {
		case policy.PrincipalTypeOrg, policy.PrincipalTypeUser:
			// Always applicable: ChainFor's chain always has an org row and a
			// user row.
		case policy.PrincipalTypeAPIKey:
			if req.APIKeyID == "" {
				c.JSON(http.StatusBadRequest, openAIError{
					Error: openAIErrorBody{
						Message: `override.level "api_key" requires a non-null api_key_id`,
						Type:    "invalid_request_error",
						Code:    "invalid_request",
					},
				})
				return
			}
		default:
			c.JSON(http.StatusBadRequest, openAIError{
				Error: openAIErrorBody{
					Message: fmt.Sprintf("override.level must be one of org, user, api_key, got %q", req.Override.Level),
					Type:    "invalid_request_error",
					Code:    "invalid_request",
				},
			})
			return
		}

		var row *policy.PolicyRow
		if req.Override.Policy != nil {
			if req.Override.Policy.Residency != nil && *req.Override.Policy.Residency != policy.ResidencySelfHostedOnly {
				c.JSON(http.StatusBadRequest, openAIError{
					Error: openAIErrorBody{
						Message: `override.policy.residency must be null or "self_hosted_only"`,
						Type:    "invalid_request_error",
						Code:    "invalid_request",
					},
				})
				return
			}
			row = &policy.PolicyRow{
				Residency:     req.Override.Policy.Residency,
				AllowedModels: req.Override.Policy.AllowedModels,
				DefaultModel:  req.Override.Policy.DefaultModel,
			}
		}
		override = &policy.PolicyOverride{Level: req.Override.Level, Policy: row}
	}

	snap, loaded := h.policy.Current()
	if !loaded {
		respondPolicyUnavailable(c)
		return
	}

	table := h.routes.Snapshot()
	resp := previewEffective(snap, table, req.UserID, req.APIKeyID, override)

	// Keys (every non-revoked key owned by req.UserID, each re-evaluated
	// under the same override) is populated whenever the caller is looking at
	// a user-scoped view: either no specific key was named at all (the plain
	// GET-equivalent case, which doubles as the "before" half of the api's
	// preview diff — see DESIGN4.md's Impact preview section), or the
	// override itself targets "user"/"org" (the "after" half, previewing a
	// change that can ripple to every key this user owns). An "api_key"-level
	// override with an api_key_id only ever affects that one key, already
	// covered by the top-level response, so no list is needed there.
	if req.APIKeyID == "" || (override != nil && (override.Level == policy.PrincipalTypeUser || override.Level == policy.PrincipalTypeOrg)) {
		for _, keyID := range snap.UserKeys[req.UserID] {
			resp.Keys = append(resp.Keys, effectivePolicyKeyEntry{
				APIKeyID:  keyID,
				Effective: previewEffective(snap, table, req.UserID, keyID, override),
			})
		}
	}

	c.JSON(http.StatusOK, resp)
}

// policyImpactRequest is the body of POST /v1/policy/impact. Only an org
// level override is accepted: a user or api_key change only ever ripples to
// that principal and the keys under it, which POST /v1/policy/effective
// already answers; an org change ripples to every principal, which is what
// this endpoint exists to enumerate in one call.
type policyImpactRequest struct {
	Override *effectivePolicyOverride `json:"override"`
}

// policyImpactPrincipal names one principal POST /v1/policy/impact found
// locked out (empty effective allowlist) under the proposed org policy. A
// user entry has no api_key_id; a key entry carries both.
type policyImpactPrincipal struct {
	UserID   string `json:"user_id"`
	APIKeyID string `json:"api_key_id,omitempty"`
}

// policyImpactResponse is the body of a 200 from POST /v1/policy/impact.
type policyImpactResponse struct {
	// OrgLockedOut is true when the proposed org policy itself has an empty
	// allowlist: every principal in the organisation, explicit row or not,
	// would then be locked out, so LockedOut is not enumerated (it would be
	// every user and key there is).
	OrgLockedOut bool `json:"org_locked_out"`
	// LockedOut lists every principal with an explicit policy row (or a key
	// owned by a user with one) whose effective allowlist would be empty
	// under the override. Never null.
	LockedOut []policyImpactPrincipal `json:"locked_out"`
	// OrgNarrowed is true when the org's own allowed set (allowed ∧ enabled
	// ∧ residency-ok — policy only, not backend health) under the override
	// loses a model it has now: everyone who simply inherits the
	// organisation's policy is allowed fewer models than before.
	OrgNarrowed bool `json:"org_narrowed"`
	// Narrowed lists every principal WITH A POLICY ROW OF ITS OWN whose
	// allowed set under the override loses a model it has now. Principals
	// that inherit (a user with no row, a key with no row) are the org or
	// their owner respectively and are covered by OrgNarrowed or by their
	// owner's entry; listing them here would count one change many times.
	// A locked-out principal is also narrowed unless it already had nothing.
	// Never null.
	Narrowed []policyImpactPrincipal `json:"narrowed"`
	// Evaluated is how many user/key principals were re-merged.
	Evaluated int `json:"evaluated"`
}

// HandleEffectivePolicyImpact handles POST /v1/policy/impact: the org-wide
// counterpart of HandleEffectivePolicyOverride. The api's org PUT
// /api/v1/routing-policy needs the same lock-out guard the user/key PUTs get
// from POST /v1/policy/effective (refuse with 409 when the write would leave
// a principal with no reachable model), but an org change can affect every
// principal at once, and calling POST /v1/policy/effective once per principal
// from inside a single admin request is exactly the per-principal-call-in-a-
// loop anti-pattern the review checklist flags. So this endpoint does the
// walk here, against the snapshot already in memory, using the SAME
// Effective() merge — no second implementation on the api side.
//
// Which principals can a non-empty org allowlist lock out? Only ones with
// their own allowlist somewhere in their chain: a principal that inherits is
// the org, and the org is non-empty by hypothesis. So the walk covers every
// user with a row, every key with a row (via its owner), and every key those
// users own. An EMPTY org allowlist locks everyone out; that is reported as
// org_locked_out rather than enumerated.
//
// Service-key only, identical rule to GET/POST /v1/policy/effective.
func (h *ChatHandler) HandleEffectivePolicyImpact(c *gin.Context) {
	if c.GetString(middleware.ContextKeyType) != middleware.KeyTypeService {
		c.JSON(http.StatusForbidden, openAIError{
			Error: openAIErrorBody{
				Message: "POST /v1/policy/impact is only available to the service key",
				Type:    "permission_error",
				Code:    "service_key_required",
			},
		})
		return
	}

	var req policyImpactRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("invalid request body: %s", err.Error()),
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}
	if req.Override == nil || req.Override.Level != policy.PrincipalTypeOrg {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: `override.level must be "org" (use POST /v1/policy/effective for a user or api_key preview)`,
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}
	var row *policy.PolicyRow
	if req.Override.Policy != nil {
		if req.Override.Policy.Residency != nil && *req.Override.Policy.Residency != policy.ResidencySelfHostedOnly {
			c.JSON(http.StatusBadRequest, openAIError{
				Error: openAIErrorBody{
					Message: `override.policy.residency must be null or "self_hosted_only"`,
					Type:    "invalid_request_error",
					Code:    "invalid_request",
				},
			})
			return
		}
		row = &policy.PolicyRow{
			Residency:     req.Override.Policy.Residency,
			AllowedModels: req.Override.Policy.AllowedModels,
			DefaultModel:  req.Override.Policy.DefaultModel,
		}
	}
	override := &policy.PolicyOverride{Level: policy.PrincipalTypeOrg, Policy: row}

	snap, loaded := h.policy.Current()
	if !loaded {
		respondPolicyUnavailable(c)
		return
	}

	c.JSON(http.StatusOK, policyImpact(snap, h.routes.Snapshot(), override))
}

// policyImpact is HandleEffectivePolicyImpact's pure core: it re-merges every
// principal that could be affected by an org-level override (see the
// handler's doc comment for why that set is exactly "users with a row, owners
// of keys with a row, and every key those users own"), reports the ones
// whose effective allowlist comes out empty (LockedOut), the ones with a row
// of their own whose allowed set (allowedModels: allowed ∧ enabled ∧
// residency-ok) shrinks (Narrowed), and whether the org's own allowed set —
// hence every inheriting principal's — shrinks (OrgNarrowed). Split from the
// handler so tests can drive it with a hand-built Snapshot and route table.
func policyImpact(snap policy.Snapshot, table *routing.Table, override *policy.PolicyOverride) policyImpactResponse {
	resp := policyImpactResponse{LockedOut: []policyImpactPrincipal{}, Narrowed: []policyImpactPrincipal{}}

	// The org's own chain is just its row (ChainFor's first element; the
	// user placeholder is dropped so an inheriting principal's view is
	// exactly the org's).
	orgChain := snap.ChainFor("", "")[:1]
	orgNow := policy.Effective(orgChain)
	orgAfter := policy.Effective(override.Apply(orgChain))
	if lockedOut(orgAfter) {
		resp.OrgLockedOut = true
		resp.OrgNarrowed = len(allowedModels(orgNow, snap, table)) > 0
		return resp
	}
	resp.OrgNarrowed = losesAny(allowedModels(orgNow, snap, table), allowedModels(orgAfter, snap, table))

	users := make(map[string]bool, len(snap.UserRows)+len(snap.KeyRows))
	for userID := range snap.UserRows {
		users[userID] = true
	}
	for keyID := range snap.KeyRows {
		if owner, ok := snap.KeyOwner[keyID]; ok {
			users[owner] = true
		}
	}
	userIDs := make([]string, 0, len(users))
	for userID := range users {
		userIDs = append(userIDs, userID)
	}
	sort.Strings(userIDs)

	evaluate := func(userID, keyID string) {
		resp.Evaluated++
		chain := snap.ChainFor(userID, keyID)
		now := policy.Effective(chain)
		after := policy.Effective(override.Apply(chain))
		p := policyImpactPrincipal{UserID: userID, APIKeyID: keyID}
		if lockedOut(after) {
			resp.LockedOut = append(resp.LockedOut, p)
		}
		_, ownRow := snap.UserRows[userID]
		if keyID != "" {
			_, ownRow = snap.KeyRows[keyID]
		}
		if ownRow && losesAny(allowedModels(now, snap, table), allowedModels(after, snap, table)) {
			resp.Narrowed = append(resp.Narrowed, p)
		}
	}
	for _, userID := range userIDs {
		evaluate(userID, "")
		for _, keyID := range snap.UserKeys[userID] {
			evaluate(userID, keyID)
		}
	}
	return resp
}

// allowedModels is the policy-side counterpart of reachableModels: the
// enabled models eff permits under its residency, WITHOUT the routability
// test. Impact counts are about what an admin's policy change takes away,
// and must not move because a backend happens to be down at save time.
// Under self_hosted_only a model the route table has never seen has no
// known class and is EXCLUDED — the same deny-when-uncertain rule
// enforcement applies (such a request is refused anyway), and the safe
// direction for a preview: it may warn of a loss that turns out not to be
// one, never stay silent about one that is. Under residency any, class is
// irrelevant and the model is kept.
func allowedModels(eff policy.EffectivePolicy, snap policy.Snapshot, table *routing.Table) []string {
	ids := make([]string, 0, len(snap.Enabled))
	for id, enabled := range snap.Enabled {
		if enabled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !eff.Allows(id) {
			continue
		}
		if eff.Residency == policy.ResidencySelfHostedOnly {
			route, ok := table.Lookup(id)
			if !ok || route.Class == routing.ClassCloudManaged {
				continue
			}
		}
		out = append(out, id)
	}
	return out
}

// losesAny reports whether before contains a model after does not — the
// "allowed fewer models than before" test. A reordering or a widening is
// not a loss.
func losesAny(before, after []string) bool {
	keep := make(map[string]bool, len(after))
	for _, id := range after {
		keep[id] = true
	}
	for _, id := range before {
		if !keep[id] {
			return true
		}
	}
	return false
}

// lockedOut reports whether eff permits no model at all — a non-nil, empty
// allowlist. nil (unrestricted) is not locked out.
func lockedOut(eff policy.EffectivePolicy) bool {
	return eff.AllowedModels != nil && len(*eff.AllowedModels) == 0
}

// previewEffective builds userID/keyID's raw chain (Snapshot.ChainFor),
// applies override when non-nil, and runs policy.Effective() — the shared
// core of HandleEffectivePolicyOverride's top-level response and its
// per-key Keys entries. keyID == "" builds the 2-row (org, user) chain.
func previewEffective(snap policy.Snapshot, table *routing.Table, userID, keyID string, override *policy.PolicyOverride) effectivePolicyResponse {
	chain := snap.ChainFor(userID, keyID)
	if override != nil {
		chain = override.Apply(chain)
	}
	eff := policy.Effective(chain)
	return effectivePolicyResponse{
		EffectivePolicy: eff,
		ReachableModels: reachableModels(eff, snap, table),
	}
}

// reachableModels computes the models that are allowed by eff AND currently
// routable AND enabled AND residency-ok — the "right now" view GET
// /v1/policy/effective reports separately from the raw allowed_models
// allowlist (which may name models that don't exist yet, aren't enabled, or
// are cloud_managed under a self_hosted_only residency).
func reachableModels(eff policy.EffectivePolicy, snap policy.Snapshot, table *routing.Table) []string {
	ids := table.IDs() // already sorted
	reachable := make([]string, 0, len(ids))
	for _, id := range ids {
		if !eff.Allows(id) {
			continue
		}
		if !snap.Enabled[id] {
			continue
		}
		route, ok := table.Lookup(id)
		if !ok {
			continue
		}
		if eff.Residency == policy.ResidencySelfHostedOnly && route.Class == routing.ClassCloudManaged {
			continue
		}
		reachable = append(reachable, id)
	}
	return reachable
}

// routingDryrunRequest is the body of POST /v1/routing/dryrun.
type routingDryrunRequest struct {
	Alias string `json:"alias"`
	Text  string `json:"text"`
}

// routingDryrunRefusal describes the enforcement outcome a real chat
// completion against RouteName's target would hit, without ever calling it.
type routingDryrunRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// routingDryrunResponse is the body of a 200 from POST /v1/routing/dryrun.
type routingDryrunResponse struct {
	RouteName      string                `json:"route_name"`
	TargetModelID  string                `json:"target_model_id"`
	Reason         string                `json:"reason"`
	LatencyMS      int                   `json:"latency_ms"`
	WouldBeRefused *routingDryrunRefusal `json:"would_be_refused"`
}

// HandleRoutingDryrun handles POST /v1/routing/dryrun: the admin "test a
// prompt" panel. It runs alias's classifier against text exactly as a real
// chat completion would, evaluates whether the resulting pick would be
// refused (route lookup / enabled / residency), and reports the answer —
// but it NEVER calls the picked route's target model, and it records
// nothing: a semantic-router classifier call is CPU-only intent
// classification, not an LLM inference, so there is nothing to audit beyond
// the response body itself.
func (h *ChatHandler) HandleRoutingDryrun(c *gin.Context) {
	var req routingDryrunRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil || req.Alias == "" || req.Text == "" {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: "alias and text are both required",
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	snap, loaded := h.policy.Current()
	if !loaded {
		respondPolicyUnavailable(c)
		return
	}

	alias, ok := snap.Aliases[req.Alias]
	if !ok || alias.Kind != policy.AliasKindRouted {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("alias %q is not a routed alias", req.Alias),
				Type:    "invalid_request_error",
				Code:    "not_a_routed_alias",
			},
		})
		return
	}

	in := router.Input{
		Text:        router.Truncate(req.Text, router.DefaultTruncateChars),
		OriginalLen: len([]rune(req.Text)),
	}

	start := time.Now()
	decision, err := h.classifierFor(alias.ClassifierKind).Classify(c.Request.Context(), alias, in)
	latency := int(time.Since(start).Milliseconds())

	if err != nil {
		// ErrAborted: the caller's request was canceled mid-classification.
		c.JSON(499, openAIError{
			Error: openAIErrorBody{
				Message: "request canceled before a routing decision completed",
				Type:    "invalid_request_error",
				Code:    "request_canceled",
			},
		})
		return
	}

	rd, _ := alias.Route(decision.RouteName)
	target := rd.Target

	resp := routingDryrunResponse{
		RouteName:     decision.RouteName,
		TargetModelID: target,
		Reason:        decision.Reason,
		LatencyMS:     latency,
	}

	// Evaluate the same route/enabled/residency chain HandleChatCompletions
	// would apply to target — WITHOUT ever calling h.providers.Get or
	// dispatching to it.
	if rt, ok := h.routes.Snapshot().Lookup(target); !ok {
		resp.WouldBeRefused = &routingDryrunRefusal{
			Code:    "model_not_found",
			Message: fmt.Sprintf("model %q is not available on this gateway", target),
		}
	} else if !snap.Enabled[target] {
		resp.WouldBeRefused = &routingDryrunRefusal{
			Code:    "model_not_enabled",
			Message: fmt.Sprintf("model %q is not enabled on this gateway", target),
		}
	} else if snap.Residency == policy.ResidencySelfHostedOnly && rt.Class == routing.ClassCloudManaged {
		resp.WouldBeRefused = &routingDryrunRefusal{
			Code:    "model_residency_violation",
			Message: fmt.Sprintf("model %q is cloud-managed, which the current residency policy (self-hosted only) does not permit", target),
		}
	}

	c.JSON(http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

// respondModelNotFound writes the 404 for a model with no route table entry.
// There is no default-provider fall-through: an unrecognised model id is
// always refused rather than silently sent to whichever backend happens to be
// primary. rc names the routed alias/route that selected model when the pick
// came from a routed alias's classifier (nil for a plain model id or a
// static alias) — the message then names the route instead of just the
// model, and the pick is never swapped for default_route or any other route.
func respondModelNotFound(c *gin.Context, model string, rc *routeContext) {
	msg := fmt.Sprintf("model %q is not available on this gateway", model)
	if rc != nil {
		msg = fmt.Sprintf("route %q of alias %q selects model %q, which is not available on this gateway", rc.Route, rc.Alias, model)
	}
	c.JSON(http.StatusNotFound, openAIError{
		Error: openAIErrorBody{
			Message: msg,
			Type:    "invalid_request_error",
			Code:    "model_not_found",
		},
	})
}

// respondModelNotEnabled writes the 403 for a model that has a route but is
// not enabled in policy (models.enabled). See respondModelNotFound for rc.
func respondModelNotEnabled(c *gin.Context, model string, rc *routeContext) {
	msg := fmt.Sprintf("model %q is not enabled on this gateway", model)
	if rc != nil {
		msg = fmt.Sprintf("route %q of alias %q selects model %q, which is not enabled/allowed by residency", rc.Route, rc.Alias, model)
	}
	c.JSON(http.StatusForbidden, openAIError{
		Error: openAIErrorBody{
			Message: msg,
			Type:    "permission_error",
			Code:    "model_not_enabled",
		},
	})
}

// respondResidencyViolation writes the 403 for a cloud_managed route refused
// by a self_hosted_only residency policy. See respondModelNotFound for rc.
// Uses the SAME message text as respondModelNotEnabled for a routed pick —
// "not enabled/allowed by residency" covers both refusal reasons in one
// phrasing, exactly as specified for the routed-alias forbidden-pick error.
func respondResidencyViolation(c *gin.Context, model string, rc *routeContext) {
	msg := fmt.Sprintf("model %q is cloud-managed, which the current residency policy (self-hosted only) does not permit", model)
	if rc != nil {
		msg = fmt.Sprintf("route %q of alias %q selects model %q, which is not enabled/allowed by residency", rc.Route, rc.Alias, model)
	}
	c.JSON(http.StatusForbidden, openAIError{
		Error: openAIErrorBody{
			Message: msg,
			Type:    "permission_error",
			Code:    "model_residency_violation",
		},
	})
}

// respondModelNotAllowed writes the 403 for a model that is available,
// enabled, and residency-compliant but outside the calling principal's
// effective allowlist (policy.EffectivePolicy.Allows). See respondModelNotFound
// for rc. Message text for the non-routed case matches DESIGN4.md's contract
// exactly: `model %q is not allowed for this user/API key`.
func respondModelNotAllowed(c *gin.Context, model string, rc *routeContext) {
	msg := fmt.Sprintf("model %q is not allowed for this user/API key", model)
	if rc != nil {
		msg = fmt.Sprintf("route %q of alias %q selects model %q, which is not allowed for this user/API key", rc.Route, rc.Alias, model)
	}
	c.JSON(http.StatusForbidden, openAIError{
		Error: openAIErrorBody{
			Message: msg,
			Type:    "permission_error",
			Code:    "model_not_allowed",
		},
	})
}

// respondRequestCanceled writes the response for a routed alias's classifier
// aborting because the request's context was canceled (router.ErrAborted) —
// the one case where the handler must not dispatch anywhere at all, not even
// to default_route. 499 is not a standard Go http.Status* constant, but it
// is the conventional "client disconnected" status several proxies (nginx)
// use; in practice the caller has usually already gone by the time this
// writes, since a canceled request context most often means the underlying
// connection is gone.
func respondRequestCanceled(c *gin.Context) {
	c.JSON(499, openAIError{
		Error: openAIErrorBody{
			Message: "request canceled before a routing decision completed",
			Type:    "invalid_request_error",
			Code:    "request_canceled",
		},
	})
}

// logDenial emits one structured log line for a refused chat completion so an
// auditor can see WHY a request was denied without cross-referencing
// inference_events, which today has no column to carry the denial reason
// (model=<alias> with requested_model NULL would otherwise be the only trace
// of an alias-routed 404/403). code is one of model_not_found,
// model_not_enabled, model_residency_violation, model_not_allowed,
// policy_unavailable, invalid_request, request_canceled. requestedModel is always the raw value
// the caller sent in the request body (regardless of whether it was an
// alias), except for invalid_request where no reliable value exists —
// noModelRecorded ("(none)") is passed instead so the log and the audit
// table agree; resolvedModel and class are empty when not yet known (e.g.
// policy_unavailable, invalid_request, request_canceled, or model_not_found
// which by definition has no route to read a class from).
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
