package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ModelHandler manages the models table and exposes both the chat-facing
// /v1/models endpoint (enabled models only) and the admin endpoints for listing
// and toggling models.
type ModelHandler struct {
	gatewayURL string
	serviceKey string
	pool       *pgxpool.Pool
	client     *http.Client

	// availabilityWindow defaults to modelAvailabilityWindow and can be
	// overridden via SetAvailabilityWindow, which cmd/main.go does from
	// MODEL_AVAILABILITY_WINDOW_SECONDS so an operator can tune it without a
	// rebuild.
	availabilityWindow time.Duration
}

// NewModelHandler constructs a ModelHandler. gatewayURL and serviceKey are used
// by SyncModels to call the gateway's /v1/models endpoint at startup and on the
// periodic sync ticker.
func NewModelHandler(gatewayURL, serviceKey string, pool *pgxpool.Pool) *ModelHandler {
	return &ModelHandler{
		gatewayURL: gatewayURL,
		serviceKey: serviceKey,
		pool:       pool,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		availabilityWindow: modelAvailabilityWindow,
	}
}

// SetAvailabilityWindow overrides the default modelAvailabilityWindow used by
// ListModels/ListAdminModels/ToggleModel. Exported so cmd/main.go (a
// different package) can apply an operator-configured
// MODEL_AVAILABILITY_WINDOW_SECONDS value after construction.
func (h *ModelHandler) SetAvailabilityWindow(w time.Duration) {
	h.availabilityWindow = w
}

// modelRow is the full DB representation returned by admin endpoints.
// Available is not a column — it's derived from DiscoveredAt at response
// time via modelIsAvailable (see models_logic.go), so admin UI and toggle
// responses always reflect the current cutoff rather than a stale snapshot.
type modelRow struct {
	ID           string    `json:"id"`
	ModelID      string    `json:"model_id"`
	Provider     string    `json:"provider"`
	DisplayName  *string   `json:"display_name,omitempty"`
	Enabled      bool      `json:"enabled"`
	BackendClass string    `json:"backend_class"`
	Available    bool      `json:"available"`
	DiscoveredAt time.Time `json:"discovered_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// openAIModelEntry is the wire shape for one model in the OpenAI-compatible
// /v1/models response. It doubles as the decode target for the gateway's own
// GET /v1/models response in SyncModels — the gateway sends the same fields
// (minus alias_of, which only the api's chat-facing endpoint produces).
type openAIModelEntry struct {
	ID           string  `json:"id"`
	Object       string  `json:"object"`
	Created      int64   `json:"created"`
	OwnedBy      string  `json:"owned_by"`
	BackendClass string  `json:"backend_class"`
	AliasOf      *string `json:"alias_of,omitempty"`
}

// openAIModelsResponse is the OpenAI-compatible envelope for the models list.
type openAIModelsResponse struct {
	Object string             `json:"object"`
	Data   []openAIModelEntry `json:"data"`
}

// ListModels handles GET /v1/models — chat-facing, returns enabled models plus
// aliases whose target is enabled, in OpenAI-compatible format. Under the
// self_hosted_only residency policy, cloud_managed models (and aliases
// pointing at them) are omitted entirely — this is a UX convenience for the
// picker, not the enforcement point (that's the gateway; see policy.Snapshot).
func (h *ModelHandler) ListModels(c *gin.Context) {
	ctx := c.Request.Context()

	residency, err := h.getResidency(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "list models: read routing policy failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}

	modelRows, err := h.pool.Query(ctx, `
		SELECT model_id, provider, backend_class, discovered_at
		FROM models
		WHERE enabled = true
		ORDER BY provider, model_id
	`)
	if err != nil {
		slog.ErrorContext(ctx, "list models query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	defer modelRows.Close()

	var models []modelDBRow
	for modelRows.Next() {
		var m modelDBRow
		if err := modelRows.Scan(&m.ModelID, &m.Provider, &m.BackendClass, &m.DiscoveredAt); err != nil {
			slog.ErrorContext(ctx, "scan model row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
		models = append(models, m)
	}
	if err := modelRows.Err(); err != nil {
		slog.ErrorContext(ctx, "models rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	aliasRows, err := h.pool.Query(ctx, `
		SELECT a.alias, a.target_model_id, m.provider, m.backend_class, m.discovered_at
		FROM model_aliases a
		JOIN models m ON m.model_id = a.target_model_id
		WHERE m.enabled = true
		ORDER BY a.alias
	`)
	if err != nil {
		slog.ErrorContext(ctx, "list model aliases query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	defer aliasRows.Close()

	var aliases []aliasDBRow
	for aliasRows.Next() {
		var a aliasDBRow
		if err := aliasRows.Scan(&a.Alias, &a.TargetModelID, &a.OwnedBy, &a.BackendClass, &a.TargetDiscoveredAt); err != nil {
			slog.ErrorContext(ctx, "scan model alias row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
		aliases = append(aliases, a)
	}
	if err := aliasRows.Err(); err != nil {
		slog.ErrorContext(ctx, "model aliases rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	data := buildModelsList(models, aliases, residency, time.Now(), h.availabilityWindow)
	c.JSON(http.StatusOK, openAIModelsResponse{Object: "list", Data: data})
}

// getResidency reads the routing_policy singleton's current residency value.
func (h *ModelHandler) getResidency(ctx context.Context) (string, error) {
	var residency string
	err := h.pool.QueryRow(ctx, `SELECT residency FROM routing_policy WHERE id = true`).Scan(&residency)
	return residency, err
}

// ListAdminModels handles GET /api/v1/models — admin view, returns all models
// with full metadata including the enabled flag.
func (h *ModelHandler) ListAdminModels(c *gin.Context) {
	rows, err := h.pool.Query(c.Request.Context(), `
		SELECT id, model_id, provider, display_name, enabled, backend_class, discovered_at, updated_at
		FROM models
		ORDER BY provider, model_id
	`)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "list admin models query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	defer rows.Close()

	now := time.Now()
	models := make([]modelRow, 0)
	for rows.Next() {
		var m modelRow
		if err := rows.Scan(&m.ID, &m.ModelID, &m.Provider, &m.DisplayName, &m.Enabled, &m.BackendClass, &m.DiscoveredAt, &m.UpdatedAt); err != nil {
			slog.ErrorContext(c.Request.Context(), "scan admin model row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
		m.Available = modelIsAvailableWithin(m.DiscoveredAt, now, h.availabilityWindow)
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(c.Request.Context(), "admin models rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, models)
}

// toggleModelRequest is the request body for PATCH /api/v1/models/:id.
type toggleModelRequest struct {
	Enabled bool `json:"enabled"`
}

// ToggleModel handles PATCH /api/v1/models/:id — admin only.
// Accepts {"enabled": true|false} and returns the updated model row.
func (h *ModelHandler) ToggleModel(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}

	var req toggleModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// UPDATE first — matching the pattern in users.go (Exec + RowsAffected).
	tag, err := h.pool.Exec(c.Request.Context(), `
		UPDATE models SET enabled = $1, updated_at = now() WHERE id = $2
	`, req.Enabled, id)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "toggle model update failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "toggle model failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}

	// Fetch and return the updated row so the frontend can refresh without a
	// separate GET.
	var m modelRow
	if err := h.pool.QueryRow(c.Request.Context(), `
		SELECT id, model_id, provider, display_name, enabled, backend_class, discovered_at, updated_at
		FROM models WHERE id = $1
	`, id).Scan(&m.ID, &m.ModelID, &m.Provider, &m.DisplayName, &m.Enabled, &m.BackendClass, &m.DiscoveredAt, &m.UpdatedAt); err != nil {
		slog.ErrorContext(c.Request.Context(), "fetch updated model failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fetch updated model failed"})
		return
	}
	m.Available = modelIsAvailableWithin(m.DiscoveredAt, time.Now(), h.availabilityWindow)

	c.JSON(http.StatusOK, m)
}

// gatewayModelsResponse is the shape we expect from GET {gatewayURL}/v1/models.
type gatewayModelsResponse struct {
	Object string             `json:"object"`
	Data   []openAIModelEntry `json:"data"`
}

// modelPurgeAfter documents the purge cutoff used in the DELETE below (kept
// as a literal `interval '7 days'` in the SQL itself, since pgx has no clean
// way to bind a Go time.Duration as a Postgres interval — keep the two in
// sync by hand if this changes). It is intentionally much longer than
// modelAvailabilityWindow: "available" flips off quickly (see
// models_logic.go) to hide a model that isn't currently routable, but the row
// itself — and with it the admin's enabled flag and any aliases pointing at
// it — must survive a slow backend restart (a vLLM cold start can take 15-25
// minutes) or a longer maintenance window without the admin having to
// re-toggle it or recreate aliases once it comes back.
const modelPurgeAfter = 7 * 24 * time.Hour

// SyncModels fetches the model list from the gateway and upserts each model
// into the models table. Re-discovered models do not have their enabled flag
// or display_name overwritten — only discovered_at, backend_class and
// provider are refreshed, so admin settings survive a resync while a model
// that changes backend (e.g. a Bedrock filter change) is reclassified. New
// models default to enabled = false per the schema.
//
// Called once at startup and then on a periodic ticker in main.
func (h *ModelHandler) SyncModels(ctx context.Context) {
	target := fmt.Sprintf("%s/v1/models", h.gatewayURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		slog.ErrorContext(ctx, "sync models: build request failed", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.serviceKey)

	resp, err := h.client.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "sync models: gateway request failed", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "sync models: gateway returned non-200", "status", resp.StatusCode)
		return
	}

	var gwResp gatewayModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&gwResp); err != nil {
		slog.ErrorContext(ctx, "sync models: decode gateway response failed", "err", err)
		return
	}

	// The upsert runs in a transaction serialised against PutAlias (routing.go)
	// via the same fixed advisory lock, so a concurrent alias PUT can't
	// interleave with a discovery upsert of a model sharing that id and leave
	// both a model row and an alias row for it (see PutAlias's comment for the
	// other half of this). The lock is per-transaction (pg_advisory_xact_lock)
	// and the transaction holds ONLY the upsert — the alias-shadow check and the
	// purge below run after commit, outside the lock, so an admin's PutAlias
	// waits for one statement, not for the whole sync.
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "sync models: begin transaction failed", "err", err)
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, modelRoutingAdvisoryLockKey); err != nil {
		slog.ErrorContext(ctx, "sync models: acquire routing lock failed", "err", err)
		return
	}

	// Normalize+de-dup once, up front, so both the batch path and its
	// per-row fallback see the same input (see normalizeGatewayModels).
	normalized := normalizeGatewayModels(gwResp.Data)

	// The common case (a healthy sync) is one set-based upsert instead of
	// three round trips per model inside a per-row savepoint — with a few
	// hundred Bedrock profiles, the old per-row loop held
	// modelRoutingAdvisoryLockKey long enough to stall admin PutAlias calls.
	// A batch statement failure (e.g. one row's data Postgres rejects, which
	// fails the whole INSERT) falls back to the old per-row savepoint path so
	// one bad row still can't take the rest of the sync down.
	inserted, updated, discoveredIDs, err := h.syncModelsBatch(ctx, tx, normalized)
	if err != nil {
		slog.WarnContext(ctx, "sync models: batch upsert failed, falling back to per-row upserts",
			"model_count", len(normalized), "err", err)
		var fatalErr error
		inserted, updated, discoveredIDs, fatalErr = h.syncModelsPerRow(ctx, tx, normalized)
		if fatalErr != nil {
			slog.ErrorContext(ctx, "sync models: per-row fallback failed", "err", fatalErr)
			return
		}
	} else if len(normalized) > 0 {
		slog.InfoContext(ctx, "sync models: batch upsert succeeded",
			"model_count", len(normalized), "inserted", inserted, "updated", updated)
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "sync models: commit failed", "err", err)
		return
	}

	// A discovered model id that equals an existing alias is not an error —
	// the gateway's route table lets the alias win by design — but the admin
	// needs to know, since it means that alias no longer reaches the model it
	// looks like it targets. Log one WARNING per sync naming every alias
	// shadowed this run, rather than failing the sync or silently proceeding.
	if len(discoveredIDs) > 0 {
		rows, err := h.pool.Query(ctx, `SELECT alias FROM model_aliases WHERE alias = ANY($1)`, discoveredIDs)
		if err != nil {
			slog.ErrorContext(ctx, "sync models: check alias shadowing failed", "err", err)
		} else {
			var shadowed []string
			for rows.Next() {
				var alias string
				if err := rows.Scan(&alias); err != nil {
					slog.ErrorContext(ctx, "sync models: scan shadowed alias failed", "err", err)
					continue
				}
				shadowed = append(shadowed, alias)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				slog.ErrorContext(ctx, "sync models: shadowed alias rows error", "err", err)
			}
			if len(shadowed) > 0 {
				slog.Warn("sync models: discovered model id(s) shadow existing alias(es) of the same name — the alias wins at the gateway by design",
					"aliases", shadowed)
			}
		}
	}

	// Purge rows unseen for a long time — not "not re-discovered this tick",
	// which would delete a model whose backend is mid-restart (see
	// modelPurgeAfter). Aliases pointing at a purged model are left alone;
	// they already tolerate a missing target (target_exists/target_available
	// go false, but the alias row survives so it doesn't need recreating).
	res, err := h.pool.Exec(ctx, `
		DELETE FROM models WHERE discovered_at < now() - interval '7 days'
	`)
	var purged int64
	if err != nil {
		slog.ErrorContext(ctx, "sync models: purge stale rows failed", "err", err)
	} else {
		purged = res.RowsAffected()
	}

	slog.Info("sync models: complete",
		"fetched", len(gwResp.Data),
		"inserted", inserted,
		"updated", updated,
		"purged", purged,
	)
}

// syncModelsBatch upserts every discovered model in one set-based statement,
// run inside its own savepoint of tx so a statement-level failure (e.g. one
// row's data Postgres rejects) can be rolled back without aborting tx itself
// — the caller falls back to syncModelsPerRow on any returned error.
//
// "xmax = 0" is true only for the row version created by the INSERT branch
// (xmax is unset on a freshly inserted tuple); the ON CONFLICT UPDATE path
// always sets a nonzero xmax. CommandTag can't distinguish
// INSERT-from-conflict-update because both report the same tag under
// ON CONFLICT DO UPDATE ... RETURNING, so RETURNING (xmax = 0) is the
// reliable way to tell which branch ran for each row.
func (h *ModelHandler) syncModelsBatch(ctx context.Context, tx pgx.Tx, models []normalizedModel) (inserted, updated int, discoveredIDs []string, err error) {
	if len(models) == 0 {
		return 0, 0, nil, nil
	}

	ids := make([]string, len(models))
	providers := make([]string, len(models))
	backendClasses := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
		providers[i] = m.Provider
		backendClasses[i] = m.BackendClass
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("open batch savepoint: %w", err)
	}
	defer sp.Rollback(ctx) // no-op once committed

	rows, err := sp.Query(ctx, `
		INSERT INTO models (model_id, provider, backend_class, discovered_at, updated_at)
		SELECT id, provider, backend_class, now(), now()
		FROM unnest($1::text[], $2::text[], $3::text[]) AS t(id, provider, backend_class)
		ON CONFLICT (model_id) DO UPDATE
			SET discovered_at = now(),
				backend_class = EXCLUDED.backend_class,
				provider = EXCLUDED.provider
		RETURNING model_id, (xmax = 0)
	`, ids, providers, backendClasses)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("batch upsert: %w", err)
	}

	discoveredIDs = make([]string, 0, len(models))
	for rows.Next() {
		var id string
		var wasInsert bool
		if scanErr := rows.Scan(&id, &wasInsert); scanErr != nil {
			rows.Close()
			return 0, 0, nil, fmt.Errorf("scan batch upsert row: %w", scanErr)
		}
		discoveredIDs = append(discoveredIDs, id)
		if wasInsert {
			inserted++
		} else {
			updated++
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return 0, 0, nil, fmt.Errorf("batch upsert rows: %w", rowsErr)
	}
	rows.Close()

	if err := sp.Commit(ctx); err != nil {
		return 0, 0, nil, fmt.Errorf("commit batch savepoint: %w", err)
	}

	return inserted, updated, discoveredIDs, nil
}

// syncModelsPerRow is the fallback for syncModelsBatch: each model is
// upserted in its own savepoint, so one row Postgres rejects doesn't abort
// the rest of the sync (and the purge that follows). This is the original
// (pre-batch) upsert strategy, kept as-is for the case a discovered set
// can't go through in one statement.
//
// A savepoint-open or savepoint-commit failure (as opposed to a rejected
// upsert statement) means something is wrong with the connection/transaction
// itself, not a single row's data — that is returned as a fatal error, which
// the caller must treat as reason to abort the whole sync without
// committing, exactly as this code did before being pulled out of SyncModels.
func (h *ModelHandler) syncModelsPerRow(ctx context.Context, tx pgx.Tx, models []normalizedModel) (inserted, updated int, discoveredIDs []string, fatalErr error) {
	discoveredIDs = make([]string, 0, len(models))
	for _, m := range models {
		var wasInsert bool
		sp, err := tx.Begin(ctx)
		if err != nil {
			return inserted, updated, discoveredIDs, fmt.Errorf("savepoint failed for model_id %s: %w", m.ID, err)
		}
		err = sp.QueryRow(ctx, `
			INSERT INTO models (model_id, provider, backend_class, discovered_at, updated_at)
			VALUES ($1, $2, $3, now(), now())
			ON CONFLICT (model_id) DO UPDATE
				SET discovered_at = now(),
					backend_class = EXCLUDED.backend_class,
					provider = EXCLUDED.provider
			RETURNING (xmax = 0)
		`, m.ID, m.Provider, m.BackendClass).Scan(&wasInsert)
		if err != nil {
			slog.ErrorContext(ctx, "sync models: upsert failed", "model_id", m.ID, "err", err)
			_ = sp.Rollback(ctx)
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return inserted, updated, discoveredIDs, fmt.Errorf("release savepoint failed for model_id %s: %w", m.ID, err)
		}
		// Only ids the database accepted take part in the alias-shadow check.
		discoveredIDs = append(discoveredIDs, m.ID)

		if wasInsert {
			inserted++
		} else {
			updated++
		}
	}
	return inserted, updated, discoveredIDs, nil
}
