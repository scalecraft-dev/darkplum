// Server-side conversation history for the webchat (see migration
// 012_conversations.sql and DESIGN2.md "Why").
//
// PRIVACY: a conversation is readable ONLY by its owner. Every query below
// filters by user_id in the SQL itself, not only in the Go handler -- an id
// that exists but belongs to a different user 404s exactly like an id that
// doesn't exist at all, never 403 (which would confirm the id exists).
// There is intentionally NO admin endpoint anywhere in this codebase that
// lists or reads another user's conversations or their content: compare
// cmd/main.go's adminOnly group (users, api-keys, models, activity, usage)
// against the conversation routes, which sit in the plain session-authenticated
// group. The only trace a conversation's deletion leaves in the admin-visible
// activity_events log is a "conversation.deleted" event carrying a message
// count -- never a title, never message content.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

// ConversationHandler serves the session-authenticated conversation-history
// endpoints.
type ConversationHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
}

// NewConversationHandler returns a ConversationHandler backed by db.
func NewConversationHandler(db *pgxpool.Pool, rec *activity.Recorder) *ConversationHandler {
	return &ConversationHandler{db: db, activity: rec}
}

// rowQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, so
// resolveAssistantProvenance can run either as a standalone lookup or inside
// an already-open transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// messageInput is the wire shape for one message in a create/append request
// body. There is deliberately no "model" field: a client-supplied model on a
// message is never trusted (see resolveAssistantProvenance) and is silently
// ignored if sent -- encoding/json drops unknown fields on decode, so no
// explicit handling is needed here.
type messageInput struct {
	Role      string  `json:"role"`
	Content   string  `json:"content"`
	RequestID *string `json:"request_id,omitempty"`
	// ToolEvents is the tool calls an assistant turn made and their
	// results, as the chat proxy reported them to the browser (see
	// chat_library.go toolEvent). Stored as given, bounded in size; it is
	// the user's own conversation content. Ignored on user turns.
	ToolEvents json.RawMessage `json:"tool_events,omitempty"`
}

// conversationSummary is the wire shape returned by list/patch and embedded
// in conversationFull. PinnedModel is always present (never omitted) so a
// client can distinguish "no pin" (null) from a field the server forgot to
// send -- see patchConversationRequest for how it is set/cleared.
type conversationSummary struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Model        string    `json:"model"`
	PinnedModel  *string   `json:"pinned_model"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// LibraryItemIDs are the library items attached to this conversation
	// (the chip row), restored on reopen. Always an array, never null.
	LibraryItemIDs []string `json:"library_item_ids"`
}

// conversationMessage is the wire shape for one persisted message.
// Model and RequestID are provenance fields the api derived server-side
// (see resolveAssistantProvenance) -- never an echo of client input.
type conversationMessage struct {
	Seq       int       `json:"seq"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Model     *string   `json:"model"`
	RequestID *string   `json:"request_id"`
	CreatedAt time.Time `json:"created_at"`
	// ToolEvents is null on turns that made no tool calls.
	ToolEvents json.RawMessage `json:"tool_events"`
}

// conversationFull is the wire shape for get/create: summary fields plus the
// full message list, ordered by seq.
type conversationFull struct {
	conversationSummary
	Messages []conversationMessage `json:"messages"`
}

// resolveAssistantProvenance looks up the verified model that produced an
// assistant turn from inference_events, scoped to the requesting user so a
// forged request_id -- or another user's real one -- can never attribute
// output to a model it didn't come from. Returns (nil, nil) if requestID is
// nil/empty or no inference_events row matches (request_id, user_id); the
// caller stores NULL for both conversation_messages columns in that case,
// never falling back to anything client-supplied.
func resolveAssistantProvenance(ctx context.Context, q rowQuerier, userID string, requestID *string) (model, resolvedRequestID *string, err error) {
	if requestID == nil || *requestID == "" {
		return nil, nil, nil
	}
	var m, r string
	err = q.QueryRow(ctx, `
		SELECT model, request_id FROM inference_events
		WHERE request_id = $1 AND user_id = $2 AND purpose = 'inference'
		ORDER BY created_at DESC
		LIMIT 1
	`, *requestID, userID).Scan(&m, &r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The gateway's recorder writes inference_events in batches (about
			// once a second), and the webchat appends the reply the moment the
			// stream ends, so the audit row is often not there yet. Keep the
			// request id as a correlation key — it is only ever matched against
			// THIS user's audit rows — with model NULL; loadConversationFull
			// resolves it on the next read (see backfillProvenance).
			return nil, requestID, nil
		}
		return nil, nil, err
	}
	return &m, &r, nil
}

// backfillProvenance resolves assistant turns whose request_id was stored
// before their audit row existed. Same rule as at append time: the model comes
// only from an inference_events row with a matching request_id AND this
// user's id; anything else stays NULL. Rows that resolve are updated in place
// so the work is done once.
func (h *ConversationHandler) backfillProvenance(ctx context.Context, userID, convID string, msgs []conversationMessage) {
	for i := range msgs {
		m := &msgs[i]
		if m.Role != roleAssistant || m.Model != nil || m.RequestID == nil {
			continue
		}
		var model string
		err := h.db.QueryRow(ctx, `
			SELECT model FROM inference_events
			WHERE request_id = $1 AND user_id = $2 AND purpose = 'inference'
			ORDER BY created_at DESC
			LIMIT 1
		`, *m.RequestID, userID).Scan(&model)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.WarnContext(ctx, "conversation provenance backfill lookup failed", "conversation", convID, "seq", m.Seq, "err", err)
			}
			continue
		}
		// request_id is part of the predicate: between the SELECT above and this
		// UPDATE a regenerate may have truncated and re-used the same seq for a
		// different reply, and that reply must not inherit this one's model.
		tag, err := h.db.Exec(ctx, `
			UPDATE conversation_messages SET model = $1
			WHERE conversation_id = $2 AND seq = $3 AND request_id = $4 AND model IS NULL
		`, model, convID, m.Seq, *m.RequestID)
		if err != nil {
			slog.WarnContext(ctx, "conversation provenance backfill update failed", "conversation", convID, "seq", m.Seq, "err", err)
			continue
		}
		if tag.RowsAffected() == 0 {
			continue // the turn at this seq is no longer the one we looked up
		}
		m.Model = &model
	}
}

// resolveMessageProvenance applies resolveAssistantProvenance to a role-typed
// message input: user turns always get (nil, nil) regardless of what the
// client sent, matching the DB comment's invariant.
func resolveMessageProvenance(ctx context.Context, q rowQuerier, userID string, m messageInput) (model, requestID *string, err error) {
	if m.Role != roleAssistant {
		return nil, nil, nil
	}
	return resolveAssistantProvenance(ctx, q, userID, m.RequestID)
}

// loadConversationFull fetches a conversation (scoped to userID) plus all of
// its messages ordered by seq. Returns (nil, nil) if no such conversation is
// owned by userID -- callers translate that into 404, never 403, so a
// nonexistent id and someone else's id are indistinguishable to the caller.
func (h *ConversationHandler) loadConversationFull(ctx context.Context, userID, convID string) (*conversationFull, error) {
	var full conversationFull
	err := h.db.QueryRow(ctx, `
		SELECT id, title, model, pinned_model, created_at, updated_at, library_item_ids::text[]
		FROM conversations
		WHERE id = $1 AND user_id = $2
	`, convID, userID).Scan(&full.ID, &full.Title, &full.Model, &full.PinnedModel, &full.CreatedAt, &full.UpdatedAt, &full.LibraryItemIDs)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	// conversation_messages has no user_id column (see migration 012); it's
	// safe to query by conversation_id alone here only because the row above
	// already proved convID belongs to userID.
	rows, err := h.db.Query(ctx, `
		SELECT seq, role, content, model, request_id, created_at, tool_events
		FROM conversation_messages
		WHERE conversation_id = $1
		ORDER BY seq
	`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	full.Messages = make([]conversationMessage, 0)
	for rows.Next() {
		var m conversationMessage
		var events []byte
		if err := rows.Scan(&m.Seq, &m.Role, &m.Content, &m.Model, &m.RequestID, &m.CreatedAt, &events); err != nil {
			return nil, err
		}
		if len(events) > 0 {
			m.ToolEvents = json.RawMessage(events)
		}
		full.Messages = append(full.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	full.MessageCount = len(full.Messages)
	if full.LibraryItemIDs == nil {
		full.LibraryItemIDs = []string{}
	}
	h.backfillProvenance(ctx, userID, convID, full.Messages)

	return &full, nil
}

// loadFullByImportKey resolves (userID, importKey) to a conversation id and
// returns its full representation, or (nil, nil) if no such conversation
// exists.
func (h *ConversationHandler) loadFullByImportKey(ctx context.Context, userID, importKey string) (*conversationFull, error) {
	var convID string
	err := h.db.QueryRow(ctx,
		`SELECT id FROM conversations WHERE user_id = $1 AND import_key = $2`, userID, importKey,
	).Scan(&convID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return h.loadConversationFull(ctx, userID, convID)
}

// ListConversations handles GET /api/v1/conversations.
// Query params: limit (default/max 200).
func (h *ConversationHandler) ListConversations(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")

	limit, _, err := parsePagination(c, conversationsListDefaultLimit, conversationsListMaxLimit)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT id, title, model, pinned_model, created_at, updated_at,
			(SELECT COUNT(*) FROM conversation_messages cm WHERE cm.conversation_id = c.id) AS message_count,
			library_item_ids::text[]
		FROM conversations c
		WHERE user_id = $1
		ORDER BY updated_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		slog.ErrorContext(ctx, "list conversations query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list conversations failed"})
		return
	}
	defer rows.Close()

	summaries := make([]conversationSummary, 0)
	for rows.Next() {
		var s conversationSummary
		if err := rows.Scan(&s.ID, &s.Title, &s.Model, &s.PinnedModel, &s.CreatedAt, &s.UpdatedAt, &s.MessageCount, &s.LibraryItemIDs); err != nil {
			slog.ErrorContext(ctx, "scan conversation row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "list conversations failed"})
			return
		}
		if s.LibraryItemIDs == nil {
			s.LibraryItemIDs = []string{}
		}
		summaries = append(summaries, s)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "conversations rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, summaries)
}

// createConversationRequest is the body of POST /api/v1/conversations.
type createConversationRequest struct {
	Title     *string        `json:"title,omitempty"`
	Model     *string        `json:"model,omitempty"`
	ImportKey *string        `json:"import_key,omitempty"`
	Messages  []messageInput `json:"messages,omitempty"`
}

// CreateConversation handles POST /api/v1/conversations.
//
// If import_key is given and a conversation with (user_id, import_key)
// already exists, that existing conversation is returned as-is with 200 --
// idempotent, no append, no field updates. Otherwise a new conversation (and
// any initial messages) is created and returned with 201.
func (h *ConversationHandler) CreateConversation(c *gin.Context) {
	ctx := c.Request.Context()
	userID := c.GetString("user_id")

	var req createConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := validateInitialMessages(req.Messages); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validImportKey(req.ImportKey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "import_key too long"})
		return
	}

	title := deriveConversationTitle(req.Messages)
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if !validConversationTitle(t) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "title must be 1-200 characters"})
			return
		}
		title = t
	}

	model := ""
	if req.Model != nil {
		model = strings.TrimSpace(*req.Model)
		if !validConversationModel(model) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("model must be at most %d characters", conversationModelMaxLen)})
			return
		}
	}

	// An empty import_key is "no idempotency requested", same as absent --
	// otherwise a client that always sends "" could only ever create one
	// conversation.
	if req.ImportKey != nil && *req.ImportKey == "" {
		req.ImportKey = nil
	}

	// Idempotent import: an existing (user_id, import_key) row wins outright.
	if req.ImportKey != nil {
		existing, err := h.loadFullByImportKey(ctx, userID, *req.ImportKey)
		if err != nil {
			slog.ErrorContext(ctx, "create conversation: check import_key failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create conversation failed"})
			return
		}
		if existing != nil {
			c.JSON(http.StatusOK, existing)
			return
		}
	}

	tx, err := h.db.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "create conversation: begin transaction failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create conversation failed"})
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	var convID string
	var createdAt, updatedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO conversations (user_id, title, model, import_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, import_key) DO NOTHING
		RETURNING id, created_at, updated_at
	`, userID, title, model, req.ImportKey).Scan(&convID, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race: another request imported the same import_key
			// between our pre-check above and this insert. import_key is
			// never nil here -- with a nil import_key the UNIQUE constraint
			// never fires (NULLs are distinct), so ON CONFLICT DO NOTHING
			// can only have suppressed a row for a non-nil key.
			existing, ferr := h.loadFullByImportKey(ctx, userID, *req.ImportKey)
			if ferr != nil || existing == nil {
				slog.ErrorContext(ctx, "create conversation: fetch after import race failed", "err", ferr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "create conversation failed"})
				return
			}
			c.JSON(http.StatusOK, existing)
			return
		}
		slog.ErrorContext(ctx, "create conversation: insert failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create conversation failed"})
		return
	}

	messages := make([]conversationMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		seq := i + 1
		msgModel, msgRequestID, err := resolveMessageProvenance(ctx, tx, userID, m)
		if err != nil {
			slog.ErrorContext(ctx, "create conversation: resolve provenance failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create conversation failed"})
			return
		}

		var createdAtMsg time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO conversation_messages (conversation_id, seq, role, content, model, request_id, tool_events)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING created_at
		`, convID, seq, m.Role, m.Content, msgModel, msgRequestID, toolEventsArg(m)).Scan(&createdAtMsg); err != nil {
			slog.ErrorContext(ctx, "create conversation: insert message failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create conversation failed"})
			return
		}
		messages = append(messages, conversationMessage{
			Seq: seq, Role: m.Role, Content: m.Content,
			Model: msgModel, RequestID: msgRequestID, CreatedAt: createdAtMsg,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "create conversation: commit failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create conversation failed"})
		return
	}

	c.JSON(http.StatusCreated, conversationFull{
		conversationSummary: conversationSummary{
			LibraryItemIDs: []string{},
			ID:             convID, Title: title, Model: model, MessageCount: len(messages),
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		},
		Messages: messages,
	})
}

// GetConversation handles GET /api/v1/conversations/:id.
// ?format=export additionally sets Content-Disposition so a browser saves the
// response as a file instead of rendering it, letting a user keep their own
// copy; the JSON body is identical either way.
func (h *ConversationHandler) GetConversation(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	full, err := h.loadConversationFull(ctx, userID, id)
	if err != nil {
		slog.ErrorContext(ctx, "get conversation failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get conversation failed"})
		return
	}
	if full == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return
	}

	if c.Query("format") == "export" {
		c.Header("Content-Disposition", `attachment; filename="conversation-`+id+`.json"`)
	}
	c.JSON(http.StatusOK, full)
}

// pinnedModelMaxLen mirrors no DB CHECK (pinned_model has none) but is
// validated the same defensive way conversation titles/import keys are.
const pinnedModelMaxLen = 200

func validPinnedModel(s string) bool {
	return utf8.RuneCountInString(s) <= pinnedModelMaxLen
}

// patchConversationRequest is the body of PATCH /api/v1/conversations/:id.
// PinnedModel is json.RawMessage, not *string, so PatchConversation can tell
// "the key is absent" (nil slice, zero value -- no change) apart from "the
// key is present with the JSON literal null" (RawMessage("null") -- clear
// the pin) apart from "the key is present with a string" (RawMessage(`"x"`)
// -- set the pin). A plain *string can't make that distinction:
// encoding/json's null-handling for pointer-typed struct fields sets the
// field to Go nil for BOTH an absent key and an explicit JSON null, without
// ever running custom unmarshal logic -- there is no hook to tell them apart
// once the field itself is declared as a pointer.
type patchConversationRequest struct {
	Title       *string         `json:"title,omitempty"`
	Model       *string         `json:"model,omitempty"`
	PinnedModel json.RawMessage `json:"pinned_model,omitempty"`
	// LibraryItemIDs replaces the attached items (at most 8 ids). Nothing
	// here checks that the ids are visible to the user: that is decided on
	// every chat turn, where it matters, and a stale id simply refuses the
	// turn with its id named.
	LibraryItemIDs *[]string `json:"library_item_ids,omitempty"`
}

// PatchConversation handles PATCH /api/v1/conversations/:id. Always bumps
// updated_at, even if no field is set.
func (h *ConversationHandler) PatchConversation(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req patchConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if !validConversationTitle(t) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "title must be 1-200 characters"})
			return
		}
		req.Title = &t
	}
	if req.Model != nil {
		m := strings.TrimSpace(*req.Model)
		if !validConversationModel(m) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("model must be at most %d characters", conversationModelMaxLen)})
			return
		}
		req.Model = &m
	}

	var setPinnedModel bool
	var pinnedModelValue *string
	if req.PinnedModel != nil {
		setPinnedModel = true
		trimmed := bytes.TrimSpace(req.PinnedModel)
		if !bytes.Equal(trimmed, []byte("null")) {
			var s string
			if err := json.Unmarshal(req.PinnedModel, &s); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "pinned_model must be a string or null"})
				return
			}
			if !validPinnedModel(s) {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("pinned_model must be at most %d characters", pinnedModelMaxLen)})
				return
			}
			// "" means unpinned, exactly like null: there is no fourth state.
			if s = strings.TrimSpace(s); s != "" {
				pinnedModelValue = &s
			}
		}
	}

	setLibrary := false
	var libraryIDs []string
	if req.LibraryItemIDs != nil {
		setLibrary = true
		libraryIDs = dedupeStrings(*req.LibraryItemIDs)
		if len(libraryIDs) > libraryMaxAttached {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("library_item_ids must hold at most %d ids", libraryMaxAttached)})
			return
		}
		for _, lid := range libraryIDs {
			if !isValidUUID(lid) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "library_item_ids must be item ids"})
				return
			}
		}
		if libraryIDs == nil {
			libraryIDs = []string{}
		}
	}

	var s conversationSummary
	err := h.db.QueryRow(ctx, `
		UPDATE conversations
		SET title = COALESCE($1, title),
			model = COALESCE($2, model),
			pinned_model = CASE WHEN $5 THEN $6 ELSE pinned_model END,
			library_item_ids = CASE WHEN $7 THEN $8::uuid[] ELSE library_item_ids END,
			updated_at = now()
		WHERE id = $3 AND user_id = $4
		RETURNING id, title, model, pinned_model, created_at, updated_at,
			(SELECT COUNT(*) FROM conversation_messages cm WHERE cm.conversation_id = conversations.id),
			library_item_ids::text[]
	`, req.Title, req.Model, id, userID, setPinnedModel, pinnedModelValue, setLibrary, libraryIDs).Scan(
		&s.ID, &s.Title, &s.Model, &s.PinnedModel, &s.CreatedAt, &s.UpdatedAt, &s.MessageCount, &s.LibraryItemIDs)
	if s.LibraryItemIDs == nil {
		s.LibraryItemIDs = []string{}
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
			return
		}
		slog.ErrorContext(ctx, "patch conversation failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "patch conversation failed"})
		return
	}

	c.JSON(http.StatusOK, s)
}

// appendMessagesRequest is the body of POST
// /api/v1/conversations/:id/messages.
type appendMessagesRequest struct {
	Messages []messageInput `json:"messages"`
}

// AppendMessages handles POST /api/v1/conversations/:id/messages: 1-2
// messages (a user turn, or user+assistant) appended in one transaction,
// with seq = COALESCE(MAX(seq), 0) + 1... Concurrent appends to the same
// conversation are serialized with SELECT ... FOR UPDATE on the conversation
// row, which also doubles as the ownership check.
func (h *ConversationHandler) AppendMessages(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var req appendMessagesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateAppendMessages(req.Messages); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tx, err := h.db.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "append messages: begin transaction failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "append messages failed"})
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	// Lock the conversation row so concurrent appends to it serialize instead
	// of racing on MAX(seq): the lock is held for the whole transaction, so a
	// second append blocks here until the first commits and releases it, and
	// only then computes its own MAX(seq) -- which by then reflects the
	// first append. This query is also the ownership check: no row for this
	// (id, user_id) pair means not found, whether that's because the id
	// doesn't exist or because it belongs to a different user.
	var exists bool
	err = tx.QueryRow(ctx, `
		SELECT true FROM conversations WHERE id = $1 AND user_id = $2 FOR UPDATE
	`, id, userID).Scan(&exists)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
			return
		}
		slog.ErrorContext(ctx, "append messages: lock conversation failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "append messages failed"})
		return
	}

	var nextSeq int
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1 FROM conversation_messages WHERE conversation_id = $1
	`, id).Scan(&nextSeq); err != nil {
		slog.ErrorContext(ctx, "append messages: compute next seq failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "append messages failed"})
		return
	}

	inserted := make([]conversationMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		seq := nextSeq + i
		msgModel, msgRequestID, err := resolveMessageProvenance(ctx, tx, userID, m)
		if err != nil {
			slog.ErrorContext(ctx, "append messages: resolve provenance failed", "id", id, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "append messages failed"})
			return
		}

		var createdAt time.Time
		events := toolEventsForStorage(m)
		if err := tx.QueryRow(ctx, `
			INSERT INTO conversation_messages (conversation_id, seq, role, content, model, request_id, tool_events)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING created_at
		`, id, seq, m.Role, m.Content, msgModel, msgRequestID, toolEventsArg(m)).Scan(&createdAt); err != nil {
			slog.ErrorContext(ctx, "append messages: insert failed", "id", id, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "append messages failed"})
			return
		}
		inserted = append(inserted, conversationMessage{
			Seq: seq, Role: m.Role, Content: m.Content,
			Model: msgModel, RequestID: msgRequestID, CreatedAt: createdAt,
			ToolEvents: json.RawMessage(events),
		})
	}

	// conversations.model is the user's picker choice (which may be an alias
	// such as "default-chat") and is only ever changed by the user via PATCH.
	// Provenance — the model that actually served each reply — is recorded per
	// message above; overwriting the preference with the resolved model would
	// silently swap an alias for its current target in the user's picker.
	_, err = tx.Exec(ctx,
		`UPDATE conversations SET updated_at = now() WHERE id = $1 AND user_id = $2`,
		id, userID)
	if err != nil {
		slog.ErrorContext(ctx, "append messages: update conversation failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "append messages failed"})
		return
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "append messages: commit failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "append messages failed"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"messages": inserted})
}

// DeleteConversation handles DELETE /api/v1/conversations/:id. This is a
// hard delete (see migration 012's comment) -- no soft-delete flag, no
// recovery path. Records a "conversation.deleted" activity event with only a
// message count, never content.
func (h *ConversationHandler) DeleteConversation(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	tx, err := h.db.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "delete conversation: begin transaction failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete conversation failed"})
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	// Counted before the delete, scoped by ownership via the JOIN -- a
	// mismatched or nonexistent id yields 0 rows and thus count 0, which is
	// harmless since the DELETE below (not this count) decides 404 vs success.
	var msgCount int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM conversation_messages cm
		JOIN conversations c ON c.id = cm.conversation_id
		WHERE c.id = $1 AND c.user_id = $2
	`, id, userID).Scan(&msgCount); err != nil {
		slog.ErrorContext(ctx, "delete conversation: count messages failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete conversation failed"})
		return
	}

	tag, err := tx.Exec(ctx, `DELETE FROM conversations WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		slog.ErrorContext(ctx, "delete conversation failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete conversation failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
		return
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "delete conversation: commit failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete conversation failed"})
		return
	}

	evt := activity.FromContext(c, activity.ConversationDeleted)
	evt.TargetType = activity.StrPtr("conversation")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"message_count": msgCount}
	h.activity.Record(c.Request.Context(), evt)

	c.Status(http.StatusNoContent)
}

// TruncateMessages handles DELETE /api/v1/conversations/:id/messages?from_seq=N.
// It removes every message with seq >= from_seq, owner-scoped. The webchat
// uses it for "regenerate": the previous assistant turn is dropped before the
// new reply is appended, so a reload does not show both. Like every other
// delete here it is a hard delete. Returns 204 with the number of removed rows
// in X-Removed-Count; 404 when the conversation is not owned by the caller.
func (h *ConversationHandler) TruncateMessages(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid conversation id"})
		return
	}
	// seq is an INT column: anything past int32 is not a sequence number
	// this conversation could have, so it is a 400 here rather than a
	// driver encoding failure (500) in the DELETE below.
	fromSeq64, err := strconv.ParseInt(c.Query("from_seq"), 10, 32)
	if err != nil || fromSeq64 < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from_seq must be a positive integer"})
		return
	}
	fromSeq := int(fromSeq64)
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	tx, err := h.db.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "truncate messages: begin transaction failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "truncate messages failed"})
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	// Lock the conversation row (same lock AppendMessages takes) so a truncate
	// cannot interleave with a concurrent append and leave a gap in seq.
	var owned string
	err = tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID).Scan(&owned)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
			return
		}
		slog.ErrorContext(ctx, "truncate messages: lock conversation failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "truncate messages failed"})
		return
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM conversation_messages WHERE conversation_id = $1 AND seq >= $2`, id, fromSeq)
	if err != nil {
		slog.ErrorContext(ctx, "truncate messages failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "truncate messages failed"})
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET updated_at = now() WHERE id = $1`, id); err != nil {
		slog.ErrorContext(ctx, "truncate messages: touch conversation failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "truncate messages failed"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "truncate messages: commit failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "truncate messages failed"})
		return
	}

	c.Header("X-Removed-Count", strconv.FormatInt(tag.RowsAffected(), 10))
	c.Status(http.StatusNoContent)
}
