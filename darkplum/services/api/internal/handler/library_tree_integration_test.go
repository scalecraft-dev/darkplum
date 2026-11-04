package handler

// The library tree against a real Postgres (TEST_DATABASE_URL) with an
// in-memory object store: folders, moves, share/unshare, the sync worker and
// its failure modes, Sync from bucket, erasure through the queue, and
// cross-user denial on every new route.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
	"github.com/scalecraft-dev/darkplum/api/internal/libkey"
	"github.com/scalecraft-dev/darkplum/api/internal/objectstore"
)

type treeEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	h     *LibraryHandler
	store *objectstore.MemStore
	sync  *LibrarySyncer
}

func newTreeEnv(t *testing.T) *treeEnv {
	t.Helper()
	pool := setupTestDB(t)
	h := newLibraryHandlerForTest(pool)
	st := objectstore.NewMemStore()
	h.SetStore(st)
	sy := NewLibrarySyncer(pool, st)
	h.SetSyncer(sy)
	return &treeEnv{t: t, pool: pool, h: h, store: st, sync: sy}
}

// user makes a non-admin person; asAdmin routers get the admin role in the
// request context (what auth.SessionRequired sets in production).
func (e *treeEnv) user(label string) string {
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO users (email, name, role) VALUES ($1, 'T', 'user') RETURNING id`,
		strings.ToLower(e.t.Name())+"-"+label+"@example.com").Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *treeEnv) router(userID string, admin bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		if admin {
			c.Set("user_role", "admin")
		} else {
			c.Set("user_role", "user")
		}
		c.Next()
	})
	h := e.h
	r.GET("/api/v1/library", h.List)
	r.POST("/api/v1/library", h.Create)
	r.GET("/api/v1/library/export", h.ExportAll)
	r.POST("/api/v1/library/import", h.Import)
	r.GET("/api/v1/library/folders", h.ListFolders)
	r.POST("/api/v1/library/folders", h.CreateFolder)
	r.POST("/api/v1/library/folders/move", h.MoveFolder)
	r.DELETE("/api/v1/library/folders", h.DeleteFolder)
	r.POST("/api/v1/library/sync", h.SyncFromBucket)
	r.GET("/api/v1/admin/library/status", h.AdminStatus)
	r.GET("/api/v1/library/shared", h.ListShared)
	r.POST("/api/v1/library/shared/:id/unshare", h.AdminUnshare)
	r.DELETE("/api/v1/library/shared/:id", h.AdminDelete)
	r.GET("/api/v1/library/:id", h.Get)
	r.PUT("/api/v1/library/:id", h.Update)
	r.DELETE("/api/v1/library/:id", h.Delete)
	r.POST("/api/v1/library/:id/share", h.Share)
	r.POST("/api/v1/library/:id/move", h.MoveItem)
	r.GET("/api/v1/library/:id/export", h.ExportOne)
	return r
}

func (e *treeEnv) do(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	return libraryDo(e.t, r, method, path, body)
}

func (e *treeEnv) mustItem(r *gin.Engine, body map[string]any) libraryItem {
	e.t.Helper()
	w := e.do(r, http.MethodPost, "/api/v1/library", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create %v: %d %s", body, w.Code, w.Body.String())
	}
	var it libraryItem
	_ = json.Unmarshal(w.Body.Bytes(), &it)
	return it
}

func (e *treeEnv) code(w *httptest.ResponseRecorder) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Code
}

// drain runs the worker until the queue is empty (or fails the test).
func (e *treeEnv) drain() {
	e.t.Helper()
	for i := 0; i < 20; i++ {
		if _, err := e.pool.Exec(context.Background(), `UPDATE library_sync SET not_before = now(), locked_until = NULL`); err != nil {
			e.t.Fatal(err)
		}
		if e.sync.RunOnce(context.Background()) == 0 {
			break
		}
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync`).Scan(&n)
	if n != 0 {
		e.t.Fatalf("queue not drained: %d rows", n)
	}
}

func (e *treeEnv) person(userID string) string {
	var s string
	_ = e.pool.QueryRow(context.Background(), `SELECT library_slug FROM users WHERE id = $1`, userID).Scan(&s)
	return s
}

func hasKey(st *objectstore.MemStore, k string) bool { _, ok := st.Body(k); return ok }

// ---------------------------------------------------------------------------

func TestTree_FoldersCreateListMoveDelete(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)

	w := e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": "legal/contracts"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create folder: %d %s", w.Code, w.Body.String())
	}
	var f folderEntry
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	if f.Path != "legal/contracts" || f.Name != "contracts" {
		t.Errorf("entry = %+v", f)
	}
	// Parent was auto-created.
	w = e.do(a, http.MethodGet, "/api/v1/library/folders?scope=private&path=", nil)
	var lst struct {
		Path    string        `json:"path"`
		Folders []folderEntry `json:"folders"`
		Items   int           `json:"items"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &lst)
	if len(lst.Folders) != 1 || lst.Folders[0].Path != "legal" || lst.Folders[0].Folders != 1 {
		t.Errorf("root listing = %s", w.Body.String())
	}
	if w = e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": "legal/contracts"}); w.Code != 409 || e.code(w) != "folder_exists" {
		t.Errorf("duplicate: %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{"Legal", "a/b/c/d/e/f", "../x", "con", "a//b", "has space"} {
		if w = e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": bad}); w.Code != 400 || e.code(w) != "folder_invalid" {
			t.Errorf("path %q: %d %s", bad, w.Code, w.Body.String())
		}
	}
	if w = e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "nope", "path": "x"}); w.Code != 400 {
		t.Errorf("bad scope: %d", w.Code)
	}

	// Items placed by path; auto-created parents; list filters.
	it := e.mustItem(a, map[string]any{"kind": "skill", "name": "NDA review", "body": "Read it.", "scope": "private", "path": "legal/contracts"})
	if it.Path != "legal/contracts" || it.Scope != "private" || it.FileName != "nda-review.skill.md" {
		t.Errorf("item = %+v", it)
	}
	e.mustItem(a, map[string]any{"kind": "context", "name": "Misc", "body": "x"})
	e.mustItem(a, map[string]any{"kind": "skill", "name": "Auto parents", "body": "x", "path": "new/deep/er"})
	count := func(q string) int {
		w := e.do(a, http.MethodGet, "/api/v1/library?"+q, nil)
		var items []libraryItem
		_ = json.Unmarshal(w.Body.Bytes(), &items)
		return len(items)
	}
	if n := count("scope=private"); n != 3 {
		t.Errorf("all private = %d", n)
	}
	if n := count("scope=private&path=legal/contracts"); n != 1 {
		t.Errorf("exact path = %d", n)
	}
	if n := count("scope=private&path=legal&recursive=1"); n != 1 {
		t.Errorf("recursive = %d", n)
	}
	if n := count("scope=private&path=legal"); n != 0 {
		t.Errorf("exact parent folder must not include descendants = %d", n)
	}
	if n := count("scope=private&path=&recursive=0"); n != 1 {
		t.Errorf("root only = %d", n)
	}
	if n := count("scope=private&q=contracts"); n != 1 {
		t.Errorf("q matches path segments = %d", n)
	}
	if n := count("scope=org"); n != 0 {
		t.Errorf("org = %d", n)
	}

	// Rename = move. Items follow; the whole subtree is re-queued.
	e.drain()
	if w = e.do(a, http.MethodPost, "/api/v1/library/folders/move", map[string]any{"scope": "private", "from": "legal", "to": "law"}); w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	var mv struct{ Moved, Folders, Items int }
	_ = json.Unmarshal(w.Body.Bytes(), &mv)
	if mv.Moved != 3 || mv.Folders != 2 || mv.Items != 1 {
		t.Errorf("moved = %+v", mv)
	}
	var q int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync`).Scan(&q)
	if q != 3 {
		t.Errorf("queued after folder move = %d, want 3 (two folders and the item)", q)
	}
	p := e.person(alice)
	e.drain()
	if !hasKey(e.store, "people/"+p+"/law/contracts/nda-review.skill.md") || hasKey(e.store, "people/"+p+"/legal/contracts/nda-review.skill.md") {
		t.Errorf("bucket after rename: %v", e.store.Keys())
	}
	if v := e.store.Versions("people/" + p + "/legal/contracts/nda-review.skill.md"); v != 0 {
		t.Errorf("old key keeps %d versions after a rename", v)
	}
	if !hasKey(e.store, "people/"+p+"/law/contracts/") || hasKey(e.store, "people/"+p+"/legal/") {
		t.Errorf("markers after rename: %v", e.store.Keys())
	}
	if w = e.do(a, http.MethodPost, "/api/v1/library/folders/move", map[string]any{"scope": "private", "from": "law", "to": "law/contracts/inner"}); w.Code != 400 {
		t.Errorf("move into itself: %d", w.Code)
	}
	if w = e.do(a, http.MethodPost, "/api/v1/library/folders/move", map[string]any{"scope": "private", "from": "law", "to": "new"}); w.Code != 409 || e.code(w) != "folder_exists" {
		t.Errorf("move onto existing: %d %s", w.Code, w.Body.String())
	}
	if w = e.do(a, http.MethodPost, "/api/v1/library/folders/move", map[string]any{"scope": "private", "from": "new", "to": "x/a/b/c/d"}); w.Code != 400 {
		t.Errorf("move too deep (new/deep/er has 3 levels): %d %s", w.Code, w.Body.String())
	}

	// Delete: non-empty needs recursive.
	if w = e.do(a, http.MethodDelete, "/api/v1/library/folders?scope=private&path=law", nil); w.Code != 409 || e.code(w) != "folder_not_empty" {
		t.Errorf("non-recursive: %d %s", w.Code, w.Body.String())
	}
	w = e.do(a, http.MethodDelete, "/api/v1/library/folders?scope=private&path=law&recursive=1", nil)
	var del struct{ Folders, Items int }
	_ = json.Unmarshal(w.Body.Bytes(), &del)
	if w.Code != 200 || del.Folders != 2 || del.Items != 1 {
		t.Errorf("recursive delete: %d %s", w.Code, w.Body.String())
	}
	e.drain()
	for _, k := range e.store.Keys() {
		if strings.Contains(k, "/law/") {
			t.Errorf("object survived folder delete: %v", e.store.Keys())
		}
	}
	if v := e.store.Versions("people/" + p + "/law/contracts/nda-review.skill.md"); v != 0 {
		t.Errorf("versions left after delete: %d", v)
	}
	if w = e.do(a, http.MethodDelete, "/api/v1/library/folders?scope=private&path=law", nil); w.Code != 404 {
		t.Errorf("delete missing: %d", w.Code)
	}
	var ev int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM activity_events WHERE event_type IN ('library.folder_created','library.folder_renamed','library.folder_deleted') AND detail::text ILIKE '%law%' OR detail::text ILIKE '%legal%' OR detail::text ILIKE '%contracts%'`).Scan(&ev)
	if ev != 0 {
		t.Errorf("a folder name reached an activity row")
	}
}

func TestTree_FolderCapAndItemCapIndependent(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	for i := 0; i < libraryFoldersPerScope; i++ {
		if w := e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": fmt.Sprintf("f%d", i)}); w.Code != 201 {
			t.Fatalf("folder %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": "one-too-many"}); w.Code != 422 || e.code(w) != "folder_limit" {
		t.Errorf("201st folder: %d %s", w.Code, w.Body.String())
	}
	// An item may still be created at an existing path; folders do not count as items.
	e.mustItem(a, map[string]any{"kind": "skill", "name": "ok", "body": "x", "path": "f0"})
	// Another person has their own allowance.
	bob := e.user("bob")
	if w := e.do(e.router(bob, false), http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": "mine"}); w.Code != 201 {
		t.Errorf("bob folder: %d", w.Code)
	}
}

func TestTree_CrossUserDenialOnEveryNewRoute(t *testing.T) {
	e := newTreeEnv(t)
	alice, bob, root := e.user("alice"), e.user("bob"), e.user("root")
	a, b, adm := e.router(alice, false), e.router(bob, false), e.router(root, true)
	secret := e.mustItem(a, map[string]any{"kind": "skill", "name": "Secret plan", "body": "private body", "path": "hr/reviews"})
	e.drain()
	ap := e.person(alice)

	// Bob: an item id of Alice's 404s on move/share/get/export/update/delete.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/library/" + secret.ID + "/move", map[string]any{"scope": "org", "path": ""}},
		{http.MethodPost, "/api/v1/library/" + secret.ID + "/move", map[string]any{"scope": "private", "path": "x"}},
		{http.MethodPost, "/api/v1/library/" + secret.ID + "/share", map[string]any{"visibility": "org"}},
		{http.MethodGet, "/api/v1/library/" + secret.ID, nil},
		{http.MethodGet, "/api/v1/library/" + secret.ID + "/export", nil},
	} {
		for who, r := range map[string]*gin.Engine{"bob": b, "admin": adm} {
			if w := e.do(r, tc.method, tc.path, tc.body); w.Code != http.StatusNotFound {
				t.Errorf("%s %s %s: %d %s", who, tc.method, tc.path, w.Code, w.Body.String())
			}
		}
	}
	// Folder routes act on the caller's own tree: Alice's path does not exist for anyone else.
	for who, r := range map[string]*gin.Engine{"bob": b, "admin": adm} {
		if w := e.do(r, http.MethodGet, "/api/v1/library/folders?scope=private&path=hr", nil); w.Code != 404 {
			t.Errorf("%s folders list: %d %s", who, w.Code, w.Body.String())
		}
		if w := e.do(r, http.MethodPost, "/api/v1/library/folders/move", map[string]any{"scope": "private", "from": "hr", "to": "stolen"}); w.Code != 404 {
			t.Errorf("%s folder move: %d", who, w.Code)
		}
		if w := e.do(r, http.MethodDelete, "/api/v1/library/folders?scope=private&path=hr&recursive=1", nil); w.Code != 404 {
			t.Errorf("%s folder delete: %d", who, w.Code)
		}
		w := e.do(r, http.MethodGet, "/api/v1/library/folders?scope=private&path=", nil)
		if strings.Contains(w.Body.String(), "hr") {
			t.Errorf("%s sees alice's folder at the root: %s", who, w.Body.String())
		}
		for _, q := range []string{"scope=all", "scope=private", "scope=org", "scope=all&q=hr", "scope=all&path=hr/reviews", "scope=all&path=hr&recursive=1", "scope=mine", "scope=shared"} {
			if w := e.do(r, http.MethodGet, "/api/v1/library?"+q, nil); strings.Contains(w.Body.String(), secret.ID) {
				t.Errorf("%s list ?%s shows alice's private item", who, q)
			}
		}
		if w := e.do(r, http.MethodGet, "/api/v1/library/export", nil); strings.Contains(zipListing(t, w), "secret-plan") {
			t.Errorf("%s export holds alice's private item", who)
		}
		if w := e.do(r, http.MethodGet, "/api/v1/library/shared", nil); strings.Contains(w.Body.String(), secret.ID) {
			t.Errorf("%s governance list shows a private item", who)
		}
	}
	// All of Alice's items still exist after the attempts.
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_folders WHERE owner_user_id = $1`, alice).Scan(&n)
	if n != 2 {
		t.Errorf("alice's folders = %d after others' delete attempts", n)
	}

	// Sync reads only the caller's own prefix: Bob syncing never touches Alice's objects,
	// even though they sit in the same bucket; and a file with Alice's item id dropped into
	// Bob's prefix becomes a NEW item for Bob, leaving Alice's untouched.
	e.store.Seed("people/"+e.person(bob)+"/x/planted.skill.md", []byte("---\ndarkplum_id: "+secret.ID+"\nname: Planted\n---\n\nhijack\n"), objectstore.Meta{"darkplum-item-id": secret.ID})
	w := e.do(b, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"})
	var res syncResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res.Imported != 1 || res.Updated != 0 || len(res.Conflicts) != 0 {
		t.Fatalf("bob sync: %d %s", w.Code, w.Body.String())
	}
	var body, owner string
	_ = e.pool.QueryRow(context.Background(), `SELECT body, owner_user_id::text FROM library_items WHERE id = $1`, secret.ID).Scan(&body, &owner)
	if body != "private body" || owner != alice {
		t.Errorf("alice's item changed by bob's sync: %q owner %s", body, owner)
	}
	// Org sync is admin-only.
	if w := e.do(b, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "org"}); w.Code != 403 {
		t.Errorf("non-admin org sync: %d", w.Code)
	}
	// Admin's private sync sees nothing of Alice's even if the prefix of alice is full of objects.
	w = e.do(adm, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"})
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res.Imported+res.Updated+res.Skipped != 0 {
		t.Errorf("admin private sync touched something: %s", w.Body.String())
	}
	if !hasKey(e.store, "people/"+ap+"/hr/reviews/secret-plan.skill.md") {
		t.Errorf("alice's object vanished")
	}
	// Org folder mutations: admin only.
	if w := e.do(b, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "org", "path": "shared-area"}); w.Code != 201 {
		t.Errorf("anyone may create an org folder: %d", w.Code)
	}
	if w := e.do(b, http.MethodPost, "/api/v1/library/folders/move", map[string]any{"scope": "org", "from": "shared-area", "to": "x"}); w.Code != 403 {
		t.Errorf("non-admin org folder move: %d", w.Code)
	}
	if w := e.do(b, http.MethodDelete, "/api/v1/library/folders?scope=org&path=shared-area", nil); w.Code != 403 {
		t.Errorf("non-admin org folder delete: %d", w.Code)
	}
	if w := e.do(adm, http.MethodDelete, "/api/v1/library/folders?scope=org&path=shared-area", nil); w.Code != 200 {
		t.Errorf("admin org folder delete: %d", w.Code)
	}
}

func zipListing(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return strings.Join(names, ",")
}

func TestTree_ShareCarriesPathAndUnshareRestores(t *testing.T) {
	e := newTreeEnv(t)
	alice, bob := e.user("alice"), e.user("bob")
	a, b := e.router(alice, false), e.router(bob, false)
	adm := e.router(e.user("root"), true)

	it := e.mustItem(a, map[string]any{"kind": "skill", "name": "Code review", "body": "x", "path": "eng/guides"})
	e.drain()
	ap := e.person(alice)
	private := "people/" + ap + "/eng/guides/code-review.skill.md"
	if !hasKey(e.store, private) {
		t.Fatalf("keys = %v", e.store.Keys())
	}
	// Share: private/eng/guides/x -> org/eng/guides/x, org folders created.
	w := e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/share", map[string]any{"visibility": "org"})
	var shared libraryItem
	_ = json.Unmarshal(w.Body.Bytes(), &shared)
	if w.Code != 200 || shared.Scope != "org" || shared.Path != "eng/guides" {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	e.drain()
	if !hasKey(e.store, "org/eng/guides/code-review.skill.md") || hasKey(e.store, private) || !hasKey(e.store, "org/eng/guides/") {
		t.Errorf("after share: %v", e.store.Keys())
	}
	// Its old private versions are purged (it left the access boundary).
	if v := e.store.Versions(private); v != 0 {
		t.Errorf("old private key keeps %d versions after share", v)
	}
	// Bob can read it; Bob's own same-named item cannot be shared into the same place.
	if w := e.do(b, http.MethodGet, "/api/v1/library/"+it.ID, nil); w.Code != 200 {
		t.Errorf("bob read shared: %d", w.Code)
	}
	bobs := e.mustItem(b, map[string]any{"kind": "skill", "name": "Code review", "body": "mine", "path": "eng/guides"})
	if w := e.do(b, http.MethodPost, "/api/v1/library/"+bobs.ID+"/share", map[string]any{"visibility": "org"}); w.Code != 409 || e.code(w) != "shared_path_taken" {
		t.Errorf("collision share: %d %s", w.Code, w.Body.String())
	}
	// And Bob's still private, still at his path.
	w = e.do(b, http.MethodGet, "/api/v1/library/"+bobs.ID, nil)
	if !strings.Contains(w.Body.String(), `"scope":"private"`) || !strings.Contains(w.Body.String(), `"path":"eng/guides"`) {
		t.Errorf("bob's item moved by a failed share: %s", w.Body.String())
	}
	// The same through the move route, with a different slug it succeeds.
	if w := e.do(b, http.MethodPost, "/api/v1/library/"+bobs.ID+"/move", map[string]any{"scope": "org", "path": "eng/guides", "slug": "code-review-bob"}); w.Code != 200 {
		t.Errorf("move+rename into org: %d %s", w.Code, w.Body.String())
	}
	// Only the owner moves an org item; an admin may move it within the org tree.
	if w := e.do(b, http.MethodPost, "/api/v1/library/"+it.ID+"/move", map[string]any{"scope": "org", "path": "eng"}); w.Code != 404 {
		t.Errorf("non-owner org move: %d", w.Code)
	}
	if w := e.do(adm, http.MethodPost, "/api/v1/library/"+it.ID+"/move", map[string]any{"scope": "org", "path": "eng"}); w.Code != 200 {
		t.Errorf("admin org-to-org move: %d %s", w.Code, w.Body.String())
	}
	if w := e.do(adm, http.MethodPost, "/api/v1/library/"+it.ID+"/move", map[string]any{"scope": "private", "path": "eng/guides"}); w.Code != 204 {
		// An admin's move to private is an unshare to the owner, not a steal.
		t.Errorf("admin unshare via move: %d %s", w.Code, w.Body.String())
	}
	e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/share", map[string]any{"visibility": "org"})
	if w := e.do(adm, http.MethodPost, "/api/v1/library/shared/"+it.ID+"/unshare", nil); w.Code != 204 {
		t.Errorf("admin unshare route: %d %s", w.Code, w.Body.String())
	}
	var owner, vis string
	_ = e.pool.QueryRow(context.Background(), `SELECT owner_user_id::text, visibility FROM library_items WHERE id = $1`, it.ID).Scan(&owner, &vis)
	if owner != alice || vis != "private" {
		t.Errorf("after admin unshare owner=%s vis=%s", owner, vis)
	}
	// Restored to where it sat when shared (eng/guides), not the root.
	w = e.do(a, http.MethodGet, "/api/v1/library/"+it.ID, nil)
	if !strings.Contains(w.Body.String(), `"path":"eng/guides"`) {
		t.Errorf("unshare did not restore the path: %s", w.Body.String())
	}
	e.drain()
	if !hasKey(e.store, private) || hasKey(e.store, "org/eng/code-review.skill.md") {
		t.Errorf("after unshare: %v", e.store.Keys())
	}
	if v := e.store.Versions("org/eng/code-review.skill.md"); v != 0 {
		t.Errorf("org copy keeps %d versions after unshare", v)
	}
	// Restoring creates the folder again when it was deleted meanwhile.
	e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/share", map[string]any{"visibility": "org"})
	if w := e.do(a, http.MethodDelete, "/api/v1/library/folders?scope=private&path=eng&recursive=0", nil); w.Code != 409 && w.Code != 200 {
		t.Logf("delete eng: %d", w.Code)
	}
	e.do(a, http.MethodDelete, "/api/v1/library/folders?scope=private&path=eng&recursive=1", nil)
	if w := e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/share", map[string]any{"visibility": "private"}); w.Code != 200 {
		t.Fatalf("unshare after folder deleted: %d %s", w.Code, w.Body.String())
	}
	w = e.do(a, http.MethodGet, "/api/v1/library/"+it.ID, nil)
	if !strings.Contains(w.Body.String(), `"path":"eng/guides"`) {
		t.Errorf("folders not recreated on unshare: %s", w.Body.String())
	}
	// Private item deleted along with the folder? No: it was shared (org) during delete, so it survived.
}

func TestTree_WorkerWritesExportBytesAndIsIdempotent(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	e.mustItem(a, map[string]any{"kind": "skill", "name": "Tab\tand: colon", "description": "line1\r\nline2", "body": "# Hello\n\nbody\n\n\n", "path": "p"})
	e.mustItem(a, map[string]any{"kind": "context", "name": "Ctx", "body": "plain"})
	tool := e.mustItem(a, map[string]any{"kind": "tool", "name": "Jira", "body": `{"server_url":"https://mcp.example.com/jira","allowed_tools":["a"]}`})
	e.drain()
	puts := e.store.Calls("put")

	// Byte-identical to the export, entry by entry.
	w := e.do(a, http.MethodGet, "/api/v1/library/export", nil)
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 3 {
		t.Fatalf("zip entries = %d", len(zr.File))
	}
	for _, f := range zr.File {
		rc, _ := f.Open()
		zb, _ := io.ReadAll(rc)
		rc.Close()
		bb, ok := e.store.Body(f.Name)
		if !ok || !bytes.Equal(zb, bb) {
			t.Errorf("%s: export and bucket differ (in bucket: %v)", f.Name, ok)
		}
	}
	// Single-item export matches too, and carries the item id.
	w = e.do(a, http.MethodGet, "/api/v1/library/"+tool.ID+"/export", nil)
	bb, _ := e.store.Body("people/" + e.person(alice) + "/jira.tool.json")
	if !bytes.Equal(w.Body.Bytes(), bb) || !strings.Contains(string(bb), `"darkplum_id": "`+tool.ID+`"`) {
		t.Errorf("tool bytes differ: %s vs %s", w.Body.String(), bb)
	}
	// Tab/CR quoting survives a parse: front matter round-trips name and description.
	body, _ := e.store.Body("people/" + e.person(alice) + "/p/tab-and-colon.skill.md")
	keys, _, _ := frontMatter(string(body))
	if keys["name"] != "Tab\tand: colon" || keys["description"] != "line1\r\nline2" {
		t.Errorf("front matter round trip: %q", keys)
	}

	// Queue everything again: same state -> same bytes (idempotent).
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO library_sync (item_id) SELECT id FROM library_items ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	e.drain()
	if e.store.Calls("put") != puts+3 {
		t.Errorf("re-sync did %d puts", e.store.Calls("put")-puts)
	}
	for _, k := range e.store.Keys() {
		if k == syncSentinelKey {
			continue
		}
		if strings.Count(k, "/") > 0 && !strings.HasPrefix(k, "people/"+e.person(alice)+"/") {
			t.Errorf("unexpected key %q", k)
		}
	}
	// Every key's owner segment is the row owner's library_slug (the libkey invariant, on real rows).
	rows, _ := e.pool.Query(context.Background(), `SELECT li.s3_key, u.library_slug FROM library_items li JOIN users u ON u.id = li.owner_user_id`)
	n := 0
	for rows.Next() {
		var k, slug string
		_ = rows.Scan(&k, &slug)
		loc, perr := libkey.Parse(k)
		if perr != nil || loc.Scope != libkey.ScopeUser || loc.Person != slug {
			t.Errorf("key %q owner segment != row owner %q (%v)", k, slug, perr)
		}
		n++
	}
	rows.Close()
	if n != 3 {
		t.Errorf("checked %d keys", n)
	}
	var synced int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_items WHERE s3_synced_sha = content_sha256 AND s3_obj_sha IS NOT NULL AND s3_pending_key IS NULL`).Scan(&synced)
	if synced != 3 {
		t.Errorf("synced rows = %d", synced)
	}
	// The wire reports it.
	w = e.do(a, http.MethodGet, "/api/v1/library/"+tool.ID, nil)
	var got libraryItem
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Synced || got.S3URI == nil || !strings.HasPrefix(*got.S3URI, "s3://mem-bucket/") {
		t.Errorf("wire = synced %v uri %v", got.Synced, got.S3URI)
	}
}

func TestTree_WorkerFailuresRetryAndNeverLoseWork(t *testing.T) {
	e := newTreeEnv(t)
	logs := captureLogs(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	secretName := "Quarterly layoffs"
	it := e.mustItem(a, map[string]any{"kind": "skill", "name": secretName, "body": "x", "path": "hr-confidential"})

	// Put fails: the row stays, attempts and a code are recorded, nothing leaks into logs.
	boom := &objectstore.Error{Op: "put", Code: "SlowDown", Status: 503, RequestID: "req-1"}
	e.store.FailAlways("put", boom)
	if n := e.sync.RunOnce(context.Background()); n == 0 {
		t.Fatal("nothing claimed")
	}
	var attempts int
	var lastErr *string
	var notBefore time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT attempts, last_error, not_before FROM library_sync WHERE item_id = $1`, it.ID).Scan(&attempts, &lastErr, &notBefore); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr == nil || *lastErr != "SlowDown" || !notBefore.After(time.Now()) {
		t.Errorf("after failure: attempts %d err %v not_before %v", attempts, lastErr, notBefore)
	}
	// Backoff: not due, so an immediate pass claims nothing.
	if n := e.sync.RunOnce(context.Background()); n != 0 {
		t.Errorf("claimed a row inside its backoff: %d", n)
	}
	out := logs.String()
	for _, bad := range []string{secretName, "hr-confidential", "people/", "quarterly", e.person(alice)} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(bad)) {
			t.Errorf("log leaks %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, it.ID) || !strings.Contains(out, "SlowDown") || !strings.Contains(out, "req-1") {
		t.Errorf("log lacks item id / code / request id:\n%s", out)
	}
	// The user-facing write was never blocked by the bucket outage.
	if w := e.do(a, http.MethodPut, "/api/v1/library/"+it.ID, map[string]any{"name": secretName, "body": "x2"}); w.Code != 200 {
		t.Errorf("update during outage: %d", w.Code)
	}
	// Recovery.
	e.store.ClearFaults()
	e.drain()
	b, ok := e.store.Body("people/" + e.person(alice) + "/hr-confidential/quarterly-layoffs.skill.md")
	if !ok || !strings.Contains(string(b), "x2") {
		t.Errorf("recovered object = %q %v", b, ok)
	}

	// A move whose old-key delete fails: both copies exist, the row retries, and the old one goes.
	if w := e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/move", map[string]any{"scope": "private", "path": "other"}); w.Code != 200 {
		t.Fatalf("move: %d", w.Code)
	}
	e.store.FailAlways("delete", &objectstore.Error{Op: "delete", Code: "InternalError"})
	e.sync.RunOnce(context.Background())
	p := "people/" + e.person(alice) + "/"
	if !hasKey(e.store, p+"other/quarterly-layoffs.skill.md") || !hasKey(e.store, p+"hr-confidential/quarterly-layoffs.skill.md") {
		t.Errorf("Put-then-Delete: expected both copies during the failure, got %v", e.store.Keys())
	}
	e.store.ClearFaults()
	e.drain()
	if hasKey(e.store, p+"hr-confidential/quarterly-layoffs.skill.md") {
		t.Errorf("old copy not removed after recovery: %v", e.store.Keys())
	}

	// A change that lands mid-sync keeps its queue row (rev), so it is not lost.
	var once atomic.Bool
	e.store.FailWhen(func(op, key string) error {
		if op == "put" && strings.HasSuffix(key, "quarterly-layoffs.skill.md") && once.CompareAndSwap(false, true) {
			_, _ = e.pool.Exec(context.Background(), `UPDATE library_items SET body = 'changed mid sync', content_sha256 = 'x' WHERE id = $1`, it.ID)
			_ = enqueueItem(context.Background(), e.pool, it.ID)
		}
		return nil
	})
	_, _ = e.pool.Exec(context.Background(), `SELECT 1`)
	if err := enqueueItem(context.Background(), e.pool, it.ID); err != nil {
		t.Fatal(err)
	}
	e.sync.RunOnce(context.Background())
	var rows int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync WHERE item_id = $1`, it.ID).Scan(&rows)
	if rows != 1 {
		t.Errorf("a mid-sync change lost its queue row")
	}
	e.store.ClearFaults()
	e.drain()
	if b, _ := e.store.Body(p + "other/quarterly-layoffs.skill.md"); !strings.Contains(string(b), "changed mid sync") {
		t.Errorf("mid-sync change not applied: %s", b)
	}
}

func TestTree_ErasureThroughQueue(t *testing.T) {
	e := newTreeEnv(t)
	alice, root := e.user("alice"), e.user("root")
	a := e.router(alice, false)
	adm := e.router(root, true)
	priv := e.mustItem(a, map[string]any{"kind": "skill", "name": "Private one", "body": "x", "path": "mine"})
	pub := e.mustItem(a, map[string]any{"kind": "skill", "name": "Shared one", "body": "x", "scope": "org", "path": "team"})
	e.drain()
	ap := e.person(alice)
	privKey := "people/" + ap + "/mine/private-one.skill.md"
	if !hasKey(e.store, privKey) || !hasKey(e.store, "org/team/shared-one.skill.md") {
		t.Fatalf("keys = %v", e.store.Keys())
	}
	// Make extra versions so purge is observable.
	for i := 0; i < 2; i++ {
		e.do(a, http.MethodPut, "/api/v1/library/"+priv.ID, map[string]any{"name": "Private one", "body": fmt.Sprint("v", i)})
		e.drain()
	}
	if e.store.Versions(privKey) < 3 {
		t.Fatalf("versions = %d", e.store.Versions(privKey))
	}

	// Item delete: the row (carrying the last key) is queued in the same tx; the object is not touched until the worker runs.
	if w := e.do(a, http.MethodDelete, "/api/v1/library/"+priv.ID, nil); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
	var stale []string
	if err := e.pool.QueryRow(context.Background(), `SELECT stale_keys FROM library_sync WHERE item_id = $1`, priv.ID).Scan(&stale); err != nil || len(stale) != 1 || stale[0] != privKey {
		t.Fatalf("queued delete = %v, %v", stale, err)
	}
	if !hasKey(e.store, privKey) {
		t.Error("object removed before the worker ran")
	}
	// A failing purge keeps the row (erasure is retried, not dropped).
	e.store.FailAlways("purge", &objectstore.Error{Op: "purge", Code: "InternalError"})
	e.sync.RunOnce(context.Background())
	var rows int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync WHERE item_id = $1`, priv.ID).Scan(&rows)
	if rows != 1 {
		t.Error("erasure row dropped after a failed purge")
	}
	e.store.ClearFaults()
	e.drain()
	if hasKey(e.store, privKey) || e.store.Versions(privKey) != 0 {
		t.Errorf("not erased: %d versions", e.store.Versions(privKey))
	}

	// User delete: her private items and folder markers are purged by the worker; the shared item survives, reassigned.
	e.mustItem(a, map[string]any{"kind": "context", "name": "Another", "body": "y", "path": "gone/soon"})
	e.drain()
	anotherKey := "people/" + ap + "/gone/soon/another.context.md"
	if !hasKey(e.store, anotherKey) || !hasKey(e.store, "people/"+ap+"/gone/") {
		t.Fatalf("keys = %v", e.store.Keys())
	}
	users := NewUserHandler(e.pool, activity.NewRecorder(e.pool), []byte("pepper"))
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", root); c.Next() })
	r.DELETE("/api/v1/users/:id", users.DeleteUser)
	// The admin guard needs another admin to remain; root is the acting admin and alice is a user.
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET role = 'admin' WHERE id = $1`, root); err != nil {
		t.Fatal(err)
	}
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+alice, nil))
	if rw.Code != 204 {
		t.Fatalf("delete user: %d %s", rw.Code, rw.Body.String())
	}
	var queued int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync`).Scan(&queued)
	if queued < 3 { // another.context.md, folder gone, folder gone/soon (+ mine folder)
		t.Errorf("user delete queued %d rows", queued)
	}
	e.drain()
	for _, k := range e.store.Keys() {
		if strings.Contains(k, "people/"+ap) {
			t.Errorf("object of a deleted user survived: %v", e.store.Keys())
		}
	}
	if e.store.Versions(anotherKey) != 0 {
		t.Error("versions of a deleted user's item survive")
	}
	// The shared item is still there, now the admin's, with its key untouched.
	if w := e.do(adm, http.MethodGet, "/api/v1/library/"+pub.ID, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"mine":true`) {
		t.Errorf("reassigned shared item: %d %s", w.Code, w.Body.String())
	}
	if !hasKey(e.store, "org/team/shared-one.skill.md") {
		t.Errorf("shared object lost: %v", e.store.Keys())
	}
	// Admin delete of a shared item goes through the queue too.
	if w := e.do(adm, http.MethodDelete, "/api/v1/library/shared/"+pub.ID, nil); w.Code != 204 {
		t.Fatalf("admin delete: %d", w.Code)
	}
	e.drain()
	if hasKey(e.store, "org/team/shared-one.skill.md") || e.store.Versions("org/team/shared-one.skill.md") != 0 {
		t.Error("admin-deleted shared object survives")
	}
}

func TestTree_SyncFromBucket(t *testing.T) {
	e := newTreeEnv(t)
	alice, root := e.user("alice"), e.user("root")
	a, adm := e.router(alice, false), e.router(root, true)
	ap := e.person(alice)
	pfx := "people/" + ap + "/"

	skill := e.mustItem(a, map[string]any{"kind": "skill", "name": "Edited in bucket", "description": "d", "body": "old body", "path": "docs"})
	same := e.mustItem(a, map[string]any{"kind": "skill", "name": "Untouched", "body": "stays"})
	tool := e.mustItem(a, map[string]any{"kind": "tool", "name": "T", "body": `{"server_url":"https://mcp.example.com/a"}`})
	e.drain()
	// Give the tool a verified catalogue and a credential marker; sync must clear the catalogue only.
	_, _ = e.pool.Exec(context.Background(), `UPDATE library_items SET tool_catalog = '{"tools":[]}', tool_catalog_at = now(), tool_credential_last4 = 'abcd' WHERE id = $1`, tool.ID)

	sync := func(r *gin.Engine, scope string) (int, syncResult) {
		w := e.do(r, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": scope})
		var res syncResult
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		return w.Code, res
	}
	// Nothing changed: everything skipped.
	if code, res := sync(a, "private"); code != 200 || res.Skipped != 3 || res.Imported+res.Updated != 0 || len(res.Conflicts) != 0 {
		t.Fatalf("clean sync: %d %+v", code, res)
	}

	// A person edits the file in the console (id kept), adds a file by hand, and edits the tool's server.
	e.store.Seed(pfx+"docs/edited-in-bucket.skill.md", []byte("---\ndarkplum_id: "+skill.ID+"\nname: Edited in bucket\ndescription: d2\nkind: skill\n---\n\nnew body\n"), nil)
	e.store.Seed(pfx+"inbox/brand-new.context.md", []byte("just some context\n"), nil)
	_ = e.store.Delete(context.Background(), pfx+"t.tool.json") // renamed in the console: gone from the old name
	e.store.Seed(pfx+"jira-two.tool.json", []byte(`{"darkplum_id":"`+tool.ID+`","kind":"tool","name":"T","server_url":"https://mcp.example.com/b"}`), nil)
	e.store.Seed(pfx+"README.txt", []byte("not ours"), nil)
	e.store.Seed(pfx+"bad.tool.json", []byte(`{"server_url":"https://u:p@host/x"}`), nil)
	e.store.Seed(pfx+"toolarge.context.md", bytes.Repeat([]byte("a"), libraryBodyMaxBytes+10), nil)
	code, res := sync(a, "private")
	if code != 200 {
		t.Fatalf("sync: %d", code)
	}
	if res.Imported != 1 || res.Updated != 2 || len(res.Conflicts) != 0 || len(res.Errors) != 3 {
		t.Fatalf("result = %+v", res)
	}
	names := map[string]string{}
	for _, er := range res.Errors {
		names[er.FileName] = er.Reason
	}
	for _, f := range []string{"README.txt", "bad.tool.json", "toolarge.context.md"} {
		if names[f] == "" {
			t.Errorf("no error for %s: %+v", f, res.Errors)
		}
	}
	// A second sync BEFORE the worker has rewritten anything must not conflict with itself.
	if _, res2 := sync(a, "private"); len(res2.Conflicts) != 0 || res2.Imported != 0 || res2.Updated != 0 {
		t.Errorf("back-to-back sync = %+v", res2)
	}
	var body, desc string
	_ = e.pool.QueryRow(context.Background(), `SELECT body, description FROM library_items WHERE id = $1`, skill.ID).Scan(&body, &desc)
	if body != "new body" || desc != "d2" {
		t.Errorf("bucket edit not adopted: %q %q", body, desc)
	}
	var cat *string
	var last4 *string
	var url string
	_ = e.pool.QueryRow(context.Background(), `SELECT tool_catalog::text, tool_credential_last4, body FROM library_items WHERE id = $1`, tool.ID).Scan(&cat, &last4, &url)
	if cat != nil || last4 == nil || *last4 != "abcd" || !strings.Contains(url, "/b") {
		t.Errorf("tool after sync: catalog %v last4 %v body %s", cat, last4, url)
	}
	var newPath, newKind string
	_ = e.pool.QueryRow(context.Background(), `SELECT fo.path, li.kind FROM library_items li JOIN library_folders fo ON fo.id = li.folder_id WHERE li.name = 'brand new'`).Scan(&newPath, &newKind)
	if newPath != "inbox" || newKind != "context" {
		t.Errorf("new item at %q kind %q", newPath, newKind)
	}
	// The tool item moved to its new file name in the bucket? It follows Postgres (slug jira-two) and the queue rewrites canonical bytes.
	e.drain()
	if b, ok := e.store.Body(pfx + "jira-two.tool.json"); !ok || !strings.Contains(string(b), tool.ID) {
		t.Errorf("canonical rewrite missing: %v", e.store.Keys())
	}
	// Idempotent: the second run has nothing to do (the dud files still error).
	if _, res := sync(a, "private"); res.Imported+res.Updated != 0 || len(res.Conflicts) != 0 {
		t.Errorf("second sync = %+v", res)
	}

	// Conflict: Postgres changed since the last sync AND the bucket object was edited.
	e.store.Seed(pfx+"docs/edited-in-bucket.skill.md", []byte("---\ndarkplum_id: "+skill.ID+"\nname: Edited in bucket\n---\n\nbucket wins?\n"), nil)
	if w := e.do(a, http.MethodPut, "/api/v1/library/"+skill.ID, map[string]any{"name": "Edited in bucket", "body": "postgres wins"}); w.Code != 200 {
		t.Fatalf("update: %d", w.Code)
	}
	_, res = sync(a, "private")
	if len(res.Conflicts) != 1 || res.Conflicts[0] != skill.ID || res.Updated != 0 {
		t.Errorf("conflict result = %+v", res)
	}
	_ = e.pool.QueryRow(context.Background(), `SELECT body FROM library_items WHERE id = $1`, skill.ID).Scan(&body)
	if body != "postgres wins" {
		t.Errorf("conflict let the bucket win: %q", body)
	}
	e.drain()
	if b, _ := e.store.Body(pfx + "docs/edited-in-bucket.skill.md"); !strings.Contains(string(b), "postgres wins") {
		t.Errorf("worker did not restore Postgres's version: %s", b)
	}

	// A file deleted from the bucket is not a deletion: the item stays and is re-put when synced.
	_ = e.store.Delete(context.Background(), pfx+"untouched.skill.md")
	if _, res := sync(a, "private"); res.Updated != 0 {
		t.Errorf("sync after bucket delete = %+v", res)
	}
	var still int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_items WHERE id = $1`, same.ID).Scan(&still)
	if still != 1 {
		t.Error("bucket delete removed a library item")
	}
	e.do(a, http.MethodPut, "/api/v1/library/"+same.ID, map[string]any{"name": "Untouched", "body": "stays"})
	e.drain()
	if !hasKey(e.store, pfx+"untouched.skill.md") {
		t.Error("deleted object not re-put")
	}

	// Org sync: admin only; new items are owned by the acting admin.
	e.store.Seed("org/handbook/policy.context.md", []byte("org text\n"), nil)
	code, res = sync(adm, "org")
	if code != 200 || res.Imported != 1 {
		t.Fatalf("org sync: %d %+v", code, res)
	}
	var owner string
	_ = e.pool.QueryRow(context.Background(), `SELECT owner_user_id::text FROM library_items WHERE slug = 'policy'`).Scan(&owner)
	if owner != root {
		t.Errorf("imported org item owner = %s, want the acting admin", owner)
	}
	// Store off: a clear 503.
	e.h.SetStore(nil)
	if w := e.do(a, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"}); w.Code != 503 || e.code(w) != "library_storage_unavailable" {
		t.Errorf("no store: %d %s", w.Code, w.Body.String())
	}
	e.h.SetStore(e.store)
	// Store unreachable: 503 with the same code.
	e.store.FailAlways("list", errors.New("down"))
	if w := e.do(a, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"}); w.Code != 503 || e.code(w) != "library_storage_unavailable" {
		t.Errorf("unreachable: %d %s", w.Code, w.Body.String())
	}
	var ev int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM activity_events WHERE event_type = 'library.synced'`).Scan(&ev)
	if ev < 3 {
		t.Errorf("synced events = %d", ev)
	}
}

func TestTree_ReconcileRequeuesAndReportsForeignWithoutDeleting(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	it := e.mustItem(a, map[string]any{"kind": "skill", "name": "R", "body": "x"})
	e.drain()
	e.store.Seed("stranger/file.txt", []byte("not ours"), nil)
	e.store.Seed("org/foreign.skill.md", []byte("x"), nil)
	// Drift: Postgres body changed without a queue row (a missed enqueue).
	_, _ = e.pool.Exec(context.Background(), `UPDATE library_items SET body = 'drift', content_sha256 = 'zzz' WHERE id = $1`, it.ID)
	e.sync.Reconcile(context.Background())
	var q int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync WHERE item_id = $1`, it.ID).Scan(&q)
	if q != 1 {
		t.Error("drift not re-queued")
	}
	n, at := e.sync.ForeignObjects()
	if n != 2 || at.IsZero() {
		t.Errorf("foreign = %d at %v", n, at)
	}
	if !hasKey(e.store, "stranger/file.txt") || !hasKey(e.store, "org/foreign.skill.md") {
		t.Error("reconciler deleted a foreign object")
	}
	if e.store.Calls("delete") != 0 || e.store.Calls("purge") != 0 {
		t.Error("reconciler issued deletes")
	}
}

func TestTree_HealthSentinelIsCachedAndNotAPanic(t *testing.T) {
	e := newTreeEnv(t)
	e.sync.CheckHealth(context.Background())
	if h := e.sync.Health(); !h.Reachable || !hasKey(e.store, syncSentinelKey) {
		t.Errorf("health = %+v", h)
	}
	e.store.FailAlways("head", &objectstore.Error{Op: "head", Code: "AccessDenied", Status: 403})
	e.sync.CheckHealth(context.Background())
	if h := e.sync.Health(); h.Reachable || h.Code != "AccessDenied" {
		t.Errorf("health = %+v", h)
	}
}

func TestTree_NoStoreBehavesAsBefore(t *testing.T) {
	pool := setupTestDB(t)
	h := newLibraryHandlerForTest(pool) // no store
	e := &treeEnv{t: t, pool: pool, h: h}
	alice := e.user("alice")
	a := e.router(alice, false)
	it := e.mustItem(a, map[string]any{"kind": "skill", "name": "Plain", "body": "x"})
	if it.Synced || it.S3URI != nil {
		t.Errorf("synced/uri without a store: %+v", it)
	}
	w := e.do(a, http.MethodGet, "/api/v1/library", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), it.ID) {
		t.Errorf("list: %d", w.Code)
	}
	var q int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync`).Scan(&q)
	if q != 1 {
		t.Errorf("queue rows accumulate while the worker is off: %d", q)
	}
}

func TestTree_ToolURLUserinfoRejected(t *testing.T) {
	in := libraryInput{Kind: "tool", Name: "x", Body: `{"server_url":"https://user:pw@host/x"}`}
	if msg := validateLibraryInput(&in, false); msg == "" || !strings.Contains(msg, "credentials") {
		t.Errorf("userinfo accepted: %q", msg)
	}
}

func TestTree_AdminStatus(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	e.mustItem(a, map[string]any{"kind": "skill", "name": "S", "body": "x"})
	e.sync.CheckHealth(context.Background())
	var st struct {
		Enabled             bool       `json:"enabled"`
		Reachable           bool       `json:"reachable"`
		Bucket              string     `json:"bucket"`
		QueueDepth          int        `json:"queue_depth"`
		Failing             int        `json:"failing"`
		OldestQueuedSeconds int        `json:"oldest_queued_seconds"`
		LastReconcileAt     *time.Time `json:"last_reconcile_at"`
		ForeignObjects      int        `json:"foreign_objects"`
	}
	w := e.do(e.router(e.user("root"), true), http.MethodGet, "/api/v1/admin/library/status", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || !st.Enabled || !st.Reachable || st.Bucket != "mem-bucket" || st.QueueDepth != 1 || st.Failing != 0 || st.LastReconcileAt != nil {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	for _, leak := range []string{"people/", "alice"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("status leaks %q", leak)
		}
	}
	e.sync.Reconcile(context.Background())
	e.store.FailAlways("head", &objectstore.Error{Op: "head", Code: "AccessDenied", Status: 403})
	e.sync.CheckHealth(context.Background())
	_, _ = e.pool.Exec(context.Background(), `UPDATE library_sync SET attempts = 6, enqueued_at = now() - interval '90 seconds'`)
	w = e.do(e.router(e.user("root2"), true), http.MethodGet, "/api/v1/admin/library/status", nil)
	st.LastReconcileAt = nil
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Reachable || st.Failing != 1 || st.OldestQueuedSeconds < 89 || st.LastReconcileAt == nil || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("degraded status = %s", w.Body.String())
	}
}

func TestTree_ReconcileVerifiesObjectsAndNeverDeletes(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	gone := e.mustItem(a, map[string]any{"kind": "skill", "name": "Gone", "body": "x"})
	swapped := e.mustItem(a, map[string]any{"kind": "skill", "name": "Swapped", "body": "x"})
	fine := e.mustItem(a, map[string]any{"kind": "skill", "name": "Fine", "body": "x"})
	unknown := e.mustItem(a, map[string]any{"kind": "skill", "name": "Unknown", "body": "x"})
	e.drain()
	p := "people/" + e.person(alice) + "/"
	_ = e.store.Delete(context.Background(), p+"gone.skill.md")               // deleted in the console
	e.store.Seed(p+"swapped.skill.md", []byte("someone else's bytes\n"), nil) // replaced: metadata gone
	e.store.FailWhen(func(op, key string) error {                             // an outage says nothing about the object
		if op == "head" && key == p+"unknown.skill.md" {
			return &objectstore.Error{Op: "head", Code: "InternalError", Status: 500}
		}
		return nil
	})
	deletes, purges := e.store.Calls("delete"), e.store.Calls("purge")
	e.sync.Reconcile(context.Background())
	queued := map[string]bool{}
	rows, _ := e.pool.Query(context.Background(), `SELECT item_id::text FROM library_sync WHERE item_id IS NOT NULL`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		queued[id] = true
	}
	rows.Close()
	if !queued[gone.ID] || queued[swapped.ID] || queued[fine.ID] || queued[unknown.ID] || len(queued) != 1 {
		t.Errorf("requeued = %v (only the MISSING object is re-queued)", queued)
	}
	flagged := func(id string) bool {
		var f bool
		_ = e.pool.QueryRow(context.Background(), `SELECT bucket_changed_at IS NOT NULL FROM library_items WHERE id = $1`, id).Scan(&f)
		return f
	}
	if !flagged(swapped.ID) || flagged(gone.ID) || flagged(fine.ID) || flagged(unknown.ID) {
		t.Errorf("flags: swapped=%v gone=%v fine=%v unknown=%v", flagged(swapped.ID), flagged(gone.ID), flagged(fine.ID), flagged(unknown.ID))
	}
	if w := e.do(a, http.MethodGet, "/api/v1/library/"+swapped.ID, nil); !strings.Contains(w.Body.String(), `"bucket_changed":true`) || strings.Contains(w.Body.String(), `"synced":true`) {
		t.Errorf("wire for a changed object: %s", w.Body.String())
	}
	if e.store.Calls("delete") != deletes || e.store.Calls("purge") != purges {
		t.Error("reconciler deleted something")
	}
	// The wire stops claiming they are synced, then the worker restores them.
	w := e.do(a, http.MethodGet, "/api/v1/library/"+gone.ID, nil)
	if strings.Contains(w.Body.String(), `"synced":true`) {
		t.Error("deleted object still reported synced")
	}
	e.store.ClearFaults()
	e.drain()
	if !hasKey(e.store, p+"gone.skill.md") {
		t.Error("deleted file not put back")
	}
	if b, _ := e.store.Body(p + "swapped.skill.md"); string(b) != "someone else's bytes\n" {
		t.Errorf("a replaced object was overwritten: %q", b)
	}
	e.sync.Reconcile(context.Background())
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync`).Scan(&n)
	if n != 0 {
		t.Errorf("a healthy bucket re-queued %d rows", n)
	}
}

func TestTree_BucketEditIsNeverOverwrittenUntilSynced(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	adm := e.router(e.user("root"), true)
	it := e.mustItem(a, map[string]any{"kind": "skill", "name": "Held", "body": "v1"})
	e.drain()
	key := "people/" + e.person(alice) + "/held.skill.md"
	flag := func() bool {
		var f bool
		_ = e.pool.QueryRow(context.Background(), `SELECT bucket_changed_at IS NOT NULL FROM library_items WHERE id = $1`, it.ID).Scan(&f)
		return f
	}

	// 1. Metadata missing (aws s3 cp drops it) with different bytes: flagged, not re-queued.
	edited := []byte("---\ndarkplum_id: " + it.ID + "\nname: Held\n---\n\nedited in the bucket\n")
	e.store.Seed(key, edited, nil)
	e.sync.Reconcile(context.Background())
	var q int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_sync`).Scan(&q)
	if !flag() || q != 0 {
		t.Fatalf("missing metadata: flag=%v queued=%d", flag(), q)
	}
	// 2. Metadata present but different.
	e.store.Seed(key, edited, objectstore.Meta{"darkplum-sha256": "something-else"})
	_, _ = e.pool.Exec(context.Background(), `UPDATE library_items SET bucket_changed_at = NULL WHERE id = $1`, it.ID)
	e.sync.Reconcile(context.Background())
	if !flag() {
		t.Error("differing metadata not flagged")
	}
	w := e.do(a, http.MethodGet, "/api/v1/library/"+it.ID, nil)
	if !strings.Contains(w.Body.String(), `"bucket_changed":true`) {
		t.Errorf("wire: %s", w.Body.String())
	}
	var st struct {
		Changed int `json:"changed_in_bucket"`
	}
	w = e.do(adm, http.MethodGet, "/api/v1/admin/library/status", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Changed != 1 {
		t.Errorf("status changed_in_bucket = %d: %s", st.Changed, w.Body.String())
	}

	// 3. An app edit made meanwhile does NOT clobber the bucket: the worker holds back, leaves the row, no failure counted.
	if w := e.do(a, http.MethodPut, "/api/v1/library/"+it.ID, map[string]any{"name": "Held", "body": "v2 from the app"}); w.Code != 200 {
		t.Fatalf("update: %d", w.Code)
	}
	puts := e.store.Calls("put")
	e.sync.RunOnce(context.Background())
	var attempts int
	var lastErr *string
	var notBefore time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT attempts, last_error, not_before FROM library_sync WHERE item_id = $1`, it.ID).Scan(&attempts, &lastErr, &notBefore); err != nil {
		t.Fatalf("row gone: %v", err)
	}
	if b, _ := e.store.Body(key); !bytes.Equal(b, edited) || e.store.Calls("put") != puts {
		t.Errorf("worker overwrote the bucket's version: %q", b)
	}
	if attempts != 0 || lastErr == nil || *lastErr != "bucket_changed" || !notBefore.After(time.Now().Add(time.Minute)) {
		t.Errorf("parked row: attempts %d err %v not_before %v", attempts, lastErr, notBefore)
	}
	if !flag() {
		t.Error("flag lost")
	}

	// 4. Sync reports the conflict (Postgres changed AND the bucket did), clears the flag, and the app version then wins.
	w = e.do(a, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"})
	var res syncResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Conflicts) != 1 || res.Conflicts[0] != it.ID || res.Updated != 0 {
		t.Fatalf("sync = %s", w.Body.String())
	}
	if flag() {
		t.Error("Sync did not clear the flag")
	}
	e.sync.RunOnce(context.Background())
	if b, _ := e.store.Body(key); !strings.Contains(string(b), "v2 from the app") {
		t.Errorf("app version did not win after Sync: %q", b)
	}
	e.drain()
	if flag() {
		t.Error("flag set after the worker wrote")
	}

	// 5. A clean edit (Postgres untouched) is adopted by Sync and the flag cleared; the worker then rewrites the canonical form without holding back.
	e.store.Seed(key, []byte("---\ndarkplum_id: "+it.ID+"\nname: Held\n---\n\nadopted edit\n"), nil)
	e.sync.Reconcile(context.Background())
	if !flag() {
		t.Fatal("second edit not flagged")
	}
	w = e.do(a, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"})
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Updated != 1 || flag() {
		t.Errorf("adopt: %s flag=%v", w.Body.String(), flag())
	}
	e.drain()
	var body string
	_ = e.pool.QueryRow(context.Background(), `SELECT body FROM library_items WHERE id = $1`, it.ID).Scan(&body)
	if body != "adopted edit" {
		t.Errorf("body = %q", body)
	}
	if b, _ := e.store.Body(key); !strings.Contains(string(b), "adopted edit") || !strings.Contains(string(b), "darkplum_id") {
		t.Errorf("canonical rewrite after adoption: %q", b)
	}

	// 6. A missing object is still simply put back, flag or not.
	_ = e.store.Delete(context.Background(), key)
	e.sync.Reconcile(context.Background())
	e.drain()
	if !hasKey(e.store, key) {
		t.Error("missing object not re-put")
	}
	// 7. An identical re-upload that lost its metadata does not block the worker.
	b, _ := e.store.Body(key)
	e.store.Seed(key, b, nil)
	e.do(a, http.MethodPut, "/api/v1/library/"+it.ID, map[string]any{"name": "Held", "body": "v3"})
	e.drain()
	if b, _ := e.store.Body(key); !strings.Contains(string(b), "v3") {
		t.Errorf("identical re-upload blocked the worker: %q", b)
	}
}

func TestTree_SyncTruncationAndDuplicates(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	pfx := "people/" + e.person(alice) + "/"
	it := e.mustItem(a, map[string]any{"kind": "skill", "name": "Orig", "body": "x"})
	e.drain()
	// Console "Copy": same bytes, same darkplum_id, new name.
	b, _ := e.store.Body(pfx + "orig.skill.md")
	e.store.Seed(pfx+"orig-copy.skill.md", b, nil)
	w := e.do(a, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"})
	var res syncResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res.Truncated || res.Scanned != 2 || res.Imported+res.Updated != 0 || len(res.Errors) != 1 ||
		res.Errors[0].FileName != "orig-copy.skill.md" || res.Errors[0].Reason != "duplicate of an existing item; delete one copy" {
		t.Fatalf("duplicate result = %s", w.Body.String())
	}
	var cnt int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_items WHERE id = $1`, it.ID).Scan(&cnt)
	if cnt != 1 {
		t.Error("item lost")
	}
	_ = e.store.Delete(context.Background(), pfx+"orig-copy.skill.md")

	// Over the object cap: truncated, scanned = the cap.
	for i := 0; i < syncRouteMaxObjects+5; i++ {
		e.store.Seed(fmt.Sprintf("%sjunk/f%05d.txt", pfx, i), []byte("x"), nil)
	}
	w = e.do(a, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"})
	res = syncResult{}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || !res.Truncated || res.Scanned == 0 || res.Scanned > syncRouteMaxObjects || len(res.Errors) > syncRouteMaxErrors {
		t.Errorf("cap result: scanned %d truncated %v errors %d", res.Scanned, res.Truncated, len(res.Errors))
	}
	for _, k := range e.store.Keys() {
		if strings.Contains(k, "/junk/") {
			_ = e.store.Delete(context.Background(), k)
		}
	}
	// Time budget exhausted: truncated before anything is read.
	old := syncTimeBudget
	syncTimeBudget = -time.Second
	defer func() { syncTimeBudget = old }()
	w = e.do(a, http.MethodPost, "/api/v1/library/sync", map[string]any{"scope": "private"})
	res = syncResult{}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || !res.Truncated || res.Scanned != 0 {
		t.Errorf("budget result = %s", w.Body.String())
	}
}

func TestTree_ToolURLSecretQueryRejected(t *testing.T) {
	for _, q := range []string{"token=abc", "API_KEY=1", "access_token=x", "x-api-key=1", "apikey=1", "Password=p", "auth=1", "signature=zz", "sig=1", "client_secret=s", "key=1", "passwd=1", "access_key=1", "a=1&secret=2"} {
		in := libraryInput{Kind: "tool", Name: "x", Body: `{"server_url":"https://mcp.example.com/x?` + q + `"}`}
		if msg := validateLibraryInput(&in, false); msg != toolURLSecretMsg {
			t.Errorf("?%s: %q", q, msg)
		}
	}
	for _, q := range []string{"", "?format=json", "?author=bob", "?design=1", "?monkey=1", "?page=2&tenant=acme"} {
		in := libraryInput{Kind: "tool", Name: "x", Body: `{"server_url":"https://mcp.example.com/x` + q + `"}`}
		if msg := validateLibraryInput(&in, false); msg != "" {
			t.Errorf("%s refused: %q", q, msg)
		}
	}
	// Over HTTP: 400 with the code, on create and update, 422 on import.
	e := newTreeEnv(t)
	a := e.router(e.user("alice"), false)
	w := e.do(a, http.MethodPost, "/api/v1/library", map[string]any{"kind": "tool", "name": "T", "body": `{"server_url":"https://h.example/x?token=abc"}`})
	if w.Code != 400 || e.code(w) != "tool_url_secret" || !strings.Contains(w.Body.String(), "credential field") {
		t.Errorf("create: %d %s", w.Code, w.Body.String())
	}
	ok := e.mustItem(a, map[string]any{"kind": "tool", "name": "T", "body": `{"server_url":"https://h.example/x"}`})
	w = e.do(a, http.MethodPut, "/api/v1/library/"+ok.ID, map[string]any{"name": "T", "body": `{"server_url":"https://h.example/x?api_key=1"}`})
	if w.Code != 400 || e.code(w) != "tool_url_secret" {
		t.Errorf("update: %d %s", w.Code, w.Body.String())
	}
	w = e.do(a, http.MethodPost, "/api/v1/library/import", map[string]any{"filename": "t.json", "content": `{"server_url":"https://h.example/x?secret=1"}`})
	if w.Code != 422 || e.code(w) != "tool_url_secret" {
		t.Errorf("import: %d %s", w.Code, w.Body.String())
	}
	// An item stored before the rule still renders (the rule is on write).
	_, _ = e.pool.Exec(context.Background(), `UPDATE library_items SET body = '{"server_url":"https://h.example/x?token=old"}' WHERE id = $1`, ok.ID)
	if w := e.do(a, http.MethodGet, "/api/v1/library/"+ok.ID, nil); w.Code != 200 {
		t.Errorf("read of a pre-existing item: %d", w.Code)
	}
}

func TestTree_AdminUnshareNeverCreatesFoldersInSomeoneElsesTree(t *testing.T) {
	e := newTreeEnv(t)
	alice := e.user("alice")
	a := e.router(alice, false)
	adm := e.router(e.user("root"), true)
	folders := func() int {
		var n int
		_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM library_folders WHERE owner_user_id = $1`, alice).Scan(&n)
		return n
	}
	pathOf := func(id string) string {
		w := e.do(a, http.MethodGet, "/api/v1/library/"+id, nil)
		var it libraryItem
		_ = json.Unmarshal(w.Body.Bytes(), &it)
		return it.Path
	}
	it := e.mustItem(a, map[string]any{"kind": "skill", "name": "Shared doc", "body": "x", "path": "eng/guides"})
	e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/share", map[string]any{"visibility": "org"})

	// The owner's folders are deleted while the item is shared: the admin's unshare falls back to the root and creates nothing.
	if w := e.do(a, http.MethodDelete, "/api/v1/library/folders?scope=private&path=eng&recursive=1", nil); w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	before := folders()
	// The requested path and slug are ignored for an admin.
	w := e.do(adm, http.MethodPost, "/api/v1/library/"+it.ID+"/move", map[string]any{"scope": "private", "path": "evil/planted", "slug": "renamed"})
	if w.Code != 204 {
		t.Fatalf("admin move to private: %d %s", w.Code, w.Body.String())
	}
	if folders() != before || pathOf(it.ID) != "" {
		t.Errorf("folders %d -> %d, path %q", before, folders(), pathOf(it.ID))
	}
	var slug string
	_ = e.pool.QueryRow(context.Background(), `SELECT slug FROM library_items WHERE id = $1`, it.ID).Scan(&slug)
	if slug != "shared-doc" {
		t.Errorf("admin renamed the item: %q", slug)
	}
	// With the folder still there, the previous path is restored.
	e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": "eng/guides"})
	e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/share", map[string]any{"visibility": "org"})
	// shared from the root now, so previous path is ""; move it into the folder first.
	e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/move", map[string]any{"scope": "org", "path": ""})
	e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/move", map[string]any{"scope": "private", "path": "eng/guides"})
	e.do(a, http.MethodPost, "/api/v1/library/"+it.ID+"/share", map[string]any{"visibility": "org"})
	if w := e.do(adm, http.MethodPost, "/api/v1/library/shared/"+it.ID+"/unshare", nil); w.Code != 204 {
		t.Fatalf("unshare: %d", w.Code)
	}
	if pathOf(it.ID) != "eng/guides" {
		t.Errorf("existing folder not restored: %q", pathOf(it.ID))
	}
}

func TestTree_ListFoldersRecursive(t *testing.T) {
	e := newTreeEnv(t)
	alice, bob := e.user("alice"), e.user("bob")
	a, b := e.router(alice, false), e.router(bob, false)
	for _, p := range []string{"a/b/c", "a/d", "z"} {
		e.do(a, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": p})
	}
	e.do(b, http.MethodPost, "/api/v1/library/folders", map[string]any{"scope": "private", "path": "bobs/secret"})
	paths := func(r *gin.Engine, q string) []string {
		w := e.do(r, http.MethodGet, "/api/v1/library/folders?"+q, nil)
		var l struct{ Folders []folderEntry }
		_ = json.Unmarshal(w.Body.Bytes(), &l)
		var out []string
		for _, f := range l.Folders {
			out = append(out, f.Path)
		}
		return out
	}
	if got := strings.Join(paths(a, "scope=private&path=&recursive=1"), ","); got != "a,a/b,a/b/c,a/d,z" {
		t.Errorf("whole tree = %s", got)
	}
	if got := strings.Join(paths(a, "scope=private&path=a&recursive=1"), ","); got != "a/b,a/b/c,a/d" {
		t.Errorf("under a = %s", got)
	}
	if got := strings.Join(paths(a, "scope=private&path=a"), ","); got != "a/b,a/d" {
		t.Errorf("direct children = %s", got)
	}
	// Never another person's tree.
	if got := strings.Join(paths(b, "scope=private&path=&recursive=1"), ","); got != "bobs,bobs/secret" {
		t.Errorf("bob's tree = %s", got)
	}
	if w := e.do(b, http.MethodGet, "/api/v1/library/folders?scope=private&path=a&recursive=1", nil); w.Code != 404 {
		t.Errorf("bob into alice's path: %d", w.Code)
	}
}
