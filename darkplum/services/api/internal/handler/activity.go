package handler

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/pricing"
)

// eventCostCents prices one event against an in-memory price snapshot.
// Refused requests cost "0.0000"; no matching price yields nil (unpriced).
func eventCostCents(prices []pricing.Price, provider, model string, region *string, at time.Time,
	refused bool, prompt, completion, cacheRead, cacheCreate *int) *string {
	if refused {
		z := pricing.FormatCents(new(big.Rat))
		return &z
	}
	p := pricing.Match(prices, provider, model, region, at)
	if p == nil {
		return nil
	}
	v := func(n *int) int64 {
		if n == nil {
			return 0
		}
		return int64(*n)
	}
	s := pricing.FormatCents(p.CostCents(pricing.Usage{
		Uncached:    pricing.UncachedPrompt(v(prompt), v(cacheRead), v(cacheCreate)),
		CacheRead:   v(cacheRead),
		CacheCreate: v(cacheCreate),
		Completion:  v(completion),
	}))
	return &s
}

// ActivityHandler serves read-only endpoints for activity_events and inference_events.
type ActivityHandler struct {
	db *pgxpool.Pool
}

// NewActivityHandler returns an ActivityHandler backed by db.
func NewActivityHandler(db *pgxpool.Pool) *ActivityHandler {
	return &ActivityHandler{db: db}
}

// activityEvent is the JSON shape returned by GET /api/v1/activity.
type activityEvent struct {
	ID         string         `json:"id"`
	EventType  string         `json:"event_type"`
	ActorID    *string        `json:"actor_id,omitempty"`
	ActorEmail *string        `json:"actor_email,omitempty"`
	ActorName  *string        `json:"actor_name,omitempty"`
	TargetType *string        `json:"target_type,omitempty"`
	TargetID   *string        `json:"target_id,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	IPAddress  *string        `json:"ip_address,omitempty"`
	RequestID  *string        `json:"request_id,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// ListActivity handles GET /api/v1/activity.
// Query params: actor_id, event_type, since, until, limit (default 50, max 200), offset.
func (h *ActivityHandler) ListActivity(c *gin.Context) {
	limit, offset, err := parsePagination(c, 50, 200)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	args := []any{}
	where := "WHERE 1=1"
	argN := 1

	if v := c.Query("actor_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid actor_id"})
			return
		}
		where += fmt.Sprintf(" AND ae.actor_id = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("event_type"); v != "" {
		where += fmt.Sprintf(" AND ae.event_type = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ae.created_at >= $%d", argN)
		args = append(args, t)
		argN++
	}

	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ae.created_at <= $%d", argN)
		args = append(args, t)
		argN++
	}

	// Count first (before appending limit/offset to args).
	var total int
	if err := h.db.QueryRow(c.Request.Context(),
		fmt.Sprintf("SELECT COUNT(*) FROM activity_events ae %s", where), args...,
	).Scan(&total); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "count activity failed"})
		return
	}

	args = append(args, limit, offset)
	q := fmt.Sprintf(`
		SELECT
			ae.id,
			ae.event_type,
			ae.actor_id::text,
			u.email,
			u.name,
			ae.target_type,
			ae.target_id::text,
			ae.detail,
			ae.ip_address::text,
			ae.request_id,
			ae.created_at
		FROM activity_events ae
		LEFT JOIN users u ON u.id = ae.actor_id
		%s
		ORDER BY ae.created_at DESC
		LIMIT $%d OFFSET $%d
	`, where, argN, argN+1)

	rows, err := h.db.Query(c.Request.Context(), q, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list activity failed"})
		return
	}
	defer rows.Close()

	events := make([]activityEvent, 0)
	for rows.Next() {
		var ae activityEvent
		var detailJSON []byte
		if err := rows.Scan(
			&ae.ID, &ae.EventType, &ae.ActorID, &ae.ActorEmail, &ae.ActorName,
			&ae.TargetType, &ae.TargetID, &detailJSON, &ae.IPAddress, &ae.RequestID,
			&ae.CreatedAt,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan activity failed"})
			return
		}
		if detailJSON != nil {
			if err := unmarshalJSONMap(detailJSON, &ae.Detail); err != nil {
				ae.Detail = nil
			}
		}
		events = append(events, ae)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"events": events, "total": total})
}

// inferenceEvent is the JSON shape returned by GET /api/v1/usage.
type inferenceEvent struct {
	ID        string  `json:"id"`
	RequestID string  `json:"request_id"`
	APIKeyID  *string `json:"api_key_id,omitempty"`
	UserID    *string `json:"user_id,omitempty"`
	UserName  *string `json:"user_name,omitempty"`
	UserEmail *string `json:"user_email,omitempty"`
	Model     string  `json:"model"`
	// RequestedModel is what the caller asked for when it differs from Model
	// (an alias, or a request refused before any model resolved).
	RequestedModel *string `json:"requested_model,omitempty"`
	// Provider is the resolved model's backend kind (vllm|lmstudio|bedrock|
	// anthropic|openai|mock; migration 007_inference_events_provider.sql),
	// alongside BackendClass below -- BackendClass says WHERE a request ran
	// relative to the account boundary (self_hosted|cloud_managed|
	// external_api), Provider says WHICH concrete backend/vendor served it,
	// so the audit trail can name the vendor (e.g. "anthropic") and not just
	// its class. NOT NULL, defaulted to 'vllm' at the column level.
	Provider     string  `json:"provider"`
	Purpose      string  `json:"purpose"`
	RouteName    *string `json:"route_name,omitempty"`
	RouterReason *string `json:"router_reason,omitempty"`
	StatusCode   int     `json:"status_code"`
	// DenialCode is why the gateway refused this request, when it did:
	// model_not_found, model_not_enabled, model_residency_violation,
	// model_not_allowed, invalid_request, policy_unavailable, request_canceled,
	// model_requires_data_retention.
	// Null on a request that reached a model. This is the admin's answer to "did my
	// policy fire, and on whom".
	DenialCode       *string `json:"denial_code"`
	LatencyMS        int     `json:"latency_ms"`
	PromptTokens     *int    `json:"prompt_tokens"`
	CompletionTokens *int    `json:"completion_tokens"`
	// CacheReadTokens / CacheCreationTokens: NULL = backend reported no usage;
	// 0 = usage reported without cache fields.
	CacheReadTokens     *int `json:"cache_read_tokens"`
	CacheCreationTokens *int `json:"cache_creation_tokens"`
	// CostCents is the list-price estimate (4 dp decimal string), computed at
	// query time from model_prices. null = unpriced; "0.0000" for a refusal.
	CostCents *string   `json:"cost_cents"`
	Stream    bool      `json:"stream"`
	IPAddress *string   `json:"ip_address,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// BackendClass is self_hosted, cloud_managed, or external_api -- set on
	// the gin context (and hence this row) the moment the gateway resolves a
	// route's class, so even a DENIED request carries it when the denial
	// happened after class resolution. Null ("unrecorded") on a request
	// refused before any route resolved (e.g. model_not_found) or on a row
	// written before migration 016 added the column.
	BackendClass *string `json:"backend_class,omitempty"`
}

// validUsagePurposeFilter mirrors inference_events.purpose's CHECK constraint
// (migration 013_routed_aliases.sql) plus the "all" sentinel accepted only by
// the ?purpose= query param (never stored) to mean "don't filter by purpose
// at all". Every other admin usage aggregation in this file has no override
// and always behaves as if purpose were "inference" -- that default is what
// keeps a classifier's own model call (purpose='routing') or an admin's
// "test a prompt" dry run (purpose='routing_dryrun') out of request counts,
// token sums, and latency percentiles meant to describe end-user traffic.
func validUsagePurposeFilter(v string) bool {
	switch v {
	case "inference", "routing", "routing_dryrun", "all":
		return true
	default:
		return false
	}
}

// validDenialCodeFilter accepts the ?denial_code= query param: one of the
// gateway's refusal codes (see gateway/internal/handler/chat.go's
// respond* helpers and inference_events.denial_code), or the sentinel "any"
// meaning every refused request regardless of reason. Empty = no filter.
func validDenialCodeFilter(v string) bool {
	switch v {
	case "", "any", "model_not_found", "model_not_enabled", "model_residency_violation",
		"model_not_allowed", "invalid_request", "policy_unavailable", "request_canceled",
		"model_requires_data_retention":
		return true
	default:
		return false
	}
}

// appendDenialCodeFilter adds the ?denial_code= clause to a usage WHERE.
func appendDenialCodeFilter(c *gin.Context, where string, args []any, argN int) (string, []any, int, error) {
	v := c.Query("denial_code")
	if !validDenialCodeFilter(v) {
		return where, args, argN, fmt.Errorf("denial_code must be one of any, model_not_found, model_not_enabled, model_residency_violation, model_not_allowed, invalid_request, policy_unavailable, request_canceled, model_requires_data_retention")
	}
	switch v {
	case "":
	case "any":
		where += " AND ie.denial_code IS NOT NULL"
	default:
		where += fmt.Sprintf(" AND ie.denial_code = $%d", argN)
		args = append(args, v)
		argN++
	}
	return where, args, argN, nil
}

// backendClassFilterUnrecorded is the sentinel token for the ?backend_class=
// query param that means "backend_class IS NULL" -- a request refused before
// any route resolved a class -- since NULL is not itself a value that can
// appear in a comma-separated list of literal strings.
const backendClassFilterUnrecorded = "unrecorded"

// validBackendClassFilterToken accepts one token of the comma-separated
// ?backend_class= query param: a known backend_class, or the
// backendClassFilterUnrecorded sentinel.
func validBackendClassFilterToken(v string) bool {
	switch v {
	case backendClassSelfHosted, backendClassCloudManaged, backendClassExternalAPI, backendClassFilterUnrecorded:
		return true
	default:
		return false
	}
}

// appendBackendClassFilter adds the ?backend_class= clause to a usage WHERE:
// a comma-separated list of self_hosted, cloud_managed, external_api,
// unrecorded. Empty = no filter. "unrecorded" and concrete classes combine
// via OR so `?backend_class=external_api,unrecorded` (say) matches either.
func appendBackendClassFilter(c *gin.Context, where string, args []any, argN int) (string, []any, int, error) {
	v := c.Query("backend_class")
	if v == "" {
		return where, args, argN, nil
	}

	var classes []string
	includeUnrecorded := false
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if !validBackendClassFilterToken(tok) {
			return where, args, argN, fmt.Errorf("backend_class must be a comma-separated list of self_hosted, cloud_managed, external_api, unrecorded")
		}
		if tok == backendClassFilterUnrecorded {
			includeUnrecorded = true
			continue
		}
		classes = append(classes, tok)
	}

	var clauses []string
	if len(classes) > 0 {
		clauses = append(clauses, fmt.Sprintf("ie.backend_class = ANY($%d)", argN))
		args = append(args, classes)
		argN++
	}
	if includeUnrecorded {
		clauses = append(clauses, "ie.backend_class IS NULL")
	}
	if len(clauses) > 0 {
		where += " AND (" + strings.Join(clauses, " OR ") + ")"
	}
	return where, args, argN, nil
}

// usageListResponse wraps inference events with a total count for pagination.
type usageListResponse struct {
	Total  int              `json:"total"`
	Events []inferenceEvent `json:"events"`
}

// ListUsage handles GET /api/v1/usage.
// Query params: user_id, api_key_id, model, purpose, denial_code, since, until,
// limit (default 50, max 200), offset.
func (h *ActivityHandler) ListUsage(c *gin.Context) {
	limit, offset, err := parsePagination(c, 50, 200)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	args := []any{}
	where := "WHERE 1=1"
	argN := 1

	if v := c.Query("user_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		where += fmt.Sprintf(" AND ie.user_id = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("api_key_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api_key_id"})
			return
		}
		where += fmt.Sprintf(" AND ie.api_key_id = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("model"); v != "" {
		where += fmt.Sprintf(" AND ie.model = $%d", argN)
		args = append(args, v)
		argN++
	}

	// purpose defaults to "inference" so classifier calls (purpose='routing')
	// and admin dry runs (purpose='routing_dryrun') don't show up in the
	// usage list unless explicitly asked for; "all" removes the filter.
	purpose := c.Query("purpose")
	if purpose == "" {
		purpose = "inference"
	}
	if !validUsagePurposeFilter(purpose) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purpose must be one of inference, routing, routing_dryrun, all"})
		return
	}
	if purpose != "all" {
		where += fmt.Sprintf(" AND ie.purpose = $%d", argN)
		args = append(args, purpose)
		argN++
	}

	var filterErr error
	if where, args, argN, filterErr = appendDenialCodeFilter(c, where, args, argN); filterErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": filterErr.Error()})
		return
	}
	if where, args, argN, filterErr = appendBackendClassFilter(c, where, args, argN); filterErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": filterErr.Error()})
		return
	}

	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ie.created_at >= $%d", argN)
		args = append(args, t)
		argN++
	}

	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ie.created_at <= $%d", argN)
		args = append(args, t)
		argN++
	}

	// Count first.
	var total int
	if err := h.db.QueryRow(c.Request.Context(),
		fmt.Sprintf("SELECT COUNT(*) FROM inference_events ie %s", where), args...,
	).Scan(&total); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "count usage failed"})
		return
	}

	args = append(args, limit, offset)
	q := fmt.Sprintf(`
		SELECT
			ie.id::text, ie.request_id, ie.api_key_id::text, ie.user_id::text,
			u.name, u.email,
			ie.model, ie.requested_model, ie.provider, ie.purpose, ie.route_name, ie.router_reason,
			ie.status_code, ie.denial_code, ie.latency_ms, ie.prompt_tokens, ie.completion_tokens,
			ie.stream, ie.ip_address::text, ie.created_at, ie.backend_class,
			ie.cache_read_tokens, ie.cache_creation_tokens, ie.region
		FROM inference_events ie
		LEFT JOIN users u ON u.id = ie.user_id
		%s
		ORDER BY ie.created_at DESC
		LIMIT $%d OFFSET $%d
	`, where, argN, argN+1)

	rows, err := h.db.Query(c.Request.Context(), q, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list usage failed"})
		return
	}
	defer rows.Close()

	prices, err := pricing.LoadAll(c.Request.Context(), h.db)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load model prices failed"})
		return
	}
	events := make([]inferenceEvent, 0)
	for rows.Next() {
		var ie inferenceEvent
		var region *string
		if err := rows.Scan(
			&ie.ID, &ie.RequestID, &ie.APIKeyID, &ie.UserID, &ie.UserName, &ie.UserEmail,
			&ie.Model, &ie.RequestedModel, &ie.Provider, &ie.Purpose, &ie.RouteName, &ie.RouterReason,
			&ie.StatusCode, &ie.DenialCode, &ie.LatencyMS, &ie.PromptTokens, &ie.CompletionTokens,
			&ie.Stream, &ie.IPAddress, &ie.CreatedAt, &ie.BackendClass,
			&ie.CacheReadTokens, &ie.CacheCreationTokens, &region,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan usage failed"})
			return
		}
		ie.CostCents = eventCostCents(prices, ie.Provider, ie.Model, region, ie.CreatedAt,
			ie.DenialCode != nil, ie.PromptTokens, ie.CompletionTokens, ie.CacheReadTokens, ie.CacheCreationTokens)
		events = append(events, ie)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, usageListResponse{Total: total, Events: events})
}

// usageStatsBucket is one time bucket in GET /api/v1/usage/stats.
type usageStatsBucket struct {
	BucketTime        time.Time `json:"bucket_time"`
	RequestCount      int       `json:"request_count"`
	SuccessCount      int       `json:"success_count"`
	ErrorCount        int       `json:"error_count"`
	TotalPromptTokens int       `json:"total_prompt_tokens"`
	TotalCompTokens   int       `json:"total_completion_tokens"`
	AvgLatencyMS      float64   `json:"avg_latency_ms"`
	P95LatencyMS      float64   `json:"p95_latency_ms"`
}

// UsageStats handles GET /api/v1/usage/stats.
// Required query params: since, until, interval (hour|day).
// Optional: user_id, api_key_id.
func (h *ActivityHandler) UsageStats(c *gin.Context) {
	since, until, err := requireTimeRange(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	interval := c.Query("interval")
	if interval != "hour" && interval != "day" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "interval must be 'hour' or 'day'"})
		return
	}
	truncExpr := fmt.Sprintf("date_trunc('%s', created_at)", interval)

	args := []any{since, until}
	// purpose = 'inference' is not admin-overridable here (unlike ListUsage's
	// ?purpose=): a classifier's own model call or an admin dry run has no
	// business skewing the request-count/token/latency series a chart labelled
	// "usage" implies is end-user traffic.
	where := "WHERE created_at >= $1 AND created_at <= $2 AND purpose = 'inference'"
	argN := 3

	if v := c.Query("user_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		where += fmt.Sprintf(" AND user_id = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("api_key_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api_key_id"})
			return
		}
		where += fmt.Sprintf(" AND api_key_id = $%d", argN)
		args = append(args, v)
		// argN++ — suppress unused increment warning; add if more params follow
		_ = argN
	}

	q := fmt.Sprintf(`
		SELECT
			%s                                                         AS bucket_time,
			COUNT(*)                                                   AS request_count,
			COUNT(*) FILTER (WHERE status_code < 400)                 AS success_count,
			COUNT(*) FILTER (WHERE status_code >= 400)                AS error_count,
			COALESCE(SUM(prompt_tokens), 0)                           AS total_prompt_tokens,
			COALESCE(SUM(completion_tokens), 0)                       AS total_completion_tokens,
			ROUND(AVG(latency_ms)::numeric, 2)::float8                AS avg_latency_ms,
			ROUND(PERCENTILE_CONT(0.95) WITHIN GROUP
				(ORDER BY latency_ms)::numeric, 2)::float8            AS p95_latency_ms
		FROM inference_events
		%s
		GROUP BY bucket_time
		ORDER BY bucket_time
	`, truncExpr, where)

	rows, err := h.db.Query(c.Request.Context(), q, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "usage stats query failed"})
		return
	}
	defer rows.Close()

	buckets := make([]usageStatsBucket, 0)
	for rows.Next() {
		var b usageStatsBucket
		if err := rows.Scan(
			&b.BucketTime, &b.RequestCount, &b.SuccessCount, &b.ErrorCount,
			&b.TotalPromptTokens, &b.TotalCompTokens, &b.AvgLatencyMS, &b.P95LatencyMS,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan stats failed"})
			return
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, buckets)
}

// usageSummary is the response from GET /api/v1/usage/summary.
type usageSummary struct {
	TotalRequests         int     `json:"total_requests"`
	TotalRequestsPrevious int     `json:"total_requests_previous"`
	ActiveUsers           int     `json:"active_users"`
	TotalUsers            int     `json:"total_users"`
	ActiveAPIKeys         int     `json:"active_api_keys"`
	TotalAPIKeys          int     `json:"total_api_keys"`
	AvgLatencyMS          float64 `json:"avg_latency_ms"`
	P95LatencyMS          float64 `json:"p95_latency_ms"`
}

// UsageSummary handles GET /api/v1/usage/summary.
// Required query params: since, until.
func (h *ActivityHandler) UsageSummary(c *gin.Context) {
	since, until, err := requireTimeRange(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Period length for the "previous" window comparison.
	periodLen := until.Sub(since)
	prevUntil := since
	prevSince := since.Add(-periodLen)

	var summary usageSummary

	// Current-period inference stats.
	err = h.db.QueryRow(c.Request.Context(), `
		SELECT
			COUNT(*)                                                        AS total_requests,
			COUNT(DISTINCT user_id)                                         AS active_users,
			COUNT(DISTINCT api_key_id)                                      AS active_api_keys,
			COALESCE(ROUND(AVG(latency_ms)::numeric, 2)::float8, 0)        AS avg_latency_ms,
			COALESCE(ROUND(
				PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY latency_ms)::numeric
			, 2)::float8, 0)                                               AS p95_latency_ms
		FROM inference_events
		WHERE created_at >= $1 AND created_at <= $2 AND purpose = 'inference'
	`, since, until).Scan(
		&summary.TotalRequests, &summary.ActiveUsers, &summary.ActiveAPIKeys,
		&summary.AvgLatencyMS, &summary.P95LatencyMS,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "summary query failed"})
		return
	}

	// Previous-period request count for comparison.
	if err := h.db.QueryRow(c.Request.Context(),
		`SELECT COUNT(*) FROM inference_events WHERE created_at >= $1 AND created_at < $2 AND purpose = 'inference'`,
		prevSince, prevUntil,
	).Scan(&summary.TotalRequestsPrevious); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "previous period query failed"})
		return
	}

	// User / key totals — from the users/api_keys tables, not inference_events.
	if err := h.db.QueryRow(c.Request.Context(),
		`SELECT COUNT(*) FROM users`,
	).Scan(&summary.TotalUsers); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "total users query failed"})
		return
	}

	if err := h.db.QueryRow(c.Request.Context(),
		`SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL`,
	).Scan(&summary.TotalAPIKeys); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "total api keys query failed"})
		return
	}

	c.JSON(http.StatusOK, summary)
}

// ExportActivity handles GET /api/v1/activity/export.
// Returns all matching activity_events as a CSV file (capped at 10 000 rows).
// Accepts the same filter params as ListActivity except limit/offset.
// csvSafeText neutralises a free-text cell that a spreadsheet would
// evaluate as a formula. encoding/csv quotes delimiters correctly, but
// Excel, Sheets and LibreOffice still execute a quoted cell that begins
// with = + - @ or a control character; a user who names themselves
// "=HYPERLINK(...)" would otherwise plant it in every admin's export. The
// OWASP apostrophe prefix is used, and ONLY on cells whose text a user
// controls (names, emails, model ids, route names) -- never on timestamps,
// ids, counts or costs, which must keep parsing as what they are.
func csvSafeText(cell string) string {
	if cell == "" {
		return cell
	}
	switch cell[0] {
	case '=', '+', '-', '@', '\t', '\r', '|':
		return "'" + cell
	}
	return cell
}

func (h *ActivityHandler) ExportActivity(c *gin.Context) {
	args := []any{}
	where := "WHERE 1=1"
	argN := 1

	if v := c.Query("actor_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid actor_id"})
			return
		}
		where += fmt.Sprintf(" AND ae.actor_id = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("event_type"); v != "" {
		where += fmt.Sprintf(" AND ae.event_type = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ae.created_at >= $%d", argN)
		args = append(args, t)
		argN++
	}

	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ae.created_at <= $%d", argN)
		args = append(args, t)
		argN++
	}

	args = append(args, 10000)
	q := fmt.Sprintf(`
		SELECT
			ae.created_at,
			ae.event_type,
			u.name,
			u.email,
			ae.target_type,
			ae.target_id::text,
			ae.detail,
			ae.ip_address::text
		FROM activity_events ae
		LEFT JOIN users u ON u.id = ae.actor_id
		%s
		ORDER BY ae.created_at DESC
		LIMIT $%d
	`, where, argN)

	rows, err := h.db.Query(c.Request.Context(), q, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export activity query failed"})
		return
	}
	defer rows.Close()

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", `attachment; filename="activity_events.csv"`)

	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"timestamp", "event_type", "actor_name", "actor_email", "target_type", "target_id", "detail", "ip_address"})

	for rows.Next() {
		var (
			createdAt  time.Time
			eventType  string
			actorName  *string
			actorEmail *string
			targetType *string
			targetID   *string
			detailJSON []byte
			ipAddress  *string
		)
		if err := rows.Scan(&createdAt, &eventType, &actorName, &actorEmail, &targetType, &targetID, &detailJSON, &ipAddress); err != nil {
			// Headers already sent; best we can do is flush and stop.
			w.Flush()
			return
		}
		_ = w.Write([]string{
			createdAt.UTC().Format(time.RFC3339),
			eventType,
			csvSafeText(derefStr(actorName)),
			csvSafeText(derefStr(actorEmail)),
			derefStr(targetType),
			derefStr(targetID),
			string(detailJSON),
			derefStr(ipAddress),
		})
	}
	w.Flush()
}

// ExportUsage handles GET /api/v1/usage/export.
// Returns all matching inference_events as a CSV file (capped at 10 000 rows).
// Accepts the same filter params as ListUsage except limit/offset.
func (h *ActivityHandler) ExportUsage(c *gin.Context) {
	args := []any{}
	where := "WHERE 1=1"
	argN := 1

	if v := c.Query("user_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
			return
		}
		where += fmt.Sprintf(" AND ie.user_id = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("api_key_id"); v != "" {
		if !isValidUUID(v) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api_key_id"})
			return
		}
		where += fmt.Sprintf(" AND ie.api_key_id = $%d", argN)
		args = append(args, v)
		argN++
	}

	if v := c.Query("model"); v != "" {
		where += fmt.Sprintf(" AND ie.model = $%d", argN)
		args = append(args, v)
		argN++
	}

	// See ListUsage: same default-to-"inference", "all"-removes-the-filter
	// contract, so an export matches what the admin was just looking at.
	purpose := c.Query("purpose")
	if purpose == "" {
		purpose = "inference"
	}
	if !validUsagePurposeFilter(purpose) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "purpose must be one of inference, routing, routing_dryrun, all"})
		return
	}
	if purpose != "all" {
		where += fmt.Sprintf(" AND ie.purpose = $%d", argN)
		args = append(args, purpose)
		argN++
	}

	var filterErr error
	if where, args, argN, filterErr = appendDenialCodeFilter(c, where, args, argN); filterErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": filterErr.Error()})
		return
	}
	if where, args, argN, filterErr = appendBackendClassFilter(c, where, args, argN); filterErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": filterErr.Error()})
		return
	}

	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ie.created_at >= $%d", argN)
		args = append(args, t)
		argN++
	}

	if v := c.Query("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})
			return
		}
		where += fmt.Sprintf(" AND ie.created_at <= $%d", argN)
		args = append(args, t)
		argN++
	}

	args = append(args, 10000)
	q := fmt.Sprintf(`
		SELECT
			ie.created_at,
			ie.request_id,
			ie.user_id::text,
			u.name,
			u.email,
			ie.api_key_id::text,
			ie.model,
			ie.requested_model,
			ie.provider,
			ie.purpose,
			ie.route_name,
			ie.router_reason,
			ie.status_code,
			ie.denial_code,
			ie.latency_ms,
			ie.prompt_tokens,
			ie.completion_tokens,
			ie.stream,
			ie.ip_address::text,
			ie.backend_class,
			ie.cache_read_tokens,
			ie.cache_creation_tokens,
			ie.region
		FROM inference_events ie
		LEFT JOIN users u ON u.id = ie.user_id
		%s
		ORDER BY ie.created_at DESC
		LIMIT $%d
	`, where, argN)

	rows, err := h.db.Query(c.Request.Context(), q, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "export usage query failed"})
		return
	}
	defer rows.Close()

	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", `attachment; filename="inference_events.csv"`)

	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"timestamp", "request_id", "user_id", "user_name", "user_email", "api_key_id", "model", "requested_model", "provider", "purpose", "route_name", "router_reason", "status_code", "denial_code", "latency_ms", "prompt_tokens", "completion_tokens", "stream", "ip_address", "backend_class", "cache_read_tokens", "cache_creation_tokens", "cost_cents"})

	prices, err := pricing.LoadAll(c.Request.Context(), h.db)
	if err != nil {
		prices = nil // degrade: cost column blank rather than abort the export
	}
	for rows.Next() {
		var (
			createdAt        time.Time
			requestID        string
			userID           *string
			userName         *string
			userEmail        *string
			apiKeyID         *string
			model            string
			requestedModel   *string
			provider         string
			purpose          string
			routeName        *string
			routerReason     *string
			statusCode       int
			denialCode       *string
			latencyMS        int
			promptTokens     *int
			completionTokens *int
			stream           bool
			ipAddress        *string
			backendClass     *string
			cacheRead        *int
			cacheCreate      *int
			region           *string
		)
		if err := rows.Scan(
			&createdAt, &requestID, &userID, &userName, &userEmail, &apiKeyID, &model, &requestedModel, &provider,
			&purpose, &routeName, &routerReason,
			&statusCode, &denialCode, &latencyMS, &promptTokens, &completionTokens, &stream, &ipAddress,
			&backendClass, &cacheRead, &cacheCreate, &region,
		); err != nil {
			w.Flush()
			return
		}
		costCents := eventCostCents(prices, provider, model, region, createdAt,
			denialCode != nil, promptTokens, completionTokens, cacheRead, cacheCreate)
		_ = w.Write([]string{
			createdAt.UTC().Format(time.RFC3339),
			requestID,
			derefStr(userID),
			csvSafeText(derefStr(userName)),
			csvSafeText(derefStr(userEmail)),
			derefStr(apiKeyID),
			csvSafeText(model),
			csvSafeText(derefStr(requestedModel)),
			provider,
			purpose,
			csvSafeText(derefStr(routeName)),
			derefStr(routerReason),
			strconv.Itoa(statusCode),
			derefStr(denialCode),
			strconv.Itoa(latencyMS),
			derefInt(promptTokens),
			derefInt(completionTokens),
			strconv.FormatBool(stream),
			derefStr(ipAddress),
			derefStr(backendClass),
			derefInt(cacheRead),
			derefInt(cacheCreate),
			derefStr(costCents),
		})
	}
	w.Flush()
}

// userSummaryRow is one row in the GET /api/v1/usage/user-summary response.
type userSummaryRow struct {
	UserID       string    `json:"user_id"`
	UserName     *string   `json:"user_name"`
	UserEmail    *string   `json:"user_email"`
	RequestCount int       `json:"request_count"`
	AvgLatencyMS float64   `json:"avg_latency_ms"`
	LastActive   time.Time `json:"last_active"`
}

// UserSummary handles GET /api/v1/usage/user-summary.
// Required query params: since, until.
func (h *ActivityHandler) UserSummary(c *gin.Context) {
	since, until, err := requireTimeRange(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	rows, err := h.db.Query(c.Request.Context(), `
		SELECT
			ie.user_id::text,
			u.name,
			u.email,
			COUNT(*) AS request_count,
			ROUND(AVG(ie.latency_ms)::numeric, 2)::float8 AS avg_latency_ms,
			MAX(ie.created_at) AS last_active
		FROM inference_events ie
		LEFT JOIN users u ON u.id = ie.user_id
		WHERE ie.created_at >= $1 AND ie.created_at <= $2
		  AND ie.user_id IS NOT NULL
		  AND ie.purpose = 'inference'
		GROUP BY ie.user_id, u.name, u.email
		ORDER BY request_count DESC
	`, since, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "user summary query failed"})
		return
	}
	defer rows.Close()

	result := make([]userSummaryRow, 0)
	for rows.Next() {
		var r userSummaryRow
		if err := rows.Scan(&r.UserID, &r.UserName, &r.UserEmail, &r.RequestCount, &r.AvgLatencyMS, &r.LastActive); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan user summary failed"})
			return
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, result)
}

// keySummaryRow is one row in the GET /api/v1/usage/key-summary response.
type keySummaryRow struct {
	APIKeyID     string    `json:"api_key_id"`
	KeyName      *string   `json:"key_name"`
	KeyPrefix    *string   `json:"key_prefix"`
	OwnerName    *string   `json:"owner_name"`
	RequestCount int       `json:"request_count"`
	AvgLatencyMS float64   `json:"avg_latency_ms"`
	LastActive   time.Time `json:"last_active"`
}

// KeySummary handles GET /api/v1/usage/key-summary.
// Required query params: since, until.
func (h *ActivityHandler) KeySummary(c *gin.Context) {
	since, until, err := requireTimeRange(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	rows, err := h.db.Query(c.Request.Context(), `
		SELECT
			ie.api_key_id::text,
			k.name,
			k.prefix,
			u.name AS owner_name,
			COUNT(*) AS request_count,
			ROUND(AVG(ie.latency_ms)::numeric, 2)::float8 AS avg_latency_ms,
			MAX(ie.created_at) AS last_active
		FROM inference_events ie
		JOIN api_keys k ON k.id = ie.api_key_id
		LEFT JOIN users u ON u.id = k.user_id
		WHERE ie.created_at >= $1 AND ie.created_at <= $2
		  AND ie.purpose = 'inference'
		GROUP BY ie.api_key_id, k.name, k.prefix, u.name
		ORDER BY request_count DESC
	`, since, until)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "key summary query failed"})
		return
	}
	defer rows.Close()

	result := make([]keySummaryRow, 0)
	for rows.Next() {
		var r keySummaryRow
		if err := rows.Scan(&r.APIKeyID, &r.KeyName, &r.KeyPrefix, &r.OwnerName, &r.RequestCount, &r.AvgLatencyMS, &r.LastActive); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan key summary failed"})
			return
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	c.JSON(http.StatusOK, result)
}

// derefStr safely dereferences a *string, returning "" for nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefInt safely dereferences a *int, returning "" for nil.
func derefInt(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

// parsePagination extracts and validates limit/offset from query params.
func parsePagination(c *gin.Context, defaultLimit, maxLimit int) (limit, offset int, err error) {
	limit = defaultLimit
	if v := c.Query("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit < 1 {
			return 0, 0, fmt.Errorf("limit must be a positive integer")
		}
		if limit > maxLimit {
			limit = maxLimit
		}
	}

	if v := c.Query("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative integer")
		}
	}

	return limit, offset, nil
}

// requireTimeRange parses and validates mandatory since/until query params.
func requireTimeRange(c *gin.Context) (since, until time.Time, err error) {
	sv := c.Query("since")
	uv := c.Query("until")
	if sv == "" || uv == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("since and until are required")
	}
	since, err = time.Parse(time.RFC3339, sv)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("since must be RFC3339")
	}
	until, err = time.Parse(time.RFC3339, uv)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("until must be RFC3339")
	}
	if !until.After(since) {
		return time.Time{}, time.Time{}, fmt.Errorf("until must be after since")
	}
	return since, until, nil
}

// unmarshalJSONMap decodes a JSON byte slice into a map[string]any.
func unmarshalJSONMap(data []byte, dst *map[string]any) error {
	if len(data) == 0 {
		return nil
	}
	*dst = make(map[string]any)
	return json.Unmarshal(data, dst)
}
