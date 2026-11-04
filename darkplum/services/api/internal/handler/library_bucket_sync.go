package handler

// POST /api/v1/library/sync {scope}: "Sync from bucket".
//
// Postgres is canonical and the bucket is a copy the app writes; edits made in
// the bucket are not live. This is the deliberate, user-triggered way to pull
// them in (and the disaster-recovery path: rebuild the index from the bucket).
//
// Rules:
//   - Only the caller's own prefix is read: people/<slug>/ where the slug
//     comes from Postgres (never the request), or org/ for an administrator.
//   - A file's front matter darkplum_id (tool JSON: "darkplum_id") is
//     authoritative for which item it is; object metadata darkplum-item-id is
//     only a hint. An id that is not an item in the caller's scope is ignored
//     (the file is imported as new), so a pasted foreign id cannot reach
//     another person's item. A file with no id and no known key becomes a NEW
//     item named from the file name. Imported items are owned by the caller
//     (the acting admin for org).
//   - The 64 KiB body cap and the tool rules (no userinfo in server_url) apply.
//     A changed tool server clears the verified catalogue; credentials are
//     never read or written.
//   - CONFLICT: if Postgres changed since the last sync (content differs from
//     s3_synced_sha, or a change is still queued) AND the bucket object also
//     differs from what was last synced, the file is skipped and reported as
//     a conflict. Postgres wins; the worker overwrites the object and the
//     bucket's version stays in version history.
//   - Deleting an object in the bucket is not a deletion in the library: the
//     worker puts it back the next time that item is synced.
//   - Folder markers, the health sentinel and dot files are ignored.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
	"github.com/scalecraft-dev/darkplum/api/internal/libkey"
	"github.com/scalecraft-dev/darkplum/api/internal/objectstore"
)

const (
	syncRouteMaxObjects = 2000
	syncRouteMaxErrors  = 100
	syncObjectMaxBytes  = libraryBodyMaxBytes*2 + 4096
)

// syncTimeBudget bounds one pass; a var so a test can exhaust it.
var syncTimeBudget = 60 * time.Second

type syncRequest struct {
	Scope string `json:"scope"`
}

type syncError struct {
	FileName string `json:"file_name"`
	Reason   string `json:"reason"`
}

type syncResult struct {
	Imported  int         `json:"imported"`
	Updated   int         `json:"updated"`
	Skipped   int         `json:"skipped"`
	Conflicts []string    `json:"conflicts"`
	Errors    []syncError `json:"errors"`
	// Scanned is how many objects under the prefix were looked at. Truncated
	// is true when the object cap or the time budget stopped the pass early:
	// run it again to continue (files already adopted are skipped).
	Scanned   int  `json:"scanned"`
	Truncated bool `json:"truncated"`
}

const syncDuplicateReason = "duplicate of an existing item; delete one copy"

type knownItem struct {
	id, owner       string
	s3Key           *string
	objSHA, syncSHA *string
	contentSHA      string
	queued          bool
}

// objectItemID reads darkplum_id from a file: front matter for markdown, a
// top-level field for tool JSON.
func objectItemID(kind, content string) string {
	var id string
	if kind == libraryKindTool {
		var raw struct {
			ID string `json:"darkplum_id"`
		}
		_ = json.Unmarshal([]byte(strings.TrimSpace(content)), &raw)
		id = raw.ID
	} else {
		keys, _, _ := frontMatter(content)
		id = keys["darkplum_id"]
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if isValidUUID(id) {
		return id
	}
	return ""
}

func bodyForCompare(kind, body string) string {
	if kind == libraryKindTool {
		return body
	}
	return strings.TrimRight(body, " \t\r\n")
}

// SyncFromBucket handles POST /api/v1/library/sync.
func (h *LibraryHandler) SyncFromBucket(c *gin.Context) {
	me := c.GetString("user_id")
	var req syncRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {scope: private|org}"})
		return
	}
	scope, ok := dbScope(req.Scope)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be private or org"})
		return
	}
	if scope == "org" && !isAdmin(c) {
		writeLibErr(c, errAdminRequired, "sync failed")
		return
	}
	if h.store == nil {
		writeLibErr(c, errStoreUnavailable, "sync failed")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	res, err := h.syncScope(ctx, me, scope)
	if err != nil {
		writeLibErr(c, err, "sync failed")
		return
	}
	evt := activity.FromContext(c, activity.LibrarySynced)
	evt.Detail = map[string]any{"scope": wireScope(visibilityOfScope(scope)), "imported": res.Imported, "updated": res.Updated,
		"skipped": res.Skipped, "conflicts": len(res.Conflicts), "errors": len(res.Errors)}
	h.activity.Record(c.Request.Context(), evt)
	c.JSON(http.StatusOK, res)
}

func (h *LibraryHandler) syncScope(ctx context.Context, me, scope string) (*syncResult, error) {
	res := &syncResult{Conflicts: []string{}, Errors: []syncError{}}
	prefix, person := "org/", ""
	if scope == "user" {
		if err := h.db.QueryRow(ctx, `SELECT library_slug FROM users WHERE id = $1`, me).Scan(&person); err != nil {
			return nil, err
		}
		prefix = "people/" + person + "/"
	}
	// Index what Postgres knows in this scope (no bodies).
	where := `li.visibility = 'org'`
	args := []any{}
	if scope == "user" {
		where = `li.visibility = 'private' AND li.owner_user_id = $1`
		args = append(args, me)
	}
	rows, err := h.db.Query(ctx, `SELECT li.id::text, COALESCE(li.owner_user_id::text, ''), li.s3_key, li.s3_obj_sha, li.s3_synced_sha, li.content_sha256,
		EXISTS (SELECT 1 FROM library_sync q WHERE q.item_id = li.id) FROM library_items li WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	byID, byKey := map[string]*knownItem{}, map[string]*knownItem{}
	for rows.Next() {
		k := &knownItem{}
		if err := rows.Scan(&k.id, &k.owner, &k.s3Key, &k.objSHA, &k.syncSHA, &k.contentSHA, &k.queued); err != nil {
			rows.Close()
			return nil, err
		}
		byID[k.id] = k
		if k.s3Key != nil {
			byKey[*k.s3Key] = k
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// List the caller's prefix.
	var objs []objectstore.Info
	cursor := ""
	for {
		page, next, lerr := h.store.List(ctx, prefix, cursor, 1000)
		if lerr != nil {
			return nil, errStoreUnavailable
		}
		objs = append(objs, page...)
		if next == "" {
			break
		}
		if len(objs) >= syncRouteMaxObjects {
			res.Truncated = true
			break
		}
		cursor = next
	}
	if len(objs) > syncRouteMaxObjects {
		objs = objs[:syncRouteMaxObjects]
		res.Truncated = true
	}
	listed := map[string]bool{}
	for _, o := range objs {
		listed[o.Key] = true
	}
	addErr := func(file, reason string) {
		if len(res.Errors) < syncRouteMaxErrors {
			res.Errors = append(res.Errors, syncError{FileName: file, Reason: reason})
		}
	}

	deadline := time.Now().Add(syncTimeBudget)
	handled := map[string]bool{}
	var reported []string
	defer func() {
		if len(reported) > 0 {
			_, _ = h.db.Exec(context.WithoutCancel(ctx), `UPDATE library_items SET bucket_changed_at = NULL WHERE id = ANY($1::uuid[])`, reported)
		}
	}()
	for _, o := range objs {
		if time.Now().After(deadline) {
			res.Truncated = true
			break
		}
		res.Scanned++
		base := path.Base(o.Key)
		if strings.HasSuffix(o.Key, "/") || strings.HasPrefix(base, ".") {
			continue // folder marker, sentinel, dot file
		}
		loc, perr := libkey.Parse(o.Key)
		if perr != nil || (scope == "user" && loc.Person != person) || (scope == "org" && loc.Scope != libkey.ScopeOrg) {
			addErr(base, "not a library file: names look like <name>.skill.md, <name>.context.md or <name>.tool.json, in folders of lowercase letters, digits and hyphens")
			continue
		}
		if o.Size > syncObjectMaxBytes {
			addErr(base, "file is too large")
			continue
		}
		data, info, gerr := h.store.Get(ctx, o.Key)
		if errors.Is(gerr, objectstore.ErrNotFound) {
			continue
		}
		if gerr != nil {
			return nil, errStoreUnavailable
		}
		content := string(data)
		ext := ".md"
		if loc.Kind == libraryKindTool {
			ext = ".json"
		}
		in, _, msg := parseImport(importRequest{Filename: loc.Slug + ext, Content: content, Kind: loc.Kind})
		if msg != "" {
			addErr(base, msg)
			continue
		}
		if in.Kind != loc.Kind {
			addErr(base, "the kind inside the file does not match its file name")
			continue
		}
		objSHA := objectSHA256(data)

		var ex *knownItem
		if id := objectItemID(loc.Kind, content); id != "" {
			ex = byID[id]
		} else if hint := strings.ToLower(info.Meta["darkplum-item-id"]); isValidUUID(hint) {
			ex = byID[hint]
		}
		if ex == nil {
			ex = byKey[o.Key]
		}

		if ex == nil {
			p := loc.FolderPath
			slug := loc.Slug
			nin := in
			nin.Slug, nin.Path = &slug, &p
			newID, _, err := h.createOne(ctx, me, nin, visibilityOfScope(scope))
			if err == nil {
				// Remember what we adopted, so a sync that runs before the
				// worker has rewritten the canonical bytes sees "unchanged"
				// rather than a conflict with ourselves. The key is the one
				// the worker will use (explicit slug, same folder).
				_, err = h.db.Exec(ctx, `UPDATE library_items SET s3_key = $2, s3_obj_sha = $3 WHERE id = $1`, newID, o.Key, objSHA)
			}
			if err != nil {
				var le *libError
				switch {
				case errors.As(err, &le):
					addErr(base, le.msg)
				default:
					return nil, err
				}
				continue
			}
			res.Imported++
			continue
		}

		// Whatever the outcome below, the file has now been looked at and
		// reported: the "changed in the bucket" flag is cleared.
		reported = append(reported, ex.id)
		if handled[ex.id] {
			addErr(base, syncDuplicateReason)
			continue
		}
		dirty := ex.queued || ex.s3Key == nil || ex.syncSHA == nil || *ex.syncSHA != ex.contentSHA
		bucketChanged := ex.objSHA == nil || *ex.objSHA != objSHA
		moved := ex.s3Key != nil && *ex.s3Key != o.Key
		if moved && listed[*ex.s3Key] {
			// A copy (the console's "Copy", aws s3 cp) of an item whose own
			// file is still there: not adopted, and said so.
			addErr(base, syncDuplicateReason)
			continue
		}
		handled[ex.id] = true
		if !bucketChanged && !moved {
			res.Skipped++
			continue
		}
		if dirty {
			if bucketChanged {
				res.Conflicts = append(res.Conflicts, ex.id)
				// Postgres wins: acknowledge the bucket's version (it stays in
				// version history) so the worker may overwrite it, and let it run now.
				_, _ = h.db.Exec(ctx, `UPDATE library_items SET s3_obj_sha = $2 WHERE id = $1`, ex.id, objSHA)
				_, _ = h.db.Exec(ctx, `UPDATE library_sync SET not_before = now(), last_error = NULL WHERE item_id = $1`, ex.id)
			} else {
				res.Skipped++
			}
			continue
		}
		changed, err := h.applyBucketEdit(ctx, me, scope, ex, loc, in, objSHA)
		switch {
		case err == nil && changed == "conflict":
			res.Conflicts = append(res.Conflicts, ex.id)
			_, _ = h.db.Exec(ctx, `UPDATE library_items SET s3_obj_sha = $2 WHERE id = $1`, ex.id, objSHA)
			_, _ = h.db.Exec(ctx, `UPDATE library_sync SET not_before = now(), last_error = NULL WHERE item_id = $1`, ex.id)
		case err == nil && changed == "updated":
			res.Updated++
		case err == nil:
			res.Skipped++
		default:
			var le *libError
			if errors.As(err, &le) {
				addErr(base, le.msg)
			} else {
				return nil, err
			}
		}
	}
	return res, nil
}

// applyBucketEdit writes a bucket edit into Postgres if (and only if) the item
// has not changed there in the meantime. Returns "updated", "unchanged" or
// "conflict".
func (h *LibraryHandler) applyBucketEdit(ctx context.Context, me, scope string, ex *knownItem, loc libkey.Location, in libraryInput, objSHA string) (string, error) {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var kind, name, desc, body, slug, curPath, sha string
	var syncSHA *string
	var owner *string
	err = tx.QueryRow(ctx, `SELECT li.kind, li.name, li.description, li.body, li.slug, COALESCE(fo.path, ''), li.content_sha256, li.s3_synced_sha, li.owner_user_id::text
		FROM library_items li LEFT JOIN library_folders fo ON fo.id = li.folder_id WHERE li.id = $1 FOR UPDATE OF li`, ex.id).
		Scan(&kind, &name, &desc, &body, &slug, &curPath, &sha, &syncSHA, &owner)
	if err != nil {
		return "", err
	}
	var queued bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM library_sync WHERE item_id = $1)`, ex.id).Scan(&queued); err != nil {
		return "", err
	}
	if queued || syncSHA == nil || *syncSHA != sha {
		return "conflict", nil // Postgres moved while we were reading
	}
	sameContent := name == in.Name && desc == in.Description && bodyForCompare(kind, body) == bodyForCompare(kind, in.Body)
	if sameContent && slug == loc.Slug && curPath == loc.FolderPath {
		// Only formatting differs: queue a rewrite so the object returns to
		// the canonical form, but report nothing changed.
		if _, err := tx.Exec(ctx, `UPDATE library_items SET s3_obj_sha = $2 WHERE id = $1`, ex.id, objSHA); err != nil {
			return "", err
		}
		if err := enqueueItem(ctx, tx, ex.id); err != nil {
			return "", err
		}
		return "unchanged", tx.Commit(ctx)
	}
	ownerID := me
	if owner != nil {
		ownerID = *owner
	}
	folderID, _, err := ensureFolderPath(ctx, tx, scope, ownerID, loc.FolderPath)
	if err != nil {
		return "", err
	}
	clearCatalog := ""
	if kind == libraryKindTool {
		oc, _ := parseToolConfig(body)
		nc, _ := parseToolConfig(in.Body)
		if oc.ServerURL != nc.ServerURL {
			clearCatalog = ", tool_catalog = NULL, tool_catalog_at = NULL"
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE library_items SET name = $2, description = $3, body = $4, content_sha256 = $5,
		slug = $6, folder_id = $7::uuid, updated_at = now(), updated_by = $8, s3_obj_sha = $9`+clearCatalog+` WHERE id = $1`,
		ex.id, in.Name, in.Description, in.Body, contentSHA256(in.Body), loc.Slug, folderID, me, objSHA); err != nil {
		if isUniqueViolation(err) {
			return "", &libError{status: http.StatusConflict, code: "slug_taken", msg: "another item already has that name in that folder"}
		}
		return "", err
	}
	if err := enqueueItem(ctx, tx, ex.id); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return "updated", nil
}

var _ = pgx.ErrNoRows
