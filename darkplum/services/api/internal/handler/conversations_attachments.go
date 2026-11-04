package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// GetAttachment handles GET /api/v1/conversations/:id/attachments/:aid and
// returns the stored bytes. Owner-scoped in the SQL: the attachment must sit
// under a message of conversation :id, and that conversation must belong to
// the caller. A foreign or unknown id is 404, never 403.
//
// The bytes are user-supplied, so the response is locked down: nosniff, a
// sandbox CSP, no shared caching, and only the four raster image types are
// rendered inline; everything else downloads. Nothing about the attachment
// (name, bytes) is logged.
func (h *ConversationHandler) GetAttachment(c *gin.Context) {
	convID, attID := c.Param("id"), c.Param("aid")
	if !isValidUUID(convID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return
	}
	if !isValidUUID(attID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid attachment id"})
		return
	}
	ctx := c.Request.Context()
	var mediaType, name string
	var data []byte
	err := h.db.QueryRow(ctx, `
		SELECT a.media_type, a.name, a.data
		FROM conversation_attachments a
		JOIN conversation_messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		WHERE a.id = $1 AND c.id = $2 AND c.user_id = $3
	`, attID, convID, c.GetString("user_id")).Scan(&mediaType, &name, &data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
			return
		}
		slog.ErrorContext(ctx, "get attachment failed", "conversation", convID, "attachment", attID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get attachment failed"})
		return
	}

	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "sandbox")
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Disposition", attachmentDisposition(mediaType, name))
	c.Header("Content-Length", strconv.Itoa(len(data)))
	c.Data(http.StatusOK, mediaType, data)
}
