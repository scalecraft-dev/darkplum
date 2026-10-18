package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/api/internal/auth"
)

const (
	svSessionCookie = "darkplum_session"
	svRefreshCookie = "darkplum_refresh"
)

func svRequest(t *testing.T, deps routerDeps, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.AddCookie(&http.Cookie{Name: svSessionCookie, Value: token})
	rec := httptest.NewRecorder()
	buildRouter(deps).ServeHTTP(rec, req)
	return rec
}

func svClearedCookies(rec *httptest.ResponseRecorder) map[string]bool {
	out := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 && c.Value == "" {
			out[c.Name] = true
		}
	}
	return out
}

func svInsertUser(t *testing.T, deps routerDeps, email, role string) string {
	t.Helper()
	var id string
	if err := deps.pool.QueryRow(context.Background(),
		`INSERT INTO users (email, name, role) VALUES ($1, 'U', $2) RETURNING id::text`, email, role).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// svToken mints an access token bound to a live session row when the user
// exists; for a ghost user (no row to reference) it mints a sid-less token,
// which exercises the user-gone path exactly as a stale real token would.
func svToken(t *testing.T, deps routerDeps, id, email, role string) string {
	t.Helper()
	sid := ""
	if s, _, err := auth.InsertSession(context.Background(), deps.pool, id); err == nil {
		sid = s
	}
	tok, err := auth.SignSessionJWT(sid, id, email, "U", role, deps.authCfg.Secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A validly signed JWT for a user that does not exist (wiped DB, same signing
// key) must be 401 with both session cookies cleared, on an admin route and
// on /auth/me.
func TestSession_UnknownUserRejectedAndCookiesCleared(t *testing.T) {
	deps := newProvidersRouterTestDeps(t)
	tok := svToken(t, deps, "22222222-2222-2222-2222-222222222222", "ghost@example.com", "admin")

	for _, path := range []string{"/api/v1/users/x/routing-policy", "/auth/me"} {
		rec := svRequest(t, deps, http.MethodGet, path, tok)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, body = %s, want 401", path, rec.Code, rec.Body.String())
		}
		cleared := svClearedCookies(rec)
		if !cleared[svSessionCookie] || !cleared[svRefreshCookie] {
			t.Errorf("%s: cookies not cleared: %v", path, cleared)
		}
	}
}

// DB role wins over the JWT claim, in both directions, and the user-update
// handler invalidates the cache so the change is immediate.
func TestSession_DBRoleWinsAndHandlerInvalidatesCache(t *testing.T) {
	deps := newProvidersRouterTestDeps(t)
	ctx := context.Background()

	adminID := svInsertUser(t, deps, "boss@example.com", "admin")
	adminTok := svToken(t, deps, adminID, "boss@example.com", "admin")
	targetID := svInsertUser(t, deps, "target@example.com", "admin")
	targetTok := svToken(t, deps, targetID, "target@example.com", "admin")

	// POST /users with an empty body: admin => 400 (binding), reader => 403.
	const adminRoute = "/api/v1/users"
	if rec := svRequest(t, deps, http.MethodPost, adminRoute, targetTok); rec.Code != http.StatusBadRequest {
		t.Fatalf("admin before demotion: %d %s", rec.Code, rec.Body.String())
	}

	// Demote through the real handler: cached admin entry must be invalidated.
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/"+targetID+"/role", strings.NewReader(`{"role":"reader"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.AddCookie(&http.Cookie{Name: svSessionCookie, Value: adminTok})
	rec := httptest.NewRecorder()
	buildRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote: %d %s", rec.Code, rec.Body.String())
	}
	if rec := svRequest(t, deps, http.MethodPost, adminRoute, targetTok); rec.Code != http.StatusForbidden {
		t.Fatalf("demoted user (JWT still says admin): status = %d, want 403", rec.Code)
	}

	// Promotion: a reader-claim JWT for a DB admin is honoured as admin.
	if _, err := deps.pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, targetID); err != nil {
		t.Fatal(err)
	}
	auth.InvalidateUser(targetID) // direct SQL bypassed the handler
	readerTok := svToken(t, deps, targetID, "target@example.com", "reader")
	if rec := svRequest(t, deps, http.MethodPost, adminRoute, readerTok); rec.Code != http.StatusBadRequest {
		t.Fatalf("promoted user: status = %d, want 400 (admin passed)", rec.Code)
	}

	// Delete through the real handler: next request is 401 + cleared.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+targetID, nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.AddCookie(&http.Cookie{Name: svSessionCookie, Value: adminTok})
	rec = httptest.NewRecorder()
	buildRouter(deps).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = svRequest(t, deps, http.MethodPost, adminRoute, readerTok)
	if rec.Code != http.StatusUnauthorized || !svClearedCookies(rec)[svSessionCookie] {
		t.Fatalf("deleted user: status = %d cleared=%v, want 401 + cleared", rec.Code, svClearedCookies(rec))
	}
}

// The refresh path applies the same rule: current DB role, and a vanished
// user is 401 with cookies cleared.
func TestSession_RefreshPathAppliesSameRule(t *testing.T) {
	deps := newProvidersRouterTestDeps(t)
	ctx := context.Background()
	id := svInsertUser(t, deps, "refresh@example.com", "reader")
	if _, err := deps.pool.Exec(ctx,
		`INSERT INTO sessions (user_id, refresh_token_hash, expires_at) VALUES ($1, $2, now() + interval '1 day')`,
		id, auth.HashString("rt-1")); err != nil {
		t.Fatal(err)
	}

	do := func(rt string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
		req.AddCookie(&http.Cookie{Name: svRefreshCookie, Value: rt})
		rec := httptest.NewRecorder()
		buildRouter(deps).ServeHTTP(rec, req)
		return rec
	}
	rec := do("rt-1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"role":"reader"`) {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	var newRT string
	for _, c := range rec.Result().Cookies() {
		if c.Name == svRefreshCookie {
			newRT = c.Value
		}
	}
	if newRT == "" {
		t.Fatal("no rotated refresh cookie")
	}

	// Remove the user without the sessions row cascading away.
	if _, err := deps.pool.Exec(ctx, `ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_user_id_fkey`); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	auth.InvalidateUser(id)
	rec = do(newRT)
	if rec.Code != http.StatusUnauthorized || !svClearedCookies(rec)[svRefreshCookie] {
		t.Fatalf("refresh for deleted user: %d cleared=%v", rec.Code, svClearedCookies(rec))
	}
}
