package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/recorder"
)

// statusWriter wraps gin.ResponseWriter to capture the HTTP status code after
// the handler chain completes. gin.ResponseWriter.Status() returns the written
// status, but we need it after the proxy handler sets it.
type statusWriter struct {
	gin.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) WriteHeaderNow() {
	if sw.status == 0 {
		sw.status = sw.ResponseWriter.Status()
	}
	sw.ResponseWriter.WriteHeaderNow()
}

// chatBodyFields holds the fields we parse from the request body.
type chatBodyFields struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// Recording returns a Gin middleware that captures inference metrics and sends
// them to rec after the downstream handler (the proxy) returns.
//
// X-Darkplum-User-Id is honoured ONLY when the authenticated caller used a
// service key (KeyTypeService) — the api service proxying a session
// user's request, which cannot authenticate any other way since it holds
// one shared service credential for all its users. A caller authenticated
// with a *user* API key always has its own key-level user_id attributed,
// regardless of what the header says: trusting the header unconditionally
// would let any holder of a user API key attribute their own traffic to a
// different user simply by setting the header on their request, which a
// NetworkPolicy restricting the header's *origin* does nothing to prevent
// once the caller is inside the cluster boundary the policy protects.
// See resolveAttributedUserID.
func Recording(rec *recorder.Recorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		start := time.Now()

		// Buffer the request body so we can parse it for model/stream, then
		// restore it for the proxy handler which will read it again.
		var fields chatBodyFields
		if c.Request.Body != nil {
			bodyBytes, err := io.ReadAll(c.Request.Body)
			if err == nil {
				// Best-effort parse — if it fails, fields stays zero-valued.
				_ = json.Unmarshal(bodyBytes, &fields)
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}

		// Wrap the response writer so we can read the status code afterwards.
		sw := &statusWriter{ResponseWriter: c.Writer, status: 200}
		c.Writer = sw

		c.Next()

		latencyMS := int(time.Since(start).Milliseconds())
		statusCode := sw.status
		if statusCode == 0 {
			statusCode = c.Writer.Status()
		}

		// Resolve user ID via the pure decision in resolveAttributedUserID:
		// X-Darkplum-User-Id is only trusted for service-key callers.
		userID := resolveAttributedUserID(
			c.GetString(ContextKeyType),
			c.GetString(ContextUserID),
			c.GetHeader("X-Darkplum-User-Id"),
		)

		requestID := c.GetHeader("X-Request-Id")
		keyID := c.GetString(ContextKeyID)

		// The routing handler sets provider metadata in the Gin context after it
		// has decided which backend to use. resolved_model may differ from the
		// request body model when the handler rewrites it (e.g. Bedrock requires
		// the full model ARN/ID while the client sends a short alias).
		providerName := c.GetString("provider_name")
		providerRegion := c.GetString("provider_region")

		model := fields.Model
		if resolved := c.GetString("resolved_model"); resolved != "" {
			model = resolved
		}

		// requested_model is only set by the handler when an alias was used
		// (ChatHandler.HandleChatCompletions sets it to "" otherwise), so an
		// empty string here correctly becomes NULL via recorder.nullableStr.
		requestedModel := c.GetString("requested_model")

		// Token counts are available when the backend returns them in a
		// non-streaming response (Bedrock always does; vLLM streaming does not).
		// Leave nil when not set so the DB column stays NULL rather than zero.
		var promptTokens, completionTokens *int
		if pt := c.GetInt("prompt_tokens"); pt != 0 {
			promptTokens = &pt
		}
		if ct := c.GetInt("completion_tokens"); ct != 0 {
			completionTokens = &ct
		}

		rec.Record(recorder.InferenceEvent{
			RequestID:        requestID,
			APIKeyID:         keyID,
			UserID:           userID,
			Model:            model,
			RequestedModel:   requestedModel,
			DenialCode:       c.GetString("denial_code"),
			StatusCode:       statusCode,
			LatencyMS:        latencyMS,
			Stream:           fields.Stream,
			IP:               c.ClientIP(),
			Provider:         providerName,
			Region:           providerRegion,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
		})
	}
}

// resolveAttributedUserID decides which user_id an inference event is
// attributed to. Pulled out as a pure function (no gin.Context, no DB) so the
// audit-spoofing rule can be unit-tested directly: headerUserID
// (X-Darkplum-User-Id) is honoured only when keyType is KeyTypeService; a
// user-key caller (keyType == KeyTypeUser, or any other/unknown value) always
// gets its own key-level keyUserID attributed, even if headerUserID is set.
func resolveAttributedUserID(keyType, keyUserID, headerUserID string) string {
	if keyType == KeyTypeService && headerUserID != "" {
		return headerUserID
	}
	return keyUserID
}
