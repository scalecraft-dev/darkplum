package handler

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newActivityTestRouter wires ActivityHandler's usage endpoints directly
// (they're read-only and admin-gated in production by cmd/main.go's
// adminOnly group, which these tests don't need to reconstruct).
func newActivityTestRouter(h *ActivityHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/usage", h.ListUsage)
	r.GET("/api/v1/usage/export", h.ExportUsage)
	r.GET("/api/v1/usage/summary", h.UsageSummary)
	return r
}

// insertInferenceEventWithPurpose inserts an inference_events row with an
// explicit purpose/route_name/router_reason, for exercising the purpose
// filter added across activity.go's usage endpoints (migration
// 013_routed_aliases.sql).
func insertInferenceEventWithPurpose(t *testing.T, ctx context.Context, pool *pgxpool.Pool, requestID, userID, model, purpose, routeName, routerReason string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO inference_events (request_id, user_id, model, purpose, route_name, router_reason, status_code, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, 200, 10)
	`, requestID, userID, model, purpose, routeName, routerReason)
	if err != nil {
		t.Fatalf("insert inference_events row (purpose=%s): %v", purpose, err)
	}
}

// TestListUsage_DefaultsToInferenceAndFiltersByPurpose covers the ?purpose=
// contract: a missing purpose defaults to "inference" (excluding
// routing/routing_dryrun rows); an explicit value filters to exactly that
// purpose; "all" removes the filter entirely.
func TestListUsage_DefaultsToInferenceAndFiltersByPurpose(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)

	insertInferenceEventWithPurpose(t, ctx, pool, "req-1", userID, "llama3", "inference", "", "")
	insertInferenceEventWithPurpose(t, ctx, pool, "req-1", userID, "classifier-model", "routing", "", "")
	insertInferenceEventWithPurpose(t, ctx, pool, "req-2", userID, "llama3", "routing_dryrun", "general", "rules:default")

	h := NewActivityHandler(pool)
	r := newActivityTestRouter(h)

	// Default (no ?purpose=): only the inference row.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil))
	var resp usageListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Events) != 1 || resp.Events[0].Purpose != "inference" {
		t.Fatalf("default purpose filter: total=%d events=%+v, want exactly 1 inference row", resp.Total, resp.Events)
	}

	// ?purpose=routing: only the routing row.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage?purpose=routing", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Events[0].Model != "classifier-model" {
		t.Fatalf("?purpose=routing: total=%d events=%+v", resp.Total, resp.Events)
	}

	// ?purpose=all: every row, regardless of purpose.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage?purpose=all", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 {
		t.Fatalf("?purpose=all: total=%d, want 3", resp.Total)
	}

	// ?purpose=bogus: 400.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage?purpose=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("?purpose=bogus: status = %d, want 400", rec.Code)
	}
}

// TestListUsage_ExposesRouteNameAndRouterReason covers the usage list rows
// gaining purpose/route_name/router_reason.
func TestListUsage_ExposesRouteNameAndRouterReason(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertInferenceEventWithPurpose(t, ctx, pool, "req-1", userID, "llama3-code", "inference", "code", "rules:code:builtin:code")

	h := NewActivityHandler(pool)
	r := newActivityTestRouter(h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil))
	var resp usageListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 1 {
		t.Fatalf("events = %+v, want 1", resp.Events)
	}
	e := resp.Events[0]
	if e.Purpose != "inference" || e.RouteName == nil || *e.RouteName != "code" || e.RouterReason == nil || *e.RouterReason != "rules:code:builtin:code" {
		t.Fatalf("event = %+v, want purpose/route_name/router_reason populated", e)
	}
}

// TestUsageSummary_ExcludesNonInferencePurposeRows covers "every aggregation
// in activity.go filters purpose = 'inference' by default": a routing row
// sharing time range and user must not inflate total_requests or skew
// latency stats meant to describe end-user chat traffic.
func TestUsageSummary_ExcludesNonInferencePurposeRows(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertInferenceEventWithPurpose(t, ctx, pool, "req-1", userID, "llama3", "inference", "", "")
	insertInferenceEventWithPurpose(t, ctx, pool, "req-1", userID, "classifier-model", "routing", "", "")
	insertInferenceEventWithPurpose(t, ctx, pool, "req-2", userID, "llama3", "routing_dryrun", "", "")

	h := NewActivityHandler(pool)
	r := newActivityTestRouter(h)

	since := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	until := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage/summary?since="+since+"&until="+until, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var summary usageSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if summary.TotalRequests != 1 {
		t.Fatalf("total_requests = %d, want 1 (routing/routing_dryrun rows must be excluded)", summary.TotalRequests)
	}
}

// TestExportUsage_PurposeFilterAndColumns covers the export gaining
// purpose/route_name/router_reason CSV columns, and the same ?purpose=
// default/override contract as ListUsage.
func TestExportUsage_PurposeFilterAndColumns(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	userID := newTestUser(t, ctx, pool)
	insertInferenceEventWithPurpose(t, ctx, pool, "req-1", userID, "llama3", "inference", "", "")
	insertInferenceEventWithPurpose(t, ctx, pool, "req-1", userID, "classifier-model", "routing", "", "")

	h := NewActivityHandler(pool)
	r := newActivityTestRouter(h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 2 { // header + 1 default-filtered (inference) row
		t.Fatalf("rows = %v, want header + 1 inference row", rows)
	}
	header := rows[0]
	wantCols := []string{"purpose", "route_name", "router_reason"}
	for _, col := range wantCols {
		found := false
		for _, h := range header {
			if h == col {
				found = true
			}
		}
		if !found {
			t.Errorf("csv header %v missing column %q", header, col)
		}
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage/export?purpose=all", nil))
	rows, err = csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 3 { // header + both rows
		t.Fatalf("?purpose=all rows = %v, want header + 2 rows", rows)
	}
}
