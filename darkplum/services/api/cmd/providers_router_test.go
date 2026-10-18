package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
	"github.com/scalecraft-dev/darkplum/api/internal/auth"
	"github.com/scalecraft-dev/darkplum/api/internal/billing"
	"github.com/scalecraft-dev/darkplum/api/internal/handler"
	"github.com/scalecraft-dev/darkplum/api/internal/kube"
)

// newProvidersRouterTestDeps builds routerDeps identical to
// TestRouteInventoryMatchesGolden's baseDeps -- duplicated rather than
// exported from that file so this test doesn't reach across test files for
// something this small.
func newProvidersRouterTestDeps(t *testing.T) routerDeps {
	t.Helper()
	pool := routeInventoryTestDB(t)
	activityRecorder := activity.NewRecorder(pool)

	authCfg := &auth.Config{
		Secret:       []byte(strings.Repeat("x", 32)),
		SecureCookie: true,
	}
	localProvider := auth.NewLocalProvider(authCfg, pool, activityRecorder)
	sessionMW := auth.SessionRequired(pool, authCfg.Secret, authCfg.SecureCookie)

	users := handler.NewUserHandler(pool, activityRecorder, authCfg.Pepper)
	keys := handler.NewAPIKeyHandler(pool, activityRecorder)
	chat := handler.NewChatHandler("http://unused.invalid", "unused", nil)
	activityH := handler.NewActivityHandler(pool)
	routing := handler.NewRoutingHandler(pool, activityRecorder, "http://unused.invalid", "unused")
	policy := handler.NewPolicyHandler(pool, activityRecorder, "http://unused.invalid", "unused")
	modelHandler := handler.NewModelHandler("http://unused.invalid", "unused", pool)
	conversations := handler.NewConversationHandler(pool, activityRecorder)
	prices := handler.NewPricingHandler(pool, activityRecorder)
	providers := handler.NewProvidersHandler(pool, activityRecorder, "http://unused.invalid", "unused", kube.NewWriter(), modelHandler, true)

	return routerDeps{
		pool: pool, authCfg: authCfg, sessionMW: sessionMW, localProvider: localProvider,
		users: users, keys: keys, chat: chat, activityH: activityH, routing: routing,
		policy: policy, modelHandler: modelHandler, conversations: conversations, providers: providers, prices: prices, settings: handler.NewSettingsHandler(pool, activityRecorder),
		billing: handler.NewBillingHandler(pool, activityRecorder, billing.NewService(&billing.Store{DB: pool}, billing.KeyReader{}, nil, nil, nil), kube.NewWriter(), true),
	}
}

// TestProviderRoutes_BearerRefusedBeforeAnythingElse covers PLAN-UI's
// security ruling: a request carrying Authorization: Bearer on any
// /api/v1/providers/* route is refused 403 before session auth OR the CSRF
// header check run -- proven here by sending a request with neither a
// session cookie NOR X-Requested-With (which would otherwise draw a 401 from
// session auth, since Bearer requests are exactly the ones csrfProtection
// exempts from that header check -- see csrfProtection's doc comment).
func TestProviderRoutes_BearerRefusedBeforeAnythingElse(t *testing.T) {
	r := buildRouter(newProvidersRouterTestDeps(t))

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/anthropic/key", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-some-api-key-value")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "missing required header") {
		t.Fatalf("expected the Bearer-specific refusal, got the CSRF refusal: %s", rec.Body.String())
	}
}

// TestProviderRoutes_NoCredentialGets401 proves the Bearer-refusal
// middleware doesn't swallow the normal "not authenticated" path: a request
// with neither a session cookie nor a Bearer header still reaches session
// auth and gets refused for the ordinary reason.
func TestProviderRoutes_NoCredentialGets401(t *testing.T) {
	r := buildRouter(newProvidersRouterTestDeps(t))

	req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/anthropic/key", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s, want 401", rec.Code, rec.Body.String())
	}
}

// TestProviderRoutes_BearerRefusalAppliesToAllFourRoutes covers every method
// the provider routes use (POST/PUT/DELETE), not just PUT key.
func TestProviderRoutes_BearerRefusalAppliesToAllFourRoutes(t *testing.T) {
	r := buildRouter(newProvidersRouterTestDeps(t))

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/providers/anthropic/verify"},
		{http.MethodPut, "/api/v1/providers/anthropic/key"},
		{http.MethodPut, "/api/v1/providers/anthropic/filter"},
		{http.MethodDelete, "/api/v1/providers/anthropic/key"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer sk-some-api-key-value")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, body = %s, want 403", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestProviderRoutes_NonAdminSessionRefused403 proves a signed-in
// NON-admin session is refused 403 on all four provider routes, through the
// real router-level middleware chain (rejectBearerOnProviderRoutes ->
// csrfProtection -> sessionMW -> AdminRequired) -- not a substituted
// user_id-only middleware the way internal/handler's own tests set up
// ProvidersHandler in isolation. The session cookie is a real signed JWT,
// minted directly via auth.SignSessionJWT rather than a full
// POST /auth/login round trip, since this test only needs SessionRequired
// to accept it and AdminRequired's role check to see role="member".
func TestProviderRoutes_NonAdminSessionRefused403(t *testing.T) {
	deps := newProvidersRouterTestDeps(t)
	r := buildRouter(deps)

	// The session's user must exist (ValidateSession is DB-authoritative).
	var memberID string
	if err := deps.pool.QueryRow(context.Background(),
		`INSERT INTO users (email, name, role) VALUES ('member@example.com', 'Member', 'reader') RETURNING id::text`).Scan(&memberID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	sid, _, err := auth.InsertSession(context.Background(), deps.pool, memberID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.SignSessionJWT(sid, memberID, "member@example.com", "Member", "reader", deps.authCfg.Secret, time.Hour)
	if err != nil {
		t.Fatalf("sign session jwt: %v", err)
	}
	// sessionCookieName ("darkplum_session") is unexported --
	// internal/auth/provider.go -- so it's reproduced literally here rather
	// than imported.
	const sessionCookieName = "darkplum_session"

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/providers/anthropic/verify"},
		{http.MethodPut, "/api/v1/providers/anthropic/key"},
		{http.MethodPut, "/api/v1/providers/anthropic/filter"},
		{http.MethodDelete, "/api/v1/providers/anthropic/key"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, body = %s, want 403", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}
