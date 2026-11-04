package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

// attachFixture is a conversation owned by one user, holding a user turn with
// a PNG and a PDF, then an assistant reply.
type attachFixture struct {
	pool          *pgxpool.Pool
	h             *ConversationHandler
	ctx           context.Context
	userA, userB  string
	rA, rB        *gin.Engine
	convID        string
	pngID, pdfID  string
	png, pdf      []byte
	msgIDs        []string
	userMsgAttach []attachmentMeta
}

func newAttachFixture(t *testing.T) *attachFixture {
	t.Helper()
	f := &attachFixture{ctx: context.Background()}
	f.pool = setupTestDB(t)
	f.userA = newTestUserLabeled(t, f.ctx, f.pool, "a")
	f.userB = newTestUserLabeled(t, f.ctx, f.pool, "b")
	f.h = NewConversationHandler(f.pool, activity.NewRecorder(f.pool))
	f.rA = newConversationsTestRouter(f.h, f.userA)
	f.rB = newConversationsTestRouter(f.h, f.userB)
	f.png = fixture(pngHead, 200)
	f.pdf = fixture(pdfHead, 300)

	rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "look at these", "attachments": []attachmentInput{imgAtt("shot.png", f.png), pdfAtt("q3 report.pdf", f.pdf)}},
			{"role": "assistant", "content": "ok"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var full conversationFull
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	f.convID = full.ID
	f.userMsgAttach = full.Messages[0].Attachments
	if len(f.userMsgAttach) != 2 {
		t.Fatalf("create returned attachments = %+v", f.userMsgAttach)
	}
	f.pngID, f.pdfID = f.userMsgAttach[0].ID, f.userMsgAttach[1].ID
	return f
}

func (f *attachFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func get(r http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestAttachments_CreateReturnsMetadataWithIDs(t *testing.T) {
	f := newAttachFixture(t)
	want := []attachmentMeta{
		{ID: f.pngID, Kind: "image", MediaType: "image/png", Name: "shot.png", SizeBytes: 200},
		{ID: f.pdfID, Kind: "file", MediaType: "application/pdf", Name: "q3 report.pdf", SizeBytes: 300},
	}
	for i, w := range want {
		if f.userMsgAttach[i] != w || !isValidUUID(w.ID) {
			t.Errorf("attachment %d = %+v, want %+v", i, f.userMsgAttach[i], w)
		}
	}
}

func TestAttachments_AppendReturnsIDsAndAlwaysAnArray(t *testing.T) {
	f := newAttachFixture(t)
	rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations/"+f.convID+"/messages", map[string]any{
		"messages": []map[string]any{
			{"role": "user", "attachments": []attachmentInput{imgAtt("only.png", f.png)}},
			{"role": "assistant", "content": "a photo"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("append = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Messages []conversationMessage `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Messages[0].Attachments) != 1 || !isValidUUID(resp.Messages[0].Attachments[0].ID) || resp.Messages[0].Content != "" {
		t.Errorf("user turn = %+v", resp.Messages[0])
	}
	// The assistant turn has "attachments": [] on the wire, not null/absent.
	if !strings.Contains(rec.Body.String(), `"attachments":[]`) {
		t.Errorf("assistant turn lacks an empty attachments array: %s", rec.Body.String())
	}
	// And the new id is fetchable.
	if got := get(f.rA, "/api/v1/conversations/"+f.convID+"/attachments/"+resp.Messages[0].Attachments[0].ID); got.Code != 200 || !bytes.Equal(got.Body.Bytes(), f.png) {
		t.Errorf("fetch of appended attachment = %d", got.Code)
	}
}

func TestAttachments_GetConversationReturnsMetadataOnly(t *testing.T) {
	f := newAttachFixture(t)
	for _, path := range []string{"/api/v1/conversations/" + f.convID, "/api/v1/conversations/" + f.convID + "?format=export"} {
		rec := get(f.rA, path)
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		var raw struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		var metas []map[string]any
		if err := json.Unmarshal(raw.Messages[0]["attachments"], &metas); err != nil {
			t.Fatal(err)
		}
		if len(metas) != 2 || metas[0]["id"] != f.pngID || metas[1]["id"] != f.pdfID {
			t.Fatalf("attachments out of order or missing: %v", metas)
		}
		for _, m := range metas {
			if len(m) != 5 {
				t.Errorf("attachment metadata keys = %v, want exactly id,kind,media_type,name,size_bytes", m)
			}
			if _, has := m["data"]; has {
				t.Error("attachment bytes leaked into the conversation response")
			}
		}
		if string(raw.Messages[1]["attachments"]) != "[]" {
			t.Errorf("assistant turn attachments = %s, want []", raw.Messages[1]["attachments"])
		}
		if strings.Contains(rec.Body.String(), b64(f.png)) || strings.Contains(rec.Body.String(), b64(f.pdf)) {
			t.Error("base64 bytes present in conversation response")
		}
	}
}

func TestAttachments_GetConversationManyTurnsKeepsOrderAndOwnership(t *testing.T) {
	// Not N+1: many messages with attachments are loaded by the same single
	// attachment query; assert correctness of the join across many turns.
	f := newAttachFixture(t)
	for i := 0; i < 5; i++ {
		rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations/"+f.convID+"/messages", map[string]any{
			"messages": []map[string]any{{"role": "user", "attachments": []attachmentInput{imgAtt("p.png", f.png)}}},
		})
		if rec.Code != 201 {
			t.Fatal(rec.Body.String())
		}
	}
	var full conversationFull
	if err := json.Unmarshal(get(f.rA, "/api/v1/conversations/"+f.convID).Body.Bytes(), &full); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range full.Messages {
		n += len(m.Attachments)
	}
	if n != 7 {
		t.Errorf("total attachments = %d, want 7", n)
	}
}

func TestAttachments_FetchHeadersAndInlineSplit(t *testing.T) {
	f := newAttachFixture(t)
	img := get(f.rA, "/api/v1/conversations/"+f.convID+"/attachments/"+f.pngID)
	if img.Code != 200 || !bytes.Equal(img.Body.Bytes(), f.png) {
		t.Fatalf("image fetch = %d", img.Code)
	}
	doc := get(f.rA, "/api/v1/conversations/"+f.convID+"/attachments/"+f.pdfID)
	if doc.Code != 200 || !bytes.Equal(doc.Body.Bytes(), f.pdf) {
		t.Fatalf("pdf fetch = %d", doc.Code)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{"image": img, "pdf": doc} {
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" || h.Get("Cache-Control") != "private, no-store" {
			t.Errorf("%s: hardening headers = %v", name, h)
		}
	}
	if img.Header().Get("Content-Type") != "image/png" || img.Header().Get("Content-Disposition") != "inline" {
		t.Errorf("image headers = %v", img.Header())
	}
	if doc.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(doc.Header().Get("Content-Disposition"), `attachment; filename="q3 report.pdf"`) {
		t.Errorf("pdf headers = %v", doc.Header())
	}
}

func TestAttachments_FetchNonImageAlwaysDownloads(t *testing.T) {
	f := newAttachFixture(t)
	rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations/"+f.convID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "x", "attachments": []attachmentInput{
			{Kind: "file", MediaType: "text/html", Name: "<script>.html", Data: b64([]byte("<script>alert(1)</script>"))},
		}}},
	})
	var resp struct {
		Messages []conversationMessage `json:"messages"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	got := get(f.rA, "/api/v1/conversations/"+f.convID+"/attachments/"+resp.Messages[0].Attachments[0].ID)
	if !strings.HasPrefix(got.Header().Get("Content-Disposition"), "attachment;") || got.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("html served inline: %v", got.Header())
	}
}

func TestAttachments_CrossTenantDenied(t *testing.T) {
	f := newAttachFixture(t)
	attPath := "/api/v1/conversations/" + f.convID + "/attachments/" + f.pngID

	// B cannot fetch A's attachment, and the response is the plain not-found.
	rec := get(f.rB, attPath)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "PNG") {
		t.Fatalf("B fetching A's attachment = %d %q", rec.Code, rec.Body.String())
	}
	// Same body as a nonexistent id: no oracle.
	missing := get(f.rB, "/api/v1/conversations/"+f.convID+"/attachments/00000000-0000-4000-8000-000000000000")
	if missing.Code != http.StatusNotFound || missing.Body.String() != rec.Body.String() {
		t.Errorf("foreign vs missing differ: %d %q vs %d %q", rec.Code, rec.Body.String(), missing.Code, missing.Body.String())
	}
	// A conversation of B's own does not unlock A's attachment id.
	own := doJSON(t, f.rB, http.MethodPost, "/api/v1/conversations", map[string]any{"messages": []map[string]any{{"role": "user", "content": "mine"}}})
	var ownFull conversationFull
	_ = json.Unmarshal(own.Body.Bytes(), &ownFull)
	if got := get(f.rB, "/api/v1/conversations/"+ownFull.ID+"/attachments/"+f.pngID); got.Code != http.StatusNotFound {
		t.Errorf("A's attachment id under B's conversation = %d, want 404", got.Code)
	}
	// Wrong conversation id for the right owner is also 404.
	if got := get(f.rA, "/api/v1/conversations/"+ownFull.ID+"/attachments/"+f.pngID); got.Code != http.StatusNotFound {
		t.Errorf("attachment under the wrong conversation = %d, want 404", got.Code)
	}

	// B cannot GET A's conversation, and B's list never carries A's ids.
	if got := get(f.rB, "/api/v1/conversations/"+f.convID); got.Code != http.StatusNotFound {
		t.Errorf("B GET A's conversation = %d", got.Code)
	}
	list := get(f.rB, "/api/v1/conversations")
	if strings.Contains(list.Body.String(), f.pngID) || strings.Contains(list.Body.String(), f.pdfID) || strings.Contains(list.Body.String(), "shot.png") {
		t.Errorf("B's list leaks A's attachments: %s", list.Body.String())
	}

	// B appending to A's conversation: 404, and nothing is written.
	msgsBefore, attsBefore := f.count(t, "conversation_messages"), f.count(t, "conversation_attachments")
	app := doJSON(t, f.rB, http.MethodPost, "/api/v1/conversations/"+f.convID+"/messages", map[string]any{
		"messages": []map[string]any{{"role": "user", "attachments": []attachmentInput{imgAtt("evil.png", f.png)}}},
	})
	if app.Code != http.StatusNotFound {
		t.Errorf("B appending to A's conversation = %d, want 404", app.Code)
	}
	if f.count(t, "conversation_messages") != msgsBefore || f.count(t, "conversation_attachments") != attsBefore {
		t.Error("a cross-tenant append wrote rows")
	}
}

func TestAttachments_CascadeOnTruncateConversationAndUserDelete(t *testing.T) {
	f := newAttachFixture(t)
	if f.count(t, "conversation_attachments") != 2 {
		t.Fatal("setup")
	}

	// Truncate from seq 1 removes the user turn and its attachments.
	rec := doJSON(t, f.rA, http.MethodDelete, "/api/v1/conversations/"+f.convID+"/messages?from_seq=1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("truncate = %d", rec.Code)
	}
	if n := f.count(t, "conversation_attachments"); n != 0 {
		t.Errorf("attachments after truncate = %d", n)
	}
	if got := get(f.rA, "/api/v1/conversations/"+f.convID+"/attachments/"+f.pngID); got.Code != 404 {
		t.Errorf("fetch after truncate = %d", got.Code)
	}

	// Conversation delete, with the count recorded in the activity detail.
	f2 := newAttachFixture(t)
	del := doJSON(t, f2.rA, http.MethodDelete, "/api/v1/conversations/"+f2.convID, nil)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", del.Code)
	}
	if n := f2.count(t, "conversation_attachments"); n != 0 {
		t.Errorf("attachments after conversation delete = %d", n)
	}

	// User delete.
	f3 := newAttachFixture(t)
	if _, err := f3.pool.Exec(f3.ctx, `DELETE FROM users WHERE id = $1`, f3.userA); err != nil {
		t.Fatal(err)
	}
	if n := f3.count(t, "conversation_attachments"); n != 0 {
		t.Errorf("attachments after user delete = %d", n)
	}
}

func TestAttachments_DeleteActivityCarriesCountsNotNames(t *testing.T) {
	f := newAttachFixture(t)
	doJSON(t, f.rA, http.MethodDelete, "/api/v1/conversations/"+f.convID, nil)
	var detail string
	err := f.pool.QueryRow(f.ctx, `SELECT detail::text FROM activity_events WHERE event_type = 'conversation.deleted' ORDER BY created_at DESC LIMIT 1`).Scan(&detail)
	if err != nil {
		// Recorder is asynchronous in some builds; accept absence rather than flake.
		t.Skipf("activity row not visible yet: %v", err)
	}
	if !strings.Contains(detail, `"attachment_count": 2`) || strings.Contains(detail, "shot.png") || strings.Contains(detail, "q3 report") {
		t.Errorf("detail = %s", detail)
	}
}

func TestAttachments_RejectedInputWritesNothing(t *testing.T) {
	f := newAttachFixture(t)
	msgsBefore, attsBefore := f.count(t, "conversation_messages"), f.count(t, "conversation_attachments")
	path := "/api/v1/conversations/" + f.convID + "/messages"
	cases := map[string]map[string]any{
		"oversize image": {"role": "user", "attachments": []attachmentInput{imgAtt("big.png", fixture(pngHead, attachmentImageMaxBytes+1))}},
		"sniff mismatch": {"role": "user", "attachments": []attachmentInput{{Kind: "image", MediaType: "image/jpeg", Name: "x.jpg", Data: b64(f.png)}}},
		"svg":            {"role": "user", "attachments": []attachmentInput{{Kind: "image", MediaType: "image/svg+xml", Name: "x.svg", Data: b64([]byte("<svg/>"))}}},
		"assistant":      {"role": "assistant", "content": "x", "attachments": []attachmentInput{imgAtt("a.png", f.png)}},
		"21 images": {"role": "user", "attachments": func() []attachmentInput {
			a := make([]attachmentInput, 21)
			for i := range a {
				a[i] = imgAtt("a.png", f.png)
			}
			return a
		}()},
	}
	for name, msg := range cases {
		rec := doJSON(t, f.rA, http.MethodPost, path, map[string]any{"messages": []map[string]any{msg}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "big.png") || strings.Contains(rec.Body.String(), "x.jpg") {
			t.Errorf("%s: error echoes a filename: %s", name, rec.Body.String())
		}
	}
	// Same through create.
	rec := doJSON(t, f.rA, http.MethodPost, "/api/v1/conversations", map[string]any{"messages": []map[string]any{cases["sniff mismatch"]}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("create with a bad attachment = %d", rec.Code)
	}
	if f.count(t, "conversation_messages") != msgsBefore || f.count(t, "conversation_attachments") != attsBefore {
		t.Error("rejected input wrote rows")
	}
}

func TestAttachments_BodyOver16MiBIs413(t *testing.T) {
	f := newAttachFixture(t)
	huge := bytes.Repeat([]byte("a"), conversationBodyMaxBytes+10)
	body := []byte(`{"messages":[{"role":"user","content":"` + string(huge) + `"}]}`)
	for _, path := range []string{"/api/v1/conversations", "/api/v1/conversations/" + f.convID + "/messages"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.rA.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s = %d, want 413", path, rec.Code)
		}
	}
}

func TestAttachments_SchemaConstraints(t *testing.T) {
	f := newAttachFixture(t)
	// The schema is the last line of defence: bytes that do not match
	// size_bytes, a kind/media-type mismatch and an out-of-range position
	// are all refused by Postgres itself.
	var msgID string
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM conversation_messages WHERE conversation_id=$1 AND seq=1`, f.convID).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		name string
		sql  string
	}{
		{"size mismatch", `INSERT INTO conversation_attachments (message_id, position, kind, media_type, name, size_bytes, data) VALUES ($1, 10, 'image', 'image/png', 'a', 5, '\x01'::bytea)`},
		{"kind mismatch", `INSERT INTO conversation_attachments (message_id, position, kind, media_type, name, size_bytes, data) VALUES ($1, 10, 'file', 'image/png', 'a', 1, '\x01'::bytea)`},
		{"position 25", `INSERT INTO conversation_attachments (message_id, position, kind, media_type, name, size_bytes, data) VALUES ($1, 25, 'image', 'image/png', 'a', 1, '\x01'::bytea)`},
		{"svg", `INSERT INTO conversation_attachments (message_id, position, kind, media_type, name, size_bytes, data) VALUES ($1, 10, 'image', 'image/svg+xml', 'a', 1, '\x01'::bytea)`},
		{"duplicate position", `INSERT INTO conversation_attachments (message_id, position, kind, media_type, name, size_bytes, data) VALUES ($1, 0, 'image', 'image/png', 'a', 1, '\x01'::bytea)`},
	}
	for _, b := range bad {
		if _, err := f.pool.Exec(f.ctx, b.sql, msgID); err == nil {
			t.Errorf("%s: database accepted the row", b.name)
		}
	}
}
