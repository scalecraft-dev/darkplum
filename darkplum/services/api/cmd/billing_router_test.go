package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft/darkplum/api/internal/auth"
)

var billingRoutes = []struct{ method, path string }{
	{http.MethodPost, "/api/v1/providers/anthropic/billing-key/verify"},
	{http.MethodPut, "/api/v1/providers/anthropic/billing-key"},
	{http.MethodDelete, "/api/v1/providers/openai/billing-key"},
	{http.MethodPost, "/api/v1/usage/reconciliation/refresh"},
}

// Bearer credentials are refused 403 (bearer_not_allowed) on every billing
// route, before session auth or CSRF.
func TestBillingRoutes_BearerRefused(t *testing.T) {
	r := buildRouter(newProvidersRouterTestDeps(t))
	for _, tc := range billingRoutes {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer sk-some-api-key-value")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "bearer_not_allowed") {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// A non-admin session is refused 403 on all billing routes including the
// reconciliation read.
func TestBillingRoutes_NonAdminRefused(t *testing.T) {
	deps := newProvidersRouterTestDeps(t)
	r := buildRouter(deps)
	var memberID string
	if err := deps.pool.QueryRow(context.Background(),
		`INSERT INTO users (email, name, role) VALUES ('member@example.com', 'Member', 'reader') RETURNING id::text`).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	sid, _, err := auth.InsertSession(context.Background(), deps.pool, memberID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.SignSessionJWT(sid, memberID, "member@example.com", "Member", "reader", deps.authCfg.Secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	routes := append([]struct{ method, path string }{{http.MethodGet, "/api/v1/usage/reconciliation"}}, billingRoutes...)
	for _, tc := range routes {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.AddCookie(&http.Cookie{Name: "darkplum_session", Value: token})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d %s want 403", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
