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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

const (
	libraryKindSkill   = "skill"
	libraryKindContext = "context"
	libraryKindTool    = "tool"

	libraryVisibilityPrivate = "private"
	libraryVisibilityOrg     = "org"

	// libraryBodyMaxBytes matches the CHECK constraint in migration 026.
	libraryBodyMaxBytes = 65536
	libraryNameMaxChars = 100
	libraryDescMaxChars = 500
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
}

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
}

// libraryRow is one scanned row; toWire turns it into a libraryItem.
type libraryRow struct {
	id, kind, name, slug, description, body, visibility, sha string
	ownerID, ownerName, ownerEmail                           *string
	sharedAt, catalogAt                                      *time.Time
	createdAt, updatedAt                                     time.Time
	toolCatalog                                              []byte
	credLast4                                                *string
}

const librarySelectCols = `
	li.id, li.kind, li.name, li.slug, li.description, li.body, li.visibility, li.content_sha256,
	li.owner_user_id::text, u.name, u.email, li.shared_at, li.created_at, li.updated_at,
	li.tool_catalog, li.tool_catalog_at, li.tool_credential_last4`

const libraryFrom = ` FROM library_items li LEFT JOIN users u ON u.id = li.owner_user_id `

func scanLibraryRow(row pgx.Row) (*libraryRow, error) {
	var r libraryRow
	err := row.Scan(&r.id, &r.kind, &r.name, &r.slug, &r.description, &r.body, &r.visibility, &r.sha,
		&r.ownerID, &r.ownerName, &r.ownerEmail, &r.sharedAt, &r.createdAt, &r.updatedAt,
		&r.toolCatalog, &r.catalogAt, &r.credLast4)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *libraryRow) toWire(me string) libraryItem {
	it := libraryItem{
		ID: r.id, Kind: r.kind, Name: r.name, Slug: r.slug, Description: r.description, Body: r.body,
		Visibility: r.visibility, ContentSHA256: r.sha, SizeBytes: len(r.body),
		SharedAt: r.sharedAt, CreatedAt: r.createdAt, UpdatedAt: r.updatedAt,
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
	if in.Visibility != nil && *in.Visibility != libraryVisibilityPrivate && *in.Visibility != libraryVisibilityOrg {
		return "visibility must be private or org"
	}
	if strings.TrimSpace(in.Body) == "" {
		return "body is required"
	}
	if len(in.Body) > libraryBodyMaxBytes {
		return fmt.Sprintf("body must be at most %d bytes", libraryBodyMaxBytes)
	}
	if in.Kind == libraryKindTool {
		cfg, err := parseToolConfig(in.Body)
		if err != nil {
			return "tool body: " + err.Error()
		}
		// Store the configuration normalised, so what is read back is what
		// was validated.
		b, _ := json.Marshal(cfg)
		in.Body = string(b)
	}
	return ""
}

// List handles GET /api/v1/library?kind=&scope=mine|shared|all&q=.
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
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be mine, shared or all"})
		return
	}
	if kind != "" {
		args = append(args, kind)
		where = append(where, fmt.Sprintf("li.kind = $%d", len(args)))
	}
	if q != "" {
		args = append(args, "%"+escapeLike(q)+"%")
		where = append(where, fmt.Sprintf("(li.name ILIKE $%d ESCAPE '\\' OR li.description ILIKE $%d ESCAPE '\\')", len(args), len(args)))
	}
	rows, err := h.db.Query(ctx, `SELECT `+librarySelectCols+libraryFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY li.updated_at DESC LIMIT 1000`, args...)
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
		items = append(items, r.toWire(me))
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
	c.JSON(http.StatusOK, r.toWire(me))
}

var (
	errLibraryFull = errors.New("library is full")
	errSlugTaken   = errors.New("slug already in use")
)

func truncateSlug(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(s[:n], "-")
}

// Create handles POST /api/v1/library.
func (h *LibraryHandler) Create(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	var in libraryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {kind, name, slug?, description?, body, visibility?}"})
		return
	}
	if msg := validateLibraryInput(&in, false); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	visibility := libraryVisibilityPrivate
	if in.Visibility != nil {
		visibility = *in.Visibility
	}
	id, _, err := h.createOne(ctx, me, in, visibility)
	switch {
	case errors.Is(err, errLibraryFull):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": fmt.Sprintf("your library holds the maximum of %d items", libraryItemsPerUser)})
		return
	case errors.Is(err, errSlugTaken):
		c.JSON(http.StatusConflict, gin.H{"error": "you already have an item with that slug"})
		return
	case err != nil:
		slog.ErrorContext(ctx, "library create failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create item failed"})
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
	c.JSON(http.StatusCreated, r.toWire(me))
}

// createOne runs insertItem in its own transaction, retrying slug
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
	err = tx.QueryRow(ctx, `
		INSERT INTO library_items (kind, owner_user_id, visibility, slug, name, description, body, content_sha256, shared_at, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $3 = 'org' THEN now() END, $2)
		RETURNING id
	`, in.Kind, me, visibility, slug, in.Name, in.Description, in.Body, contentSHA256(in.Body)).Scan(&id)
	if err == nil {
		return id, slug, nil
	}
	if isUniqueViolation(err) {
		if explicit {
			return "", "", errSlugTaken
		}
		return "", "", errSlugRetry
	}
	return "", "", err
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
	var in libraryInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {name, slug?, description?, body}"})
		return
	}
	// The stored kind decides how the body is validated.
	var kind, oldBody string
	err := h.db.QueryRow(ctx, `SELECT kind, body FROM library_items WHERE id = $1 AND owner_user_id = $2`, id, me).Scan(&kind, &oldBody)
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
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
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
	tag, err := h.db.Exec(ctx, fmt.Sprintf(`
		UPDATE library_items SET name = $1, description = $2, body = $3, content_sha256 = $4,
			updated_at = now(), updated_by = $5, slug = %s %s
		WHERE id = $6 AND owner_user_id = $5
	`, slugSQL, catalogSQL), args...)
	if err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "you already have an item with that slug"})
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
	c.JSON(http.StatusOK, r.toWire(me))
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
	var kind, visibility string
	var hadCred bool
	err := h.db.QueryRow(ctx, `
		DELETE FROM library_items WHERE id = $1 AND owner_user_id = $2
		RETURNING kind, visibility, tool_credential_last4 IS NOT NULL
	`, id, me).Scan(&kind, &visibility, &hadCred)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
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
// (owner only).
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
	var kind, before string
	err := h.db.QueryRow(ctx, `
		UPDATE library_items li SET visibility = $3,
			shared_at = CASE WHEN $3 = 'org' THEN COALESCE(shared_at, now()) ELSE NULL END,
			updated_at = now(), updated_by = $2
		FROM (SELECT visibility AS before FROM library_items WHERE id = $1 AND owner_user_id = $2) prev
		WHERE li.id = $1 AND li.owner_user_id = $2
		RETURNING li.kind, prev.before
	`, id, me, req.Visibility).Scan(&kind, &before)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "library share failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "share item failed"})
		return
	}
	if before != req.Visibility {
		typ := activity.LibraryItemShared
		if req.Visibility == libraryVisibilityPrivate {
			typ = activity.LibraryItemUnshared
		}
		evt := activity.FromContext(c, typ)
		evt.TargetType = activity.StrPtr("library_item")
		evt.TargetID = activity.StrPtr(id)
		evt.Detail = map[string]any{"kind": kind}
		h.activity.Record(ctx, evt)
	}
	r, err := h.loadVisible(ctx, me, id)
	if err != nil || r == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "share item failed"})
		return
	}
	c.JSON(http.StatusOK, r.toWire(me))
}

// ---------------------------------------------------------------------------
// Export / import
// ---------------------------------------------------------------------------

// renderExport renders one item as a portable file: markdown with a
// name/description/kind front matter for skill and context, JSON (without
// any credential -- none is ever stored here) for a tool.
func renderExport(it libraryItem) (filename string, contentType string, body []byte) {
	if it.Kind == libraryKindTool {
		out := map[string]any{
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

// yamlScalar quotes a value whenever it could be misread as YAML syntax.
func yamlScalar(s string) string {
	if s == "" || strings.ContainsAny(s, ":#\"'\n{}[],&*!|>%@`") || strings.TrimSpace(s) != s {
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
	name, ct, body := renderExport(r.toWire(me))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	c.Data(http.StatusOK, ct, body)
}

// ExportAll handles GET /api/v1/library/export: a zip of every item the
// user can see (their own and the organisation's shared ones), one file per
// item under skills/, context/ and tools/.
func (h *LibraryHandler) ExportAll(c *gin.Context) {
	me := c.GetString("user_id")
	ctx := c.Request.Context()
	rows, err := h.db.Query(ctx, `SELECT `+librarySelectCols+libraryFrom+` WHERE `+visibleSQL(1)+` ORDER BY li.kind, li.slug LIMIT $2`, me, libraryExportMaxItems)
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
	used := map[string]bool{}
	var failed error
	for rows.Next() {
		r, err := scanLibraryRow(rows)
		if err != nil {
			failed = err
			break
		}
		it := r.toWire(me)
		name, _, body := renderExport(it)
		dir := map[string]string{libraryKindSkill: "skills/", libraryKindContext: "context/", libraryKindTool: "tools/"}[it.Kind]
		// Two users' shared items may share a slug; every entry gets a
		// name nothing else in the archive has.
		ext := name[strings.LastIndex(name, "."):]
		stem := strings.TrimSuffix(name, ext)
		path := dir + name
		for n := 2; used[path]; n++ {
			path = fmt.Sprintf("%s%s-%d%s", dir, stem, n, ext)
		}
		used[path] = true
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Deflate, Modified: it.UpdatedAt})
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
		slog.ErrorContext(ctx, "library export interrupted", "user_id", me, "written", count, "err", failed)
	}
	_ = zw.Close()
	evt := activity.FromContext(c, activity.LibraryExported)
	evt.Detail = map[string]any{"count": count}
	h.activity.Record(ctx, evt)
}

// importRequest is the body of POST /api/v1/library/import: one file the
// browser read. kind says which tab the person imported from; a `kind:`
// front-matter key in the file overrides it.
type importRequest struct {
	Filename   string  `json:"filename"`
	Content    string  `json:"content"`
	Kind       string  `json:"kind"`
	Visibility *string `json:"visibility,omitempty"`
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
		case "name", "description", "kind":
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
			case "kind", "name", "description", "server_url", "allowed_tools":
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
	var req importRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be {filename, content, kind?, visibility?}"})
		return
	}
	in, pv, msg := parseImport(req)
	if msg != "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": msg, "dropped": pv.Dropped})
		return
	}
	if c.Query("dry_run") == "1" {
		c.JSON(http.StatusOK, pv)
		return
	}
	visibility := libraryVisibilityPrivate
	if in.Visibility != nil {
		visibility = *in.Visibility
	}
	id, _, err := h.createOne(ctx, me, in, visibility)
	switch {
	case errors.Is(err, errLibraryFull):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": fmt.Sprintf("your library holds the maximum of %d items", libraryItemsPerUser)})
		return
	case err != nil:
		slog.ErrorContext(ctx, "library import failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "import failed"})
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
	it := r.toWire(me)
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
	rows, err := h.db.Query(ctx, `SELECT `+librarySelectCols+libraryFrom+` WHERE li.visibility = 'org' ORDER BY li.updated_at DESC LIMIT 1000`)
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
		items = append(items, r.toWire(me))
	}
	c.JSON(http.StatusOK, items)
}

// AdminUnshare handles POST /api/v1/library/shared/:id/unshare (admin):
// makes an org item private again. It stays with its owner.
func (h *LibraryHandler) AdminUnshare(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid item id"})
		return
	}
	ctx := c.Request.Context()
	var kind string
	var owner *string
	err := h.db.QueryRow(ctx, `
		UPDATE library_items SET visibility = 'private', shared_at = NULL, updated_at = now()
		WHERE id = $1 AND visibility = 'org'
		RETURNING kind, owner_user_id::text
	`, id).Scan(&kind, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "shared item not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unshare failed"})
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
	var kind string
	var owner *string
	var hadCred bool
	err := h.db.QueryRow(ctx, `
		DELETE FROM library_items WHERE id = $1 AND visibility = 'org'
		RETURNING kind, owner_user_id::text, tool_credential_last4 IS NOT NULL
	`, id).Scan(&kind, &owner, &hadCred)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "shared item not found"})
		return
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
// (handler/users.go): the user's private items are removed with them, like
// their conversations, and their org-shared items pass to the acting
// administrator so a shared skill never silently vanishes or loses its
// editor. Returns the ids reassigned (for the audit event) and the ids of
// removed items that held a tool credential (for Secret cleanup after
// commit).
func reassignLibraryOnUserDelete(ctx context.Context, tx pgx.Tx, deletedUserID, adminID string) (reassigned, credentialsToRemove []string, err error) {
	rows, err := tx.Query(ctx, `
		DELETE FROM library_items WHERE owner_user_id = $1 AND visibility = 'private'
		RETURNING id::text, tool_credential_last4 IS NOT NULL
	`, deletedUserID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var hadCred bool
		if err := rows.Scan(&id, &hadCred); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if hadCred {
			credentialsToRemove = append(credentialsToRemove, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Shared items pass to the administrator. A slug the administrator
	// already uses gets the item's id appended so the (owner, slug) key
	// stays unique and nothing is lost over a name.
	rows, err = tx.Query(ctx, `
		UPDATE library_items li SET owner_user_id = $2, updated_at = now(), updated_by = $2,
			slug = CASE WHEN EXISTS (SELECT 1 FROM library_items o WHERE o.owner_user_id = $2 AND o.slug = li.slug AND o.id <> li.id)
				THEN left(li.slug, 91) || '-' || left(li.id::text, 8) ELSE li.slug END
		WHERE li.owner_user_id = $1 AND li.visibility = 'org'
		RETURNING li.id::text
	`, deletedUserID, adminID)
	if err != nil {
		return nil, credentialsToRemove, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, credentialsToRemove, err
		}
		reassigned = append(reassigned, id)
	}
	return reassigned, credentialsToRemove, rows.Err()
}
