package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Reference expansion for the session chat proxy.
//
// The webchat keeps history light by sending a stored attachment as a
// reference, {"type":"file","file":{"file_id":"<attachment id>"}} (OpenAI's own
// file_id field), instead of re-uploading its bytes every turn. Before the
// body is rewritten for the system prompt and library, each reference on a
// user turn is replaced by the stored bytes as a data: URL part, which is the
// only form the gateway accepts. Only the caller's own attachments resolve (any
// conversation of theirs); anything else is attachment_not_found, so an id
// that exists but is someone else's is indistinguishable from one that does
// not. The Bearer path never gets here: the gateway refuses a file_id itself.
//
// Nothing about a part (id aside, which is the caller's own, filename, bytes)
// is logged.

const (
	attachmentResolveWait = 10 * time.Second
	// attachmentWireFilename is the filename sent with a document. The stored
	// name is user content that Bedrock treats as a prompt-injection vector,
	// so the gateway only ever sees this constant.
	attachmentWireFilename = "document"
	// attachmentRefMax caps the references in one request (the same as the
	// per-turn attachment cap). Without it a small body of repeated refs to
	// one stored image expands into an enormous one.
	attachmentRefMax = attachmentMaxTotal
	// wirePartOverhead is the JSON around a data: URL in the rewritten part,
	// beyond the URL's own media type, with room to spare.
	wirePartOverhead = 160
)

// refPart is a reference located in the request: where it is and which id.
type refPart struct {
	msg, part int
	id        string
}

// expandAttachmentRefs returns body with every attachment reference on a user
// turn replaced by its stored bytes. A body with no reference is returned
// untouched (same slice), and a body it cannot parse is also returned
// untouched so the usual invalid_body refusal in withSystemPromptText stays the
// one place that reports it. Only messages that contain a reference are
// re-marshalled; every other message, and every other member of the body, keeps
// its bytes.
func expandAttachmentRefs(ctx context.Context, db *pgxpool.Pool, userID string, body []byte) ([]byte, *libraryRefusal) {
	if !bytes.Contains(body, []byte(`"file_id"`)) {
		return body, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return body, nil
	}
	var messages []json.RawMessage
	if json.Unmarshal(fields["messages"], &messages) != nil {
		return body, nil
	}

	// First pass: locate references without changing anything.
	type msgView struct {
		role  string
		parts []json.RawMessage // nil when content is not an array
	}
	views := make([]msgView, len(messages))
	var refs []refPart
	for i, raw := range messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		views[i].role = m.Role
		if len(m.Content) == 0 || m.Content[0] != '[' {
			continue
		}
		if json.Unmarshal(m.Content, &views[i].parts) != nil {
			views[i].parts = nil
			continue
		}
		for j, p := range views[i].parts {
			id, isRef, bad := fileRefID(p)
			if !isRef {
				continue
			}
			if m.Role != roleUser {
				return nil, &libraryRefusal{http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("messages[%d].content[%d]: file_id is only allowed on a user turn", i, j),
					map[string]any{"param": fmt.Sprintf("messages[%d].content[%d]", i, j)}}
			}
			if bad {
				return nil, &libraryRefusal{http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("messages[%d].content[%d]: file_id must be a string", i, j),
					map[string]any{"param": fmt.Sprintf("messages[%d].content[%d]", i, j)}}
			}
			if len(refs) >= attachmentRefMax {
				return nil, &libraryRefusal{http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("messages[%d].content[%d]: at most %d attached files per request", i, j, attachmentRefMax),
					map[string]any{"param": fmt.Sprintf("messages[%d].content[%d]", i, j)}}
			}
			refs = append(refs, refPart{i, j, id})
		}
	}
	if len(refs) == 0 {
		return body, nil
	}

	// Sizes first, owner-scoped, with no bytes read: the expanded body is
	// priced from them and refused before any data is loaded or encoded.
	infos, refusal := loadOwnedAttachmentInfo(ctx, db, userID, refs)
	if refusal != nil {
		return nil, refusal
	}
	expanded := int64(len(body))
	for _, r := range refs {
		in := infos[r.id]
		expanded += (int64(in.size)+2)/3*4 + int64(len(in.mediaType)) + wirePartOverhead
	}
	if expanded > chatBodyMaxBytes {
		return nil, &libraryRefusal{http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("request body exceeds %d bytes once attached files are included", chatBodyMaxBytes), nil}
	}
	stored, refusal := loadAttachmentData(ctx, db, userID, infos)
	if refusal != nil {
		return nil, refusal
	}

	// Second pass: rewrite only the messages that carried a reference.
	byMsg := map[int][]refPart{}
	for _, r := range refs {
		byMsg[r.msg] = append(byMsg[r.msg], r)
	}
	for i, rs := range byMsg {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(messages[i], &m); err != nil {
			return nil, &libraryRefusal{http.StatusBadRequest, "invalid_body", "invalid request body", nil}
		}
		parts := views[i].parts
		for _, r := range rs {
			rep, err := json.Marshal(wirePart(stored[r.id]))
			if err != nil {
				return nil, &libraryRefusal{http.StatusInternalServerError, "invalid_body", "invalid request body", nil}
			}
			parts[r.part] = rep
		}
		var err error
		if m["content"], err = json.Marshal(parts); err != nil {
			return nil, &libraryRefusal{http.StatusInternalServerError, "invalid_body", "invalid request body", nil}
		}
		if messages[i], err = json.Marshal(m); err != nil {
			return nil, &libraryRefusal{http.StatusInternalServerError, "invalid_body", "invalid request body", nil}
		}
	}
	var err error
	if fields["messages"], err = json.Marshal(messages); err != nil {
		return nil, &libraryRefusal{http.StatusInternalServerError, "invalid_body", "invalid request body", nil}
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &libraryRefusal{http.StatusInternalServerError, "invalid_body", "invalid request body", nil}
	}
	return out, nil
}

// fileRefID reports whether a content part is a {"type":"file","file":{"file_id"}}
// reference. bad is set when file_id is present but not a string.
func fileRefID(part json.RawMessage) (id string, isRef, bad bool) {
	var p struct {
		Type string `json:"type"`
		File *struct {
			FileID json.RawMessage `json:"file_id"`
		} `json:"file"`
	}
	if json.Unmarshal(part, &p) != nil || p.Type != "file" || p.File == nil || len(p.File.FileID) == 0 {
		return "", false, false
	}
	if json.Unmarshal(p.File.FileID, &id) != nil {
		return "", true, true
	}
	return id, true, false
}

type storedAttachment struct {
	kind, mediaType string
	data            []byte
}

type attachmentInfo struct {
	kind, mediaType string
	size            int
}

func attachmentNotFound() *libraryRefusal {
	return &libraryRefusal{http.StatusNotFound, "attachment_not_found", "an attached file was not found; it may have been deleted", nil}
}

func attachmentUnavailable(msg string) *libraryRefusal {
	return &libraryRefusal{http.StatusServiceUnavailable, "attachment_unavailable", msg, nil}
}

// loadOwnedAttachmentInfo resolves every distinct referenced id in one query
// that reads no bytes, joined up to conversations.user_id = caller. Any id
// that is malformed, unknown or not the caller's refuses the whole request as
// attachment_not_found.
func loadOwnedAttachmentInfo(ctx context.Context, db *pgxpool.Pool, userID string, refs []refPart) (map[string]attachmentInfo, *libraryRefusal) {
	seen := map[string]bool{}
	ids := []string{}
	for _, r := range refs {
		if !isValidUUID(r.id) {
			return nil, attachmentNotFound()
		}
		if !seen[r.id] {
			seen[r.id] = true
			ids = append(ids, r.id)
		}
	}
	if db == nil {
		return nil, attachmentUnavailable("attachments are not available on this deployment")
	}
	rctx, cancel := context.WithTimeout(ctx, attachmentResolveWait)
	defer cancel()
	rows, err := db.Query(rctx, `
		SELECT a.id::text, a.kind, a.media_type, a.size_bytes
		FROM conversation_attachments a
		JOIN conversation_messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		WHERE a.id = ANY($1::uuid[]) AND c.user_id = $2
	`, ids, userID)
	if err != nil {
		slog.ErrorContext(ctx, "resolve attachments failed", "err", err)
		return nil, attachmentUnavailable("attachments could not be read; try again")
	}
	defer rows.Close()
	infos := make(map[string]attachmentInfo, len(ids))
	for rows.Next() {
		var id string
		var in attachmentInfo
		if err := rows.Scan(&id, &in.kind, &in.mediaType, &in.size); err != nil {
			slog.ErrorContext(ctx, "resolve attachments scan failed", "err", err)
			return nil, attachmentUnavailable("attachments could not be read; try again")
		}
		infos[id] = in
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "resolve attachments rows failed", "err", err)
		return nil, attachmentUnavailable("attachments could not be read; try again")
	}
	for _, id := range ids {
		if _, ok := infos[id]; !ok {
			return nil, attachmentNotFound()
		}
	}
	return infos, nil
}

// loadAttachmentData reads the bytes of attachments already resolved (and
// priced) by loadOwnedAttachmentInfo; the owner scope is applied again.
func loadAttachmentData(ctx context.Context, db *pgxpool.Pool, userID string, infos map[string]attachmentInfo) (map[string]storedAttachment, *libraryRefusal) {
	ids := make([]string, 0, len(infos))
	for id := range infos {
		ids = append(ids, id)
	}
	rctx, cancel := context.WithTimeout(ctx, attachmentResolveWait)
	defer cancel()
	rows, err := db.Query(rctx, `
		SELECT a.id::text, a.kind, a.media_type, a.data
		FROM conversation_attachments a
		JOIN conversation_messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		WHERE a.id = ANY($1::uuid[]) AND c.user_id = $2
	`, ids, userID)
	if err != nil {
		slog.ErrorContext(ctx, "load attachments failed", "err", err)
		return nil, attachmentUnavailable("attachments could not be read; try again")
	}
	defer rows.Close()
	stored := make(map[string]storedAttachment, len(ids))
	for rows.Next() {
		var id string
		var a storedAttachment
		if err := rows.Scan(&id, &a.kind, &a.mediaType, &a.data); err != nil {
			slog.ErrorContext(ctx, "load attachments scan failed", "err", err)
			return nil, attachmentUnavailable("attachments could not be read; try again")
		}
		stored[id] = a
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "load attachments rows failed", "err", err)
		return nil, attachmentUnavailable("attachments could not be read; try again")
	}
	for _, id := range ids {
		if _, ok := stored[id]; !ok {
			return nil, attachmentNotFound()
		}
	}
	return stored, nil
}

// wirePart renders a stored attachment as the data: URL content part the
// gateway accepts. The filename sent for a document is a neutral constant.
func wirePart(a storedAttachment) map[string]any {
	url := "data:" + a.mediaType + ";base64," + base64.StdEncoding.EncodeToString(a.data)
	if a.kind == attachmentKindImage {
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
	}
	return map[string]any{"type": "file", "file": map[string]any{"filename": attachmentWireFilename, "file_data": url}}
}
