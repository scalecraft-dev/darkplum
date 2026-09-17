package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
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
	}
}

// modelRow is the full DB representation returned by admin endpoints.
type modelRow struct {
	ID           string    `json:"id"`
	ModelID      string    `json:"model_id"`
	Provider     string    `json:"provider"`
	DisplayName  *string   `json:"display_name,omitempty"`
	Enabled      bool      `json:"enabled"`
	BackendClass string    `json:"backend_class"`
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
		SELECT model_id, provider, backend_class
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
		if err := modelRows.Scan(&m.ModelID, &m.Provider, &m.BackendClass); err != nil {
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
		SELECT a.alias, a.target_model_id, m.provider, m.backend_class
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
		if err := aliasRows.Scan(&a.Alias, &a.TargetModelID, &a.OwnedBy, &a.BackendClass); err != nil {
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

	data := buildModelsList(models, aliases, residency, time.Now().Unix())
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

	models := make([]modelRow, 0)
	for rows.Next() {
		var m modelRow
		if err := rows.Scan(&m.ID, &m.ModelID, &m.Provider, &m.DisplayName, &m.Enabled, &m.BackendClass, &m.DiscoveredAt, &m.UpdatedAt); err != nil {
			slog.ErrorContext(c.Request.Context(), "scan admin model row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
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

	c.JSON(http.StatusOK, m)
}

// gatewayModelsResponse is the shape we expect from GET {gatewayURL}/v1/models.
type gatewayModelsResponse struct {
	Object string             `json:"object"`
	Data   []openAIModelEntry `json:"data"`
}

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

	var inserted, updated int
	for _, m := range gwResp.Data {
		if m.ID == "" {
			continue
		}

		providerName := m.OwnedBy
		if providerName == "" {
			providerName = "vllm"
		}
		backendClass := normalizeBackendClass(m.BackendClass)

		// INSERT new rows with enabled = false (schema default). On conflict,
		// refresh discovered_at, backend_class and provider — never enabled or
		// display_name, so admin settings survive a resync.
		//
		// "xmax = 0" is true only for the row version created by this INSERT
		// (xmax is unset on a freshly inserted tuple); the ON CONFLICT UPDATE
		// path always sets a nonzero xmax. CommandTag can't distinguish
		// INSERT-from-conflict-update because both report "INSERT 0 1" under
		// ON CONFLICT DO UPDATE ... RETURNING, so RETURNING (xmax = 0) is the
		// reliable way to tell which branch actually ran.
		var wasInsert bool
		err := h.pool.QueryRow(ctx, `
			INSERT INTO models (model_id, provider, backend_class, discovered_at, updated_at)
			VALUES ($1, $2, $3, now(), now())
			ON CONFLICT (model_id) DO UPDATE
				SET discovered_at = now(),
					backend_class = EXCLUDED.backend_class,
					provider = EXCLUDED.provider
			RETURNING (xmax = 0)
		`, m.ID, providerName, backendClass).Scan(&wasInsert)
		if err != nil {
			slog.ErrorContext(ctx, "sync models: upsert failed", "model_id", m.ID, "err", err)
			continue
		}

		if wasInsert {
			inserted++
		} else {
			updated++
		}
	}

	// Remove models that were not re-discovered. Their discovered_at will be
	// older than the current sync run (which set discovered_at = now() on
	// every model it found). A 1-minute buffer avoids clock-skew races.
	res, err := h.pool.Exec(ctx, `
		DELETE FROM models WHERE discovered_at < now() - interval '1 minute'
	`)
	var removed int64
	if err != nil {
		slog.ErrorContext(ctx, "sync models: stale cleanup failed", "err", err)
	} else {
		removed = res.RowsAffected()
	}

	slog.Info("sync models: complete",
		"fetched", len(gwResp.Data),
		"inserted", inserted,
		"updated", updated,
		"removed", removed,
	)
}
