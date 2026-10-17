package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
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

// capturedGatewayRequest records one call the api made to a fakeCapturingGateway.
// Body is the zero value for a GET (query params land in Query instead).
type capturedGatewayRequest struct {
	Method string
	Query  url.Values
	Body   gatewayEffectiveOverrideRequest
}

// fakeCapturingGateway returns the SAME canned gatewayEffectivePolicy for
// every GET/POST /v1/policy/effective call (so PUT/preview never lock out --
// these tests are about the request WIRE SHAPE, not the merge decision; see
// fakePolicyGateway above for that), and records every request so tests can
// assert the api sends the override body correctly.
type fakeCapturingGateway struct {
	*httptest.Server
	mu       sync.Mutex
	requests []capturedGatewayRequest
	resp     gatewayEffectivePolicy
}

func newFakeCapturingGateway(t *testing.T, resp gatewayEffectivePolicy) *fakeCapturingGateway {
	t.Helper()
	fg := &fakeCapturingGateway{resp: resp}
	fg.Server = httptest.NewServer(http.HandlerFunc(fg.handle))
	t.Cleanup(fg.Server.Close)
	return fg
}

func (fg *fakeCapturingGateway) handle(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer testkey" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	rec := capturedGatewayRequest{Method: req.Method}
	if req.Method == http.MethodPost {
		if err := json.NewDecoder(req.Body).Decode(&rec.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		rec.Query = req.URL.Query()
	}
	fg.mu.Lock()
	fg.requests = append(fg.requests, rec)
	fg.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fg.resp)
}

// all returns every request captured so far, in call order.
func (fg *fakeCapturingGateway) all() []capturedGatewayRequest {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]capturedGatewayRequest(nil), fg.requests...)
}

// newFakePolicyGateway spins an httptest.Server implementing GET/POST
// /v1/policy/effective for real, by reading the SAME
// principal_routing_policies/api_keys rows the api itself just wrote via
// pool -- a simplified (no aliases, no residency/enabled/routing awareness)
// reimplementation of the gateway's buildPrincipalPolicies/ChainFor/Effective
// (see gateway/internal/policy), good enough for the api's PUT/preview
// lock-out and merge assertions. The real merge algorithm's correctness is
// the gateway package's own responsibility and is tested there; this fake
// exists only to prove the api sends the override body correctly and
// consumes the response correctly, end to end against a real database.
type fakePolicyGateway struct {
	*httptest.Server
	pool *pgxpool.Pool
}

func newFakePolicyGateway(t *testing.T, pool *pgxpool.Pool) *fakePolicyGateway {
	t.Helper()
	fg := &fakePolicyGateway{pool: pool}
	fg.Server = httptest.NewServer(http.HandlerFunc(fg.handle))
	t.Cleanup(fg.Server.Close)
	return fg
}

func (fg *fakePolicyGateway) handle(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer testkey" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	ctx := req.Context()

	if req.URL.Path == "/v1/policy/impact" && req.Method == http.MethodPost {
		var body gatewayPolicyImpactRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Override.Level != principalTypeOrg {
			http.Error(w, "override.level must be org", http.StatusBadRequest)
			return
		}
		resp, err := fg.impact(ctx, &body.Override)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	if req.URL.Path != "/v1/policy/effective" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var userID, apiKeyID string
	var override *gatewayPolicyOverride
	switch req.Method {
	case http.MethodGet:
		userID = req.URL.Query().Get("user_id")
		apiKeyID = req.URL.Query().Get("api_key_id")
	case http.MethodPost:
		var body gatewayEffectiveOverrideRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		userID, apiKeyID, override = body.UserID, body.APIKeyID, body.Override
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	resp, err := fg.effective(ctx, userID, apiKeyID, override)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// fakeGatewayRow reads one principal's own stored row directly (empty id
// short-circuits to an all-nil "inherit" row, covering a key with no owner
// row and a top-level call with no api_key_id).
func (fg *fakePolicyGateway) fakeGatewayRow(ctx context.Context, principalType, id string) (policyObject, error) {
	var p policyObject
	var err error
	switch {
	case principalType == principalTypeOrg:
		err = fg.pool.QueryRow(ctx,
			`SELECT residency, allowed_models, default_model FROM principal_routing_policies WHERE principal_type = 'org'`,
		).Scan(&p.Residency, &p.AllowedModels, &p.DefaultModel)
	case id == "":
		return policyObject{}, nil
	case principalType == principalTypeAPIKey:
		err = fg.pool.QueryRow(ctx,
			`SELECT residency, allowed_models, default_model FROM principal_routing_policies WHERE principal_type = 'api_key' AND api_key_id = $1`, id,
		).Scan(&p.Residency, &p.AllowedModels, &p.DefaultModel)
	default:
		err = fg.pool.QueryRow(ctx,
			`SELECT residency, allowed_models, default_model FROM principal_routing_policies WHERE principal_type = 'user' AND user_id = $1`, id,
		).Scan(&p.Residency, &p.AllowedModels, &p.DefaultModel)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return policyObject{}, nil
	}
	if err != nil {
		return policyObject{}, err
	}
	return p, nil
}

// mergeRows re-implements Effective(chain) (gateway/internal/policy/effective.go)
// for exactly [org, user, api_key] in that order -- see that package's own
// tests for the authoritative version of this algorithm.
func mergeRows(rows [3]policyObject, present [3]bool) gatewayEffectivePolicy {
	levels := [3]string{principalTypeOrg, principalTypeUser, principalTypeAPIKey}
	eff := gatewayEffectivePolicy{
		Residency: residencyAny,
		Sources:   map[string]string{"allowed_models": "none", "residency": "none", "default_model": "none"},
	}
	var allowed *[]string
	haveAllowed, haveResidency, haveDefault := false, false, false
	for i := 2; i >= 0; i-- {
		if !present[i] {
			continue
		}
		r := rows[i]
		if r.AllowedModels != nil {
			if allowed == nil {
				// make + copy, NOT append([]string(nil), ...): see the real
				// gateway's Effective() (gateway/internal/policy/effective.go)
				// for why -- appending zero elements to a nil slice returns
				// nil, which would marshal this fake's JSON response as
				// "allowed_models":null instead of [] for an explicit
				// deny-all, exactly the bug this whole file's tests exist to
				// catch the api mishandling.
				cp := make([]string, len(*r.AllowedModels))
				copy(cp, *r.AllowedModels)
				allowed = &cp
			} else {
				set := make(map[string]bool, len(*r.AllowedModels))
				for _, id := range *r.AllowedModels {
					set[id] = true
				}
				next := make([]string, 0, len(*allowed))
				for _, id := range *allowed {
					if set[id] {
						next = append(next, id)
					}
				}
				allowed = &next
			}
			if !haveAllowed {
				eff.Sources["allowed_models"] = levels[i]
				haveAllowed = true
			}
		}
		if r.Residency != nil && *r.Residency == residencySelfHostedOnly {
			eff.Residency = residencySelfHostedOnly
			if !haveResidency {
				eff.Sources["residency"] = levels[i]
				haveResidency = true
			}
		}
		if !haveDefault && r.DefaultModel != nil {
			dm := *r.DefaultModel
			eff.DefaultModel = &dm
			eff.Sources["default_model"] = levels[i]
			haveDefault = true
		}
	}
	eff.AllowedModels = allowed
	return eff
}

// effective computes userID/apiKeyID's chain (org, user, api_key) from the
// database, applies override (if non-nil) at the named level, and returns
// the merged result -- with Keys populated exactly like the real gateway's
// HandleEffectivePolicyOverride: whenever apiKeyID is empty, or override
// targets "user"/"org".
func (fg *fakePolicyGateway) effective(ctx context.Context, userID, apiKeyID string, override *gatewayPolicyOverride) (gatewayEffectivePolicy, error) {
	orgRow, err := fg.fakeGatewayRow(ctx, principalTypeOrg, "org")
	if err != nil {
		return gatewayEffectivePolicy{}, err
	}
	if override != nil && override.Level == principalTypeOrg {
		orgRow = overriddenRow(override)
	}

	build := func(uid, kid string) (gatewayEffectivePolicy, error) {
		userRow, err := fg.fakeGatewayRow(ctx, principalTypeUser, uid)
		if err != nil {
			return gatewayEffectivePolicy{}, err
		}
		if override != nil && override.Level == principalTypeUser {
			userRow = overriddenRow(override)
		}
		rows := [3]policyObject{orgRow, userRow, {}}
		present := [3]bool{true, true, kid != ""}
		if kid != "" {
			keyRow, err := fg.fakeGatewayRow(ctx, principalTypeAPIKey, kid)
			if err != nil {
				return gatewayEffectivePolicy{}, err
			}
			if override != nil && override.Level == principalTypeAPIKey {
				keyRow = overriddenRow(override)
			}
			rows[2] = keyRow
		}
		return mergeRows(rows, present), nil
	}

	top, err := build(userID, apiKeyID)
	if err != nil {
		return gatewayEffectivePolicy{}, err
	}

	needKeys := apiKeyID == "" || (override != nil && (override.Level == principalTypeUser || override.Level == principalTypeOrg))
	if needKeys && userID != "" {
		rows, err := fg.pool.Query(ctx, `SELECT id::text FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`, userID)
		if err != nil {
			return gatewayEffectivePolicy{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var kid string
			if err := rows.Scan(&kid); err != nil {
				return gatewayEffectivePolicy{}, err
			}
			keyEff, err := build(userID, kid)
			if err != nil {
				return gatewayEffectivePolicy{}, err
			}
			top.Keys = append(top.Keys, gatewayKeyEffective{APIKeyID: kid, Effective: keyEff})
		}
		if err := rows.Err(); err != nil {
			return gatewayEffectivePolicy{}, err
		}
	}
	return top, nil
}

// impact mirrors the real gateway's policyImpact (gateway/internal/handler):
// under an org-level override, every user with a row, every owner of a key
// with a row, and every non-revoked key those users own is re-merged, and
// the ones whose allowlist comes out empty (locked_out) or whose reachable
// set shrinks (narrowed) are listed. "Reachable" here is allowed ∩ enabled
// models (no residency/routing awareness -- see the type's doc comment). An
// empty org allowlist short-circuits to org_locked_out.
func (fg *fakePolicyGateway) impact(ctx context.Context, override *gatewayPolicyOverride) (gatewayPolicyImpact, error) {
	out := gatewayPolicyImpact{LockedOut: []gatewayPolicyImpactPrincipal{}, Narrowed: []gatewayPolicyImpactPrincipal{}, UnresolvedModels: []string{}}

	var enabled []string
	eRows, err := fg.pool.Query(ctx, `SELECT model_id FROM models WHERE enabled ORDER BY model_id`)
	if err != nil {
		return out, err
	}
	for eRows.Next() {
		var id string
		if err := eRows.Scan(&id); err != nil {
			eRows.Close()
			return out, err
		}
		enabled = append(enabled, id)
	}
	eRows.Close()
	reachable := func(e gatewayEffectivePolicy) map[string]bool {
		r := map[string]bool{}
		for _, id := range enabled {
			if modelAllowed(e.AllowedModels, id) {
				r[id] = true
			}
		}
		return r
	}
	loses := func(before, after map[string]bool) bool {
		for id := range before {
			if !after[id] {
				return true
			}
		}
		return false
	}
	empty := func(e gatewayEffectivePolicy) bool { return e.AllowedModels != nil && len(*e.AllowedModels) == 0 }

	orgNow, err := fg.effective(ctx, "", "", nil)
	if err != nil {
		return out, err
	}
	orgAfter, err := fg.effective(ctx, "", "", override)
	if err != nil {
		return out, err
	}
	if empty(orgAfter) {
		out.OrgLockedOut = true
		out.OrgNarrowed = len(reachable(orgNow)) > 0
		return out, nil
	}
	out.OrgNarrowed = loses(reachable(orgNow), reachable(orgAfter))

	rows, err := fg.pool.Query(ctx, `
		SELECT DISTINCT u FROM (
			SELECT user_id::text AS u FROM principal_routing_policies WHERE principal_type = 'user'
			UNION
			SELECT k.user_id::text FROM principal_routing_policies p
			JOIN api_keys k ON k.id = p.api_key_id
			WHERE p.principal_type = 'api_key' AND k.revoked_at IS NULL
		) s ORDER BY u`)
	if err != nil {
		return out, err
	}
	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			rows.Close()
			return out, err
		}
		users = append(users, u)
	}
	rows.Close()
	for _, u := range users {
		now, err := fg.effective(ctx, u, "", nil)
		if err != nil {
			return out, err
		}
		after, err := fg.effective(ctx, u, "", override)
		if err != nil {
			return out, err
		}
		out.Evaluated++
		if empty(after) {
			out.LockedOut = append(out.LockedOut, gatewayPolicyImpactPrincipal{UserID: u})
		}
		// Only principals with a row of their own are "narrowed" (the real
		// gateway's rule): an inheriting user is the org, an inheriting key is
		// its owner.
		var userHasRow bool
		if err := fg.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principal_routing_policies WHERE principal_type='user' AND user_id=$1)`, u).Scan(&userHasRow); err != nil {
			return out, err
		}
		if userHasRow && loses(reachable(now), reachable(after)) {
			out.Narrowed = append(out.Narrowed, gatewayPolicyImpactPrincipal{UserID: u})
		}
		nowKeys := map[string]gatewayEffectivePolicy{}
		for _, k := range now.Keys {
			nowKeys[k.APIKeyID] = k.Effective
		}
		for _, k := range after.Keys {
			out.Evaluated++
			if empty(k.Effective) {
				out.LockedOut = append(out.LockedOut, gatewayPolicyImpactPrincipal{UserID: u, APIKeyID: k.APIKeyID})
			}
			var keyHasRow bool
			if err := fg.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principal_routing_policies WHERE principal_type='api_key' AND api_key_id=$1)`, k.APIKeyID).Scan(&keyHasRow); err != nil {
				return out, err
			}
			if keyHasRow && loses(reachable(nowKeys[k.APIKeyID]), reachable(k.Effective)) {
				out.Narrowed = append(out.Narrowed, gatewayPolicyImpactPrincipal{UserID: u, APIKeyID: k.APIKeyID})
			}
		}
	}
	return out, nil
}

// overriddenRow converts a gatewayPolicyOverride into the row it substitutes
// -- nil Policy ("inherit") becomes an all-nil row, exactly like the real
// gateway's PolicyOverride.Apply.
func overriddenRow(o *gatewayPolicyOverride) policyObject {
	if o.Policy == nil {
		return policyObject{}
	}
	return *o.Policy
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

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	any := residencyAny
	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{Residency: &any})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
}

// TestUserPolicy_ResidencyAcceptsCloudAccountOnly covers migration 016's
// widened principal_routing_policies.residency CHECK: before that migration,
// this write would have been rejected outright by the CHECK constraint, not
// just by validPrincipalResidency.
func TestUserPolicy_ResidencyAcceptsCloudAccountOnly(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	cloudOnly := residencyCloudAccountOnly
	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{Residency: &cloudOnly})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.Residency == nil || *got.Residency != residencyCloudAccountOnly {
		t.Fatalf("expected stored residency cloud_account_only, got %+v", got.Residency)
	}

	resp = doJSON(t, r, http.MethodGet, "/api/v1/users/"+targetID+"/routing-policy", nil)
	got = decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.Residency == nil || *got.Residency != residencyCloudAccountOnly {
		t.Fatalf("expected residency to round-trip as cloud_account_only, got %+v", got.Residency)
	}
}

// --- 409 lock-out ------------------------------------------------------------

func TestUserPolicy_EmptyAllowlistLocksOutUser409(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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
	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
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

// An all-null body means "inherit everything", which is what no row means:
// it must not leave a phantom row behind (one the UI shows as inheriting,
// the impact count treats as an exception, and the drawer cannot remove).
func TestUserPolicy_AllNullPutIsInherit(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	// Start with a real row, then "save" the drawer's inherit-everything form.
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put: %d %s", resp.Code, resp.Body.String())
	}
	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{})
	if resp.Code != http.StatusOK {
		t.Fatalf("all-null put: %d %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[principalPolicyResponse](t, resp.Body.Bytes())
	if got.UpdatedAt != nil || got.AllowedModels != nil || got.Residency != nil || got.DefaultModel != nil {
		t.Fatalf("all-null put must answer with the no-row shape, got %+v", got)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM principal_routing_policies WHERE user_id = $1`, targetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("all-null put left %d row(s) behind", n)
	}
	// And it is not listed as a principal with a policy of its own.
	list := doJSON(t, r, http.MethodGet, "/api/v1/routing-policy/principals", nil)
	if strings.Contains(list.Body.String(), targetID) {
		t.Fatalf("principals list still shows the user after an all-null put: %s", list.Body.String())
	}

	// The audit trail reads as the removal it was: before = the old row,
	// after = null -- the same shape DELETE writes.
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
	if detail["before"] == nil || detail["after"] != nil {
		t.Fatalf("all-null put must be audited as a delete (before set, after null), got %+v", detail)
	}
	var events int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM activity_events WHERE event_type = $1 AND target_id = $2`,
		string(activity.RoutingPolicyPrincipalChanged), targetID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	// An all-null put against a principal with no row is a no-op and must
	// not be recorded.
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{}); resp.Code != http.StatusOK {
		t.Fatalf("second all-null put: %d %s", resp.Code, resp.Body.String())
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM activity_events WHERE event_type = $1 AND target_id = $2`,
		string(activity.RoutingPolicyPrincipalChanged), targetID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != events {
		t.Fatalf("an all-null put with no row must record nothing; events went %d -> %d", events, after)
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

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	// Two principals with REAL rows of their own that the org narrowing will
	// bite: both allow m1,m2 explicitly today. (An all-null body is not a
	// row -- see TestUserPolicy_AllNullPutIsInherit -- so it could not be
	// used to stand up a counted principal here even by accident.)
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1", "m2")}); resp.Code != http.StatusOK {
		t.Fatalf("put user policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m1", "m2")}); resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	// A second key with NO row of its own: it inherits its owner and must
	// NOT be counted separately (the owner is).
	_ = newTestAPIKey(t, ctx, pool, targetID, "ci-key-inherits")
	// A third principal already limited to m1: it has a row, but the org
	// narrowing to m1 takes nothing from it, so it must NOT be counted.
	alreadyID := newPolicyTestUser(t, ctx, pool)
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+alreadyID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put already-narrow user policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Tightening: nil -> restricted. targetID and its key WITH a row allow
	// m1,m2 and drop to m1 (2 narrowed); the row-less key rides on its
	// owner; alreadyID stays at m1; the org itself shrank.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": []string{"m1"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("put org policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got := decodeJSON[routingPolicyResponse](t, resp.Body.Bytes())
	if got.AffectedPrincipals == nil || *got.AffectedPrincipals != 2 {
		t.Fatalf("expected affected_principals=2 (only those who lost a model), got %v", got.AffectedPrincipals)
	}
	if got.OrgNarrowed == nil || !*got.OrgNarrowed {
		t.Fatalf("expected org_narrowed=true, got %v", got.OrgNarrowed)
	}

	// Same list again (no-op): nothing shrinks, nothing reported.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": []string{"m1"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("no-op org policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got = decodeJSON[routingPolicyResponse](t, resp.Body.Bytes())
	if got.AffectedPrincipals != nil || got.OrgNarrowed != nil {
		t.Fatalf("expected no counts on a no-op change, got affected=%v org_narrowed=%v", got.AffectedPrincipals, got.OrgNarrowed)
	}

	// Loosening back must not report affected_principals.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": nil})
	if resp.Code != http.StatusOK {
		t.Fatalf("loosen org policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got = decodeJSON[routingPolicyResponse](t, resp.Body.Bytes())
	if got.AffectedPrincipals != nil || got.OrgNarrowed != nil {
		t.Fatalf("expected no counts on a loosening change, got affected=%v org_narrowed=%v", got.AffectedPrincipals, got.OrgNarrowed)
	}
}

// --- Org policy: lock-out guard (409), same rule as the user/key PUTs -------

func TestOrgPolicy_PutRefusesLockOut(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	// An empty org allowlist is refused outright, before any gateway call.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": []string{}})
	if resp.Code != http.StatusConflict {
		t.Fatalf("empty org allowlist: status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}

	// The user may only use m1; its key inherits that.
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put user policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Narrowing the org to m2 alone would leave that user (and its key) with
	// nothing: 409, naming both, and nothing written.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": []string{"m2"}})
	if resp.Code != http.StatusConflict {
		t.Fatalf("disjoint org allowlist: status = %d, want 409; body = %s", resp.Code, resp.Body.String())
	}
	var conflict struct {
		LockedOut []string `json:"locked_out"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode 409: %v", err)
	}
	var email string
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, targetID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	wantNames := map[string]bool{email: true, "ci-key (" + email + ")": true}
	if len(conflict.LockedOut) != 2 {
		t.Fatalf("locked_out = %v, want the user and its key", conflict.LockedOut)
	}
	for _, n := range conflict.LockedOut {
		if !wantNames[n] {
			t.Errorf("unexpected locked_out entry %q (want one of %v)", n, wantNames)
		}
	}
	var stored *[]string
	if err := pool.QueryRow(ctx, `SELECT allowed_models FROM principal_routing_policies WHERE principal_type = 'org'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatalf("org row was written despite the 409: %v", *stored)
	}
	_ = keyID

	// Narrowing to m1 keeps everyone reachable: 200.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": []string{"m1"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("compatible narrowing: status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestOrgPolicy_PutNarrowingIs502WhenGatewayDown(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)

	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), "http://127.0.0.1:1", "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), "http://127.0.0.1:1", "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	// A narrowing cannot be evaluated without the gateway: refuse, don't write.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencyAny, "allowed_models": []string{"m1"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("narrowing with gateway down: status = %d, want 502; body = %s", resp.Code, resp.Body.String())
	}
	// A residency-only change never needs the gateway.
	resp = doJSON(t, r, http.MethodPut, "/api/v1/routing-policy", map[string]any{"residency": residencySelfHostedOnly})
	if resp.Code != http.StatusOK {
		t.Fatalf("residency-only with gateway down: status = %d, want 200; body = %s", resp.Code, resp.Body.String())
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

// The wire shape the web's PolicyPrincipal type reads: typed ids (user_id /
// api_key_id, exactly one set), display = email or key name, owner = the
// key's owner email. Two pages of one row each must be disjoint and cover
// both rows -- the pager keys rows by these ids.
func TestListPrincipalPolicies_WireShapeAndOffsetPaging(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	ownerID := newPolicyTestUser(t, ctx, pool)
	keyID := newTestAPIKey(t, ctx, pool, ownerID, "ci-key")

	gw := fakeGatewayEffective(t, gatewayEffectivePolicy{Residency: residencyAny, ReachableModels: []string{"m1"}})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	if resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+ownerID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put user policy: %d %s", resp.Code, resp.Body.String())
	}
	if resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m1")}); resp.Code != http.StatusOK {
		t.Fatalf("put key policy: %d %s", resp.Code, resp.Body.String())
	}
	var ownerEmail string
	if err := pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, ownerID).Scan(&ownerEmail); err != nil {
		t.Fatal(err)
	}

	type entry struct {
		PrincipalType string  `json:"principal_type"`
		PrincipalID   string  `json:"principal_id"`
		UserID        *string `json:"user_id"`
		APIKeyID      *string `json:"api_key_id"`
		Display       string  `json:"display"`
		Owner         *string `json:"owner"`
	}
	type page struct {
		Principals []entry `json:"principals"`
		Total      int     `json:"total"`
	}
	seen := map[string]entry{}
	for offset := 0; offset < 2; offset++ {
		resp := doJSON(t, r, http.MethodGet, fmt.Sprintf("/api/v1/routing-policy/principals?limit=1&offset=%d", offset), nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("offset %d: %d %s", offset, resp.Code, resp.Body.String())
		}
		pg := decodeJSON[page](t, resp.Body.Bytes())
		if pg.Total != 2 || len(pg.Principals) != 1 {
			t.Fatalf("offset %d: total=%d rows=%d, want 2/1", offset, pg.Total, len(pg.Principals))
		}
		e := pg.Principals[0]
		if _, dup := seen[e.PrincipalID]; dup {
			t.Fatalf("offset %d returned %s again: paging is not disjoint", offset, e.PrincipalID)
		}
		seen[e.PrincipalID] = e
	}
	u, ok := seen[ownerID]
	if !ok {
		t.Fatalf("user row missing from the two pages: %+v", seen)
	}
	if u.PrincipalType != "user" || u.UserID == nil || *u.UserID != ownerID || u.APIKeyID != nil || u.Display != ownerEmail || u.Owner != nil {
		t.Errorf("user row shape wrong: %+v", u)
	}
	k, ok := seen[keyID]
	if !ok {
		t.Fatalf("key row missing from the two pages: %+v", seen)
	}
	if k.PrincipalType != "api_key" || k.APIKeyID == nil || *k.APIKeyID != keyID || k.UserID != nil || k.Display != "ci-key" || k.Owner == nil || *k.Owner != ownerEmail {
		t.Errorf("key row shape wrong: %+v", k)
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

// --- Wire contract: the api sends the override body correctly, and the
// preview response has the same shape as GET .../effective (sources,
// reachable_models) -- this is the exact bug being fixed: the preview
// endpoint used to return a LOCAL approximation missing both, causing
// `effective.sources.residency` to throw in the web drawer. -----------------

func TestPutUserPolicy_SendsOverrideBodyToGateway(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)

	gw := newFakeCapturingGateway(t, gatewayEffectivePolicy{
		Residency: residencyAny, AllowedModels: strs("m1"),
		Sources:         map[string]string{"allowed_models": "user", "residency": "none", "default_model": "none"},
		ReachableModels: []string{"m1"},
	})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusOK {
		t.Fatalf("put: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	reqs := gw.all()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 gateway call for the lock-out check, got %d", len(reqs))
	}
	got := reqs[0]
	if got.Method != http.MethodPost {
		t.Fatalf("expected the lock-out check to call POST, got %s", got.Method)
	}
	if got.Body.UserID != targetID {
		t.Errorf("user_id = %q, want %q", got.Body.UserID, targetID)
	}
	if got.Body.APIKeyID != "" {
		t.Errorf("api_key_id = %q, want empty (user-level write)", got.Body.APIKeyID)
	}
	if got.Body.Override == nil || got.Body.Override.Level != principalTypeUser {
		t.Fatalf("override = %+v, want level=user", got.Body.Override)
	}
	if got.Body.Override.Policy == nil || got.Body.Override.Policy.AllowedModels == nil || len(*got.Body.Override.Policy.AllowedModels) != 1 || (*got.Body.Override.Policy.AllowedModels)[0] != "m1" {
		t.Errorf("override.policy = %+v, want allowed_models=[m1]", got.Body.Override.Policy)
	}
}

func TestPutAPIKeyPolicy_SendsOverrideWithAPIKeyLevelAndOwnerUserID(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	gw := newFakeCapturingGateway(t, gatewayEffectivePolicy{
		Residency: residencyAny, AllowedModels: strs("m1"),
		Sources:         map[string]string{"allowed_models": "api_key", "residency": "none", "default_model": "none"},
		ReachableModels: []string{"m1"},
	})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusOK {
		t.Fatalf("put: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	reqs := gw.all()
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 gateway call, got %d", len(reqs))
	}
	got := reqs[0]
	if got.Body.UserID != targetID {
		t.Errorf("user_id = %q, want owner %q", got.Body.UserID, targetID)
	}
	if got.Body.APIKeyID != keyID {
		t.Errorf("api_key_id = %q, want %q", got.Body.APIKeyID, keyID)
	}
	if got.Body.Override == nil || got.Body.Override.Level != principalTypeAPIKey {
		t.Fatalf("override = %+v, want level=api_key", got.Body.Override)
	}
}

// TestPreviewUserPolicy_SendsBeforeWithoutOverrideAndAfterWithOverride is the
// core regression test for this bug: the preview endpoint must make two
// gateway calls (before with no override, after with the proposed policy as
// an override.level=user), and the response's "effective" must be the real
// gateway shape -- with "sources" and "reachable_models" present, exactly
// what the web drawer's `effective.sources.residency` access needs.
func TestPreviewUserPolicy_SendsBeforeWithoutOverrideAndAfterWithOverride(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)

	gw := newFakeCapturingGateway(t, gatewayEffectivePolicy{
		Residency: residencySelfHostedOnly, AllowedModels: strs("m1"),
		Sources:         map[string]string{"allowed_models": "user", "residency": "user", "default_model": "none"},
		ReachableModels: []string{"m1"},
	})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPost, "/api/v1/users/"+targetID+"/routing-policy/preview", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusOK {
		t.Fatalf("preview: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	reqs := gw.all()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 gateway calls (before, after), got %d: %+v", len(reqs), reqs)
	}
	before, after := reqs[0], reqs[1]
	if before.Method != http.MethodPost {
		t.Fatalf("expected the 'before' call to be POST (so it can also return the keys list), got %s", before.Method)
	}
	if before.Body.Override != nil {
		t.Errorf("expected the 'before' call to send no override, got %+v", before.Body.Override)
	}
	if before.Body.UserID != targetID || before.Body.APIKeyID != "" {
		t.Errorf("before call user_id/api_key_id = %q/%q, want %q/\"\"", before.Body.UserID, before.Body.APIKeyID, targetID)
	}
	if after.Body.Override == nil || after.Body.Override.Level != principalTypeUser {
		t.Fatalf("after call override = %+v, want level=user", after.Body.Override)
	}
	if after.Body.Override.Policy == nil || after.Body.Override.Policy.AllowedModels == nil || len(*after.Body.Override.Policy.AllowedModels) != 1 || (*after.Body.Override.Policy.AllowedModels)[0] != "m1" {
		t.Errorf("after call override.policy = %+v, want allowed_models=[m1]", after.Body.Override.Policy)
	}

	// The exact bug: assert the wire shape has sources + reachable_models on
	// "effective", not a local {residency, allowed_models, default_model}-only
	// approximation.
	var raw map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	eff, ok := raw["effective"].(map[string]any)
	if !ok {
		t.Fatalf("expected an effective object, got %+v", raw["effective"])
	}
	sources, ok := eff["sources"].(map[string]any)
	if !ok {
		t.Fatalf("effective.sources missing or not an object -- this is the exact TypeError the web drawer hit on effective.sources.residency; got %+v", eff)
	}
	if sources["residency"] != "user" {
		t.Errorf("effective.sources.residency = %v, want %q", sources["residency"], "user")
	}
	if _, ok := eff["reachable_models"]; !ok {
		t.Fatalf("effective.reachable_models missing, got %+v", eff)
	}
}

func TestPreviewAPIKeyPolicy_SendsOverrideWithAPIKeyLevelAndOwnerUserID(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	gw := newFakeCapturingGateway(t, gatewayEffectivePolicy{
		Residency: residencyAny, AllowedModels: strs("m1"),
		Sources:         map[string]string{"allowed_models": "api_key", "residency": "none", "default_model": "none"},
		ReachableModels: []string{"m1"},
	})
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	resp := doJSON(t, r, http.MethodPost, "/api/v1/api-keys/"+keyID+"/routing-policy/preview", policyObject{AllowedModels: strs("m1")})
	if resp.Code != http.StatusOK {
		t.Fatalf("preview: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	reqs := gw.all()
	if len(reqs) != 2 {
		t.Fatalf("expected exactly 2 gateway calls (before, after), got %d", len(reqs))
	}
	before, after := reqs[0], reqs[1]
	if before.Body.UserID != targetID || before.Body.APIKeyID != keyID {
		t.Errorf("before call user_id/api_key_id = %q/%q, want %q/%q", before.Body.UserID, before.Body.APIKeyID, targetID, keyID)
	}
	if after.Body.Override == nil || after.Body.Override.Level != principalTypeAPIKey {
		t.Fatalf("after call override = %+v, want level=api_key", after.Body.Override)
	}
	if after.Body.UserID != targetID || after.Body.APIKeyID != keyID {
		t.Errorf("after call user_id/api_key_id = %q/%q, want %q/%q", after.Body.UserID, after.Body.APIKeyID, targetID, keyID)
	}
}

// TestPutAndPreview_AgreeOnLockOut is the design's explicit requirement --
// "switch [the 409 lock-out check] to the gateway override call too so PUT
// and preview agree" -- using the real DB-backed fakePolicyGateway (not the
// capturing one above) so both endpoints are evaluated against the same
// merge decision.
func TestPutAndPreview_AgreeOnLockOut(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	adminID := newPolicyTestUser(t, ctx, pool)
	targetID := newPolicyTestUser(t, ctx, pool)
	insertModel(t, ctx, pool, "m1", backendClassSelfHosted, true)
	insertModel(t, ctx, pool, "m2", backendClassSelfHosted, true)
	keyID := newTestAPIKey(t, ctx, pool, targetID, "ci-key")

	gw := newFakePolicyGateway(t, pool)
	ph := NewPolicyHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	rh := NewRoutingHandler(pool, activity.NewRecorder(pool), gw.URL, "testkey")
	r := newPolicyTestRouter(ph, rh, adminID)

	if resp := doJSON(t, r, http.MethodPut, "/api/v1/api-keys/"+keyID+"/routing-policy", policyObject{AllowedModels: strs("m2")}); resp.Code != http.StatusOK {
		t.Fatalf("put key policy: status = %d, body = %s", resp.Code, resp.Body.String())
	}

	// Preview says this would lock the key out (m1 ∩ m2 = ∅).
	previewResp := doJSON(t, r, http.MethodPost, "/api/v1/users/"+targetID+"/routing-policy/preview", policyObject{AllowedModels: strs("m1")})
	if previewResp.Code != http.StatusOK {
		t.Fatalf("preview: status = %d, body = %s", previewResp.Code, previewResp.Body.String())
	}
	preview := decodeJSON[previewResponse](t, previewResp.Body.Bytes())
	if len(preview.WouldLockOut) == 0 {
		t.Fatalf("expected preview to report a lock-out, got %+v", preview)
	}

	// The actual PUT must 409 for the exact same reason.
	putResp := doJSON(t, r, http.MethodPut, "/api/v1/users/"+targetID+"/routing-policy", policyObject{AllowedModels: strs("m1")})
	if putResp.Code != http.StatusConflict {
		t.Fatalf("expected PUT to 409 in agreement with the preview, got %d; body = %s", putResp.Code, putResp.Body.String())
	}
}
