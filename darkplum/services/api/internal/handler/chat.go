package handler

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed system-prompt.md
var systemPrompt string

type ChatHandler struct {
	gatewayURL string
	serviceKey string
	client     *http.Client
}

func NewChatHandler(gatewayURL, serviceKey string) *ChatHandler {
	return &ChatHandler{
		gatewayURL: gatewayURL,
		serviceKey: serviceKey,
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false, err
	}
	var env chatEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false, err
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
		system, err := json.Marshal(map[string]string{"role": "system", "content": resolvedSystemPrompt()})
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

func resolvedSystemPrompt() string {
	now := time.Now().Format("Monday, January 2, 2006")
	return strings.ReplaceAll(systemPrompt, "{{current_date}}", now)
}

// ProxyChat handles POST /v1/chat/completions.
//
// Injects the system prompt and service API key, then proxies to the gateway,
// streaming the response back to the browser.
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

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "read request body", "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	modified, stream, err := withSystemPrompt(body)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "parse request body", "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "proxy request build failed"})
		return
	}

	proxyReq.Header.Set("Content-Type", "application/json")
	if clientAuth := c.GetHeader("Authorization"); strings.HasPrefix(clientAuth, "Bearer ") {
		proxyReq.Header.Set("Authorization", clientAuth)
	} else {
		proxyReq.Header.Set("Authorization", "Bearer "+h.serviceKey)
	}
	proxyReq.Header.Set("X-Request-Id", c.GetString("request_id"))
	proxyReq.Header.Set("X-Darkplum-User-Id", c.GetString("user_id"))

	resp, err := h.client.Do(proxyReq)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "gateway request failed", "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway unavailable"})
		return
	}
	defer resp.Body.Close()

	// Chat usage is recorded by the gateway into inference_events (the API-usage
	// log) with the resolved user id. We deliberately do not write a separate
	// activity_events row here — activity_events is the admin/auth audit log, and
	// recording chats there would pollute the Admin Events view with API traffic.

	c.Status(resp.StatusCode)
	for _, header := range []string{
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
	} {
		if v := resp.Header.Get(header); v != "" {
			c.Header(header, v)
		}
	}

	if flusher, ok := c.Writer.(http.Flusher); ok && stream {
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
