// The library: skills, context and tools a person keeps in the organisation's
// own database (migration 026), shares with colleagues, attaches to chats and
// exports again -- so their AI configuration is theirs, not one vendor's.
//
// PRIVACY: a private item is readable ONLY by its owner. Every read below
// filters in SQL with visibleSQL -- (owner_user_id = $me OR visibility =
// 'org') -- never only in Go, and an id that exists but is not visible 404s
// exactly like one that does not exist, never 403. The admin governance
// routes (ListShared, AdminUnshare, AdminDelete) are scoped to visibility =
// 'org' in their SQL: there is NO admin read path over private items anywhere
// in this codebase, the same rule conversations.go keeps for chat history.
//
// Writes are owner-only. Sharing to the organisation is a decision the owner
// makes (POST share); an administrator may undo it or remove the item, and
// both leave an activity event. Nothing here ever records an item's body in
// the activity log.
package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
	"github.com/scalecraft-dev/darkplum/api/internal/libkey"
	"github.com/scalecraft-dev/darkplum/api/internal/objectstore"
)

const (
	libraryKindSkill   = "skill"
	libraryKindContext = "context"
	libraryKindTool    = "tool"

	libraryVisibilityPrivate = "private"
	libraryVisibilityOrg     = "org"

	// libraryBodyMaxBytes matches the CHECK constraint in migration 032
	// (4 MiB). The library lives in Postgres; a reference document is far
	// longer than a page of instructions. The per-turn injection cap
	// (chat_library.go) is still the binding limit on what reaches a model.
	libraryBodyMaxBytes = 4 << 20
	// libraryRequestMaxBytes bounds a create, update or import request: a
	// body at the cap, JSON-escaped (an import carries front matter too),
	// with room for the other fields.
	libraryRequestMaxBytes = libraryBodyMaxBytes*2 + 1<<20
	libraryNameMaxChars    = 100
	libraryDescMaxChars    = 500
	// libraryItemsPerUser bounds one person's library. Enforced inside the
	// insert transaction so concurrent creates cannot overshoot.
	libraryItemsPerUser = 500
	// libraryExportMaxItems bounds the zip export.
	libraryExportMaxItems = 1000
)

// visibleSQL is the one visibility predicate, used by every read. $N is the
// positional parameter carrying the requesting user's id.
func visibleSQL(param int) string {
	return fmt.Sprintf("(li.owner_user_id = $%d OR li.visibility = 'org')", param)
}

var librarySlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// LibraryHandler serves /api/v1/library and the admin governance routes.
type LibraryHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
	// tools is the MCP side: verification, credentials, host policy. nil in
	// tests that never touch a tool item.
	tools *toolService
	// store is the object store the library is mirrored to; nil when
	// LIBRARY_STORE_URL is unset (the library then behaves as before and the
	// queue just accumulates).
	store objectstore.Store
	// syncer is the running worker (nil when no store is configured).
	syncer *LibrarySyncer
}

// SetStore wires the object store (nil = none).
func (h *LibraryHandler) SetStore(s objectstore.Store) { h.store = s }

// SetSyncer wires the worker whose health and queue the admin status reports.
func (h *LibraryHandler) SetSyncer(s *LibrarySyncer) { h.syncer = s }

// NewLibraryHandler returns a LibraryHandler backed by db.
func NewLibraryHandler(db *pgxpool.Pool, rec *activity.Recorder, tools *toolService) *LibraryHandler {
	return &LibraryHandler{db: db, activity: rec, tools: tools}
}

// libraryOwner is who an item belongs to, as the UI shows it.
type libraryOwner struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// libraryToolInfo is the tool-kind extras on the wire.
type libraryToolInfo struct {
	ServerURL    string          `json:"server_url"`
	AllowedTools []string        `json:"allowed_tools,omitempty"`
	Catalog      json.RawMessage `json:"catalog"`
	CatalogAt    *time.Time      `json:"catalog_at"`
	ServerName   string          `json:"server_name,omitempty"`
	Credential   struct {
		Set   bool   `json:"set"`
		Last4 string `json:"last4,omitempty"`
	} `json:"credential"`
}

// libraryItem is the wire shape of one item.
type libraryItem struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	// Body is the markdown (skill/context) or JSON configuration (tool).
	Body       string `json:"body"`
	Visibility string `json:"visibility"`
	// Owner is null only for an item whose owner was deleted before the
	// reassignment rule existed.
	Owner *libraryOwner `json:"owner"`
	// Mine is true when the requesting user owns the item (so can edit it).
	Mine          bool             `json:"mine"`
	ContentSHA256 string           `json:"content_sha256"`
	SizeBytes     int              `json:"size_bytes"`
	SharedAt      *time.Time       `json:"shared_at"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Tool          *libraryToolInfo `json:"tool,omitempty"`
	// Scope is the root the item lives under: "private" or "org".
	Scope string `json:"scope"`
	// Path is the folder path under that root, "" at the root, segments
	// joined by "/". Slugs, not display names.
	Path string `json:"path"`
	// FileName is the item's file name in the bucket and in an export.
	FileName string `json:"file_name"`
	// Synced is true when the bucket copy is current, false while it is
	// not, and null when the installation has no bucket (the library then
	// lives in Postgres alone and there is nothing to be in sync with).
	Synced *bool `json:"synced"`
	// S3URI is where the copy lives, only when a store is configured and the
	// caller may see it (the owner for private items, anyone for org items).
	S3URI *string `json:"s3_uri"`
	// BucketChanged is true when the object in the bucket was found to differ
	// from what the app last wrote (edited or replaced there). The app will
	// not overwrite it until Sync from bucket has been run.
	BucketChanged bool `json:"bucket_changed"`
}

// libraryRow is one scanned row; toWire turns it into a libraryItem.
type libraryRow struct {
	id, kind, name, slug, description, body, visibility, sha string
	ownerID, ownerName, ownerEmail                           *string
	sharedAt, catalogAt                                      *time.Time
	createdAt, updatedAt                                     time.Time
	toolCatalog                                              []byte
	credLast4                                                *string
	// Location and bucket bookkeeping.
	folderPath               string
	ownerSlug                *string
	s3Key, syncedSHA, objSHA *string
	previousFolderPath       *string
	queued                   bool
	bucketChanged            bool
	// size is octet_length(body) as Postgres counts it, read separately so a
	// listing can report it without carrying the body.
	size int
}

// libraryColsWith is the column list scanLibraryRow reads; bodyExpr is what
// stands in for the body column.
func libraryColsWith(bodyExpr string) string {
	return `
	li.id, li.kind, li.name, li.slug, li.description, ` + bodyExpr + `, li.visibility, li.content_sha256,
	li.owner_user_id::text, u.name, u.email, li.shared_at, li.created_at, li.updated_at,
	li.tool_catalog, li.tool_catalog_at, li.tool_credential_last4,
	COALESCE(fo.path, ''), u.library_slug, li.s3_key, li.s3_synced_sha, li.s3_obj_sha, li.previous_folder_path,
	EXISTS (SELECT 1 FROM library_sync q WHERE q.item_id = li.id), li.bucket_changed_at IS NOT NULL,
	octet_length(li.body)`
}

// librarySelectCols reads one item in full: a single read, an export, the
// sync worker.
var librarySelectCols = libraryColsWith("li.body")

// libraryListCols is what a listing reads. A skill or context body can be
// 4 MiB and a listing holds up to a thousand items, so it carries only the
// tool bodies (the small JSON configuration the row's tool block is built
// from); the row still reports size_bytes, and GET /library/:id has the body.
var libraryListCols = libraryColsWith("CASE WHEN li.kind = 'tool' THEN li.body ELSE '' END")

const libraryFrom = ` FROM library_items li LEFT JOIN users u ON u.id = li.owner_user_id LEFT JOIN library_folders fo ON fo.id = li.folder_id `

func scanLibraryRow(row pgx.Row, extra ...any) (*libraryRow, error) {
	var r libraryRow
	dest := []any{&r.id, &r.kind, &r.name, &r.slug, &r.description, &r.body, &r.visibility, &r.sha,
		&r.ownerID, &r.ownerName, &r.ownerEmail, &r.sharedAt, &r.createdAt, &r.updatedAt,
		&r.toolCatalog, &r.catalogAt, &r.credLast4,
		&r.folderPath, &r.ownerSlug, &r.s3Key, &r.syncedSHA, &r.objSHA, &r.previousFolderPath, &r.queued, &r.bucketChanged, &r.size}
	err := row.Scan(append(dest, extra...)...)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *libraryRow) toWire(me string) libraryItem {
	it := libraryItem{
		ID: r.id, Kind: r.kind, Name: r.name, Slug: r.slug, Description: r.description, Body: r.body,
		Visibility: r.visibility, ContentSHA256: r.sha, SizeBytes: r.size,
		SharedAt: r.sharedAt, CreatedAt: r.createdAt, UpdatedAt: r.updatedAt,
		Scope: wireScope(r.visibility), Path: r.folderPath, FileName: r.slug + libkey.Suffix(r.kind),
	}
	if r.ownerID != nil {
		it.Owner = &libraryOwner{ID: *r.ownerID}
		if r.ownerName != nil {
			it.Owner.Name = *r.ownerName
		}
		if r.ownerEmail != nil {
			it.Owner.Email = *r.ownerEmail
		}
		it.Mine = *r.ownerID == me
	}
	if r.kind == libraryKindTool {
		info := &libraryToolInfo{Catalog: json.RawMessage("[]")}
		if cfg, err := parseToolConfig(r.body); err == nil {
			info.ServerURL = cfg.ServerURL
			info.AllowedTools = cfg.AllowedTools
		}
		if len(r.toolCatalog) > 0 {
			var cat toolCatalog
			if json.Unmarshal(r.toolCatalog, &cat) == nil {
				info.ServerName = cat.ServerName
				if b, err := json.Marshal(cat.Tools); err == nil {
					info.Catalog = b
				}
			}
		}
		info.CatalogAt = r.catalogAt
		if r.credLast4 != nil {
			info.Credential.Set = true
			info.Credential.Last4 = *r.credLast4
		}
		it.Tool = info
	}
	return it
}

func validLibraryKind(k string) bool {
	return k == libraryKindSkill || k == libraryKindContext || k == libraryKindTool
}

// slugify derives a slug from a name: lowercase, runs of anything but
// [a-z0-9] become one hyphen, trimmed, at most 100 characters.
func slugify(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 100 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "item"
	}
	return s
}

func contentSHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// libraryInput is the create/update body.
type libraryInput struct {
	Kind        string  `json:"kind"`
	Name        string  `json:"name"`
	Slug        *string `json:"slug,omitempty"`
	Description string  `json:"description"`
	Body        string  `json:"body"`
	Visibility  *string `json:"visibility,omitempty"`
	// Scope (private|org) and Path place a new item; Scope wins over
	// Visibility. Missing folders on the path are created.
	Scope string  `json:"scope,omitempty"`
	Path  *string `json:"path,omitempty"`
}

// validateLibraryInput normalises and checks an input; forUpdate skips the
// kind check (kind is immutable). Returns a caller-readable message.
func validateLibraryInput(in *libraryInput, forUpdate bool) string {
	if !forUpdate && !validLibraryKind(in.Kind) {
		return "kind must be skill, context or tool"
	}
	in.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > libraryNameMaxChars {
		return fmt.Sprintf("name must be 1-%d characters", libraryNameMaxChars)
	}
	in.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(in.Description) > libraryDescMaxChars {
		return fmt.Sprintf("description must be at most %d characters", libraryDescMaxChars)
	}
	if in.Slug != nil {
		s := strings.TrimSpace(*in.Slug)
		if s == "" {
			in.Slug = nil
		} else if !librarySlugRE.MatchString(s) {
			return "slug must be lowercase letters, digits and hyphens, starting with a letter or digit"
		} else {
			in.Slug = &s
		}
	}
	if in.Path != nil {
		pth := strings.Trim(strings.TrimSpace(*in.Path), "/")
		if !libkey.ValidFolderPath(pth) {
			return "folder path is not valid: lowercase letters, digits and hyphens, at most 5 levels"
		}
		in.Path = &pth
	}
	if in.Visibility != nil && *in.Visibility != libraryVisibilityPrivate && *in.Visibility != libraryVisibilityOrg {
		return "visibility must be private or org"
	}
	if strings.TrimSpace(in.Body) == "" {
		return "body is required"
	}
	if len(in.Body) > libraryBodyMaxBytes {
		return bodyTooLargeMsg
	}
	if in.Kind == libraryKindTool {
		cfg, err := parseToolConfig(in.Body)
		if err != nil {
			return "tool body: " + err.Error()
		}
		// The URL lands in the bucket as plain text, so a credential in its
		// query string would be written there. Userinfo is refused by
		// parseToolConfig; this refuses the query-string equivalent.
		if toolURLHasSecretParam(cfg.ServerURL) {
			return toolURLSecretMsg
		}
		// Store the configuration normalised, so what is read back is what
		// was validated.
		b, _ := json.Marshal(cfg)
		in.Body = string(b)
	}
	return ""
}

// wireScope names a root the way the API speaks: "private" or "org".
func wireScope(visibility string) string {
	if visibility == libraryVisibilityOrg {
		return "org"
	}
	return "private"
}

// dbScope maps the API's scope word to library_folders.scope ('user'|'org').
func dbScope(s string) (string, bool) {
	switch s {
	case "private", "user":
		return "user", true
	case "org":
		return "org", true
	}
	return "", false
}

func visibilityOfScope(dbScope string) string {
	if dbScope == "org" {
		return libraryVisibilityOrg
	}
	return libraryVisibilityPrivate
}

// writeLibErr answers with {"error": message, "code": code}. Anything that is
// not a *libError is a 500 with the fallback message (never err.Error()).
func writeLibErr(c *gin.Context, err error, fallback string) {
	var le *libError
	if errors.As(err, &le) {
		c.JSON(le.status, gin.H{"error": le.msg, "code": le.code})
		return
	}
	slog.ErrorContext(c.Request.Context(), "library request failed", "route", c.FullPath(), "op", fallback)
	c.JSON(http.StatusInternalServerError, gin.H{"error": fallback})
}

func isAdmin(c *gin.Context) bool { return c.GetString("user_role") == "admin" }

// wire is toWire plus the bucket-derived fields.
func (h *LibraryHandler) wire(r *libraryRow, me string) libraryItem {
	it := r.toWire(me)
	if h.store != nil {
		if r.s3Key != nil && *r.s3Key != "" {
			// Private items are only ever returned to their owner, org items
			// to everyone, so the location is never shown to someone who
			// could not already read the item.
			u := h.store.URI(*r.s3Key)
			it.S3URI = &u
		}
		it.BucketChanged = r.bucketChanged
		synced := r.s3Key != nil && r.syncedSHA != nil && *r.syncedSHA == r.sha && !r.queued && !r.bucketChanged
		it.Synced = &synced
	}
	return it
}

// HasStore says whether the installation mirrors the library to a bucket.
func (h *LibraryHandler) HasStore() bool { return h.store != nil }

// limitRequest bounds a request that carries an item body, so a client
// cannot make the api read without end. Past the bound, binding fails and
// bindError names the limit.
func limitRequest(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, libraryRequestMaxBytes)
}

// bindError is the 400 for a request that did not bind: the body limit by
// name when that is why, otherwise the route's shape.
func bindError(c *gin.Context, err error, shape string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("request body exceeds %d bytes; an item body is at most %d bytes", libraryRequestMaxBytes, libraryBodyMaxBytes), "code": "library_body_too_large"})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": shape})
}

// toolURLSecretMsg is the message (and, via libInputError, the code
// tool_url_secret) for a server_url whose query carries a credential.
const toolURLSecretMsg = "server_url must not carry a credential in its query string; put the token in the tool's credential field instead"

// secretParamRE matches query parameter names that conventionally carry a
// credential, whole or as the last word (access_token, x-api-key, apikey).
var secretParamRE = regexp.MustCompile(`(?i)(^|[_-])(token|key|secret|password|passwd|auth|signature|sig|api_key|apikey|access_key)$`)

func toolURLHasSecretParam(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	for name := range u.Query() {
		if secretParamRE.MatchString(strings.ReplaceAll(name, "-", "_")) {
			return true
		}
	}
	return false
}

// libInputError answers a validateLibraryInput failure; the secret-in-URL
// case carries a code the UI can show by name.
func libInputError(c *gin.Context, status int, msg string) {
	switch msg {
	case toolURLSecretMsg:
		c.JSON(status, gin.H{"error": msg, "code": "tool_url_secret"})
	case bodyTooLargeMsg:
		c.JSON(status, gin.H{"error": msg, "code": "library_body_too_large"})
	default:
		c.JSON(status, gin.H{"error": msg})
	}
}

// bodyTooLargeMsg is the message (and, via libInputError, the code
// library_body_too_large) for a body over the cap.
var bodyTooLargeMsg = fmt.Sprintf("body must be at most %d bytes", libraryBodyMaxBytes)

// List handles GET /api/v1/library?scope=&path=&recursive=&kind=&q=.
//
// scope: private | org | all (default), plus the older mine (everything I
// own) and shared (org items that are not mine). path: a non-empty value is
// that exact folder; empty or absent means every folder, and with
// recursive=0 only the root. recursive=1 adds descendants of a non-empty
// path. q matches name, description, slug and folder path.
func (h *LibraryHandler) List(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	kind := c.Query("kind")
	if kind != "" && !validLibraryKind(kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be skill, context or tool"})
		return
	}
	scope := c.DefaultQuery("scope", "all")
	q := strings.TrimSpace(c.Query("q"))
	if utf8.RuneCountInString(q) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q must be at most 100 characters"})
		return
	}

	where := []string{visibleSQL(1)}
	args := []any{me}
	switch scope {
	case "all":
	case "mine":
		where = append(where, "li.owner_user_id = $1")
	case "shared":
		where = append(where, "li.visibility = 'org' AND (li.owner_user_id IS DISTINCT FROM $1)")
	case "private":
		where = append(where, "li.visibility = 'private' AND li.owner_user_id = $1")
	case "org":
		where = append(where, "li.visibility = 'org'")
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be private, org or all"})
		return
	}
	if kind != "" {
		args = append(args, kind)
		where = append(where, fmt.Sprintf("li.kind = $%d", len(args)))
	}
	path, hasPath := c.GetQuery("path")
	path = strings.Trim(strings.TrimSpace(path), "/")
	recursive := c.Query("recursive") != "0"
	switch {
	case hasPath && path != "":
		if !libkey.ValidFolderPath(path) {
			writeLibErr(c, errFolderInvalid, "list library failed")
			return
		}
		args = append(args, path)
		if c.Query("recursive") == "1" {
			where = append(where, fmt.Sprintf("(fo.path = $%d OR fo.path LIKE $%d || '/%%')", len(args), len(args)))
		} else {
			where = append(where, fmt.Sprintf("fo.path = $%d", len(args)))
		}
	case hasPath && !recursive:
		where = append(where, "li.folder_id IS NULL")
	}
	if q != "" {
		args = append(args, "%"+escapeLike(q)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf("(li.name ILIKE $%d ESCAPE '\\' OR li.description ILIKE $%d ESCAPE '\\' OR li.slug ILIKE $%d ESCAPE '\\' OR fo.path ILIKE $%d ESCAPE '\\')", n, n, n, n))
	}
	rows, err := h.db.Query(ctx, `SELECT `+libraryListCols+libraryFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY li.updated_at DESC LIMIT 1000`, args...)
	if err != nil {
		slog.ErrorContext(ctx, "library list failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list library failed"})
		return
	}
	defer rows.Close()
	items := []libraryItem{}
	for rows.Next() {
		r, err := scanLibraryRow(rows)
		if err != nil {
			slog.ErrorContext(ctx, "library list scan failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "list library failed"})
			return
		}
		items = append(items, h.wire(r, me))
	}
	c.JSON(http.StatusOK, items)
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// loadVisible fetches one item the user may see, or (nil, nil).
func (h *LibraryHandler) loadVisible(ctx context.Context, me, id string) (*libraryRow, error) {
	r, err := scanLibraryRow(h.db.QueryRow(ctx, `SELECT `+librarySelectCols+libraryFrom+` WHERE li.id = $2 AND `+visibleSQL(1), me, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Get handles GET /api/v1/library/:id.
func (h *LibraryHandler) Get(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	r, err := h.loadVisible(c.Request.Context(), me, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read item failed"})
		return
	}
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	c.JSON(http.StatusOK, h.wire(r, me))
}

func truncateSlug(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(s[:n], "-")
}

// itemVisibility resolves the root an input names: scope (private|org)
// wins, then visibility, default private.
func itemVisibility(in *libraryInput) (string, bool) {
	if in.Scope != "" {
		s, ok := dbScope(in.Scope)
		if !ok {
			return "", false
		}
		return visibilityOfScope(s), true
	}
	if in.Visibility != nil {
		return *in.Visibility, true
	}
	return libraryVisibilityPrivate, true
}

// Create handles POST /api/v1/library.
func (h *LibraryHandler) Create(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	limitRequest(c)
	var in libraryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		bindError(c, err, "body must be {kind, name, slug?, description?, body, visibility?, scope?, path?}")
		return
	}
	if msg := validateLibraryInput(&in, false); msg != "" {
		libInputError(c, http.StatusBadRequest, msg)
		return
	}
	visibility, ok := itemVisibility(&in)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be private or org"})
		return
	}
	id, _, err := h.createOne(ctx, me, in, visibility)
	if err != nil {
		writeLibErr(c, err, "create item failed")
		return
	}
	evt := activity.FromContext(c, activity.LibraryItemCreated)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"kind": in.Kind, "visibility": visibility, "bytes": len(in.Body)}
	h.activity.Record(ctx, evt)
	if visibility == libraryVisibilityOrg {
		shared := activity.FromContext(c, activity.LibraryItemShared)
		shared.TargetType, shared.TargetID = evt.TargetType, evt.TargetID
		shared.Detail = map[string]any{"kind": in.Kind}
		h.activity.Record(ctx, shared)
	}
	r, err := h.loadVisible(ctx, me, id)
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create item failed"})
		return
	}
	c.JSON(http.StatusCreated, h.wire(r, me))
}

// createOne runs insertItemOnce in its own transaction, retrying auto-slug
// collisions with a fresh transaction each time (a unique violation aborts
// the transaction it happened in).
func (h *LibraryHandler) createOne(ctx context.Context, me string, in libraryInput, visibility string) (id, slug string, err error) {
	for attempt := 0; attempt < 50; attempt++ {
		tx, err := h.db.Begin(ctx)
		if err != nil {
			return "", "", err
		}
		id, slug, err = insertItemOnce(ctx, tx, me, in, visibility, attempt)
		if err != nil {
			_ = tx.Rollback(ctx)
			if errors.Is(err, errSlugRetry) {
				continue
			}
			return "", "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", "", err
		}
		return id, slug, nil
	}
	return "", "", errSlugTaken
}

var errSlugRetry = errors.New("slug collision, retry")

// insertItemOnce is one attempt: attempt 0 uses the base slug, attempt n
// appends -(n+1). An explicit slug never retries.
func insertItemOnce(ctx context.Context, tx pgx.Tx, me string, in libraryInput, visibility string, attempt int) (id, slug string, err error) {
	// Serialise this owner's creates on their user row so two concurrent
	// creates cannot both pass the count check.
	var count int
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, me); err != nil {
		return "", "", err
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM library_items WHERE owner_user_id = $1`, me).Scan(&count); err != nil {
		return "", "", err
	}
	if count >= libraryItemsPerUser {
		return "", "", errLibraryFull
	}
	path := ""
	if in.Path != nil {
		path = *in.Path
	}
	folderID, _, err := ensureFolderPath(ctx, tx, scopeOfVisibility(visibility), me, path)
	if err != nil {
		return "", "", err
	}
	explicit := in.Slug != nil
	base := slugify(in.Name)
	if explicit {
		base = *in.Slug
	}
	slug = base
	if attempt > 0 {
		n := attempt + 1
		slug = fmt.Sprintf("%s-%d", truncateSlug(base, 100-len(fmt.Sprint(n))-1), n)
	}
	if !libkey.ValidSlug(slug) {
		// A reserved device name (con, nul, com1...) cannot be a file name.
		if explicit {
			return "", "", &libError{http.StatusBadRequest, "slug_invalid", "that slug cannot be used as a file name"}
		}
		slug = truncateSlug(base, 90) + "-item"
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO library_items (kind, owner_user_id, visibility, slug, name, description, body, content_sha256, shared_at, updated_by, folder_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $3 = 'org' THEN now() END, $2, $9)
		RETURNING id
	`, in.Kind, me, visibility, slug, in.Name, in.Description, in.Body, contentSHA256(in.Body), folderID).Scan(&id)
	if err == nil {
		if err := enqueueItem(ctx, tx, id); err != nil {
			return "", "", err
		}
		return id, slug, nil
	}
	if isUniqueViolation(err) {
		if explicit {
			if visibility == libraryVisibilityOrg {
				return "", "", errSharedPathTaken
			}
			return "", "", errSlugTaken
		}
		return "", "", errSlugRetry
	}
	return "", "", err
}

func scopeOfVisibility(v string) string {
	if v == libraryVisibilityOrg {
		return "org"
	}
	return "user"
}

// Update handles PUT /api/v1/library/:id (owner only; kind is immutable).
func (h *LibraryHandler) Update(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	limitRequest(c)
	var in libraryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		bindError(c, err, "body must be {name, slug?, description?, body}")
		return
	}
	// The stored kind decides how the body is validated.
	var kind, oldBody, visibility string
	err := h.db.QueryRow(ctx, `SELECT kind, body, visibility FROM library_items WHERE id = $1 AND owner_user_id = $2`, id, me).Scan(&kind, &oldBody, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return
	}
	in.Kind = kind
	if msg := validateLibraryInput(&in, true); msg != "" {
		libInputError(c, http.StatusBadRequest, msg)
		return
	}
	if in.Slug != nil && !libkey.ValidSlug(*in.Slug) {
		writeLibErr(c, &libError{http.StatusBadRequest, "slug_invalid", "that slug cannot be used as a file name"}, "update item failed")
		return
	}
	// A tool whose server changed has a stale catalogue: clear it so the UI
	// asks for a re-verify instead of offering tools the new server may not
	// have.
	clearCatalog := false
	if kind == libraryKindTool {
		oldCfg, _ := parseToolConfig(oldBody)
		newCfg, _ := parseToolConfig(in.Body)
		clearCatalog = oldCfg.ServerURL != newCfg.ServerURL
	}
	slugSQL := "slug"
	args := []any{in.Name, in.Description, in.Body, contentSHA256(in.Body), me, id}
	if in.Slug != nil {
		args = append(args, *in.Slug)
		slugSQL = fmt.Sprintf("$%d", len(args))
	}
	catalogSQL := ""
	if clearCatalog {
		catalogSQL = ", tool_catalog = NULL, tool_catalog_at = NULL"
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE library_items SET name = $1, description = $2, body = $3, content_sha256 = $4,
			updated_at = now(), updated_by = $5, slug = %s %s
		WHERE id = $6 AND owner_user_id = $5
	`, slugSQL, catalogSQL), args...)
	if err != nil {
		if isUniqueViolation(err) {
			if visibility == libraryVisibilityOrg {
				writeLibErr(c, errSharedPathTaken, "update item failed")
			} else {
				writeLibErr(c, errSlugTaken, "update item failed")
			}
			return
		}
		slog.ErrorContext(ctx, "library update failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if err := enqueueItem(ctx, tx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return
	}
	evt := activity.FromContext(c, activity.LibraryItemUpdated)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"kind": kind, "bytes": len(in.Body), "changed": oldBody != in.Body}
	h.activity.Record(ctx, evt)
	r, err := h.loadVisible(ctx, me, id)
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return
	}
	c.JSON(http.StatusOK, h.wire(r, me))
}

// deleteItemTx deletes one item the SQL predicate allows and queues the
// removal of its objects (every version) in the same transaction.
func deleteItemTx(ctx context.Context, tx pgx.Tx, id, ownerOrEmpty string, orgOnly bool) (kind, visibility string, owner *string, hadCred bool, err error) {
	var k1, k2 *string
	where := `id = $1`
	args := []any{id}
	if ownerOrEmpty != "" {
		where += ` AND owner_user_id = $2`
		args = append(args, ownerOrEmpty)
	}
	if orgOnly {
		where += ` AND visibility = 'org'`
	}
	err = tx.QueryRow(ctx, `DELETE FROM library_items WHERE `+where+`
		RETURNING kind, visibility, owner_user_id::text, tool_credential_last4 IS NOT NULL, s3_key, s3_pending_key`, args...).
		Scan(&kind, &visibility, &owner, &hadCred, &k1, &k2)
	if err != nil {
		return
	}
	err = enqueueItemDeleted(ctx, tx, id, k1, k2)
	return
}

// Delete handles DELETE /api/v1/library/:id (owner only).
func (h *LibraryHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete item failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	kind, visibility, _, hadCred, err := deleteItemTx(ctx, tx, id, me, false)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		slog.ErrorContext(ctx, "library delete failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete item failed"})
		return
	}
	if hadCred && h.tools != nil {
		h.tools.removeCredentialQuietly(ctx, id)
	}
	evt := activity.FromContext(c, activity.LibraryItemDeleted)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"kind": kind, "visibility": visibility}
	h.activity.Record(ctx, evt)
	c.Status(http.StatusNoContent)
}

type shareRequest struct {
	Visibility string `json:"visibility"`
}

// Share handles POST /api/v1/library/:id/share {visibility: org|private}
// (owner only). It is a thin wrapper over the item move: sharing carries the
// item's folder path into the org tree (creating missing org folders) and
// 409s shared_path_taken when the organisation already has a file with that
// name there; unsharing returns it to the folder it was shared from.
func (h *LibraryHandler) Share(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	var req shareRequest
	if err := c.ShouldBindJSON(&req); err != nil || (req.Visibility != libraryVisibilityOrg && req.Visibility != libraryVisibilityPrivate) {
		c.JSON(http.StatusBadRequest, gin.H{"error": `body must be {"visibility": "org" | "private"}`})
		return
	}
	// Admins may not use this route on others' items: AdminUnshare does that.
	out, err := h.moveItem(ctx, id, me, false, scopeOfVisibility(req.Visibility), nil, "")
	if err != nil {
		writeLibErr(c, err, "share item failed")
		return
	}
	if out.fromVis != out.toVis {
		typ := activity.LibraryItemShared
		if out.toVis == libraryVisibilityPrivate {
			typ = activity.LibraryItemUnshared
		}
		evt := activity.FromContext(c, typ)
		evt.TargetType = activity.StrPtr("library_item")
		evt.TargetID = activity.StrPtr(id)
		evt.Detail = map[string]any{"kind": out.kind}
		h.activity.Record(ctx, evt)
	}
	r, err := h.loadVisible(ctx, me, id)
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "share item failed"})
		return
	}
	c.JSON(http.StatusOK, h.wire(r, me))
}

// ---------------------------------------------------------------------------
// Export / import
// ---------------------------------------------------------------------------

// renderExport renders one item as the file the bucket holds and an export
// contains: markdown with a darkplum_id/name/description/kind front matter for
// skill and context, JSON (without any credential -- none is ever stored
// here) for a tool. This is the ONE serializer: the sync worker writes
// exactly these bytes, so an export and the bucket are byte-identical.
// darkplum_id is how Sync from bucket recognises an item that was moved or
// edited in the bucket.
func renderExport(it libraryItem) (filename string, contentType string, body []byte) {
	if it.Kind == libraryKindTool {
		out := map[string]any{
			"darkplum_id": it.ID,
			"kind":        libraryKindTool,
			"name":        it.Name,
			"description": it.Description,
		}
		if it.Tool != nil {
			out["server_url"] = it.Tool.ServerURL
			if len(it.Tool.AllowedTools) > 0 {
				out["allowed_tools"] = it.Tool.AllowedTools
			}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return it.Slug + ".json", "application/json", append(b, '\n')
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("darkplum_id: " + it.ID + "\n")
	sb.WriteString("name: " + yamlScalar(it.Name) + "\n")
	if it.Description != "" {
		sb.WriteString("description: " + yamlScalar(it.Description) + "\n")
	}
	sb.WriteString("kind: " + it.Kind + "\n")
	sb.WriteString("---\n\n")
	sb.WriteString(strings.TrimRight(it.Body, "\n"))
	sb.WriteString("\n")
	return it.Slug + ".md", "text/markdown; charset=utf-8", []byte(sb.String())
}

// normalizeObject makes the bytes of an object comparable: line endings and
// trailing newlines that a console editor may add or drop do not count as a
// change. Both the hash the worker records and the hash Sync takes of a
// bucket object go through this.
func normalizeObject(b []byte) []byte {
	return append(bytes.TrimRight(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), "\r\n"), '\n')
}

func objectSHA256(b []byte) string {
	sum := sha256.Sum256(normalizeObject(b))
	return hex.EncodeToString(sum[:])
}

// yamlScalar quotes a value whenever it could be misread as YAML syntax.
func yamlScalar(s string) string {
	if s == "" || strings.ContainsAny(s, ":#\"'\n\r\t{}[],&*!|>%@`") || strings.TrimSpace(s) != s {
		b, _ := json.Marshal(s) // JSON string syntax is valid YAML double-quoted
		return string(b)
	}
	return s
}

// ExportOne handles GET /api/v1/library/:id/export.
func (h *LibraryHandler) ExportOne(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	me := c.GetString("user_id")
	r, err := h.loadVisible(c.Request.Context(), me, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export failed"})
		return
	}
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	name, ct, body := renderExport(h.wire(r, me))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	c.Data(http.StatusOK, ct, body)
}

// ExportAll handles GET /api/v1/library/export: a zip of every item the
// user can see (their own and the organisation's shared ones). Each entry is
// named by the item's bucket key (org/<path>/<slug>.<kind>.<ext> or
// people/<you>/<path>/...) and holds exactly the bytes the bucket holds, so
// unzipping into the bucket prefix restores the tree.
func (h *LibraryHandler) ExportAll(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	rows, err := h.db.Query(ctx, `SELECT `+librarySelectCols+libraryFrom+` WHERE `+visibleSQL(1)+` ORDER BY li.visibility, fo.path NULLS FIRST, li.kind, li.slug LIMIT $2`, me, libraryExportMaxItems)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export failed"})
		return
	}
	defer rows.Close()
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "darkplum-library-"+time.Now().UTC().Format("2006-01-02")+".zip"))
	c.Status(http.StatusOK)
	zw := zip.NewWriter(c.Writer)
	count := 0
	var failed error
	for rows.Next() {
		r, err := scanLibraryRow(rows)
		if err != nil {
			failed = err
			break
		}
		key, err := keyForRow(r)
		if err != nil {
			// Cannot be a file name in the tree; skipped, counted.
			continue
		}
		_, _, body := renderExport(r.toWire(me))
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: key.String(), Method: zip.Deflate, Modified: r.updatedAt})
		if err != nil {
			failed = err
			break
		}
		if _, err := fw.Write(body); err != nil {
			failed = err
			break
		}
		count++
	}
	if failed == nil {
		failed = rows.Err()
	}
	if failed != nil {
		// The status is committed; the archive is closed short and the
		// failure is logged. A truncated zip fails to open, which is the
		// honest outcome, rather than a 200 that looks complete.
		slog.ErrorContext(ctx, "library export interrupted", "user_id", me, "written", count)
	}
	_ = zw.Close()
	evt := activity.FromContext(c, activity.LibraryExported)
	evt.Detail = map[string]any{"count": count}
	h.activity.Record(ctx, evt)
}

// keyForRow builds the bucket key of a scanned row from Postgres values only.
func keyForRow(r *libraryRow) (libkey.Key, error) {
	if r.visibility == libraryVisibilityOrg {
		return libkey.ForItem(libkey.ScopeOrg, "", r.folderPath, r.kind, r.slug)
	}
	if r.ownerSlug == nil {
		return libkey.Key{}, libkey.ErrInvalid
	}
	return libkey.ForItem(libkey.ScopeUser, *r.ownerSlug, r.folderPath, r.kind, r.slug)
}

// importRequest is the body of POST /api/v1/library/import: one file the
// browser read. kind says which tab the person imported from; a `kind:`
// front-matter key in the file overrides it.
type importRequest struct {
	Filename   string  `json:"filename"`
	Content    string  `json:"content"`
	Kind       string  `json:"kind"`
	Visibility *string `json:"visibility,omitempty"`
	// Scope (private|org) and Path say where the item lands; missing folders
	// are created. Scope wins over Visibility.
	Scope string `json:"scope,omitempty"`
	Path  string `json:"path,omitempty"`
}

type importPreview struct {
	Kind        string       `json:"kind"`
	Name        string       `json:"name"`
	Slug        string       `json:"slug"`
	Description string       `json:"description"`
	SizeBytes   int          `json:"size_bytes"`
	Dropped     []string     `json:"dropped"`
	Item        *libraryItem `json:"item,omitempty"`
}

// frontMatter splits a markdown document into front-matter keys and body.
// Only `---` fences are recognised; keys are `name: value` with an optional
// JSON/double-quoted value. Anything else in the block is reported as
// dropped rather than parsed -- no YAML library runs on an imported file.
func frontMatter(content string) (keys map[string]string, dropped []string, body string) {
	keys = map[string]string{}
	content = strings.TrimPrefix(content, "\xEF\xBB\xBF")
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return keys, nil, content
	}
	rest := content[strings.Index(content, "\n")+1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return keys, nil, content
	}
	block := rest[:end]
	after := rest[end+4:]
	// Skip the rest of the closing fence line.
	if nl := strings.Index(after, "\n"); nl >= 0 {
		after = after[nl+1:]
	} else {
		after = ""
	}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			dropped = append(dropped, strings.TrimSpace(line))
			continue
		}
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "\"") {
			var s string
			if json.Unmarshal([]byte(v), &s) == nil {
				v = s
			}
		} else if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
			v = v[1 : len(v)-1]
		}
		switch k {
		case "name", "description", "kind", "darkplum_id":
			keys[k] = v
		default:
			dropped = append(dropped, k)
		}
	}
	return keys, dropped, strings.TrimLeft(after, "\n")
}

// parseImport turns a file into a libraryInput plus a preview.
func parseImport(req importRequest) (libraryInput, importPreview, string) {
	var in libraryInput
	var pv importPreview
	if len(req.Content) > libraryBodyMaxBytes*2 {
		return in, pv, fmt.Sprintf("file is larger than %d bytes", libraryBodyMaxBytes*2)
	}
	req.Content = strings.TrimPrefix(req.Content, "\xEF\xBB\xBF")
	name := strings.TrimSpace(req.Filename)
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	ext := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
	if !strings.Contains(name, ".") {
		ext = ""
	}
	stem := strings.TrimSuffix(name, "."+ext)
	if stem == "" {
		stem = "imported"
	}
	kind := req.Kind
	if kind == "" && ext == "json" {
		kind = libraryKindTool
	}

	trimmed := strings.TrimSpace(req.Content)
	if ext == "json" || (strings.HasPrefix(trimmed, "{") && kind == libraryKindTool) {
		var raw struct {
			Kind         string          `json:"kind"`
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			ServerURL    string          `json:"server_url"`
			AllowedTools []string        `json:"allowed_tools"`
			Extra        json.RawMessage `json:"-"`
		}
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			return in, pv, "a .json import must be a tool definition object"
		}
		var all map[string]json.RawMessage
		_ = json.Unmarshal([]byte(trimmed), &all)
		for k := range all {
			switch k {
			case "kind", "name", "description", "server_url", "allowed_tools", "darkplum_id":
			default:
				pv.Dropped = append(pv.Dropped, k)
			}
		}
		if raw.Kind != "" && raw.Kind != libraryKindTool {
			return in, pv, "a .json import must have kind \"tool\""
		}
		cfg := toolConfig{ServerURL: raw.ServerURL, AllowedTools: raw.AllowedTools}
		b, _ := json.Marshal(cfg)
		in = libraryInput{Kind: libraryKindTool, Name: firstNonEmpty(raw.Name, stem), Description: raw.Description, Body: string(b), Visibility: req.Visibility}
	} else {
		keys, dropped, body := frontMatter(req.Content)
		pv.Dropped = dropped
		if k := keys["kind"]; k != "" {
			if !validLibraryKind(k) || k == libraryKindTool {
				return in, pv, "front matter kind must be skill or context"
			}
			kind = k
		}
		if kind == "" {
			kind = libraryKindSkill
		}
		in = libraryInput{Kind: kind, Name: firstNonEmpty(keys["name"], humanize(stem)), Description: keys["description"], Body: strings.TrimRight(body, " \t\r\n"), Visibility: req.Visibility}
	}
	if msg := validateLibraryInput(&in, false); msg != "" {
		return in, pv, msg
	}
	pv.Kind, pv.Name, pv.Slug, pv.Description, pv.SizeBytes = in.Kind, in.Name, slugify(in.Name), in.Description, len(in.Body)
	if pv.Dropped == nil {
		pv.Dropped = []string{}
	}
	return in, pv, ""
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}

// humanize turns "contract-review_v2" into "contract review v2".
func humanize(stem string) string {
	s := strings.NewReplacer("-", " ", "_", " ").Replace(stem)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "Imported item"
	}
	// SKILL.md is the conventional file name inside a Claude skill folder;
	// it names nothing.
	if strings.EqualFold(s, "skill") {
		return "Imported skill"
	}
	return s
}

// Import handles POST /api/v1/library/import[?dry_run=1].
func (h *LibraryHandler) Import(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	limitRequest(c)
	var req importRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		bindError(c, err, "body must be {filename, content, kind?, scope?, path?}")
		return
	}
	in, pv, msg := parseImport(req)
	if msg != "" {
		body := gin.H{"error": msg, "dropped": pv.Dropped}
		if msg == toolURLSecretMsg {
			body["code"] = "tool_url_secret"
		}
		c.JSON(http.StatusUnprocessableEntity, body)
		return
	}
	if c.Query("dry_run") == "1" {
		c.JSON(http.StatusOK, pv)
		return
	}
	in.Scope = req.Scope
	if p := strings.Trim(strings.TrimSpace(req.Path), "/"); p != "" {
		if !libkey.ValidFolderPath(p) {
			writeLibErr(c, errFolderInvalid, "import failed")
			return
		}
		in.Path = &p
	}
	visibility, ok := itemVisibility(&in)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be private or org"})
		return
	}
	id, _, err := h.createOne(ctx, me, in, visibility)
	if err != nil {
		writeLibErr(c, err, "import failed")
		return
	}
	evt := activity.FromContext(c, activity.LibraryImported)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"kind": in.Kind, "bytes": len(in.Body), "dropped": len(pv.Dropped)}
	h.activity.Record(ctx, evt)
	r, err := h.loadVisible(ctx, me, id)
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "import failed"})
		return
	}
	it := h.wire(r, me)
	pv.Item = &it
	c.JSON(http.StatusCreated, pv)
}

// ---------------------------------------------------------------------------
// Admin governance: org-shared items only
// ---------------------------------------------------------------------------

// ListShared handles GET /api/v1/library/shared (admin): every org-shared
// item with its owner. Private items are excluded in SQL.
func (h *LibraryHandler) ListShared(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	rows, err := h.db.Query(ctx, `SELECT `+libraryListCols+libraryFrom+` WHERE li.visibility = 'org' ORDER BY li.updated_at DESC LIMIT 1000`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list shared items failed"})
		return
	}
	defer rows.Close()
	items := []libraryItem{}
	for rows.Next() {
		r, err := scanLibraryRow(rows)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "list shared items failed"})
			return
		}
		items = append(items, h.wire(r, me))
	}
	c.JSON(http.StatusOK, items)
}

// AdminUnshare handles POST /api/v1/library/shared/:id/unshare (admin):
// makes an org item private again. It stays with its owner, in the folder it
// was shared from (previous_folder_path), or the root of their tree.
func (h *LibraryHandler) AdminUnshare(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	ctx := c.Request.Context()
	me := c.GetString("user_id")
	var kind string
	var owner *string
	if err := h.db.QueryRow(ctx, `SELECT kind, owner_user_id::text FROM library_items WHERE id = $1 AND visibility = 'org'`, id).Scan(&kind, &owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "shared item not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unshare failed"})
		return
	}
	if _, err := h.moveItem(ctx, id, me, true, "user", nil, ""); err != nil {
		var le *libError
		if errors.As(err, &le) && le.code == "not_found" {
			c.JSON(http.StatusNotFound, gin.H{"error": "shared item not found"})
			return
		}
		writeLibErr(c, err, "unshare failed")
		return
	}
	evt := activity.FromContext(c, activity.LibraryItemUnshared)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"kind": kind, "by_admin": true, "owner_id": owner}
	h.activity.Record(ctx, evt)
	c.Status(http.StatusNoContent)
}

// AdminDelete handles DELETE /api/v1/library/shared/:id (admin): removes an
// org-shared item. A private item is not found, by construction.
func (h *LibraryHandler) AdminDelete(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	ctx := c.Request.Context()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	kind, _, owner, hadCred, err := deleteItemTx(ctx, tx, id, "", true)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "shared item not found"})
		return
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}
	if hadCred && h.tools != nil {
		h.tools.removeCredentialQuietly(ctx, id)
	}
	evt := activity.FromContext(c, activity.LibraryItemDeleted)
	evt.TargetType = activity.StrPtr("library_item")
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{"kind": kind, "visibility": "org", "by_admin": true, "owner_id": owner}
	h.activity.Record(ctx, evt)
	c.Status(http.StatusNoContent)
}

// reassignLibraryOnUserDelete runs inside the user-delete transaction
// (handler/users.go), under its admin lock: the user's private items are
// removed with them, like their conversations, and their org-shared items
// pass to the acting administrator so a shared skill never silently vanishes
// or loses its editor. Erasure of the bucket copies runs through the queue:
// each removed item and each of the person's folder markers gets a
// library_sync row, in this same transaction, carrying the last key; the
// worker deletes the objects and purges every version. Returns the ids
// reassigned (for the audit event) and the ids of removed items that held a
// tool credential (for Secret cleanup after commit).
func reassignLibraryOnUserDelete(ctx context.Context, tx pgx.Tx, deletedUserID, adminID string) (reassigned, credentialsToRemove []string, err error) {
	rows, err := tx.Query(ctx, `
		DELETE FROM library_items WHERE owner_user_id = $1 AND visibility = 'private'
		RETURNING id::text, tool_credential_last4 IS NOT NULL, s3_key, s3_pending_key
	`, deletedUserID)
	if err != nil {
		return nil, nil, err
	}
	type gone struct {
		id     string
		k1, k2 *string
	}
	var removed []gone
	for rows.Next() {
		var g gone
		var hadCred bool
		if err := rows.Scan(&g.id, &hadCred, &g.k1, &g.k2); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if hadCred {
			credentialsToRemove = append(credentialsToRemove, g.id)
		}
		removed = append(removed, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, g := range removed {
		if err := enqueueItemDeleted(ctx, tx, g.id, g.k1, g.k2); err != nil {
			return nil, nil, err
		}
	}
	// The person's folders cascade with the user row; queue their markers.
	frows, err := tx.Query(ctx, `SELECT id::text, s3_key FROM library_folders WHERE owner_user_id = $1`, deletedUserID)
	if err != nil {
		return nil, credentialsToRemove, err
	}
	type fgone struct {
		id  string
		key *string
	}
	var folders []fgone
	for frows.Next() {
		var f fgone
		if err := frows.Scan(&f.id, &f.key); err != nil {
			frows.Close()
			return nil, credentialsToRemove, err
		}
		folders = append(folders, f)
	}
	frows.Close()
	for _, f := range folders {
		if err := enqueueFolderDeleted(ctx, tx, f.id, f.key); err != nil {
			return nil, credentialsToRemove, err
		}
	}
	// Shared items pass to the administrator. They sit in the org tree, whose
	// uniqueness does not involve the owner, so no slug has to change and
	// their bucket keys are untouched.
	rows, err = tx.Query(ctx, `
		UPDATE library_items SET owner_user_id = $2, updated_at = now(), updated_by = $2
		WHERE owner_user_id = $1 AND visibility = 'org'
		RETURNING id::text
	`, deletedUserID, adminID)
	if err != nil {
		return nil, credentialsToRemove, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, credentialsToRemove, err
		}
		reassigned = append(reassigned, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, credentialsToRemove, err
	}
	// Rewritten with a new updated_by is not a content change, but the
	// stored previous_folder_path is the dead user's private path: clear it.
	if len(reassigned) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE library_items SET previous_folder_path = NULL WHERE id = ANY($1::uuid[])`, reassigned); err != nil {
			return nil, credentialsToRemove, err
		}
	}
	return reassigned, credentialsToRemove, nil
}
