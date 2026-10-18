package handler

// Library visibility, sharing, governance, export/import and ownership on
// user deletion, against a real Postgres (TEST_DATABASE_URL).

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

func newLibraryTestRouter(h *LibraryHandler, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	})
	r.GET("/api/v1/library", h.List)
	r.POST("/api/v1/library", h.Create)
	r.GET("/api/v1/library/export", h.ExportAll)
	r.POST("/api/v1/library/import", h.Import)
	r.GET("/api/v1/library/:id", h.Get)
	r.PUT("/api/v1/library/:id", h.Update)
	r.DELETE("/api/v1/library/:id", h.Delete)
	r.POST("/api/v1/library/:id/share", h.Share)
	r.GET("/api/v1/library/:id/export", h.ExportOne)
	r.GET("/api/v1/library/shared", h.ListShared)
	r.POST("/api/v1/library/shared/:id/unshare", h.AdminUnshare)
	r.DELETE("/api/v1/library/shared/:id", h.AdminDelete)
	return r
}

func libraryDo(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newLibraryHandlerForTest(pool *pgxpool.Pool) *LibraryHandler {
	rec := activity.NewRecorder(pool)
	return NewLibraryHandler(pool, rec, newToolService(pool, rec, nil, toolCredentialReader{}))
}

func TestLibrary_VisibilityAndSharing(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	alice := newTestUserLabeled(t, ctx, pool, "alice")
	bob := newTestUserLabeled(t, ctx, pool, "bob")
	h := newLibraryHandlerForTest(pool)
	asAlice, asBob := newLibraryTestRouter(h, alice), newLibraryTestRouter(h, bob)

	// Alice creates a private skill.
	w := libraryDo(t, asAlice, http.MethodPost, "/api/v1/library", map[string]any{"kind": "skill", "name": "Contract review", "body": "Read every clause."})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var item libraryItem
	_ = json.Unmarshal(w.Body.Bytes(), &item)
	if item.Slug != "contract-review" || item.Visibility != "private" || !item.Mine || item.Owner == nil || item.Owner.ID != alice {
		t.Errorf("item = %+v", item)
	}

	// Bob cannot see, edit, delete, share or export it: 404 every time.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/library/" + item.ID},
		{http.MethodPut, "/api/v1/library/" + item.ID},
		{http.MethodDelete, "/api/v1/library/" + item.ID},
		{http.MethodPost, "/api/v1/library/" + item.ID + "/share"},
		{http.MethodGet, "/api/v1/library/" + item.ID + "/export"},
	} {
		body := map[string]any{"name": "x", "body": "y", "visibility": "org"}
		w := libraryDo(t, asBob, tc.method, tc.path, body)
		if w.Code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	w = libraryDo(t, asBob, http.MethodGet, "/api/v1/library", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), item.ID) {
		t.Errorf("bob's list shows alice's private item: %s", w.Body.String())
	}
	// A chat attachment resolves the same way.
	if _, ref := resolveAttachments(ctx, pool, bob, []string{item.ID}); ref == nil || ref.code != "library_item_not_found" {
		t.Errorf("bob attach private: %+v", ref)
	}
	// The admin governance list never shows a private item.
	w = libraryDo(t, asBob, http.MethodGet, "/api/v1/library/shared", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), item.ID) {
		t.Errorf("shared list shows a private item: %s", w.Body.String())
	}

	// Alice shares it.
	w = libraryDo(t, asAlice, http.MethodPost, "/api/v1/library/"+item.ID+"/share", map[string]any{"visibility": "org"})
	if w.Code != http.StatusOK {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	// Bob can read and attach, not write.
	w = libraryDo(t, asBob, http.MethodGet, "/api/v1/library/"+item.ID, nil)
	var seen libraryItem
	_ = json.Unmarshal(w.Body.Bytes(), &seen)
	if w.Code != http.StatusOK || seen.Mine || seen.Owner.ID != alice {
		t.Errorf("bob read shared: %d %+v", w.Code, seen)
	}
	if att, ref := resolveAttachments(ctx, pool, bob, []string{item.ID}); ref != nil || len(att.text) != 1 || att.text[0].body != "Read every clause." {
		t.Errorf("bob attach shared: %+v %+v", att, ref)
	}
	w = libraryDo(t, asBob, http.MethodPut, "/api/v1/library/"+item.ID, map[string]any{"name": "Mine now", "body": "y"})
	if w.Code != http.StatusNotFound {
		t.Errorf("bob edit shared: %d", w.Code)
	}
	w = libraryDo(t, asBob, http.MethodGet, "/api/v1/library?scope=shared", nil)
	if !strings.Contains(w.Body.String(), item.ID) {
		t.Errorf("shared scope missing the item: %s", w.Body.String())
	}
	w = libraryDo(t, asBob, http.MethodGet, "/api/v1/library?scope=mine", nil)
	if strings.Contains(w.Body.String(), item.ID) {
		t.Errorf("mine scope shows someone else's item: %s", w.Body.String())
	}
	// Governance sees it; unshare makes it private again, still Alice's.
	w = libraryDo(t, asBob, http.MethodGet, "/api/v1/library/shared", nil)
	if !strings.Contains(w.Body.String(), item.ID) {
		t.Errorf("shared list missing the shared item: %s", w.Body.String())
	}
	w = libraryDo(t, asBob, http.MethodPost, "/api/v1/library/shared/"+item.ID+"/unshare", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("admin unshare: %d %s", w.Code, w.Body.String())
	}
	w = libraryDo(t, asBob, http.MethodGet, "/api/v1/library/"+item.ID, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("after unshare bob still sees it: %d", w.Code)
	}
	w = libraryDo(t, asAlice, http.MethodGet, "/api/v1/library/"+item.ID, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"visibility":"private"`) {
		t.Errorf("after unshare alice: %d %s", w.Code, w.Body.String())
	}
	// Admin delete only reaches org items.
	w = libraryDo(t, asBob, http.MethodDelete, "/api/v1/library/shared/"+item.ID, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("admin delete of a private item must be 404, got %d", w.Code)
	}
	// Slug collisions auto-suffix for the same owner.
	w = libraryDo(t, asAlice, http.MethodPost, "/api/v1/library", map[string]any{"kind": "context", "name": "Contract review", "body": "ref"})
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"slug":"contract-review-2"`) {
		t.Errorf("second slug: %d %s", w.Code, w.Body.String())
	}
}

func TestLibrary_ExportImportAndOwnerDeletion(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	alice := newTestUserLabeled(t, ctx, pool, "alice")
	admin := newTestUserLabeled(t, ctx, pool, "admin")
	h := newLibraryHandlerForTest(pool)
	asAlice, asAdmin := newLibraryTestRouter(h, alice), newLibraryTestRouter(h, admin)

	// Import a SKILL.md with keys we drop, preview first.
	content := "---\nname: PDF tools\ndescription: Work with PDFs\nallowed-tools: Bash\n---\n# Steps\nRead the file.\n"
	w := libraryDo(t, asAlice, http.MethodPost, "/api/v1/library/import?dry_run=1", map[string]any{"filename": "SKILL.md", "kind": "skill", "content": content})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"dropped":["allowed-tools"]`) || strings.Contains(w.Body.String(), `"item"`) {
		t.Fatalf("dry run: %d %s", w.Code, w.Body.String())
	}
	w = libraryDo(t, asAlice, http.MethodPost, "/api/v1/library/import", map[string]any{"filename": "SKILL.md", "kind": "skill", "content": content, "visibility": "org"})
	if w.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	var pv importPreview
	_ = json.Unmarshal(w.Body.Bytes(), &pv)
	if pv.Item == nil || pv.Item.Name != "PDF tools" || pv.Item.Visibility != "org" || !strings.HasPrefix(pv.Item.Body, "# Steps") {
		t.Fatalf("imported item = %+v", pv.Item)
	}
	shared := pv.Item.ID
	// A private tool and a private context for Alice.
	w = libraryDo(t, asAlice, http.MethodPost, "/api/v1/library", map[string]any{"kind": "tool", "name": "Jira", "body": `{"server_url":"https://mcp.corp.example/jira"}`})
	if w.Code != http.StatusCreated {
		t.Fatalf("tool create: %d %s", w.Code, w.Body.String())
	}
	var tool libraryItem
	_ = json.Unmarshal(w.Body.Bytes(), &tool)
	if tool.Tool == nil || tool.Tool.ServerURL != "https://mcp.corp.example/jira" || tool.Tool.Credential.Set {
		t.Errorf("tool wire = %+v", tool.Tool)
	}
	w = libraryDo(t, asAlice, http.MethodPost, "/api/v1/library", map[string]any{"kind": "context", "name": "Style", "body": "Short sentences."})
	if w.Code != http.StatusCreated {
		t.Fatalf("context create: %d", w.Code)
	}
	// Attaching an unverified tool is refused with its id.
	if _, ref := resolveAttachments(ctx, pool, alice, []string{tool.ID}); ref == nil || ref.code != "library_tool_unverified" {
		t.Errorf("unverified tool attach: %+v", ref)
	}

	// Single export is a markdown file with front matter.
	w = libraryDo(t, asAlice, http.MethodGet, "/api/v1/library/"+shared+"/export", nil)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "---\nname: PDF tools\n") || !strings.Contains(w.Header().Get("Content-Disposition"), "pdf-tools.md") {
		t.Errorf("export one: %d %q %s", w.Code, w.Header().Get("Content-Disposition"), w.Body.String())
	}
	// Admin's zip holds the shared skill only; Alice's holds all three.
	zipNames := func(w *httptest.ResponseRecorder) []string {
		zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
		if err != nil {
			t.Fatalf("zip: %v", err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		return names
	}
	w = libraryDo(t, asAdmin, http.MethodGet, "/api/v1/library/export", nil)
	if n := zipNames(w); len(n) != 1 || n[0] != "skills/pdf-tools.md" {
		t.Errorf("admin zip = %v", n)
	}
	w = libraryDo(t, asAlice, http.MethodGet, "/api/v1/library/export", nil)
	if n := zipNames(w); len(n) != 3 || strings.Join(n, ",") != "context/style.md,skills/pdf-tools.md,tools/jira.json" {
		t.Errorf("alice zip = %v", n)
	}

	// The administrator already uses the shared item's slug: reassignment
	// must rename rather than fail the whole deletion.
	w = libraryDo(t, asAdmin, http.MethodPost, "/api/v1/library", map[string]any{"kind": "skill", "name": "PDF tools", "body": "admin's own"})
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"slug":"pdf-tools"`) {
		t.Fatalf("admin slug: %d %s", w.Code, w.Body.String())
	}

	// Deleting Alice: her private items go, the shared one passes to the
	// acting administrator and stays editable.
	users := NewUserHandler(pool, activity.NewRecorder(pool), []byte("pepper"))
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", admin); c.Next() })
	r.DELETE("/api/v1/users/:id", users.DeleteUser)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+alice, nil)
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)
	if rw.Code != http.StatusNoContent && rw.Code != http.StatusOK {
		t.Fatalf("delete user: %d %s", rw.Code, rw.Body.String())
	}
	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM library_items`).Scan(&count)
	if count != 2 {
		t.Errorf("items after owner deletion = %d, want 2 (the shared one and the admin's own)", count)
	}
	w = libraryDo(t, asAdmin, http.MethodGet, "/api/v1/library/"+shared, nil)
	var reassigned libraryItem
	_ = json.Unmarshal(w.Body.Bytes(), &reassigned)
	if w.Code != http.StatusOK || !reassigned.Mine || reassigned.Owner == nil || reassigned.Owner.ID != admin {
		t.Errorf("reassigned = %d %+v", w.Code, reassigned)
	}
	if !strings.HasPrefix(reassigned.Slug, "pdf-tools-") || reassigned.Slug == "pdf-tools" {
		t.Errorf("reassigned slug = %q, want a suffixed one", reassigned.Slug)
	}
	w = libraryDo(t, asAdmin, http.MethodPut, "/api/v1/library/"+shared, map[string]any{"name": "PDF tools (kept)", "body": "# Steps\nRead the file.\n"})
	if w.Code != http.StatusOK {
		t.Errorf("admin edit after reassignment: %d %s", w.Code, w.Body.String())
	}
	var ev int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM activity_events WHERE event_type = $1`, string(activity.LibraryOwnerReassigned)).Scan(&ev)
	if ev != 1 {
		t.Errorf("owner_reassigned events = %d", ev)
	}
}

func TestLibrary_ConversationAttachmentsAndToolEvents(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	alice := newTestUserLabeled(t, ctx, pool, "alice")
	conv := NewConversationHandler(pool, activity.NewRecorder(pool))
	r := newConversationsTestRouter(conv, alice)
	w := libraryDo(t, r, http.MethodPost, "/api/v1/conversations", map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create conversation: %d %s", w.Code, w.Body.String())
	}
	var full conversationFull
	_ = json.Unmarshal(w.Body.Bytes(), &full)
	if full.LibraryItemIDs == nil || len(full.LibraryItemIDs) != 0 {
		t.Errorf("new conversation library ids = %v (want empty array)", full.LibraryItemIDs)
	}
	id := "11111111-1111-4111-8111-111111111111"
	w = libraryDo(t, r, http.MethodPatch, "/api/v1/conversations/"+full.ID, map[string]any{"library_item_ids": []string{id}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("patch ids: %d %s", w.Code, w.Body.String())
	}
	events := `[{"type":"tool_call","call_id":"c1","tool":"echo"},{"type":"tool_result","call_id":"c1","content":"ok"}]`
	w = libraryDo(t, r, http.MethodPost, "/api/v1/conversations/"+full.ID+"/messages", map[string]any{"messages": []map[string]any{{"role": "assistant", "content": "done", "tool_events": json.RawMessage(events)}}})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("append: %d %s", w.Code, w.Body.String())
	}
	w = libraryDo(t, r, http.MethodGet, "/api/v1/conversations/"+full.ID, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &full)
	var gotEvents, wantEvents []map[string]any
	_ = json.Unmarshal(full.Messages[1].ToolEvents, &gotEvents)
	_ = json.Unmarshal([]byte(events), &wantEvents)
	if len(full.Messages) != 2 || fmt.Sprint(gotEvents) != fmt.Sprint(wantEvents) || (full.Messages[0].ToolEvents != nil && string(full.Messages[0].ToolEvents) != "null") {
		t.Errorf("messages = %+v (events %s)", full.Messages, full.Messages[1].ToolEvents)
	}
	if fmt.Sprint(full.LibraryItemIDs) != "["+id+"]" {
		t.Errorf("library ids after reload = %v", full.LibraryItemIDs)
	}
	w = libraryDo(t, r, http.MethodPatch, "/api/v1/conversations/"+full.ID, map[string]any{"library_item_ids": []string{}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"library_item_ids":[]`) {
		t.Errorf("clear ids: %d %s", w.Code, w.Body.String())
	}
}
