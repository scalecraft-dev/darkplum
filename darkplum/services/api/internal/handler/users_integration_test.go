package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

func newUsersTestRouter(h *UserHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/users", h.CreateUser)
	return r
}

// TestCreateUser_OptionalRole covers DESIGN4's parity item: POST
// /api/v1/users accepts an optional role, defaulting to 'reader'.
func TestCreateUser_OptionalRole(t *testing.T) {
	pool := setupTestDB(t)
	h := NewUserHandler(pool, activity.NewRecorder(pool), nil)
	r := newUsersTestRouter(h)

	resp := doJSON(t, r, http.MethodPost, "/api/v1/users", map[string]any{
		"email": "reader-default@example.com", "name": "Default Reader",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var u User
	if err := json.Unmarshal(resp.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.Role != "reader" {
		t.Fatalf("expected default role 'reader', got %q", u.Role)
	}

	resp = doJSON(t, r, http.MethodPost, "/api/v1/users", map[string]any{
		"email": "explicit-admin@example.com", "name": "Explicit Admin", "role": "admin",
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.Role != "admin" {
		t.Fatalf("expected role 'admin', got %q", u.Role)
	}

	resp = doJSON(t, r, http.MethodPost, "/api/v1/users", map[string]any{
		"email": "bad-role@example.com", "name": "Bad Role", "role": "superuser",
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an invalid role; body = %s", resp.Code, resp.Body.String())
	}

	// The rejected create above must not have inserted a row.
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM users WHERE email = 'bad-role@example.com'`,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no row for the rejected create, found %d", count)
	}
}
