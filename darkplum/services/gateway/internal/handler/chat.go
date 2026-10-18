// Package handler contains the HTTP handlers for the gateway's OpenAI-compatible
// API surface. ChatHandler is the enforcement point for model routing: every
// caller (session or API key) passes through HandleChatCompletions, which
// resolves aliases, looks up the route table, and enforces the enabled and
// residency policy checks before dispatching to a backend provider.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
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
	// Param names the request field a 400 unsupported_parameter response
	// refers to (e.g. "tools", "tool_choice", "response_format") — see
	// unsupportedRequestParam. Empty/omitted for every other error code.
	Param string `json:"param,omitempty"`
}

// stopField accepts the OpenAI `stop` request field in either shape the API
// allows: a single string, or an array of strings. Normalises to []string so
// provider.ChatRequest.Stop always has one representation regardless of
// which shape the caller sent — the same dual-shape tolerance
// provider.Message.UnmarshalJSON already applies to `content`.
type stopField []string

func (s *stopField) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		if single != "" {
			*s = stopField{single}
		}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(data, &multi); err != nil {
		return err
	}
	*s = stopField(multi)
	return nil
}

// responseFormatField is the subset of the OpenAI `response_format` object
// the gateway inspects: only Type matters, to detect the structured-output
// shape ("json_schema") request-shape honesty refuses on every route (see
// unsupportedRequestParam). "json_object" (plain JSON mode, no schema) is
// left alone — a backend that ignores it silently degrades to prose, which
// is the same pre-existing behaviour every other inert field gets.
type responseFormatField struct {
	Type string `json:"type"`
}

// chatCompletionRequest is the subset of the OpenAI chat/completions request
// body that the gateway needs to inspect for routing, recording, and
// request-shape honesty. Inert fields (user, n, seed, logit_bias, metadata,
// ...) are deliberately not modelled here — Go's json package silently
// ignores fields with no matching struct tag, which is exactly the
// "round-tripped transparently" behaviour those fields should get, except
// that today's providers rebuild the outbound request from ChatRequest and
// so never actually forward them; see unsupportedRequestParam for the
// fields that must NOT be silently dropped this way.
// chatBodyMaxBytes caps a chat completion body; the api's proxy and
// nginx.conf apply the same 16 MiB so every hop refuses the same request.
// 16 MiB is ~4M tokens of text, past any model's context, and small enough
// that a burst of maximal requests stays inside the gateway's memory limit.
const chatBodyMaxBytes = 16 << 20

// invalidMessages returns the 400 message for a messages array no backend
// could accept, or "" when the shape is fine. Content is left to the
// provider (a tool-result turn may legitimately be empty for some).
func invalidMessages(msgs []provider.Message) string {
	if len(msgs) == 0 {
		return "messages must be a non-empty array"
	}
	for i, m := range msgs {
		switch m.Role {
		case "system", "user", "assistant", "tool", "developer", "function":
		case "":
			return fmt.Sprintf("messages[%d]: role is required", i)
		default:
			return fmt.Sprintf("messages[%d]: unknown role %q", i, m.Role)
		}
	}
	return ""
}

type chatCompletionRequest struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	Stream      bool               `json:"stream"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	Stop        stopField          `json:"stop,omitempty"`

	// The fields below are inspected ONLY to produce an honest 400
	// unsupported_parameter (see unsupportedRequestParam) — never forwarded
	// to any provider.
	Tools          []json.RawMessage    `json:"tools,omitempty"`
	ToolChoice     json.RawMessage      `json:"tool_choice,omitempty"`
	Functions      []json.RawMessage    `json:"functions,omitempty"`
	FunctionCall   json.RawMessage      `json:"function_call,omitempty"`
	ResponseFormat *responseFormatField `json:"response_format,omitempty"`
}

// unsupportedRequestParam inspects req for fields the gateway does not
// implement on ANY backend today (tool calling, structured JSON output, and
// multi-part image/audio content) and returns the OpenAI-style parameter
// name to report in a 400 unsupported_parameter response, or "" if none of
// them are present. This runs before route resolution, for EVERY request,
// regardless of which class the resolved model belongs to — a caller must
// get an honest, immediate 400 rather than have the parameter silently
// dropped no matter which backend it lands on.
//
// tools/functions are refused only when non-empty; tool_choice/function_call
// are refused only when set to something other than "none" — an SDK that
// always includes an empty/off default must not be penalised for it. Inert
// fields (user, n, seed, logit_bias, metadata, top_p, stop, ...) are
// deliberately not inspected here.
func unsupportedRequestParam(req chatCompletionRequest) string {
	if len(req.Tools) > 0 {
		return "tools"
	}
	if isSetAndNotNone(req.ToolChoice) {
		return "tool_choice"
	}
	if len(req.Functions) > 0 {
		return "functions"
	}
	if isSetAndNotNone(req.FunctionCall) {
		return "function_call"
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_schema" {
		return "response_format"
	}
	for _, m := range req.Messages {
		if m.HasNonTextContent {
			return "messages[].content"
		}
	}
	return ""
}

// isSetAndNotNone reports whether raw is present and is neither JSON null
// nor the string "none" — the two ways a client signals "tool use is off"
// that must not trigger a 400.
func isSetAndNotNone(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "null" && trimmed != `"none"`
}

// providerFeatureUIKeyInstall is the one entry GET /v1/models' top-level
// provider_features array carries today: this gateway build implements
// POST /v1/providers/:vendor/verify and POST /v1/providers/:vendor/refresh
// (see ProvidersHandler), so the api's admin "install a vendor key from the
// UI" flow is safe to offer. The api's PUT/DELETE .../providers/:vendor/*
// routes refuse 409 gateway_too_old when a cached providers block lacks this
// feature, so a rolling deploy never lets an admin install a key against a
// gateway pod that predates the feature.
const providerFeatureUIKeyInstall = "ui_key_install"

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

// providerStatusSource supplies the current vendor provider status block for
// GET /v1/models' `providers` array (service-key + X-Darkplum-Classes:
// external_api callers only — see HandleListModels). Satisfied by
// *provider.StatusRegistry; defined as an interface so tests can substitute
// a fake without constructing a real registry. May be nil (a build with no
// registry wired in reports no providers array at all, exactly like an
// empty one).
type providerStatusSource interface {
	Snapshot() []provider.ProviderStatus
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
//  2. request-shape honesty (400 unsupported_parameter) — tools, tool_choice,
//     functions, function_call, response_format of type json_schema, and any
//     non-text content block are refused before route resolution, on EVERY
//     route, since no backend implements any of them today (see
//     unsupportedRequestParam)
//  3. policy snapshot must be loaded, else fail closed (503 policy_unavailable)
//  4. alias resolve (admin intent wins over a same-named discovered model; no
//     chains). A routed alias additionally classifies the request here (step
//     4a below) to pick which of its routes' targets to resolve to; a static
//     alias resolves straight to its target exactly as before.
//  5. route lookup on the resolved id (404 model_not_found — no default fall-through, ever)
//  6. enabled check (403 model_not_enabled)
//  7. residency check (403 model_residency_violation) — against the
//     PRINCIPAL'S effective (strictest) residency, not just the org's, using
//     policy.ResidencyPermits(eff.Residency, route.Class) so the ladder
//     (any/cloud_account_only/self_hosted_only) and every backend class
//     (self_hosted/cloud_managed/external_api) are enforced in one place
//  8. model_not_allowed check (403) — the resolved id must be in the
//     principal's effective allowlist
//  9. dispatch
//
// Steps 5-8 run unchanged regardless of how the target was resolved: a route
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

	// providerStatus supplies GET /v1/models' `providers` array. May be nil.
	providerStatus providerStatusSource

	// policyRefresh, when set (SetPolicyRefresher), reloads the policy
	// snapshot from Postgres on demand. The api calls POST /v1/policy/refresh
	// after every policy-shaped write, and the preview/impact handlers call
	// it before evaluating, so an admin's lock-out guard never judges a
	// write against a snapshot that predates the write before it.
	policyRefresh func(context.Context) error
}

// SetPolicyRefresher wires the on-demand policy reload (see policyRefresh).
func (h *ChatHandler) SetPolicyRefresher(fn func(context.Context) error) { h.policyRefresh = fn }

// refreshPolicyNow reloads the snapshot if a refresher is wired. A failed
// reload is an error for the caller: a lock-out guard evaluated on a
// snapshot that may predate the previous write is the bug this exists to
// close, so preview/impact answer 503 rather than guess.
func (h *ChatHandler) refreshPolicyNow(ctx context.Context) error {
	if h.policyRefresh == nil {
		return nil
	}
	if err := h.policyRefresh(ctx); err != nil {
		log.Printf("policy: on-demand refresh failed: %v", err)
		return err
	}
	return nil
}

// respondRefreshFailed is the 503 preview/impact return when the snapshot
// could not be brought up to date first.
func respondRefreshFailed(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, openAIError{Error: openAIErrorBody{
		Message: "policy snapshot could not be refreshed; retry",
		Type:    "server_error",
		Code:    "policy_refresh_failed",
	}})
}

// HandlePolicyRefresh handles POST /v1/policy/refresh (service key only):
// reload the policy snapshot now. 204 when reloaded, 503 when the reload
// failed (the last good snapshot stays in force), 501 when no refresher is
// wired.
func (h *ChatHandler) HandlePolicyRefresh(c *gin.Context) {
	if c.GetString(middleware.ContextKeyType) != middleware.KeyTypeService {
		c.JSON(http.StatusForbidden, openAIError{
			Error: openAIErrorBody{
				Message: "POST /v1/policy/refresh is only available to the service key",
				Type:    "permission_error",
				Code:    "service_key_required",
			},
		})
		return
	}
	if h.policyRefresh == nil {
		c.JSON(http.StatusNotImplemented, openAIError{Error: openAIErrorBody{Message: "policy refresh is not available", Type: "server_error"}})
		return
	}
	if err := h.policyRefresh(c.Request.Context()); err != nil {
		log.Printf("policy: refresh on request failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, openAIError{Error: openAIErrorBody{Message: "policy refresh failed; last snapshot kept", Type: "server_error", Code: "policy_refresh_failed"}})
		return
	}
	c.Status(http.StatusNoContent)
}

// NewChatHandler constructs a ChatHandler. providerStatus may be nil (no
// providers array is ever included in that case) — cmd/main.go always passes
// a real *provider.StatusRegistry seeded with every known vendor name.
func NewChatHandler(providers providerLookup, routes routeSnapshotter, pol policy.Store, rulesClassifier, semanticClassifier router.Classifier, providerStatus providerStatusSource) *ChatHandler {
	return &ChatHandler{
		providers:          providers,
		routes:             routes,
		policy:             pol,
		rulesClassifier:    rulesClassifier,
		semanticClassifier: semanticClassifier,
		providerStatus:     providerStatus,
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

// routedPickRefusal is the refusal for a routed alias whose picked route
// (and, where applicable, its default route) cannot serve: disabled or not in
// the route table.
type routedPickRefusal struct {
	Code    string
	Message string
}

// routedPick is the effective routing decision after the classifier's pick
// has been checked for disabled/unreachable targets.
type routedPick struct {
	RouteName string
	Reason    string
	Target    string
	Refusal   *routedPickRefusal // non-nil: refuse; Target is the picked route's target
}

// targetState reports why target cannot serve: "" if usable,
// "model_not_found" if absent from the route table, "model_not_enabled" if
// disabled. Policy (residency/allowlist) is deliberately NOT considered.
func targetState(snap policy.Snapshot, tbl *routing.Table, target string) string {
	if _, ok := tbl.Lookup(target); !ok {
		return "model_not_found"
	}
	if !snap.Enabled[target] {
		return "model_not_enabled"
	}
	return ""
}

func targetStateText(code string) string {
	if code == "model_not_found" {
		return "not available on this gateway"
	}
	return "disabled"
}

// resolveRoutedPick applies the owner rule to a classifier decision: if the
// picked route's target is disabled or unreachable and the pick is not the
// default route, serve the alias's own default route instead (when its
// target is usable). Policy refusals (residency, allowlist) are never
// fallback cases — they are evaluated afterwards on the returned target.
// Shared by HandleChatCompletions and HandleRoutingDryrun so both report the
// same decision. A pick naming an unknown route is returned untouched (the
// caller fails closed).
func resolveRoutedPick(snap policy.Snapshot, tbl *routing.Table, aliasName string, alias policy.AliasDef, d router.Decision) routedPick {
	pick := routedPick{RouteName: d.RouteName, Reason: d.Reason}
	rd, ok := alias.Route(d.RouteName)
	if !ok {
		return pick
	}
	pick.Target = rd.Target
	state := targetState(snap, tbl, rd.Target)
	if state == "" {
		return pick
	}
	if d.RouteName == alias.DefaultRoute {
		pick.Refusal = &routedPickRefusal{
			Code: state,
			Message: fmt.Sprintf("route %q of alias %q is the alias's default route and selects model %q, which is %s. Enable it on the Models page or retarget the route.",
				d.RouteName, aliasName, rd.Target, targetStateText(state)),
		}
		return pick
	}
	def, dok := alias.Route(alias.DefaultRoute)
	if !dok {
		return pick // unreachable per buildAliases; refuse via the normal path
	}
	dstate := targetState(snap, tbl, def.Target)
	if dstate != "" {
		pick.Refusal = &routedPickRefusal{
			Code: state,
			Message: fmt.Sprintf("route %q of alias %q selects model %q, which is %s, and the alias's default route %q selects %q, which is also %s. Enable one of them on the Models page or retarget the route.",
				d.RouteName, aliasName, rd.Target, targetStateText(state), alias.DefaultRoute, def.Target, targetStateText(dstate)),
		}
		return pick
	}
	kind := "route_target_disabled"
	if state == "model_not_found" {
		kind = "route_target_unreachable"
	}
	return routedPick{
		RouteName: alias.DefaultRoute,
		Reason:    "fallback:" + kind + ":" + d.RouteName,
		Target:    def.Target,
	}
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
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, chatBodyMaxBytes)).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.Set("denial_code", "invalid_request")
			c.Set("resolved_model", noModelRecorded)
			c.Header("X-Darkplum-Model", noModelRecorded)
			logDenial(c, "invalid_request", noModelRecorded, "", "")
			c.JSON(http.StatusRequestEntityTooLarge, openAIError{
				Error: openAIErrorBody{
					Message: fmt.Sprintf("request body exceeds %d bytes", chatBodyMaxBytes),
					Type:    "invalid_request_error",
					Code:    "request_too_large",
				},
			})
			return
		}
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

	// messages must be a non-empty array of turns that each name a role:
	// every backend rejects an empty or role-less conversation, and the
	// refusal belongs here, before the request is attributed to a vendor.
	if msg := invalidMessages(req.Messages); msg != "" {
		c.Set("denial_code", "invalid_request")
		c.Set("resolved_model", req.Model)
		logDenial(c, "invalid_request", req.Model, "", "")
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: msg,
				Type:    "invalid_request_error",
				Code:    "invalid_messages",
				Param:   "messages",
			},
		})
		return
	}

	// Request-shape honesty: tools/tool_choice/functions/function_call/
	// response_format(json_schema)/non-text content blocks are refused
	// before route resolution, on EVERY route — not just external_api —
	// because no backend today actually implements any of them; forwarding
	// req.Model or dispatching first would mean the caller only sometimes
	// gets told, depending on what the request happened to resolve to.
	if param := unsupportedRequestParam(req); param != "" {
		c.Set("denial_code", "unsupported_parameter")
		logDenial(c, "unsupported_parameter", req.Model, "", "")
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("parameter %q is not supported by this gateway on any backend", param),
				Type:    "invalid_request_error",
				Code:    "unsupported_parameter",
				Param:   param,
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
	var routedRefusal *routedPickRefusal

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

		pick := resolveRoutedPick(snap, h.routes.Snapshot(), req.Model, alias, decision)
		routeName = pick.RouteName
		routerReason = pick.Reason
		routedRefusal = pick.Refusal

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

	if routedRefusal != nil {
		// Both the classifier's pick and the alias's default route are
		// unusable (disabled/unreachable): nothing to fall back to.
		if rt, found := h.routes.Snapshot().Lookup(resolvedModel); found {
			c.Set("backend_class", rt.Class)
			c.Header("X-Darkplum-Backend-Class", rt.Class)
			c.Header("X-Darkplum-Provider", rt.Kind)
		}
		c.Set("denial_code", routedRefusal.Code)
		logDenial(c, routedRefusal.Code, req.Model, resolvedModel, c.GetString("backend_class"))
		status, typ := http.StatusForbidden, "permission_error"
		if routedRefusal.Code == "model_not_found" {
			status, typ = http.StatusNotFound, "invalid_request_error"
		}
		c.JSON(status, openAIError{Error: openAIErrorBody{
			Message: routedRefusal.Message,
			Type:    typ,
			Code:    routedRefusal.Code,
		}})
		return
	}

	route, ok := h.routes.Snapshot().Lookup(resolvedModel)
	if !ok {
		c.Set("denial_code", "model_not_found")
		logDenial(c, "model_not_found", req.Model, resolvedModel, "")
		respondModelNotFound(c, resolvedModel, routeCtx)
		return
	}

	// backend_class is written to the context (so a DENIED row still carries
	// it — see middleware.Recording) and to a response header the moment
	// route.Class is known, mirroring X-Darkplum-Model's placement: every
	// check from here on can refuse the request, and every one of those
	// refusals still names the class and the provider kind that was about to
	// be used.
	c.Set("backend_class", route.Class)
	c.Header("X-Darkplum-Backend-Class", route.Class)
	c.Header("X-Darkplum-Provider", route.Kind)

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

	if !policy.ResidencyPermits(eff.Residency, route.Class) {
		c.Set("denial_code", "model_residency_violation")
		logDenial(c, "model_residency_violation", req.Model, resolvedModel, route.Class)
		respondResidencyViolation(c, resolvedModel, eff.Residency, route.Class, routeCtx)
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

	// provider.Regional (implemented by BedrockProvider, and by any future
	// region-bound vendor provider) replaces a direct *bedrock.BedrockProvider
	// type assertion — this package no longer imports internal/provider/bedrock
	// at all, so adding another regional backend never requires touching the
	// handler.
	providerRegion := ""
	if rp, ok := prov.(provider.Regional); ok {
		providerRegion = rp.Region()
	}
	c.Set("provider_region", providerRegion)

	chatReq := provider.ChatRequest{
		Model:       resolvedModel,
		Messages:    req.Messages,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        []string(req.Stop),
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
			if _, _, ok := dataRetentionResponse("", err); ok {
				c.Set("denial_code", provider.DenialModelRequiresDataRetention)
			}
			_ = writeStreamError(c.Writer, err, c.GetString("route_name"))
		}
		return
	}

	if stats != nil {
		c.Set("prompt_tokens", stats.PromptTokens)
		c.Set("completion_tokens", stats.CompletionTokens)
		c.Set("cache_read_tokens", stats.CacheReadTokens)
		c.Set("cache_creation_tokens", stats.CacheCreationTokens)
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
	c.Set("cache_read_tokens", resp.CacheReadTokens)
	c.Set("cache_creation_tokens", resp.CacheCreationTokens)

	c.Data(http.StatusOK, "application/json", body)
}

// classesHeaderAllowsExternalAPI reports whether the request's
// X-Darkplum-Classes header (comma-separated; only the "external_api" token
// is defined today) names external_api. Gates external_api models out of
// GET /v1/models for EVERY caller — service or user key alike — when absent,
// not merely those a user key's own policy would already exclude: this is
// the order-independence capability handshake (see the wire contract in
// DARKPLUM_IMPLEMENTATION.md), which exists so an old, not-yet-upgraded api
// (whose SyncModels never sends the header) can never even catalogue a
// vendor model, regardless of what any principal's allowlist says.
func classesHeaderAllowsExternalAPI(c *gin.Context) bool {
	for _, tok := range strings.Split(c.GetHeader("X-Darkplum-Classes"), ",") {
		if strings.TrimSpace(tok) == routing.ClassExternalAPI {
			return true
		}
	}
	return false
}

// HandleListModels returns every model currently in the route table,
// regardless of enabled status — the api's SyncModels needs the full
// discovered set, not just what's currently enabled. Each entry reports
// owned_by (the backend kind) and backend_class (self_hosted/cloud_managed/
// external_api) straight from the route, so this handler never needs a
// provider lookup.
//
// Filtering by effective routing policy applies ONLY to a user API key
// (ContextKeyType == middleware.KeyTypeUser): the caller's own allowlist
// hides models it could never successfully call anyway. A SERVICE key's
// response is otherwise the full list — the api's SyncModels is the one
// caller of this endpoint using a service key, and it needs the complete
// discovered set (including models no principal is currently allowed to
// use) to keep its own models table in sync; filtering that away would
// silently stop syncing models the moment an admin tightened policy.
// external_api models are the one exception even for a service key: they are
// omitted unless the caller sent X-Darkplum-Classes: external_api (see
// classesHeaderAllowsExternalAPI) regardless of key type or policy.
func (h *ChatHandler) HandleListModels(c *gin.Context) {
	type modelEntry struct {
		ID           string `json:"id"`
		Object       string `json:"object"`
		Created      int64  `json:"created"`
		OwnedBy      string `json:"owned_by"`
		BackendClass string `json:"backend_class"`
		// Availability is set only for models whose provider can report it
		// (Bedrock). Not sensitive, so it rides on service-key and user-key
		// responses alike.
		Availability *provider.Availability `json:"availability,omitempty"`
	}
	// DefaultModel is the caller's effective default_model (what a request
	// that names no model will run on) — set only for a user key, where the
	// list is also filtered; nil for the service key's unfiltered view. The
	// api relays it to Bearer callers of its own /v1/models so the two lists
	// agree on both membership and default. Providers is populated only for
	// a SERVICE key that sent the classes header — see the wire contract's
	// `providers` array; omitted entirely (not even `[]`) for every other
	// caller, so a user-key/old-api response shape is unchanged.
	type modelsResponse struct {
		Object       string                    `json:"object"`
		Data         []modelEntry              `json:"data"`
		DefaultModel *string                   `json:"default_model,omitempty"`
		Providers    []provider.ProviderStatus `json:"providers,omitempty"`
		// ProviderFeatures names the vendor-key-management capabilities THIS
		// gateway build supports, alongside Providers — the api's PUT
		// .../providers/:vendor/key|filter and DELETE .../key refuse (409
		// gateway_too_old) when a cached providers block lacks
		// "ui_key_install", so an old gateway (whose Providers is nil because
		// it predates this field, or whose cached block was never refreshed)
		// never has a vendor key installed against it and then silently
		// ignored. Populated under the exact same condition as Providers
		// (service key + X-Darkplum-Classes: external_api) — never included
		// for a user key or an old api that never sends the header.
		ProviderFeatures []string `json:"provider_features,omitempty"`
	}

	now := time.Now().Unix()
	table := h.routes.Snapshot()
	ids := table.IDs() // already sorted
	allowExternalAPI := classesHeaderAllowsExternalAPI(c)

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
		if route.Class == routing.ClassExternalAPI && !allowExternalAPI {
			continue
		}
		// A user key's list is what that key can actually call: the same
		// allowlist AND residency checks HandleChatCompletions applies, so an
		// SDK that lists models never offers one it would then be refused on.
		if filterByPolicy && (!eff.Allows(id) || !policy.ResidencyPermits(eff.Residency, route.Class)) {
			continue
		}
		entry := modelEntry{
			ID:           id,
			Object:       "model",
			Created:      now,
			OwnedBy:      route.Kind,
			BackendClass: route.Class,
		}
		if prov, ok := h.providers.Get(route.ProviderKey); ok {
			if ar, ok := prov.(provider.AvailabilityReporter); ok {
				av := ar.ModelAvailability(id)
				entry.Availability = &av
			}
		}
		models = append(models, entry)
	}

	resp := modelsResponse{Object: "list", Data: models}
	if filterByPolicy {
		resp.DefaultModel = eff.DefaultModel
	}
	if allowExternalAPI && c.GetString(middleware.ContextKeyType) == middleware.KeyTypeService && h.providerStatus != nil {
		resp.Providers = h.providerStatus.Snapshot()
		resp.ProviderFeatures = []string{providerFeatureUIKeyInstall}
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
	ReachableModels []string `json:"reachable_models"`
	// LockedOut is the gateway's own verdict (see lockedOut): the api
	// shows and refuses on this rather than re-deriving it from its own
	// tables, which can disagree with the route table (an enabled model
	// with no route is "enabled" to the api and unreachable here).
	LockedOut bool                      `json:"locked_out"`
	Keys      []effectivePolicyKeyEntry `json:"keys,omitempty"`
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

	table := h.routes.Snapshot()
	c.JSON(http.StatusOK, effectivePolicyResponse{
		EffectivePolicy: eff,
		ReachableModels: reachableModels(eff, snap, table),
		LockedOut:       lockedOut(eff, snap, table),
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

// invalidOverrideResidencyMessage is the 400 body for an
// override.policy.residency value validOverrideResidency rejects.
var invalidOverrideResidencyMessage = fmt.Sprintf(
	`override.policy.residency must be null, %q, or %q`,
	policy.ResidencyCloudAccountOnly, policy.ResidencySelfHostedOnly,
)

// validOverrideResidency reports whether v is an acceptable
// override.policy.residency value for POST /v1/policy/effective and POST
// /v1/policy/impact: nil (inherit) or one of the two values a level may
// explicitly tighten to. There is no explicit "any" at this level — every
// PolicyRow's own "any" is expressed by leaving Residency nil, exactly like
// every other level (see policy.PolicyRow's doc comment); an org-level
// override wanting "any" uses policy:null (inherit/remove the row) instead.
func validOverrideResidency(v *string) bool {
	return v == nil || *v == policy.ResidencyCloudAccountOnly || *v == policy.ResidencySelfHostedOnly
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
	// A preview is the api's lock-out guard: it must see every write that
	// already happened, not a snapshot up to one interval old. Two drawer
	// saves within that interval used to pass the guard and leave a key
	// with nothing reachable.
	if err := h.refreshPolicyNow(c.Request.Context()); err != nil {
		respondRefreshFailed(c)
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
			if !validOverrideResidency(req.Override.Policy.Residency) {
				c.JSON(http.StatusBadRequest, openAIError{
					Error: openAIErrorBody{
						Message: invalidOverrideResidencyMessage,
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
	// owned by a user with one) that the override NEWLY locks out. Never null.
	LockedOut []policyImpactPrincipal `json:"locked_out"`
	// AlreadyLockedOut lists principals locked out both before and under
	// the override: not this change's doing, so never a reason to refuse
	// it, but the admin looking at blast radius must still see them.
	AlreadyLockedOut []policyImpactPrincipal `json:"already_locked_out"`
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
	// UnresolvedModels lists enabled models the proposed org policy allows
	// whose class is unknown (the route table has never seen them — a
	// backend still starting, or not yet discovered) at a time when it
	// matters: the proposed residency is self_hosted_only. They are left out
	// of the Narrowed/OrgNarrowed comparison, which states only what is
	// certain; the caller says separately that any of these that turn out
	// to be cloud-managed will also be refused. Empty under residency any.
	// Never null.
	UnresolvedModels []string `json:"unresolved_models"`
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
	if err := h.refreshPolicyNow(c.Request.Context()); err != nil {
		respondRefreshFailed(c)
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
		if !validOverrideResidency(req.Override.Policy.Residency) {
			c.JSON(http.StatusBadRequest, openAIError{
				Error: openAIErrorBody{
					Message: invalidOverrideResidencyMessage,
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
	resp := policyImpactResponse{LockedOut: []policyImpactPrincipal{}, AlreadyLockedOut: []policyImpactPrincipal{}, Narrowed: []policyImpactPrincipal{}, UnresolvedModels: []string{}}

	// The org's own chain is just its row (ChainFor's first element; the
	// user placeholder is dropped so an inheriting principal's view is
	// exactly the org's).
	orgChain := snap.ChainFor("", "")[:1]
	orgNow := policy.Effective(orgChain)
	orgAfter := policy.Effective(override.Apply(orgChain))
	if lockedOut(orgAfter, snap, table) {
		resp.OrgLockedOut = true
		resp.OrgNarrowed = len(allowedModels(orgNow, snap, table)) > 0
		return resp
	}
	// Models whose fate under the proposed policy cannot be judged are
	// reported, not counted: the comparison below is made without them.
	resp.UnresolvedModels = unresolvedModels(orgAfter, snap, table)
	unresolved := make(map[string]bool, len(resp.UnresolvedModels))
	for _, id := range resp.UnresolvedModels {
		unresolved[id] = true
	}
	resp.OrgNarrowed = losesAny(without(allowedModels(orgNow, snap, table), unresolved), allowedModels(orgAfter, snap, table))

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
		// Only a principal this change locks out is this change's fault.
		// One already locked out by its own row must not block every org
		// tightening forever -- it is reported by its own policy views.
		if lockedOut(after, snap, table) {
			if lockedOut(now, snap, table) {
				resp.AlreadyLockedOut = append(resp.AlreadyLockedOut, p)
			} else {
				resp.LockedOut = append(resp.LockedOut, p)
			}
		}
		_, ownRow := snap.UserRows[userID]
		if keyID != "" {
			_, ownRow = snap.KeyRows[keyID]
		}
		if ownRow && losesAny(without(allowedModels(now, snap, table), unresolved), allowedModels(after, snap, table)) {
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

// ---------------------------------------------------------------------------
// Classes-header note for policy preview/impact endpoints
//
// GET/POST /v1/policy/effective, POST /v1/policy/impact, and POST
// /v1/routing/dryrun deliberately do NOT check classesHeaderAllowsExternalAPI
// the way HandleListModels/GET /v1/models does: every model in the route
// table, external_api included, is always considered when computing the
// allowed/reachable/unresolved sets and dry-run refusals below, regardless of
// X-Darkplum-Classes.
//
// These four endpoints are service-key-only (enforced at the top of each
// handler) and exist purely for the api to preview or dry-run a policy
// change — an admin evaluating "what would this change do" needs the TRUE
// answer, not one artificially narrowed by a header whose entire purpose is
// keeping a caller that doesn't understand a class from ever seeing it in a
// catalogue response. Gating these on the header would make an org's
// residency/impact preview silently wrong the moment a vendor model exists
// but the caller happened not to set it — a correctness bug hiding behind a
// capability handshake that was never meant to apply to a preview surface.
// (The new api, the only caller of these four endpoints, is classes-aware by
// construction — it is the same binary whose SyncModels sends the header to
// GET /v1/models — so this is not a real-world gap, only a documented
// decision about what these endpoints are for.)
// ---------------------------------------------------------------------------

// allowedModels is the policy-side counterpart of reachableModels: the
// enabled models eff permits under its residency, WITHOUT the routability
// test. Impact counts are about what an admin's policy change takes away,
// and must not move because a backend happens to be down at save time. A
// model the route table has never seen has no known class and is EXCLUDED
// under every residency, including "any" (see policy.ResidencyPermits'
// deny-unknown-class rule) — policyImpact does not let that exclusion count
// as a loss, though: such models are reported as UnresolvedModels and taken
// out of the before/after comparison, so the counts state only what is
// certain and the caveat is said separately.
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
		route, ok := table.Lookup(id)
		if !ok {
			// No route table entry -> unknown class -> refused under every
			// residency; see unresolvedModels, which reports this case
			// separately rather than silently counting it as a loss.
			continue
		}
		if !policy.ResidencyPermits(eff.Residency, route.Class) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// unresolvedModels lists the enabled models eff allows whose class the route
// table does not know at all. This applies under EVERY residency, including
// "any" — deny-when-uncertain is not limited to self_hosted_only (see
// policy.ResidencyPermits) — so a model with no route table entry is always
// unresolved, never silently treated as reachable just because the current
// policy happens to be permissive. See policyImpactResponse.UnresolvedModels.
func unresolvedModels(eff policy.EffectivePolicy, snap policy.Snapshot, table *routing.Table) []string {
	out := []string{}
	for id, enabled := range snap.Enabled {
		if !enabled || !eff.Allows(id) {
			continue
		}
		if _, ok := table.Lookup(id); !ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// without returns ids minus the members of drop, preserving order.
func without(ids []string, drop map[string]bool) []string {
	if len(drop) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !drop[id] {
			out = append(out, id)
		}
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

// lockedOut reports whether eff leaves a principal with nothing to chat
// with: a non-nil, empty allowlist, OR no reachable model (allowed ∧ enabled
// ∧ routable ∧ residency-ok) while the gateway has at least one enabled,
// routable model at all. A policy naming only disabled models, or a
// residency that excludes every allowed model, locks a principal out
// exactly as [] does, and the preview/409 must say so. With nothing enabled
// anywhere, nobody can chat whatever the policy says, so that state is not
// blamed on the policy under evaluation. nil (unrestricted) with reachable
// models is not locked out.
func lockedOut(eff policy.EffectivePolicy, snap policy.Snapshot, table *routing.Table) bool {
	if eff.AllowedModels != nil && len(*eff.AllowedModels) == 0 {
		return true
	}
	if !anyModelRoutable(snap, table) {
		return false
	}
	return len(reachableModels(eff, snap, table)) == 0
}

// anyModelRoutable reports whether at least one enabled model has a route.
func anyModelRoutable(snap policy.Snapshot, table *routing.Table) bool {
	for id, enabled := range snap.Enabled {
		if !enabled {
			continue
		}
		if _, ok := table.Lookup(id); ok {
			return true
		}
	}
	return false
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
		LockedOut:       lockedOut(eff, snap, table),
	}
}

// reachableModels computes the models that are allowed by eff AND currently
// routable AND enabled AND residency-ok — the "right now" view GET
// /v1/policy/effective reports separately from the raw allowed_models
// allowlist (which may name models that don't exist yet, aren't enabled, or
// are excluded by the current residency policy — see policy.ResidencyPermits).
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
		if !policy.ResidencyPermits(eff.Residency, route.Class) {
			continue
		}
		reachable = append(reachable, id)
	}
	return reachable
}

// orgEffectiveResidency is the residency chat enforces for a principal with
// no row of its own: the org row's value when it sets one, else the
// snapshot's org-level value (the legacy mirror, which pgstore already folds
// into the org row on load; hand-built test snapshots set only the field).
func orgEffectiveResidency(snap policy.Snapshot) string {
	eff := policy.Effective(snap.ChainFor("", "")[:1])
	if eff.Sources.Residency == policy.SourceNone {
		return snap.Residency
	}
	return eff.Residency
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

	pick := resolveRoutedPick(snap, h.routes.Snapshot(), req.Alias, alias, decision)
	target := pick.Target

	resp := routingDryrunResponse{
		RouteName:     pick.RouteName,
		TargetModelID: target,
		Reason:        pick.Reason,
		LatencyMS:     latency,
	}
	if pick.Refusal != nil {
		resp.WouldBeRefused = &routingDryrunRefusal{Code: pick.Refusal.Code, Message: pick.Refusal.Message}
		c.JSON(http.StatusOK, resp)
		return
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
	} else if orgResidency := orgEffectiveResidency(snap); !policy.ResidencyPermits(orgResidency, rt.Class) {
		// The org row's residency, exactly as chat enforces it -- NOT
		// snap.Residency, which is read from the legacy routing_policy
		// mirror that migration 016 downgrades cloud_account_only to any,
		// so a dryrun there said "fine" for an external route the real
		// request would refuse.
		resp.WouldBeRefused = &routingDryrunRefusal{
			Code:    "model_residency_violation",
			Message: residencyViolationMessage(target, rt.Class, orgResidency),
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

// residencyClassLabel renders a backend class for denial text in the form an
// admin reading an error/audit line would recognise, rather than the raw
// wire value.
func residencyClassLabel(class string) string {
	switch class {
	case routing.ClassSelfHosted:
		return "self-hosted"
	case routing.ClassCloudManaged:
		return "cloud-managed"
	case routing.ClassExternalAPI:
		return "an external API"
	default:
		return "of an unrecognised class"
	}
}

// residencyLabel renders a residency policy value for denial text.
func residencyLabel(residency string) string {
	switch residency {
	case policy.ResidencyAny:
		return "any"
	case policy.ResidencyCloudAccountOnly:
		return "cloud account only"
	case policy.ResidencySelfHostedOnly:
		return "self-hosted only"
	default:
		return "self-hosted only" // unrecognised value, same fallback residencyRankOf uses
	}
}

// residencyViolationMessage names the ACTUAL class of model and the ACTUAL
// residency policy that refused it, rather than a fixed "cloud-managed... self-
// hosted only" string — the wire contract requires the denial text to be
// accurate for external_api and cloud_account_only, not just the original
// cloud_managed/self_hosted_only pair. It also names plainly that the
// gateway does not substitute another route across the account boundary,
// per the v3.1 ruling that a vendor-provider refusal must say so.
func residencyViolationMessage(model, class, residency string) string {
	msg := fmt.Sprintf("model %q is %s, which the current residency policy (%s) does not permit",
		model, residencyClassLabel(class), residencyLabel(residency))
	if class == routing.ClassExternalAPI {
		msg += "; darkplum does not substitute another model across the account boundary"
	}
	return msg
}

// respondResidencyViolation writes the 403 for a route refused by the
// calling principal's residency policy. See respondModelNotFound for rc.
// residency/class are the ACTUAL values involved (eff.Residency and
// route.Class), so the message always names what actually happened rather
// than a hardcoded cloud_managed/self_hosted_only pair. Uses the SAME
// message text as respondModelNotEnabled for a routed pick — "not
// enabled/allowed by residency" covers both refusal reasons in one phrasing,
// exactly as specified for the routed-alias forbidden-pick error.
func respondResidencyViolation(c *gin.Context, model, residency, class string, rc *routeContext) {
	msg := residencyViolationMessage(model, class, residency)
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
// policy_unavailable, invalid_request, unsupported_parameter,
// request_canceled. requestedModel is always the raw value
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

// genericUpstreamError classifies a plain (non-provider.APIError) error into
// one of the wire contract's two catch-all codes: upstream_timeout (504) for
// anything that looks like a network-level timeout, upstream_error (502)
// otherwise. context.DeadlineExceeded satisfies net.Error (Timeout() ==
// true), so it is covered here without a separate check — see
// internal/routing's classifyError, which applies the same rule. This is the
// only classification a caller error not implementing provider.APIError can
// get: unlike a structured APIError, there is no reliable way to tell a rate
// limit or an auth failure from a plain wrapped error without parsing
// upstream-specific text, which would defeat the entire point of never
// echoing that text to the client.
func genericUpstreamError(err error) (status int, code string) {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout, "upstream_timeout"
	}
	return http.StatusBadGateway, "upstream_error"
}

// genericUpstreamMessage returns a fixed, scrubbed message for a generic
// upstream failure — never the upstream's own error text, which can carry
// internal hostnames or vendor request/account detail.
func genericUpstreamMessage(code string) string {
	if code == "upstream_timeout" {
		return "the upstream model backend timed out"
	}
	return "the upstream model backend returned an error"
}

// dataRetentionResponse renders the 422 model_requires_data_retention response
// when err is a provider.DataRetentionRefusal. It is a refusal, not an upstream
// failure: the caller records denial_code so Activity can filter on it.
func dataRetentionResponse(route string, err error) (int, []byte, bool) {
	var dr provider.DataRetentionRefusal
	if !errors.As(err, &dr) {
		return 0, nil, false
	}
	model, mode, required := dr.DataRetentionRefusal()
	return http.StatusUnprocessableEntity, provider.DataRetentionErrorBody(model, route, mode, required), true
}

// handleProviderError maps a provider error to an HTTP response. A
// provider.APIError (BedrockAPIError today; any future vendor provider's own
// error type) carries a pre-translated status, body, and retry-after value —
// written through verbatim, with Retry-After passed on for a 429. Every
// other error is classified generically (see genericUpstreamError) and
// answered with a scrubbed, closed-vocabulary message: err.Error() is NEVER
// echoed to the client on this path, since an upstream failure (e.g. an
// openaicompat dial error, which embeds the upstream URL) can carry internal
// detail that must not leave the gateway.
func handleProviderError(c *gin.Context, err error) {
	if status, body, ok := dataRetentionResponse(c.GetString("route_name"), err); ok {
		c.Set("denial_code", provider.DenialModelRequiresDataRetention)
		c.Data(status, "application/json", body)
		return
	}
	var apiErr provider.APIError
	if errors.As(err, &apiErr) {
		if ra := apiErr.RetryAfterSeconds(); ra > 0 {
			c.Header("Retry-After", strconv.Itoa(ra))
		}
		c.Data(apiErr.HTTPStatus(), "application/json", apiErr.ResponseBody())
		return
	}

	status, code := genericUpstreamError(err)
	c.JSON(status, openAIError{
		Error: openAIErrorBody{
			Message: genericUpstreamMessage(code),
			Type:    "upstream_error",
			Code:    code,
		},
	})
}

// writeStreamError attempts to write an SSE error event to w. This is
// best-effort: if headers have already been sent and partial SSE written, we
// cannot guarantee the client will parse this correctly, but some clients do
// handle "event: error" frames. Like handleProviderError, this never echoes
// err.Error() — a provider.APIError's already-scrubbed ResponseBody is
// embedded directly in the SSE frame; any other error gets the same generic,
// closed-vocabulary message handleProviderError's JSON path would have used.
// There is no way to relay Retry-After on a committed SSE stream, so a
// mid-stream 429 carries no header equivalent here.
func writeStreamError(w io.Writer, err error, route string) error {
	var apiErr provider.APIError
	var payload []byte
	if _, body, ok := dataRetentionResponse(route, err); ok {
		payload = body
	} else if errors.As(err, &apiErr) {
		payload = apiErr.ResponseBody()
	} else {
		_, code := genericUpstreamError(err)
		payload, _ = json.Marshal(openAIError{
			Error: openAIErrorBody{
				Message: genericUpstreamMessage(code),
				Type:    "upstream_error",
				Code:    code,
			},
		})
	}
	_, writeErr := fmt.Fprintf(w, "data: %s\n\n", payload)
	return writeErr
}
