package handler

// Folders and moves. Folders are rows in library_folders (migration 031);
// `path` is the slug path from the root, kept current here on create/move.
// Display names are user content: they never reach a log line or an activity
// row. Audit rows carry ids and counts only.
//
// Who may do what:
//   - private tree: its owner, nobody else (an id that is not yours 404s).
//   - org tree: anyone may create a folder (sharing needs somewhere to go);
//     moving, renaming and deleting an org folder affects other people's
//     items, so those are administrator-only.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
	"github.com/scalecraft-dev/darkplum/api/internal/libkey"
)

const libraryFoldersPerScope = 200

func ownerArg(scope, owner string) any {
	if scope == "org" {
		return nil
	}
	return owner
}

// lockFolderScope serialises folder creation per owner (or for the org tree)
// so the folder cap cannot be overshot.
func lockFolderScope(ctx context.Context, tx pgx.Tx, scope, owner string) error {
	if scope == "org" {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('darkplum_library_org_folders'))`)
		return err
	}
	_, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, owner)
	return err
}

// ensureFolderPath resolves a folder path to its id, creating every missing
// segment (each created folder is queued for its bucket marker). "" is the
// root and returns a nil id. leafCreated says whether the last segment is new.
func ensureFolderPath(ctx context.Context, tx pgx.Tx, scope, owner, path string) (id *string, leafCreated bool, err error) {
	if path == "" {
		return nil, false, nil
	}
	if !libkey.ValidFolderPath(path) {
		return nil, false, errFolderInvalid
	}
	segs := strings.Split(path, "/")
	var parent *string
	locked := false
	for i, seg := range segs {
		cur := strings.Join(segs[:i+1], "/")
		var fid string
		err := tx.QueryRow(ctx, `SELECT id::text FROM library_folders WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND path = $3`,
			scope, ownerArg(scope, owner), cur).Scan(&fid)
		created := false
		if errors.Is(err, pgx.ErrNoRows) {
			if !locked {
				if err := lockFolderScope(ctx, tx, scope, owner); err != nil {
					return nil, false, err
				}
				locked = true
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM library_folders WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid`,
				scope, ownerArg(scope, owner)).Scan(&n); err != nil {
				return nil, false, err
			}
			if n >= libraryFoldersPerScope {
				return nil, false, errFolderLimit
			}
			err = tx.QueryRow(ctx, `
				INSERT INTO library_folders (scope, owner_user_id, parent_id, slug, display_name, path)
				VALUES ($1, $2::uuid, $3::uuid, $4, $4, $5)
				ON CONFLICT DO NOTHING RETURNING id::text`,
				scope, ownerArg(scope, owner), parent, seg, cur).Scan(&fid)
			if errors.Is(err, pgx.ErrNoRows) { // lost a race: it exists now
				err = tx.QueryRow(ctx, `SELECT id::text FROM library_folders WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND path = $3`,
					scope, ownerArg(scope, owner), cur).Scan(&fid)
			} else if err == nil {
				created = true
				if err := enqueueFolder(ctx, tx, fid); err != nil {
					return nil, false, err
				}
			}
		}
		if err != nil {
			return nil, false, err
		}
		if i == len(segs)-1 {
			leafCreated = created
		}
		f := fid
		parent = &f
	}
	return parent, leafCreated, nil
}

// ---------------------------------------------------------------------------
// Item move
// ---------------------------------------------------------------------------

type moveOutcome struct {
	kind           string
	fromVis, toVis string
	slugChanged    bool
	pathChanged    bool
	noop           bool
}

// moveItem relocates one item: a new folder, a new slug, and/or the other
// root. private -> org IS the share (the folder path is carried over unless
// toPath is given, missing org folders are created, and a (kind, slug)
// collision there is shared_path_taken); org -> private is the unshare (back
// to previous_folder_path, falling back to the root). The owner may move
// their items anywhere; an administrator may move org items within the org
// tree and unshare them. The bucket move is Put-then-Delete by the worker, so
// for a moment both copies exist; that is normal.
func (h *LibraryHandler) moveItem(ctx context.Context, itemID, actor string, admin bool, toScope string, toPath *string, newSlug string) (*moveOutcome, error) {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kind, vis, slug, curPath string
	var owner, prevPath *string
	err = tx.QueryRow(ctx, `
		SELECT li.kind, li.owner_user_id::text, li.visibility, li.slug, COALESCE(fo.path, ''), li.previous_folder_path
		FROM library_items li LEFT JOIN library_folders fo ON fo.id = li.folder_id
		WHERE li.id = $1 AND (li.owner_user_id = $2 OR ($3 AND li.visibility = 'org'))
		FOR UPDATE OF li`, itemID, actor, admin).Scan(&kind, &owner, &vis, &slug, &curPath, &prevPath)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errItemNotFound
	}
	if err != nil {
		return nil, err
	}
	isOwner := owner != nil && *owner == actor
	toVis := visibilityOfScope(toScope)
	if !isOwner && vis != libraryVisibilityOrg {
		return nil, errItemNotFound
	}
	if !isOwner && !admin {
		return nil, errItemNotFound
	}
	// Folders of a private destination belong to the item's owner.
	var destOwner string
	if owner != nil {
		destOwner = *owner
	}
	if toScope == "user" && destOwner == "" {
		return nil, errNoOwner
	}

	out := &moveOutcome{kind: kind, fromVis: vis, toVis: toVis}
	// An administrator returning someone else's shared item to private does
	// not choose where it goes (nor rename it): it goes back to the folder it
	// was shared from, or the root, and no folder is ever created in another
	// person's private tree.
	adminUnshare := !isOwner && toScope == "user"
	if adminUnshare {
		toPath, newSlug = nil, ""
	}
	path := curPath
	switch {
	case toPath != nil:
		path = *toPath
	case vis == libraryVisibilityOrg && toVis == libraryVisibilityPrivate:
		path = ""
		if prevPath != nil {
			path = *prevPath
		}
	}
	if !libkey.ValidFolderPath(path) {
		return nil, errFolderInvalid
	}
	newSlug = strings.TrimSpace(newSlug)
	if newSlug == "" {
		newSlug = slug
	}
	if !libkey.ValidSlug(newSlug) {
		return nil, &libError{http.StatusBadRequest, "slug_invalid", "that slug cannot be used as a file name"}
	}
	out.slugChanged = newSlug != slug
	out.pathChanged = path != curPath
	if vis == toVis && !out.slugChanged && !out.pathChanged {
		out.noop = true
		return out, nil
	}

	var folderID *string
	if adminUnshare {
		if path != "" {
			id, ferr := h.folderID(ctx, tx, "user", destOwner, path)
			switch {
			case ferr == nil:
				folderID = &id
			case errors.Is(ferr, errFolderNotFound):
				path = "" // the folder is gone: root, nothing created
			default:
				return nil, ferr
			}
		}
	} else {
		folderID, _, err = ensureFolderPath(ctx, tx, toScope, destOwner, path)
		if err != nil {
			return nil, err
		}
	}
	args := []any{itemID, toVis, folderID, newSlug, actor}
	prev := "previous_folder_path"
	switch {
	case vis == libraryVisibilityPrivate && toVis == libraryVisibilityOrg:
		// Remember where it sat so an unshare can put it back.
		args = append(args, curPath)
		prev = "$6"
	case vis == libraryVisibilityOrg && toVis == libraryVisibilityPrivate:
		prev = "NULL"
	}
	q := `UPDATE library_items SET visibility = $2, folder_id = $3::uuid, slug = $4,
		shared_at = CASE WHEN $2 = 'org' THEN COALESCE(shared_at, now()) ELSE NULL END,
		updated_at = now(), updated_by = $5, previous_folder_path = ` + prev + ` WHERE id = $1`
	if _, err := tx.Exec(ctx, q, args...); err != nil {
		if isUniqueViolation(err) {
			if toVis == libraryVisibilityOrg {
				return nil, errSharedPathTaken
			}
			return nil, errSlugTaken
		}
		return nil, err
	}
	if err := enqueueItem(ctx, tx, itemID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

type moveItemRequest struct {
	Scope string  `json:"scope"`
	Path  *string `json:"path"`
	Slug  string  `json:"slug"`
}

// MoveItem handles POST /api/v1/library/:id/move {scope, path, slug?}.
func (h *LibraryHandler) MoveItem(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	var req moveItemRequest
	scope := ""
	if err := c.ShouldBindJSON(&req); err != nil || req.Path == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {scope: private|org, path, slug?}"})
		return
	}
	scope, ok := dbScope(req.Scope)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be private or org"})
		return
	}
	p := strings.Trim(strings.TrimSpace(*req.Path), "/")
	out, err := h.moveItem(ctx, id, me, isAdmin(c), scope, &p, req.Slug)
	if err != nil {
		writeLibErr(c, err, "move item failed")
		return
	}
	if !out.noop {
		evt := activity.FromContext(c, activity.LibraryItemMoved)
		evt.TargetType = activity.StrPtr("library_item")
		evt.TargetID = activity.StrPtr(id)
		evt.Detail = map[string]any{"kind": out.kind, "from": wireScope(out.fromVis), "to": wireScope(out.toVis), "renamed": out.slugChanged}
		h.activity.Record(ctx, evt)
		if out.fromVis != out.toVis {
			typ := activity.LibraryItemShared
			if out.toVis == libraryVisibilityPrivate {
				typ = activity.LibraryItemUnshared
			}
			se := activity.FromContext(c, typ)
			se.TargetType, se.TargetID = evt.TargetType, evt.TargetID
			se.Detail = map[string]any{"kind": out.kind}
			h.activity.Record(ctx, se)
		}
	}
	r, err := h.loadVisible(ctx, me, id)
	if err == nil && r == nil && isAdmin(c) && out.toVis == libraryVisibilityPrivate {
		// An administrator unshared someone else's item: it is private again,
		// and not theirs to read.
		c.Status(http.StatusNoContent)
		return
	}
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "move item failed"})
		return
	}
	c.JSON(http.StatusOK, h.wire(r, me))
}

// ---------------------------------------------------------------------------
// Folder routes
// ---------------------------------------------------------------------------

type folderEntry struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	Folders   int       `json:"folders"`
	Items     int       `json:"items"`
	UpdatedAt time.Time `json:"updated_at"`
}

// folderScope reads and checks ?scope= / {scope}. Org mutations other than
// create need an administrator.
func folderScopeParam(c *gin.Context, raw string) (string, bool) {
	s, ok := dbScope(raw)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be private or org", "code": "scope_invalid"})
	}
	return s, ok
}

func cleanPath(p string) string { return strings.Trim(strings.TrimSpace(p), "/") }

// folderID looks a path up within the caller's view of a scope.
func (h *LibraryHandler) folderID(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope, me, path string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM library_folders WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND path = $3`,
		scope, ownerArg(scope, me), path).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errFolderNotFound
	}
	return id, err
}

const folderCountCols = `f.path, f.display_name,
	(SELECT count(*) FROM library_folders c WHERE c.parent_id = f.id),
	(SELECT count(*) FROM library_items i WHERE i.folder_id = f.id),
	f.updated_at`

// ListFolders handles GET /api/v1/library/folders?scope=&path=&recursive=0|1.
// With recursive=1 it returns every descendant folder under path, not only
// the direct children (same entry shape), so a client can draw the whole tree.
func (h *LibraryHandler) ListFolders(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	scope, ok := folderScopeParam(c, c.Query("scope"))
	if !ok {
		return
	}
	path := cleanPath(c.Query("path"))
	if !libkey.ValidFolderPath(path) {
		writeLibErr(c, errFolderInvalid, "list folders failed")
		return
	}
	var parent *string
	if path != "" {
		id, err := h.folderID(ctx, h.db, scope, me, path)
		if err != nil {
			writeLibErr(c, err, "list folders failed")
			return
		}
		parent = &id
	}
	var rows pgx.Rows
	var err error
	if c.Query("recursive") == "1" {
		// Every descendant under path (the whole tree for path ""), in path order.
		rows, err = h.db.Query(ctx, `SELECT `+folderCountCols+` FROM library_folders f
			WHERE f.scope = $1 AND f.owner_user_id IS NOT DISTINCT FROM $2::uuid AND ($3 = '' OR f.path LIKE $3 || '/%')
			ORDER BY f.path`, scope, ownerArg(scope, me), path)
	} else {
		rows, err = h.db.Query(ctx, `SELECT `+folderCountCols+` FROM library_folders f
			WHERE f.scope = $1 AND f.owner_user_id IS NOT DISTINCT FROM $2::uuid AND f.parent_id IS NOT DISTINCT FROM $3::uuid
			ORDER BY f.display_name, f.path`, scope, ownerArg(scope, me), parent)
	}
	if err != nil {
		writeLibErr(c, err, "list folders failed")
		return
	}
	defer rows.Close()
	folders := []folderEntry{}
	for rows.Next() {
		var f folderEntry
		if err := rows.Scan(&f.Path, &f.Name, &f.Folders, &f.Items, &f.UpdatedAt); err != nil {
			writeLibErr(c, err, "list folders failed")
			return
		}
		folders = append(folders, f)
	}
	rows.Close()
	var items int
	if err := h.db.QueryRow(ctx, `SELECT count(*) FROM library_items i WHERE i.folder_id IS NOT DISTINCT FROM $1::uuid AND
		((i.visibility = 'org' AND $2 = 'org') OR (i.visibility = 'private' AND $2 = 'user' AND i.owner_user_id = $3))`,
		parent, scope, me).Scan(&items); err != nil {
		writeLibErr(c, err, "list folders failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": path, "folders": folders, "items": items})
}

type createFolderRequest struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
	// Name optionally sets the leaf folder's display name (default: the
	// last path segment).
	Name string `json:"name"`
}

// CreateFolder handles POST /api/v1/library/folders {scope, path, name?}.
func (h *LibraryHandler) CreateFolder(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	var req createFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {scope: private|org, path}"})
		return
	}
	scope, ok := folderScopeParam(c, req.Scope)
	if !ok {
		return
	}
	path := cleanPath(req.Path)
	name := strings.TrimSpace(req.Name)
	if path == "" || !libkey.ValidFolderPath(path) || len([]rune(name)) > libraryNameMaxChars {
		writeLibErr(c, errFolderInvalid, "create folder failed")
		return
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		writeLibErr(c, err, "create folder failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, created, err := ensureFolderPath(ctx, tx, scope, me, path)
	if err == nil && !created {
		err = errFolderExists
	}
	if err == nil && name != "" {
		_, err = tx.Exec(ctx, `UPDATE library_folders SET display_name = $2 WHERE id = $1`, *id, name)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		writeLibErr(c, err, "create folder failed")
		return
	}
	evt := activity.FromContext(c, activity.LibraryFolderCreated)
	evt.TargetType = activity.StrPtr("library_folder")
	evt.TargetID = id
	evt.Detail = map[string]any{"scope": wireScope(visibilityOfScope(scope)), "depth": strings.Count(path, "/") + 1}
	h.activity.Record(ctx, evt)
	var f folderEntry
	if err := h.db.QueryRow(ctx, `SELECT `+folderCountCols+` FROM library_folders f WHERE f.id = $1`, *id).
		Scan(&f.Path, &f.Name, &f.Folders, &f.Items, &f.UpdatedAt); err != nil {
		writeLibErr(c, err, "create folder failed")
		return
	}
	c.JSON(http.StatusCreated, f)
}

type moveFolderRequest struct {
	Scope string `json:"scope"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// MoveFolder handles POST /api/v1/library/folders/move {scope, from, to}:
// a rename and a move are the same operation. Atomic in Postgres; every
// folder and item under it is re-queued so the bucket catches up. Org
// folders: administrators only.
func (h *LibraryHandler) MoveFolder(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	var req moveFolderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {scope, from, to}"})
		return
	}
	scope, ok := folderScopeParam(c, req.Scope)
	if !ok {
		return
	}
	if scope == "org" && !isAdmin(c) {
		writeLibErr(c, errAdminRequired, "move folder failed")
		return
	}
	from, to := cleanPath(req.From), cleanPath(req.To)
	if from == "" || to == "" || !libkey.ValidFolderPath(from) || !libkey.ValidFolderPath(to) || to == from || strings.HasPrefix(to, from+"/") {
		writeLibErr(c, errFolderInvalid, "move folder failed")
		return
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		writeLibErr(c, err, "move folder failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	owner := ownerArg(scope, me)

	fromID, err := h.folderID(ctx, tx, scope, me, from)
	if err != nil {
		writeLibErr(c, err, "move folder failed")
		return
	}
	// Depth: the deepest descendant must still fit under the new path.
	var height int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(length(path) - length(replace(path, '/', '')) - $4::int), 0)
		FROM library_folders WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND (path = $3 OR path LIKE $3 || '/%')`,
		scope, owner, from, strings.Count(from, "/")).Scan(&height); err != nil {
		writeLibErr(c, err, "move folder failed")
		return
	}
	if strings.Count(to, "/")+1+height > libkey.MaxDepth {
		writeLibErr(c, errFolderInvalid, "move folder failed")
		return
	}
	if _, err := h.folderID(ctx, tx, scope, me, to); err == nil {
		writeLibErr(c, errFolderExists, "move folder failed")
		return
	} else if !errors.Is(err, errFolderNotFound) {
		writeLibErr(c, err, "move folder failed")
		return
	}
	toParent := ""
	if i := strings.LastIndex(to, "/"); i >= 0 {
		toParent = to[:i]
	}
	parentID, _, err := ensureFolderPath(ctx, tx, scope, me, toParent)
	if err != nil {
		writeLibErr(c, err, "move folder failed")
		return
	}
	leaf := to[strings.LastIndex(to, "/")+1:]
	oldLeaf := from[strings.LastIndex(from, "/")+1:]
	// Descendants first (their path still has the old prefix), then the folder.
	if _, err := tx.Exec(ctx, `UPDATE library_folders SET path = $4 || substr(path, length($3) + 1), updated_at = now()
		WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND path LIKE $3 || '/%'`, scope, owner, from, to); err != nil {
		writeLibErr(c, err, "move folder failed")
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE library_folders SET path = $2, slug = $3, parent_id = $4::uuid, updated_at = now(),
		display_name = CASE WHEN $3 <> $5 THEN $3 ELSE display_name END WHERE id = $1`, fromID, to, leaf, parentID, oldLeaf); err != nil {
		if isUniqueViolation(err) {
			writeLibErr(c, errFolderExists, "move folder failed")
			return
		}
		writeLibErr(c, err, "move folder failed")
		return
	}
	// Re-queue the whole subtree: the folders' markers and every item in it.
	var folders, items int
	if err := tx.QueryRow(ctx, `
		WITH sub AS (
			SELECT id FROM library_folders WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND (path = $3 OR path LIKE $3 || '/%')
		), qf AS (
			INSERT INTO library_sync (folder_id) SELECT id FROM sub
			ON CONFLICT (folder_id) WHERE folder_id IS NOT NULL DO UPDATE`+queueBump+` RETURNING 1
		), qi AS (
			INSERT INTO library_sync (item_id) SELECT i.id FROM library_items i WHERE i.folder_id IN (SELECT id FROM sub)
			ON CONFLICT (item_id) WHERE item_id IS NOT NULL DO UPDATE`+queueBump+` RETURNING 1
		)
		SELECT (SELECT count(*) FROM qf), (SELECT count(*) FROM qi)`, scope, owner, to).Scan(&folders, &items); err != nil {
		writeLibErr(c, err, "move folder failed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeLibErr(c, err, "move folder failed")
		return
	}
	evt := activity.FromContext(c, activity.LibraryFolderRenamed)
	evt.TargetType = activity.StrPtr("library_folder")
	evt.TargetID = &fromID
	evt.Detail = map[string]any{"scope": wireScope(visibilityOfScope(scope)), "folders": folders, "items": items}
	h.activity.Record(ctx, evt)
	c.JSON(http.StatusOK, gin.H{"moved": folders + items, "folders": folders, "items": items})
}

// DeleteFolder handles DELETE /api/v1/library/folders?scope=&path=&recursive=0|1.
func (h *LibraryHandler) DeleteFolder(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	scope, ok := folderScopeParam(c, c.Query("scope"))
	if !ok {
		return
	}
	if scope == "org" && !isAdmin(c) {
		writeLibErr(c, errAdminRequired, "delete folder failed")
		return
	}
	path := cleanPath(c.Query("path"))
	if path == "" || !libkey.ValidFolderPath(path) {
		writeLibErr(c, errFolderInvalid, "delete folder failed")
		return
	}
	recursive := c.Query("recursive") == "1" || c.Query("recursive") == "true"
	tx, err := h.db.Begin(ctx)
	if err != nil {
		writeLibErr(c, err, "delete folder failed")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	owner := ownerArg(scope, me)
	fid, err := h.folderID(ctx, tx, scope, me, path)
	if err != nil {
		writeLibErr(c, err, "delete folder failed")
		return
	}
	type fo struct {
		id  string
		key *string
	}
	var subs []fo
	rows, err := tx.Query(ctx, `SELECT id::text, s3_key FROM library_folders
		WHERE scope = $1 AND owner_user_id IS NOT DISTINCT FROM $2::uuid AND (path = $3 OR path LIKE $3 || '/%') ORDER BY path DESC`, scope, owner, path)
	if err != nil {
		writeLibErr(c, err, "delete folder failed")
		return
	}
	for rows.Next() {
		var f fo
		if err := rows.Scan(&f.id, &f.key); err != nil {
			rows.Close()
			writeLibErr(c, err, "delete folder failed")
			return
		}
		subs = append(subs, f)
	}
	rows.Close()
	ids := make([]string, 0, len(subs))
	for _, f := range subs {
		ids = append(ids, f.id)
	}
	irows, err := tx.Query(ctx, `DELETE FROM library_items WHERE folder_id = ANY($1::uuid[])
		RETURNING id::text, tool_credential_last4 IS NOT NULL, s3_key, s3_pending_key`, ids)
	if err != nil {
		writeLibErr(c, err, "delete folder failed")
		return
	}
	type gone struct {
		id     string
		cred   bool
		k1, k2 *string
	}
	var removed []gone
	for irows.Next() {
		var g gone
		if err := irows.Scan(&g.id, &g.cred, &g.k1, &g.k2); err != nil {
			irows.Close()
			writeLibErr(c, err, "delete folder failed")
			return
		}
		removed = append(removed, g)
	}
	irows.Close()
	if (len(removed) > 0 || len(subs) > 1) && !recursive {
		writeLibErr(c, errFolderNotEmpty, "delete folder failed")
		return
	}
	var creds []string
	for _, g := range removed {
		if err := enqueueItemDeleted(ctx, tx, g.id, g.k1, g.k2); err != nil {
			writeLibErr(c, err, "delete folder failed")
			return
		}
		if g.cred {
			creds = append(creds, g.id)
		}
	}
	for _, f := range subs { // deepest first: children before parents
		if _, err := tx.Exec(ctx, `DELETE FROM library_folders WHERE id = $1`, f.id); err != nil {
			writeLibErr(c, err, "delete folder failed")
			return
		}
		if err := enqueueFolderDeleted(ctx, tx, f.id, f.key); err != nil {
			writeLibErr(c, err, "delete folder failed")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeLibErr(c, err, "delete folder failed")
		return
	}
	if h.tools != nil && len(creds) > 0 {
		h.tools.RemoveCredentialsQuietly(ctx, creds)
	}
	evt := activity.FromContext(c, activity.LibraryFolderDeleted)
	evt.TargetType = activity.StrPtr("library_folder")
	evt.TargetID = &fid
	evt.Detail = map[string]any{"scope": wireScope(visibilityOfScope(scope)), "folders": len(subs), "items": len(removed)}
	h.activity.Record(ctx, evt)
	c.JSON(http.StatusOK, gin.H{"folders": len(subs), "items": len(removed)})
}

var _ = fmt.Sprintf
