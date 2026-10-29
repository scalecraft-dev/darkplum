package handler

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed system-prompt.md
var systemPrompt string

// chatBodyMaxBytes caps a chat completion request body. 16 MiB (~4M tokens
// of text) holds any prompt a 1M-token model accepts; nginx.conf's
// client_max_body_size and the gateway's cap match it.
const chatBodyMaxBytes = 16 << 20

// chatProxyError is the OpenAI error envelope for refusals this proxy itself
// produces, so an SDK reads error.message here exactly as it does for a
// gateway refusal.
func chatProxyError(message, typ, code string) gin.H {
	e := gin.H{"message": message, "type": typ}
	if code != "" {
		e["code"] = code
	}
	return gin.H{"error": e}
}

type ChatHandler struct {
	gatewayURL string
	serviceKey string
	client     *http.Client
	// db reads the admin-set system prompt (org_settings) and resolves
	// library attachments; nil means the built-in text is always used and
	// attachments are refused (tests).
	db *pgxpool.Pool
	// tools runs library tool items (MCP servers); nil refuses tool
	// attachments.
	tools *toolService
}

// WithTools wires the MCP side in; without it a turn with a tool item
// attached is refused library_unavailable.
func (h *ChatHandler) WithTools(t *toolService) *ChatHandler {
	h.tools = t
	return h
}

func NewChatHandler(gatewayURL, serviceKey string, db *pgxpool.Pool) *ChatHandler {
	return &ChatHandler{
		gatewayURL: gatewayURL,
		serviceKey: serviceKey,
		db:         db,
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
	}
}

// chatEnvelope is the little of a chat request this proxy reads. Everything
// else in the body -- max_tokens, temperature, top_p, stop, and fields the
// gateway refuses by name such as tools -- is carried through untouched: the
// gateway is the one place that decides what a request may contain, and it can
// only do that for fields it actually receives.
type chatEnvelope struct {
	Messages []json.RawMessage `json:"messages"`
	Stream   bool              `json:"stream,omitempty"`
}

// withSystemPrompt returns body with the platform system prompt prepended to
// messages, unless the caller already leads with a system message. Only the
// "messages" member is rewritten; every other member, and every member of each
// message, is preserved byte for byte.
func withSystemPrompt(body []byte) (modified []byte, stream bool, err error) {
	return withSystemPromptText(body, resolvedSystemPrompt(systemPrompt))
}

// withSystemPromptText is withSystemPrompt with the prompt text supplied:
// the admin-set one when there is one, else the built-in default.
func withSystemPromptText(body []byte, prompt string) (modified []byte, stream bool, err error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false, err
	}
	if fields == nil {
		// A literal `null` body unmarshals into a nil map without error;
		// assigning "messages" into it would panic.
		return nil, false, fmt.Errorf("request body must be a JSON object")
	}
	var env chatEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false, err
	}

	if len(env.Messages) == 0 {
		// The platform system prompt is prepended below, so an empty
		// client array would reach the gateway as a one-message
		// "conversation" with no user turn. OpenAI refuses this; so do we.
		return nil, false, fmt.Errorf("messages must be a non-empty array")
	}
	hasSystemMsg := false
	if len(env.Messages) > 0 {
		var first struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(env.Messages[0], &first); err != nil {
			return nil, false, err
		}
		hasSystemMsg = first.Role == "system"
	}
	if !hasSystemMsg {
		system, err := json.Marshal(map[string]string{"role": "system", "content": prompt})
		if err != nil {
			return nil, false, err
		}
		env.Messages = append([]json.RawMessage{system}, env.Messages...)
	}

	messages, err := json.Marshal(env.Messages)
	if err != nil {
		return nil, false, err
	}
	fields["messages"] = messages
	modified, err = json.Marshal(fields)
	return modified, env.Stream, err
}

// resolvedSystemPrompt fills the {{current_date}} placeholder in text.
func resolvedSystemPrompt(text string) string {
	now := time.Now().Format("Monday, January 2, 2006")
	return strings.ReplaceAll(text, "{{current_date}}", now)
}

// effectiveSystemPrompt is the text prepended to this request: the
// organisation's custom prompt when an admin set one, else the built-in.
func (h *ChatHandler) effectiveSystemPrompt(ctx context.Context) string {
	if custom, ok := systemPromptCache.get(ctx, h.db); ok {
		return resolvedSystemPrompt(custom)
	}
	return resolvedSystemPrompt(systemPrompt)
}

// hasBearerAuth reports whether an Authorization header is a Bearer
// credential (case-sensitive "Bearer ", as everywhere in this service).
func hasBearerAuth(authorization string) bool {
	return strings.HasPrefix(authorization, "Bearer ")
}

// peekStream reads only the "stream" flag, decoding exactly as the gateway
// does; any error means false.
func peekStream(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return false
	}
	return req.Stream
}

// ProxyChat handles POST /v1/chat/completions.
//
// Session (webchat) requests get the organisation system prompt prepended and
// the service API key, then are proxied to the gateway, streaming the
// response back to the browser. The org prompt is session-only: Bearer
// (API-key) requests are forwarded to the gateway byte for byte, with the
// caller's own key, and the gateway alone validates and refuses them.
//
// Clears this response's write deadline (http.ResponseController,
// SetWriteDeadline(zero) -- "no deadline") before writing anything: the
// server's WriteTimeout (cmd/main.go, 120s) is a fine default for every other
// endpoint, but a frontier model's completion can legitimately take longer
// than that to fully stream, and Go's net/http applies WriteTimeout to the
// entire response, not per-write -- so without this, a slow-but-healthy
// generation gets its connection cut mid-stream at the 120s mark. This is
// scoped to this one handler/route; every other endpoint keeps the server's
// WriteTimeout. A ResponseWriter that doesn't support SetWriteDeadline (e.g.
// httptest.NewRecorder in a unit test, never a real net/http server) returns
// http.ErrNotSupported here, which is not a proxy failure and is only logged
// at debug level.
func (h *ChatHandler) ProxyChat(c *gin.Context) {
	if err := http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{}); err != nil {
		slog.DebugContext(c.Request.Context(), "chat proxy: clear write deadline unsupported by response writer", "err", err)
	}

	// Bounded read: a prompt for a 1M-context model is a few megabytes, not
	// hundreds, and without a cap any key holder could make this process
	// buffer whatever it was sent.
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, chatBodyMaxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, chatProxyError(fmt.Sprintf("request body exceeds %d bytes", chatBodyMaxBytes), "invalid_request_error", "request_too_large"))
			return
		}
		slog.ErrorContext(c.Request.Context(), "read request body", "err", err)
		c.JSON(http.StatusBadRequest, chatProxyError("failed to read request body", "invalid_request_error", "invalid_body"))
		return
	}

	// Library attachments (chat_library.go). Resolved before the body is
	// rewritten so a refusal costs nothing else.
	isBearer := hasBearerAuth(c.GetHeader("Authorization"))
	itemIDs, refusal := parseLibraryHeader(c.GetHeader(libraryItemsHeader))
	if refusal != nil {
		refusal.write(c)
		return
	}
	var att *attachments
	if len(itemIDs) > 0 {
		if isBearer || c.GetString("user_id") == "" {
			(&libraryRefusal{http.StatusBadRequest, "library_requires_session", "library items can only be attached from a signed-in browser session, not with an API key", nil}).write(c)
			return
		}
		resolveStart := time.Now()
		att, refusal = resolveAttachments(c.Request.Context(), h.db, c.GetString("user_id"), itemIDs)
		if refusal != nil {
			slog.WarnContext(c.Request.Context(), "library attachments refused", "request_id", c.GetString("request_id"), "user_id", c.GetString("user_id"), "code", refusal.code, "items", itemIDs)
			refusal.write(c)
			return
		}
		if len(att.tools) > 0 && h.tools == nil {
			(&libraryRefusal{http.StatusServiceUnavailable, "library_unavailable", "tools are not available on this deployment", nil}).write(c)
			return
		}
		slog.InfoContext(c.Request.Context(), "library attachments", "request_id", c.GetString("request_id"), "user_id", c.GetString("user_id"), "items", itemIDs, "text_items", len(att.text), "tool_servers", len(att.tools), "resolve_ms", time.Since(resolveStart).Milliseconds())
	}

	var modified []byte
	var stream bool
	if isBearer {
		// API-key traffic is the caller's own conversation: the org system
		// prompt is a webchat setting, so the body goes to the gateway byte
		// for byte (no re-marshal, no prompt, no systemPromptCache read).
		// The gateway validates it and owns every refusal.
		modified = body
		stream = peekStream(body)
	} else {
		// Session path. This re-marshals the body (key order, escaping and
		// number formatting are not preserved) and prepends the org prompt
		// unless the first message has role "system"; a leading "developer"
		// message still gets the prompt (unchanged behaviour).
		var err error
		modified, stream, err = withSystemPromptText(body, h.effectiveSystemPrompt(c.Request.Context()))
		if err != nil {
			slog.WarnContext(c.Request.Context(), "parse request body", "err", err)
			c.JSON(http.StatusBadRequest, chatProxyError("invalid request body: expected a JSON object with a messages array", "invalid_request_error", "invalid_body"))
			return
		}
	}
	if att != nil {
		var err error
		modified, err = withLibraryMessage(modified, composeLibraryText(att.text))
		if err != nil {
			c.JSON(http.StatusBadRequest, chatProxyError("invalid request body", "invalid_request_error", "invalid_body"))
			return
		}
		c.Header(libraryUsedHeader, att.usedHeaderValue())
		if len(att.tools) > 0 {
			defs, bindings, refusal := buildWireTools(att.tools)
			if refusal != nil {
				refusal.write(c)
				return
			}
			c.Header(libraryToolsHeader, fmt.Sprint(len(defs)))
			h.proxyWithTools(c, modified, stream, defs, bindings)
			return
		}
	}

	target := fmt.Sprintf("%s/v1/chat/completions", h.gatewayURL)

	proxyReq, err := http.NewRequestWithContext(
		c.Request.Context(),
		http.MethodPost,
		target,
		bytes.NewReader(modified),
	)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "build proxy request", "err", err)
		c.JSON(http.StatusInternalServerError, chatProxyError("proxy request build failed", "server_error", ""))
		return
	}

	h.setProxyHeaders(c, proxyReq)

	resp, err := h.client.Do(proxyReq)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "gateway request failed", "err", err)
		c.JSON(http.StatusBadGateway, chatProxyError("gateway unavailable", "server_error", "gateway_unavailable"))
		return
	}
	defer resp.Body.Close()

	// Chat usage is recorded by the gateway into inference_events (the API-usage
	// log) with the resolved user id. We deliberately do not write a separate
	// activity_events row here — activity_events is the admin/auth audit log, and
	// recording chats there would pollute the Admin Events view with API traffic.

	c.Status(resp.StatusCode)
	copyProxyHeaders(c, resp)

	if flusher, ok := c.Writer.(http.Flusher); ok && (stream || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")) {
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
					slog.WarnContext(c.Request.Context(), "stream write interrupted", "err", writeErr)
					break
				}
				flusher.Flush()
			}
			if readErr != nil {
				if readErr != io.EOF {
					slog.WarnContext(c.Request.Context(), "stream read interrupted", "err", readErr)
				}
				break
			}
		}
	} else {
		if _, err := io.Copy(c.Writer, resp.Body); err != nil {
			slog.WarnContext(c.Request.Context(), "response copy interrupted", "err", err)
		}
	}
}

// setProxyHeaders sets the headers every hop to the gateway carries: the
// caller's own Bearer key when it sent one, else the service key; the
// request id; the person's address and id for the audit row. Nothing else
// from the inbound request is forwarded, so no inbound X-Darkplum-* header
// can reach the gateway.
func (h *ChatHandler) setProxyHeaders(c *gin.Context, proxyReq *http.Request) {
	proxyReq.Header.Set("Content-Type", "application/json")
	if clientAuth := c.GetHeader("Authorization"); hasBearerAuth(clientAuth) {
		proxyReq.Header.Set("Authorization", clientAuth)
	} else {
		proxyReq.Header.Set("Authorization", "Bearer "+h.serviceKey)
	}
	proxyReq.Header.Set("X-Request-Id", c.GetString("request_id"))
	// The gateway records the inference audit row; give it the person's
	// address, not this pod's (it trusts in-cluster hops only).
	proxyReq.Header.Set("X-Forwarded-For", c.ClientIP())
	proxyReq.Header.Set("X-Darkplum-User-Id", c.GetString("user_id"))
}

// proxiedResponseHeaders are the gateway response headers passed through to
// the browser.
var proxiedResponseHeaders = []string{
	"Content-Type",
	"X-Request-Id",
	"Cache-Control",
	// Routing headers (routed aliases only, but harmless to check
	// unconditionally): the resolved model, the route the classifier
	// picked, and its one-line reason. Set by the gateway before its
	// first response byte, for both stream and non-stream requests (see
	// DESIGN3's Semantics section) -- passed through here unmodified so
	// the webchat can show "answered by <model> (route <name>)" and
	// implement pinned_model stickiness without a second round trip.
	"X-Darkplum-Model",
	"X-Darkplum-Route",
	"X-Darkplum-Router-Reason",
	// Backend class (self_hosted|cloud_managed|external_api) and provider
	// kind (vllm|lmstudio|bedrock|anthropic|openai|mock) the request
	// actually resolved to -- set by the gateway before its first response
	// byte, same as the routing headers above, so the webchat can render
	// residency provenance ("sent to Anthropic — leaves your cloud
	// account") without a second round trip.
	"X-Darkplum-Backend-Class",
	"X-Darkplum-Provider",
	// A vendor's rate limit reaches the caller as 429 with the vendor's own
	// Retry-After, so an SDK's backoff engages instead of hammering.
	"Retry-After",
}

// copyProxyHeaders copies proxiedResponseHeaders from the gateway response.
func copyProxyHeaders(c *gin.Context, resp *http.Response) {
	for _, header := range proxiedResponseHeaders {
		if v := resp.Header.Get(header); v != "" {
			c.Header(header, v)
		}
	}
}
