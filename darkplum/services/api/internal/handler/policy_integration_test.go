package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

// newPolicyTestUser inserts a real users row with a unique-per-call email
// (unlike routing_integration_test.go's newTestUser, which keys off t.Name()
// alone and so only supports one call per test) -- most tests in this file
// need at least two users (an admin actor plus a target principal).
func newPolicyTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, name, role)
		VALUES ($1, 'Test User', 'admin')
		RETURNING id
	`, t.Name()+"+"+hex.EncodeToString(b)+"@example.com").Scan(&id)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return id
}

// newTestAPIKey inserts a real api_keys row owned by userID and returns its id.
func newTestAPIKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID, name string) string {
	t.Helper()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO api_keys (user_id, name, prefix, key_hash)
		VALUES ($1, $2, 'dp_test', $3)
		RETURNING id
	`, userID, name, hex.EncodeToString(b)).Scan(&id)
	if err != nil {
		t.Fatalf("insert test api key: %v", err)
	}
	return id
}

// newPolicyTestRouter wires PolicyHandler's endpoints (plus RoutingHandler's
// org-level GET/PUT, needed by several tests below) behind a fake session
// middleware, mirroring newRoutingTestRouter.
func newPolicyTestRouter(ph *PolicyHandler, rh *RoutingHandler, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Next()
	})
	r.GET("/api/v1/routing-policy", rh.GetRoutingPolicy)
	r.PUT("/api/v1/routing-policy", rh.PutRoutingPolicy)
	r.GET("/api/v1/routing-policy/principals", ph.ListPrincipalPolicies)
	r.GET("/api/v1/me/routing-policy/effective", ph.GetMyPolicyEffective)

	r.GET("/api/v1/users/:id/routing-policy", ph.GetUserPolicy)
	r.PUT("/api/v1/users/:id/routing-policy", ph.PutUserPolicy)
	r.DELETE("/api/v1/users/:id/routing-policy", ph.DeleteUserPolicy)
	r.GET("/api/v1/users/:id/routing-policy/effective", ph.GetUserPolicyEffective)
	r.POST("/api/v1/users/:id/routing-policy/preview", ph.PreviewUserPolicy)

	r.GET("/api/v1/api-keys/:id/routing-policy", ph.GetAPIKeyPolicy)
	r.PUT("/api/v1/api-keys/:id/routing-policy", ph.PutAPIKeyPolicy)
	r.DELETE("/api/v1/api-keys/:id/routing-policy", ph.DeleteAPIKeyPolicy)
	r.GET("/api/v1/api-keys/:id/routing-policy/effective", ph.GetAPIKeyPolicyEffective)
	r.POST("/api/v1/api-keys/:id/routing-policy/preview", ph.PreviewAPIKeyPolicy)

	keys := NewAPIKeyHandler(ph.db, ph.activity)
	r.DELETE("/api/v1/api-keys/:id", keys.RevokeAPIKey)
	return r
}

// fakeGatewayEffective spins an httptest.Server implementing enough of the
// gateway's GET /v1/policy/effective for the api's proxying endpoints —
// it just echoes back a canned response regardless of query params, which
// is all these tests need (the merge algorithm itself is gateway-owned and
// tested there; here we're only testing that the api proxies correctly and
// degrades on failure).
func fakeGatewayEffective(t *testing.T, resp gatewayEffectivePolicy) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/policy/effective" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if auth := req.Header.Get("Authorization"); auth != "Bearer testkey" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func decodeJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode JSON %s: %v", body, err)
	}
	return v
}

// --- User policy: GET/PUT/DELETE round trip ---------------------------------

func TestUserPolicy_GetPutDeleteRoundTrip(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)

	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	// No row yet: everything nil.
	resp := doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("get (no row): status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.Residency != nil || got.AllowedModels != nil || got.DefaultModel != nil {
		t.Fatalf("expected all-nil policy with no row, got %+v", got)
	}

	// PUT sets an explicit policy.
	self := residencySelfHostedOnly
	m1 := "m1"
	body := policyObject{Residency: &self, AllowedModels: strs("m1", "m2"), DefaultModel: &m1}
	resp = doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", body)
	if resp.Code != http.StatusOK {
		t.Fatalf("put: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got = decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.Residency == nil || *got.Residency != residencySelfHostedOnly {
		t.Fatalf("expected residency self_hosted_only, got %+v", got)
	}
	if got.AllowedModels == nil || len(*got.AllowedModels) != 2 {
		t.Fatalf("expected 2 allowed models, got %+v", got.AllowedModels)
	}
	if got.DefaultModel == nil || *got.DefaultModel != "m1" {
		t.Fatalf("expected default_model m1, got %v", got.DefaultModel)
	}

	// Activity event recorded with before/after.
	var detailJSON []byte
	if err := pool.QueryRow(ctx,
		`SELECT detail FROM activity_events WHERE event_type = $1 ORDER BY created_at DESC LIMIT 1`,
		string(activity.RoutingPolicyPrincipalChanged),
	).Scan(&detailJSON); err != nil {
		t.Fatalf("fetch principal_changed event: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatalf("decode event detail: %v", err)
	}
	if detail["principal_type"] != "user" {
		t.Fatalf("expected principal_type=user, got %+v", detail)
	}
	if detail["before"] != nil {
		t.Fatalf("expected before=null for a first-time PUT, got %+v", detail["before"])
	}
	if detail["after"] == nil {
		t.Fatal("expected after to be populated")
	}

	// GET reflects the stored row.
	resp = doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy", nil)
	got = decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.UpdatedAt == nil {
		t.Fatal("expected updated_at to be set")
	}

	// DELETE resets to inherit.
	resp = doJSON(t, r, http.MethodDelete, "/api/v1/users/"+targetID+"/routing-policy", nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	resp = doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy", nil)
	got = decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.Residency != nil || got.AllowedModels != nil {
		t.Fatalf("expected inherit (all nil) after delete, got %+v", got)
	}
}

func TestUserPolicy_UnknownUser404(t *testing.T) {
	pool := setupTestDB(t)
	adminID := newPolicyTestUser(t, context.Background(), pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodGet, "/api/v1/users/00000000-0000-0000-0000-000000000000/routing-policy", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", resp.Code, resp.Body.String())
	}
}

func TestUserPolicy_UnknownModelIDRejected(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("does-not-exist")})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "unknown model id") {
		t.Fatalf("expected 'unknown model id' error, got %s", resp.Body.String())
	}
}

func TestUserPolicy_AliasRejectedInAllowlist(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	if _, err := pool.Exec(ctx, `INSERT INTO model_aliases (alias, kind, target_model_id) VALUES ('default', 'static', 'm1')`); err != nil {
		t.Fatalf("insert alias: %v", err)
	}
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("default")})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "add the alias's targets instead") {
		t.Fatalf("expected the contract alias-rejection message, got %s", resp.Body.String())
	}
}

func TestUserPolicy_DefaultModelMustBeInEffectiveAllowlist(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	m2 := "m2"
	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy",
		policyObject{AllowedModels: strs("m1"), DefaultModel: &m2})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

func TestUserPolicy_ResidencyRejectsAny(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	any := residencyAny
	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{Residency: &any})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

// --- 409 lock-out ------------------------------------------------------------

func TestUserPolicy_EmptyAllowlistLocksOutUser409(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	empty := []string{}
	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: &empty})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}

	// Must not have written anything.
	resp = doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy", nil)
	got := decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.AllowedModels != nil {
		t.Fatalf("expected no row written after a 409, got %+v", got)
	}
}

func TestUserPolicy_TighteningLocksOutOwnedKey409(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	// The key is narrowed to m2 only.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m2")})
	if resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Narrowing the USER to m1 only would leave the key with an empty
	// intersection (m1 ∩ m2 = ∅) — must 409 and name the key.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ci-key") {
		t.Fatalf("expected the locked-out key's name in the 409 body, got %s", resp.Body.String())
	}
}

func TestUserPolicy_RevokedKeyIgnoredForLockout(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m2")})
	if resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	resp = doJSON(t, r, http.MethodDelete, "/api/v1/api-keys/"+keyID, nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("revoke key: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Now tightening the user to m1 only must succeed — the only key that
	// would have been locked out is revoked and can't make requests anyway.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}
}

// --- API key policy: owner chain, lock-out, revocation, cascade -------------

func TestAPIKeyPolicy_UnknownKey404(t *testing.T) {
	pool := setupTestDB(t)
	adminID := newPolicyTestUser(t, context.Background(), pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodGet, "/api/v1/api-keys/00000000-0000-0000-0000-000000000000/routing-policy", nil)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (also covers a service-key id, which is never in api_keys); body = %s", resp.Code, resp.Body.String())
	}
}

func TestAPIKeyPolicy_EffectiveRespectsOwnerChain(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m3", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	// Owner restricted to m1+m2.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1", "m2")})
	if resp.Code != http.StatusOK {
		t.Fatalf("put owner policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	// Key asks for m2+m3 — m3 is a real model (so it passes format
	// validation) but is outside the owner's allowlist, so the STORED value
	// still includes it (a key's own row is never truncated to what's
	// currently reachable) while the effective merge would exclude it.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m2", "m3")})
	if resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.AllowedModels == nil || len(*got.AllowedModels) != 2 {
		t.Fatalf("expected the stored row to keep both requested ids, got %+v", got.AllowedModels)
	}
}

func TestAPIKeyPolicy_EmptyAllowlistLocksOutKey409(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	empty := []string{}
	resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: &empty})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}
}

func TestAPIKeyRevocation_KeepsPolicyRow(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	resp = doJSON(t, r, http.MethodDelete, "/api/v1/api-keys/"+keyID, nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	// Revocation must not touch the policy row (audit trail survives).
	resp = doJSON(t, r, http.MethodGet, "/api/v1/api-keys/"+keyID+"/routing-policy", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("get policy after revoke: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.AllowedModels == nil || len(*got.AllowedModels) != 1 {
		t.Fatalf("expected the policy row to survive revocation, got %+v", got)
	}
}

func TestUserDeletion_CascadesPolicyRows(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put user policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, targetID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM principal_routing_policies WHERE (user_id = $1 OR api_key_id = $2)`,
		targetID, keyID,
	).Scan(&count); err != nil {
		t.Fatalf("count policy rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected both the user's and key's policy rows to cascade-delete, found %d remaining", count)
	}
}

// --- Preview (no writes) -----------------------------------------------------

func TestPreviewUserPolicy_NoWritesAndReportsLockOut(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	if resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m2")}); resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp := doJSON(t, r, http.MethodPost, "/api/v1/users/"+targetID+"/routing-policy/preview", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusOK {
		t.Fatalf("preview: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[previewResponse](t, resp.Body.Bytes())
	if len(got.WouldLockOut) == 0 {
		t.Fatalf("expected would_lock_out to name the key, got %+v", got)
	}
	found := false
	for _, name := range got.WouldLockOut {
		if name == "ci-key" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ci-key in would_lock_out, got %v", got.WouldLockOut)
	}

	// No write happened.
	resp = doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy", nil)
	after := decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if after.AllowedModels != nil {
		t.Fatalf("preview must not write; got stored policy %+v", after)
	}
}

// --- Org policy: affected_principals on tighten -----------------------------

func TestOrgPolicy_AffectedPrincipalsOnTighten(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://unused.invalid", "unused")
	r := newPolicyTestRouter(ph, rh, adminID)

	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{DefaultModel: nil}); resp.Code != http.StatusOK {
		t.Fatalf("put user policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{DefaultModel: nil}); resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Tightening: nil -> restricted.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": []string{"m1"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("put org policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[routingPolicyResponse](t, resp.Body.Bytes())
	if got.AffectedPrincipals == nil || *got.AffectedPrincipals != 2 {
		t.Fatalf("expected affected_principals=2, got %v", got.AffectedPrincipals)
	}

	// Loosening back must not report affected_principals.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": nil})
	if resp.Code != http.StatusOK {
		t.Fatalf("loosen org policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got = decodeJSON[routingPolicyResponse](t, resp.Body.Bytes())
	if got.AffectedPrincipals != nil {
		t.Fatalf("expected no affected_principals on a loosening change, got %v", *got.AffectedPrincipals)
	}
}

// --- Effective endpoints: gateway proxy, stored, and 502 --------------------

func TestGetUserPolicyEffective_ProxiesGatewayAndAddsStored(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)

	gw := fakeGatewayEffective(t, gatewayEffectivePolicy{
		AllowedModels:   strs("m1"),
		Residency:       residencyAny,
		DefaultModel:    nil,
		Sources:         map[string]string{"allowed_models": "user"},
		ReachableModels: []string{"m1"},
	})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put user policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp := doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy/effective", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["stored"] == nil {
		t.Fatalf("expected admin effective view to include stored, got %+v", body)
	}
	if body["reachable_models"] == nil {
		t.Fatalf("expected reachable_models proxied from the gateway, got %+v", body)
	}
}

func TestGetMyPolicyEffective_NoStoredField(t *testing.T) {
	pool := setupTestDB(t)
	userID := newPolicyTestUser(t, context.Background(), pool)
	gw := fakeGatewayEffective(t, gatewayEffectivePolicy{Residency: residencyAny, ReachableModels: []string{"m1"}})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, userID)

	resp := doJSON(t, r, http.MethodGet, "/api/v1/me/routing-policy/effective", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["stored"]; ok {
		t.Fatalf("expected /me effective to omit stored, got %+v", body)
	}
}

func TestEffectiveEndpoint_GatewayUnreachableReturns502(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://127.0.0.1:1", "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://127.0.0.1:1", "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy/effective", nil)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", resp.Code, resp.Body.String())
	}
}

// --- Principals list: pagination + cap --------------------------------------

func TestListPrincipalPolicies_PaginationAndLimitCap(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)

	gw := fakeGatewayEffective(t, gatewayEffectivePolicy{Residency: residencyAny, ReachableModels: []string{"m1"}})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	for i := 0; i < 3; i++ {
		u := newPolicyTestUser(t, ctx, pool)
		if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+u+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
			t.Fatalf("put user policy: status = %d, body = %s", resp.Code, resp.Body.String())
		}
	}

	resp := doJSON(t, r, http.MethodGet, "/api/v1/routing-policy/principals?limit=99999", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if int(body["limit"].(float64)) != principalsListMaxLimit {
		t.Fatalf("expected limit clamped to %d, got %v", principalsListMaxLimit, body["limit"])
	}
	if int(body["total"].(float64)) != 3 {
		t.Fatalf("expected total=3, got %v", body["total"])
	}
	principals, ok := body["principals"].([]any)
	if !ok || len(principals) != 3 {
		t.Fatalf("expected 3 principal entries, got %+v", body["principals"])
	}
}

func TestListPrincipalPolicies_DefaultLimit(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	gw := fakeGatewayEffective(t, gatewayEffectivePolicy{Residency: residencyAny})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodGet, "/api/v1/routing-policy/principals", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	if int(body["limit"].(float64)) != principalsListDefaultLimit {
		t.Fatalf("expected default limit %d, got %v", principalsListDefaultLimit, body["limit"])
	}
}

// TestParsePositiveIntUsedByQueryEscape is a smoke test that limit/offset
// query values round-trip through url.Values the same way gin's c.Query does
// -- guards against a future refactor changing how those are read.
func TestParsePositiveIntUsedByQueryEscape(t *testing.T) {
	v := url.Values{}
	v.Set("limit", "10")
	n, err := parsePositiveInt(v.Get("limit"))
	if err != nil || n != 10 {
		t.Fatalf("parsePositiveInt(%q) = (%d, %v), want (10, nil)", v.Get("limit"), n, err)
	}
}
