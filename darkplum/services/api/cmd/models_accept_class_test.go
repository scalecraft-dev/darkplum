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

// accept_class loosens where a model's prompts may go: a non-admin session is
// refused 403 through the real middleware chain and nothing is registered.
func TestModelsPatchAcceptClass_NonAdminRefused403(t *testing.T) {
	deps := newProvidersRouterTestDeps(t)
	r := buildRouter(deps)
	ctx := context.Background()

	var modelRow string
	if err := deps.pool.QueryRow(ctx,
		`INSERT INTO models (model_id, provider, backend_class) VALUES ('hf/llama', 'openai', 'external_api') RETURNING id::text`).Scan(&modelRow); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.pool.Exec(ctx, `INSERT INTO model_registered_classes (model_id, registered_class) VALUES ('hf/llama', 'self_hosted')`); err != nil {
		t.Fatal(err)
	}
	var memberID string
	if err := deps.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, role) VALUES ('member@example.com', 'Member', 'reader') RETURNING id::text`).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	sid, _, err := auth.InsertSession(ctx, deps.pool, memberID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.SignSessionJWT(sid, memberID, "member@example.com", "Member", "reader", deps.authCfg.Secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/models/"+modelRow, strings.NewReader(`{"accept_class":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.AddCookie(&http.Cookie{Name: "darkplum_session", Value: token})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", rec.Code, rec.Body.String())
	}
	var got string
	_ = deps.pool.QueryRow(ctx, `SELECT registered_class FROM model_registered_classes WHERE model_id='hf/llama'`).Scan(&got)
	if got != "self_hosted" {
		t.Errorf("registered class changed to %q", got)
	}
}
