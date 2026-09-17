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

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
}

func resolvedSystemPrompt() string {
	now := time.Now().Format("Monday, January 2, 2006")
	return strings.ReplaceAll(systemPrompt, "{{current_date}}", now)
}

// ProxyChat handles POST /v1/chat/completions.
//
// Injects the system prompt and service API key, then proxies to the gateway,
// streaming the response back to the browser.
func (h *ChatHandler) ProxyChat(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "read request body", "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.ErrorContext(c.Request.Context(), "parse request body", "err", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	hasSystemMsg := len(req.Messages) > 0 && req.Messages[0].Role == "system"
	if !hasSystemMsg {
		systemContent, _ := json.Marshal(resolvedSystemPrompt())
		req.Messages = append([]chatMessage{{
			Role:    "system",
			Content: systemContent,
		}}, req.Messages...)
	}

	modified, err := json.Marshal(req)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "marshal modified request", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
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
	} {
		if v := resp.Header.Get(header); v != "" {
			c.Header(header, v)
		}
	}

	if flusher, ok := c.Writer.(http.Flusher); ok && req.Stream {
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
