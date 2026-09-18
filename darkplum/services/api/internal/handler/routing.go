package handler

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

// aliasRe mirrors the CHECK constraint on model_aliases.alias in migration
// 010_model_routing.sql: lowercase alphanumeric, '.', '_', '-', starting with
// an alphanumeric character, 1-63 characters total.
var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// validAliasFormat is a pure predicate so alias validation can be unit tested
// without a database; the same shape is also enforced by the DB CHECK
// constraint as a second, authoritative line of defense.
func validAliasFormat(alias string) bool {
	return aliasRe.MatchString(alias)
}

// validResidency is a pure predicate mirroring routing_policy.residency's
// CHECK constraint.
func validResidency(v string) bool {
	return v == residencyAny || v == residencySelfHostedOnly
}

// RoutingHandler serves the model-alias and routing-policy admin endpoints.
type RoutingHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
	// availabilityWindow must match ModelHandler's so an alias's
	// target_available flips at the same moment as the model's available.
	availabilityWindow time.Duration
}

// NewRoutingHandler returns a RoutingHandler backed by db.
func NewRoutingHandler(db *pgxpool.Pool, rec *activity.Recorder) *RoutingHandler {
	return &RoutingHandler{db: db, activity: rec, availabilityWindow: modelAvailabilityWindow}
}

// SetAvailabilityWindow applies the operator-configured
// MODEL_AVAILABILITY_WINDOW_SECONDS; see ModelHandler.SetAvailabilityWindow.
func (h *RoutingHandler) SetAvailabilityWindow(w time.Duration) {
	h.availabilityWindow = w
}

// aliasResponse is the wire shape for one row in the model-aliases endpoints.
// TargetAvailable is false whenever TargetExists is false (a missing target
// is never available) or the target row exists but hasn't been re-discovered
// within modelAvailabilityWindow (see models_logic.go).
type aliasResponse struct {
	Alias              string    `json:"alias"`
	TargetModelID      string    `json:"target_model_id"`
	TargetExists       bool      `json:"target_exists"`
	TargetEnabled      bool      `json:"target_enabled"`
	TargetBackendClass *string   `json:"target_backend_class"`
	TargetAvailable    bool      `json:"target_available"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// ListAliases handles GET /api/v1/model-aliases.
func (h *RoutingHandler) ListAliases(c *gin.Context) {
	rows, err := h.db.Query(c.Request.Context(), `
		SELECT
			a.alias,
			a.target_model_id,
			(m.model_id IS NOT NULL) AS target_exists,
			COALESCE(m.enabled, false) AS target_enabled,
			m.backend_class,
			m.discovered_at,
			a.created_at,
			a.updated_at
		FROM model_aliases a
		LEFT JOIN models m ON m.model_id = a.target_model_id
		ORDER BY a.alias
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list aliases failed"})
		return
	}
	defer rows.Close()

	now := time.Now()
	aliases := make([]aliasResponse, 0)
	for rows.Next() {
		var a aliasResponse
		var targetDiscoveredAt *time.Time
		if err := rows.Scan(
			&a.Alias, &a.TargetModelID, &a.TargetExists, &a.TargetEnabled,
			&a.TargetBackendClass, &targetDiscoveredAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan alias failed"})
			return
		}
		a.TargetAvailable = targetDiscoveredAt != nil && modelIsAvailableWithin(*targetDiscoveredAt, now, h.availabilityWindow)
		aliases = append(aliases, a)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, aliases)
}

type putAliasRequest struct {
	TargetModelID string `json:"target_model_id"`
}

// PutAlias handles PUT /api/v1/model-aliases/:alias — create or retarget an
// alias. Validation order: alias format -> missing target -> alias/model-id
// conflict (409) -> target-is-itself-an-alias (400, no chains) -> target must
// exist in models (404).
//
// The whole handler runs in one transaction that opens by taking
// modelRoutingAdvisoryLockKey (see models_logic.go), the same lock
// ModelHandler.SyncModels takes around its upsert transaction. Without this,
// the model-id conflict check (below) and the alias upsert are separate
// statements a concurrent SyncModels run can interleave with: SyncModels
// could insert a model row for this exact id in the gap between the conflict
// check and the upsert, leaving both a model row and an alias row for the
// same id. The lock serialises the two transactions so one of them always
// completes (and the other observes its result) before proceeding.
func (h *RoutingHandler) PutAlias(c *gin.Context) {
	alias := c.Param("alias")
	if !validAliasFormat(alias) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alias format"})
		return
	}

	var req putAliasRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.TargetModelID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_model_id is required"})
		return
	}

	ctx := c.Request.Context()

	tx, err := h.db.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "begin transaction failed"})
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, modelRoutingAdvisoryLockKey); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "acquire routing lock failed"})
		return
	}

	// 409: the alias name would shadow a real model id. The gateway resolves
	// alias-vs-model collisions by letting the alias win (admin intent is
	// authoritative), but allowing that ambiguity to be created here would
	// just be confusing, so it's rejected outright.
	var modelIDConflict bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM models WHERE model_id = $1)`, alias,
	).Scan(&modelIDConflict); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check alias conflict failed"})
		return
	}
	if modelIDConflict {
		c.JSON(http.StatusConflict, gin.H{"error": "alias conflicts with an existing model id"})
		return
	}

	// 400: no alias chains — the target must not itself be an alias.
	var targetIsAlias bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM model_aliases WHERE alias = $1)`, req.TargetModelID,
	).Scan(&targetIsAlias); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check alias target failed"})
		return
	}
	if targetIsAlias {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target is itself an alias"})
		return
	}

	// 404: the target must be a currently known model.
	var targetEnabled bool
	var targetBackendClass string
	var targetDiscoveredAt time.Time
	err = tx.QueryRow(ctx,
		`SELECT enabled, backend_class, discovered_at FROM models WHERE model_id = $1`, req.TargetModelID,
	).Scan(&targetEnabled, &targetBackendClass, &targetDiscoveredAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "target model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check target model failed"})
		return
	}

	var row aliasResponse
	var created bool
	err = tx.QueryRow(ctx, `
		INSERT INTO model_aliases (alias, target_model_id, created_at, updated_at)
		VALUES ($1, $2, now(), now())
		ON CONFLICT (alias) DO UPDATE
			SET target_model_id = EXCLUDED.target_model_id, updated_at = now()
		RETURNING alias, target_model_id, created_at, updated_at, (xmax = 0)
	`, alias, req.TargetModelID).Scan(&row.Alias, &row.TargetModelID, &row.CreatedAt, &row.UpdatedAt, &created)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upsert alias failed"})
		return
	}
	row.TargetExists = true
	row.TargetEnabled = targetEnabled
	row.TargetBackendClass = &targetBackendClass
	row.TargetAvailable = modelIsAvailableWithin(targetDiscoveredAt, time.Now(), h.availabilityWindow)

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "commit alias failed"})
		return
	}

	evtType := activity.ModelAliasUpdated
	if created {
		evtType = activity.ModelAliasCreated
	}
	evt := activity.FromContext(c, evtType)
	evt.TargetType = activity.StrPtr("model_alias")
	// activity_events.target_id is a UUID column; a model alias is an
	// arbitrary TEXT identifier (e.g. "default"), not a UUID, so it cannot go
	// in TargetID (the insert would fail on a type mismatch and the write
	// would be silently dropped — Recorder.Record only logs errors). Carry it
	// in Detail instead, alongside the target model id.
	evt.Detail = map[string]any{"alias": alias, "target_model_id": req.TargetModelID}
	h.activity.Record(ctx, evt)

	c.JSON(http.StatusOK, row)
}

// DeleteAlias handles DELETE /api/v1/model-aliases/:alias.
func (h *RoutingHandler) DeleteAlias(c *gin.Context) {
	alias := c.Param("alias")

	tag, err := h.db.Exec(c.Request.Context(), `DELETE FROM model_aliases WHERE alias = $1`, alias)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete alias failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "alias not found"})
		return
	}

	evt := activity.FromContext(c, activity.ModelAliasDeleted)
	evt.TargetType = activity.StrPtr("model_alias")
	// See PutAlias: target_id is UUID-typed, alias is not — carry it in Detail.
	evt.Detail = map[string]any{"alias": alias}
	h.activity.Record(c.Request.Context(), evt)

	c.Status(http.StatusNoContent)
}

// routingPolicyResponse is the wire shape for GET/PUT /api/v1/routing-policy.
type routingPolicyResponse struct {
	Residency string    `json:"residency"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GetRoutingPolicy handles GET /api/v1/routing-policy.
func (h *RoutingHandler) GetRoutingPolicy(c *gin.Context) {
	var resp routingPolicyResponse
	err := h.db.QueryRow(c.Request.Context(),
		`SELECT residency, updated_at FROM routing_policy WHERE id = true`,
	).Scan(&resp.Residency, &resp.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get routing policy failed"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

type putRoutingPolicyRequest struct {
	Residency string `json:"residency"`
}

// PutRoutingPolicy handles PUT /api/v1/routing-policy.
func (h *RoutingHandler) PutRoutingPolicy(c *gin.Context) {
	var req putRoutingPolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validResidency(req.Residency) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "residency must be 'any' or 'self_hosted_only'"})
		return
	}

	var updatedBy *string
	if uid := c.GetString("user_id"); uid != "" {
		updatedBy = &uid
	}

	var resp routingPolicyResponse
	err := h.db.QueryRow(c.Request.Context(), `
		UPDATE routing_policy
		SET residency = $1, updated_at = now(), updated_by = $2
		WHERE id = true
		RETURNING residency, updated_at
	`, req.Residency, updatedBy).Scan(&resp.Residency, &resp.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update routing policy failed"})
		return
	}

	evt := activity.FromContext(c, activity.RoutingPolicyChanged)
	evt.TargetType = activity.StrPtr("routing_policy")
	evt.Detail = map[string]any{"residency": resp.Residency}
	h.activity.Record(c.Request.Context(), evt)

	c.JSON(http.StatusOK, resp)
}
