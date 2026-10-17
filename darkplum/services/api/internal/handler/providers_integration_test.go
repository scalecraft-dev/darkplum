package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/kube"
)

// fakeProviderGateway fakes the gateway's POST /v1/providers/:vendor/{verify,refresh}
// for ProvidersHandler's tests. verifyFunc, when set, overrides
// verifyStatus/verifyBody and receives the raw submitted request body -- so a
// test can echo the submitted key back in an error, proving providers.go
// never forwards gateway-supplied text into its own response (see
// translateVerifyError).
type fakeProviderGateway struct {
	*httptest.Server
	mu sync.Mutex

	verifyStatus int
	verifyBody   any
	verifyFunc   func(vendor string, body []byte) (status int, body2 any)
	verifyCalls  int

	refreshStatus int
	refreshCalls  int
}

func newFakeProviderGateway(t *testing.T) *fakeProviderGateway {
	t.Helper()
	f := &fakeProviderGateway{verifyStatus: http.StatusOK, refreshStatus: http.StatusAccepted}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/providers/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/providers/"), "/")
		if len(parts) != 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		vendor, action := parts[0], parts[1]
		body, _ := io.ReadAll(r.Body)

		switch action {
		case "verify":
			f.mu.Lock()
			f.verifyCalls++
			fn := f.verifyFunc
			status, respBody := f.verifyStatus, f.verifyBody
			f.mu.Unlock()
			if fn != nil {
				status, respBody = fn(vendor, body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(respBody)
		case "refresh":
			f.mu.Lock()
			f.refreshCalls++
			status := f.refreshStatus
			f.mu.Unlock()
			w.WriteHeader(status)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProviderGateway) getVerifyCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifyCalls
}

// fakeProviderKube fakes internal/kube.ProviderObjects for ProvidersHandler
// tests, with injectable errors so PutKey/PutFilter/DeleteKey's kube-error
// paths (503 mapping, hygiene) can be exercised without a real apiserver.
type fakeProviderKube struct {
	mu sync.Mutex

	available bool

	secrets map[string][]byte
	config  map[string]string

	setSecretErr    error
	removeSecretErr error
	setConfigErr    error

	setSecretCalls int
	setConfigCalls int
}

func (f *fakeProviderKube) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

func (f *fakeProviderKube) SetSecretKey(_ context.Context, name string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setSecretCalls++
	if f.setSecretErr != nil {
		return f.setSecretErr
	}
	if f.secrets == nil {
		f.secrets = map[string][]byte{}
	}
	f.secrets[name] = append([]byte{}, value...)
	return nil
}

func (f *fakeProviderKube) RemoveSecretKey(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeSecretErr != nil {
		return f.removeSecretErr
	}
	delete(f.secrets, name)
	return nil
}

func (f *fakeProviderKube) SetConfig(_ context.Context, kv map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setConfigCalls++
	if f.setConfigErr != nil {
		return f.setConfigErr
	}
	if f.config == nil {
		f.config = map[string]string{}
	}
	for k, v := range kv {
		f.config[k] = v
	}
	return nil
}

func newProvidersTestRouter(h *ProvidersHandler, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if userID != "" {
			c.Set("user_id", userID)
		}
		c.Next()
	})
	r.POST("/api/v1/providers/:vendor/verify", h.PostVerify)
	r.PUT("/api/v1/providers/:vendor/key", h.PutKey)
	r.PUT("/api/v1/providers/:vendor/filter", h.PutFilter)
	r.DELETE("/api/v1/providers/:vendor/key", h.DeleteKey)
	return r
}

// newReadyProvidersHandler wires a ProvidersHandler against real Postgres
// (pool), gw (defaults: verify 200 {}, refresh 202), and a fake kube writer
// that's Available() and reports the "ui_key_install" feature -- the
// baseline every test starts from, tweaked per test via the returned fakes.
func newReadyProvidersHandler(t *testing.T, pool *pgxpool.Pool, gw *fakeProviderGateway) (*ProvidersHandler, *fakeProviderKube, *ModelHandler) {
	t.Helper()
	fk := &fakeProviderKube{available: true}
	mh := NewModelHandler(gw.URL, "svc", pool)
	mh.providerFeatures = []string{providerFeatureUIKeyInstall}
	ph := NewProvidersHandler(pool, activity.NewRecorder(pool), gw.URL, "svc", fk, mh, true)
	return ph, fk, mh
}

func countActivityEventsContaining(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventType, needle string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activity_events WHERE event_type = $1 AND detail::text LIKE '%' || $2 || '%'`,
		eventType, needle,
	).Scan(&count); err != nil {
		t.Fatalf("count activity events: %v", err)
	}
	return count
}

// insertVendorModel inserts a models row with an explicit provider column --
// unlike routing_integration_test.go's insertModel (which hard-codes
// provider='vllm'), DeleteKey's models_enabled check keys off
// models.provider = :vendor, so these tests need the provider column to
// actually vary.
func insertVendorModel(t *testing.T, ctx context.Context, pool *pgxpool.Pool, modelID, provider string, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO models (model_id, provider, backend_class, enabled)
		VALUES ($1, $2, 'external_api', $3)
	`, modelID, provider, enabled); err != nil {
		t.Fatalf("insert vendor model %s: %v", modelID, err)
	}
}

func insertVendorSettings(t *testing.T, ctx context.Context, pool *pgxpool.Pool, vendor string, filter []string, filterSetAt time.Time, installingUntil *time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, installing_until)
		VALUES ($1, $2, $3, $4)
	`, vendor, filter, filterSetAt, installingUntil); err != nil {
		t.Fatalf("insert vendor_provider_settings: %v", err)
	}
}

// ---- PostVerify ----

func TestPostVerify_Success(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus", "claude-3-haiku"}, "admitted": []string{"claude-3-opus"}}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPost, "/api/v1/providers/anthropic/verify", map[string]any{
		"key": "sk-ant-abcdefghijklmnopqrstuvwx",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var got verifyResponseBody
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Reachable) != 2 || len(got.Admitted) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestPostVerify_KeyFormatRejectedBeforeNetworkCall(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPost, "/api/v1/providers/anthropic/verify", map[string]any{"key": "too-short"})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", resp.Code, resp.Body.String())
	}
	if gw.getVerifyCalls() != 0 {
		t.Fatalf("expected no gateway call for a malformed key, got %d", gw.getVerifyCalls())
	}
}

func TestPostVerify_UnknownVendor404s(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPost, "/api/v1/providers/bedrock/verify", map[string]any{})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Code)
	}
}

// ---- PutKey ----

func TestPutKey_InstallSuccess(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	userID := newTestUser(t, ctx, pool)
	r := newProvidersTestRouter(ph, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var got putKeyResponseBody
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Admitted) != 1 || got.Admitted[0] != "claude-3-opus" {
		t.Fatalf("admitted = %v", got.Admitted)
	}
	if got.InstallingUntil.Before(time.Now().Add(4*time.Minute)) || got.InstallingUntil.After(time.Now().Add(6*time.Minute)) {
		t.Fatalf("installing_until = %v, want ~5 minutes out", got.InstallingUntil)
	}

	if string(fk.secrets["anthropic-api-key"]) != "sk-ant-abcdefghijklmnopqrstuvwx" {
		t.Fatalf("secret not stored, got %v", fk.secrets)
	}
	if fk.config["ANTHROPIC_MODEL_FILTER"] != "claude-*" {
		t.Fatalf("configmap filter = %q", fk.config["ANTHROPIC_MODEL_FILTER"])
	}

	var filter []string
	if err := pool.QueryRow(ctx, `SELECT model_filter FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&filter); err != nil {
		t.Fatalf("read settings row: %v", err)
	}
	if len(filter) != 1 || filter[0] != "claude-*" {
		t.Fatalf("stored filter = %v", filter)
	}

	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderKeyInstalled), "anthropic"); n != 1 {
		t.Fatalf("expected exactly one provider.key_installed event, got %d", n)
	}
}

func TestPutKey_RotateWhenAlreadyConfigured(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, _, mh := newReadyProvidersHandler(t, pool, gw)
	mh.providers = []gatewayProviderStatus{{Name: "anthropic", Configured: true}}
	userID := newTestUser(t, ctx, pool)
	r := newProvidersTestRouter(ph, userID)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-rotatedkeyabcdefghijklmno",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderKeyRotated), "anthropic"); n != 1 {
		t.Fatalf("expected exactly one provider.key_rotated event, got %d", n)
	}
	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderKeyInstalled), "anthropic"); n != 0 {
		t.Fatalf("expected zero provider.key_installed events on a rotate, got %d", n)
	}
}

func TestPutKey_BareWildcardRefusedUnlessAllowAll(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"*"},
	})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s, want 422", resp.Code, resp.Body.String())
	}
	if gw.getVerifyCalls() != 0 {
		t.Fatalf("expected no gateway call before the bare-wildcard check, got %d calls", gw.getVerifyCalls())
	}
	if fk.setSecretCalls != 0 {
		t.Fatalf("expected no kube call before the bare-wildcard check, got %d calls", fk.setSecretCalls)
	}

	resp = doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"*"},
		"allow_all":    true,
	})
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	resp = doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"*"},
		"allow_all":    true,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("allow_all=true: status = %d, body = %s, want 200", resp.Code, resp.Body.String())
	}
}

func TestPutKey_GatewayTooOldRefused(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, fk, mh := newReadyProvidersHandler(t, pool, gw)
	mh.providerFeatures = nil // simulate an old gateway with no provider_features at all
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409", resp.Code, resp.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "gateway_too_old" {
		t.Fatalf("code = %v, want gateway_too_old", errObj["code"])
	}
	if gw.getVerifyCalls() != 0 || fk.setSecretCalls != 0 {
		t.Fatalf("expected no gateway/kube calls, got verify=%d secret=%d", gw.getVerifyCalls(), fk.setSecretCalls)
	}
}

func TestPutKey_KubeUnavailableRefused503(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	fk.available = false
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", resp.Code, resp.Body.String())
	}
	if gw.getVerifyCalls() != 0 {
		t.Fatalf("expected no gateway verify call when kube is unavailable, got %d", gw.getVerifyCalls())
	}
}

func TestPutKey_KubeForbidden_NothingStored(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	fk.setSecretErr = kube.ErrForbidden
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", resp.Code, resp.Body.String())
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&count); err != nil {
		t.Fatalf("count settings rows: %v", err)
	}
	if count != 0 {
		t.Fatal("expected no settings row when the Secret PATCH failed -- nothing should be stored")
	}
}

func TestPutKey_FilterMatchedNothing(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{}}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"gpt-*"},
	})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s, want 422", resp.Code, resp.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "filter_matched_nothing" {
		t.Fatalf("code = %v", errObj["code"])
	}
}

// installTriggerFailingVendorSettingsWrite installs a BEFORE INSERT OR
// UPDATE trigger on vendor_provider_settings that unconditionally raises,
// simulating the settings upsert specifically failing (both the initial
// attempt and its savepoint retry) while the rest of Postgres -- including
// the advisory lock this vendor's PutKey/PutFilter/DeleteKey acquire before
// ever reaching this table -- stays completely healthy. Scoped to the
// test's own throwaway schema (setupTestDB), so it can't affect anything
// else.
func installTriggerFailingVendorSettingsWrite(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_fail_vendor_settings_write() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'simulated vendor_provider_settings write failure';
		END;
		$$ LANGUAGE plpgsql;

		CREATE TRIGGER test_fail_vendor_settings_write_trigger
		BEFORE INSERT OR UPDATE ON vendor_provider_settings
		FOR EACH ROW EXECUTE FUNCTION test_fail_vendor_settings_write();
	`); err != nil {
		t.Fatalf("install failing-write trigger: %v", err)
	}
}

func TestPutKey_SettingsUpsertFailure_ReturnsPartiallyInstalled(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	// The Secret PATCH (fake kube, no DB involved) still succeeds; the
	// settings upsert (real DB, real advisory lock) fails on both its
	// attempt and its savepoint retry, via a trigger rather than taking the
	// whole database down (a total outage would also prevent acquiring the
	// per-vendor advisory lock in the first place, which is a different,
	// more severe failure mode than "this one write failed").
	installTriggerFailingVendorSettingsWrite(t, ctx, pool)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "partially_installed") {
		t.Fatalf("body = %s, want it to name partially_installed", resp.Body.String())
	}
	if fk.secrets["anthropic-api-key"] == nil {
		t.Fatal("expected the key to have been stored in the Secret despite the DB failure")
	}
	if !strings.Contains(resp.Body.String(), "no models are offered") {
		t.Fatalf("body = %s, want it to name the in-force filter (none cached here)", resp.Body.String())
	}
}

// TestPutKey_SettingsUpsertFailure_NamesThePreviouslyEffectiveFilter covers
// the other half of partiallyInstalledMessage: when the gateway's
// last-synced providers block DOES report an effective filter for this
// vendor, the 500 names it (not the filter this failed request tried to
// save).
func TestPutKey_SettingsUpsertFailure_NamesThePreviouslyEffectiveFilter(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus", "claude-3-haiku"}, "admitted": []string{"claude-3-opus", "claude-3-haiku"}}
	ph, fk, mh := newReadyProvidersHandler(t, pool, gw)
	mh.providers = []gatewayProviderStatus{{Name: "anthropic", Configured: true, Filter: []string{"claude-3-opus"}}}
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	installTriggerFailingVendorSettingsWrite(t, ctx, pool)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-rotatedkeyabcdefghijklmno",
		"model_filter": []string{"claude-3-opus", "claude-3-haiku"},
	})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "claude-3-opus") {
		t.Fatalf("body = %s, want it to name the previously effective filter claude-3-opus", resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "no models are offered") {
		t.Fatalf("body = %s, should not say no models are offered when a filter was already in force", resp.Body.String())
	}
	if fk.secrets["anthropic-api-key"] == nil {
		t.Fatal("expected the key to have been stored in the Secret despite the DB failure")
	}
}

// installTriggerFailingVendorSettingsCommit installs a DEFERRABLE INITIALLY
// DEFERRED constraint trigger on vendor_provider_settings, unlike
// installTriggerFailingVendorSettingsWrite's immediate BEFORE trigger: the
// INSERT/UPDATE statement itself succeeds, and only tx.Commit() fails, once
// the deferred check runs at commit time. Proves the item-2 fix: a Commit
// failure that happens AFTER the Secret PATCH already landed must be
// indistinguishable from a failed write, and must return
// partially_installed rather than falling back to the generic internal_error
// that would wrongly claim nothing changed.
func installTriggerFailingVendorSettingsCommit(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_fail_vendor_settings_commit() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'simulated commit-time failure';
		END;
		$$ LANGUAGE plpgsql;

		CREATE CONSTRAINT TRIGGER test_fail_vendor_settings_commit_trigger
		AFTER INSERT OR UPDATE ON vendor_provider_settings
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION test_fail_vendor_settings_commit();
	`); err != nil {
		t.Fatalf("install failing-commit trigger: %v", err)
	}
}

func TestPutKey_SettingsCommitFailure_ReturnsPartiallyInstalled(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	// The upsert statement itself succeeds; only the deferred constraint
	// check at Commit time fails.
	installTriggerFailingVendorSettingsCommit(t, ctx, pool)

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "partially_installed") {
		t.Fatalf("body = %s, want it to name partially_installed (the Secret PATCH already landed)", resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "\"internal_error\"") {
		t.Fatalf("body = %s, must not fall back to internal_error once the Secret PATCH has landed", resp.Body.String())
	}
	if fk.secrets["anthropic-api-key"] == nil {
		t.Fatal("expected the key to have been stored in the Secret despite the commit failure")
	}
}

func TestPutKey_MirrorFailureIsWarningNotFailure(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	fk.setConfigErr = fmt.Errorf("configmap write failed")
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 with warnings, not a failure", resp.Code, resp.Body.String())
	}
	var got putKeyResponseBody
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("expected a non-fatal warning about the mirror failure")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&count); err != nil {
		t.Fatalf("count settings rows: %v", err)
	}
	if count != 1 {
		t.Fatal("expected the settings row to still be written despite the mirror failure")
	}
}

// ---- Secret hygiene ----

func TestPutKey_SecretHygiene_GatewayErrorNeverEchoesSubmittedKey(t *testing.T) {
	const sentinel = "SENTINELKEYABCDEFGHIJKLMNOPQRSTUVWXYZ0"
	pool := setupTestDB(t)
	ctx := context.Background()
	logs := captureLogs(t)

	gw := newFakeProviderGateway(t)
	gw.verifyFunc = func(vendor string, body []byte) (int, any) {
		var req struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(body, &req)
		return http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "auth_failed", "message": "vendor rejected key: " + req.Key},
		}
	}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          sentinel,
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), sentinel) {
		t.Fatalf("response body leaked the submitted key: %s", resp.Body.String())
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Fatalf("logs leaked the submitted key: %s", logs.String())
	}
	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderKeyInstalled), sentinel); n != 0 {
		t.Fatal("activity_events leaked the submitted key")
	}
}

func TestPutKey_SecretHygiene_KubeErrorNeverLeaksSubmittedKeyOrBody(t *testing.T) {
	const sentinel = "SENTINELKUBEABCDEFGHIJKLMNOPQRSTUVWXY1"
	pool := setupTestDB(t)
	ctx := context.Background()
	logs := captureLogs(t)

	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	// Simulate a hypothetical buggy ProviderObjects implementation whose
	// error text embeds request content -- providers.go's kubeErrorJSON
	// must classify via errors.Is and never call err.Error() into a
	// response, so this must still not leak even though the fake here
	// deliberately misbehaves.
	fk.setSecretErr = fmt.Errorf("%w: request body contained %s", kube.ErrForbidden, sentinel)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          sentinel,
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), sentinel) {
		t.Fatalf("response body leaked: %s", resp.Body.String())
	}
	if strings.Contains(logs.String(), sentinel) {
		t.Fatalf("logs leaked: %s", logs.String())
	}
	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderKeyInstalled), sentinel); n != 0 {
		t.Fatal("activity_events leaked the submitted key")
	}
}

// ---- PutFilter ----

func TestPutFilter_Success(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertVendorSettings(t, ctx, pool, "anthropic", []string{"claude-3-*"}, time.Now().Add(-time.Hour), nil)

	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus", "claude-3-haiku"}, "admitted": []string{"claude-3-opus", "claude-3-haiku"}}
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{
		"model_filter": []string{"claude-3-opus", "claude-3-haiku"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if fk.config["ANTHROPIC_MODEL_FILTER"] != "claude-3-opus,claude-3-haiku" {
		t.Fatalf("configmap filter = %q", fk.config["ANTHROPIC_MODEL_FILTER"])
	}
	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderFilterChanged), "anthropic"); n != 1 {
		t.Fatalf("expected one provider.filter_changed event, got %d", n)
	}
}

func TestPutFilter_KeyRequiredWhenNothingInstalled(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"error": map[string]any{"code": "key_required", "message": "no key"}}
	gw.verifyStatus = http.StatusUnprocessableEntity
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s, want 422", resp.Code, resp.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "key_required" {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestPutFilter_DoesNotTouchInstallingUntil(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	until := time.Now().Add(3 * time.Minute).UTC().Truncate(time.Second)
	insertVendorSettings(t, ctx, pool, "anthropic", []string{"claude-3-*"}, time.Now().Add(-time.Hour), &until)

	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/filter", map[string]any{
		"model_filter": []string{"claude-3-opus"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var got *time.Time
	if err := pool.QueryRow(ctx, `SELECT installing_until FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&got); err != nil {
		t.Fatalf("read installing_until: %v", err)
	}
	if got == nil || !got.Equal(until) {
		t.Fatalf("installing_until = %v, want unchanged %v", got, until)
	}
}

// ---- DeleteKey ----

func TestDeleteKey_ModelsEnabledRefusedWithout409(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertVendorModel(t, ctx, pool, "claude-3-opus", "anthropic", true)

	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{"confirm": "anthropic"})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409", resp.Code, resp.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "models_enabled" {
		t.Fatalf("code = %v", errObj["code"])
	}
	models, _ := errObj["models"].([]any)
	if len(models) != 1 || models[0] != "claude-3-opus" {
		t.Fatalf("models = %v", errObj["models"])
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Fatal("expected a non-empty message alongside code and models")
	}
	if fk.setSecretCalls != 0 {
		t.Fatal("expected no kube call when refused for enabled models")
	}
}

func TestDeleteKey_DisableModelsSucceedsAndKeepsFilter(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertVendorModel(t, ctx, pool, "claude-3-opus", "anthropic", true)
	until := time.Now().Add(3 * time.Minute)
	insertVendorSettings(t, ctx, pool, "anthropic", []string{"claude-3-*"}, time.Now(), &until)

	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{
		"confirm":        "anthropic",
		"disable_models": true,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM models WHERE model_id = 'claude-3-opus'`).Scan(&enabled); err != nil {
		t.Fatalf("read model: %v", err)
	}
	if enabled {
		t.Fatal("expected the model to be disabled")
	}

	var filter []string
	var installingUntil *time.Time
	if err := pool.QueryRow(ctx, `SELECT model_filter, installing_until FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&filter, &installingUntil); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if len(filter) != 1 || filter[0] != "claude-3-*" {
		t.Fatalf("expected model_filter to be kept, got %v", filter)
	}
	if installingUntil != nil {
		t.Fatalf("expected installing_until to be cleared, got %v", installingUntil)
	}

	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderKeyRemoved), "claude-3-opus"); n != 1 {
		t.Fatalf("expected the removal event's detail.disabled_models to name claude-3-opus, got %d matching rows", n)
	}

	if _, ok := fk.secrets["anthropic-api-key"]; ok {
		t.Fatal("expected the secret key to have been removed")
	}
}

// TestDeleteKey_NoModelsEnabled_DetailOmitsDisabledModels covers the web
// engineer's exact contract: disabled_models is OMITTED (not an empty
// array) from the audit detail when nothing was disabled.
func TestDeleteKey_NoModelsEnabled_DetailOmitsDisabledModels(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{"confirm": "anthropic"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var detailJSON []byte
	if err := pool.QueryRow(ctx,
		`SELECT detail FROM activity_events WHERE event_type = $1 ORDER BY created_at DESC LIMIT 1`,
		string(activity.ProviderKeyRemoved),
	).Scan(&detailJSON); err != nil {
		t.Fatalf("read activity detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if _, ok := detail["disabled_models"]; ok {
		t.Fatalf("expected disabled_models to be omitted entirely, got %v", detail["disabled_models"])
	}
}

func TestDeleteKey_ConfirmMismatchRejected(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{"confirm": "openai"})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", resp.Code, resp.Body.String())
	}
	if fk.setSecretCalls != 0 {
		t.Fatal("expected no kube call on a confirm mismatch")
	}
}

func TestDeleteKey_KubeUnavailable503(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	fk.available = false
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{"confirm": "anthropic"})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", resp.Code, resp.Body.String())
	}
}

// ---- Per-vendor in-process lock (busy) ----

// TestAcquireVendorLock_ConcurrentRequestGetsBusy proves a second acquire
// for the SAME vendor is refused (non-blocking) while the first still holds
// the lock, and that a DIFFERENT vendor is never blocked by it. Unlike the
// old pg_try_advisory_xact_lock version, acquireVendorLock is a plain
// sync.Mutex.TryLock, so this needs no goroutines or channels to observe.
func TestAcquireVendorLock_ConcurrentRequestGetsBusy(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)

	releaseAnthropic, ok := ph.acquireVendorLock("anthropic")
	if !ok {
		t.Fatal("expected to acquire anthropic's lock")
	}

	if _, ok := ph.acquireVendorLock("anthropic"); ok {
		t.Fatal("expected a second acquire for the SAME vendor to be refused while the first is held")
	}

	// A different vendor's lock is entirely independent.
	releaseOpenAI, ok := ph.acquireVendorLock("openai")
	if !ok {
		t.Fatal("openai lock should not contend with anthropic's")
	}
	releaseOpenAI()

	releaseAnthropic()

	// Once released, anthropic's lock is available again.
	releaseAgain, ok := ph.acquireVendorLock("anthropic")
	if !ok {
		t.Fatal("expected the lock to be available again after release")
	}
	releaseAgain()
}

// TestPutKey_ConcurrentRequestForSameVendorGets409Busy_OtherVendorProceeds
// covers the same race through the actual PUT key HTTP handler and end to
// end through gatewayVerify: a first request is parked INSIDE the fake
// gateway's verify call (holding anthropic's lock the whole time, per
// acquireVendorLock's doc comment), a second request for the SAME vendor is
// refused 409 busy without ever reaching the gateway, and a concurrent
// request for a DIFFERENT vendor is not blocked by it at all and completes
// normally while the first is still parked.
func TestPutKey_ConcurrentRequestForSameVendorGets409Busy_OtherVendorProceeds(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	gw := newFakeProviderGateway(t)
	blockedInVerify := make(chan struct{})
	releaseVerify := make(chan struct{})
	gw.verifyFunc = func(vendor string, body []byte) (int, any) {
		if vendor == "anthropic" {
			close(blockedInVerify)
			<-releaseVerify
		}
		return http.StatusOK, map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
			"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
			"model_filter": []string{"claude-*"},
		})
	}()

	<-blockedInVerify // the first request now holds anthropic's lock, parked inside gatewayVerify

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusConflict {
		t.Fatalf("same vendor: status = %d, body = %s, want 409", resp.Code, resp.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "busy" {
		t.Fatalf("same vendor: code = %v, want busy", errObj["code"])
	}

	openaiResp := doJSON(t, r, http.MethodPut, "/api/v1/providers/openai/key", map[string]any{
		"key":          "sk-openai-abcdefghijklmnopqrstuv",
		"model_filter": []string{"gpt-*"},
	})
	if openaiResp.Code != http.StatusOK {
		t.Fatalf("other vendor: status = %d, body = %s, want 200 (must not be blocked by anthropic's lock)", openaiResp.Code, openaiResp.Body.String())
	}

	close(releaseVerify)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("first (parked) request: status = %d, body = %s, want 200", first.Code, first.Body.String())
	}
}

// TestPutKey_NoConnectionHeldDuringGatewayVerify is the pool-exhaustion
// regression test: while a request is parked inside gatewayVerify (an
// external HTTP call, not a database call), the pool must have ZERO
// acquired connections -- the entire point of moving from a
// pg_try_advisory_xact_lock held for a whole transaction to an in-process
// mutex (acquireVendorLock) plus short, independent transactions
// (withShortTx) is that no pooled connection is ever held across an
// external call.
func TestPutKey_NoConnectionHeldDuringGatewayVerify(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	blockedInVerify := make(chan struct{})
	releaseVerify := make(chan struct{})
	gw.verifyFunc = func(vendor string, body []byte) (int, any) {
		close(blockedInVerify)
		<-releaseVerify
		return http.StatusOK, map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
			"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
			"model_filter": []string{"claude-*"},
		})
	}()

	<-blockedInVerify
	if got := pool.Stat().AcquiredConns(); got != 0 {
		t.Fatalf("acquired connections while parked inside gatewayVerify = %d, want 0 -- a connection must never be held across an external call", got)
	}

	close(releaseVerify)
	resp := <-done
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", resp.Code, resp.Body.String())
	}
}

// ---- Insecure transport (PUT key / POST verify with a key) ----

func TestPutKey_InsecureTransportRefused403(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	ph.secureCookies = false // SECURE_COOKIES=false, i.e. the api itself is plain HTTP
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	// doJSON's httptest.NewRequest defaults Host to "example.com" -- not
	// loopback.
	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", resp.Code, resp.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "insecure_transport" {
		t.Fatalf("code = %v, want insecure_transport", errObj["code"])
	}
	if gw.getVerifyCalls() != 0 {
		t.Fatalf("expected no gateway call before the transport check, got %d", gw.getVerifyCalls())
	}
	if fk.setSecretCalls != 0 {
		t.Fatal("expected no kube call before the transport check")
	}
}

func TestPutKey_LoopbackHostAllowedEvenOverPlainHTTP(t *testing.T) {
	for _, host := range []string{"localhost:5173", "127.0.0.1:5173", "[::1]:5173", "localhost"} {
		t.Run(host, func(t *testing.T) {
			pool := setupTestDB(t)
			gw := newFakeProviderGateway(t)
			gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
			ph, _, _ := newReadyProvidersHandler(t, pool, gw)
			ph.secureCookies = false
			r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

			b, _ := json.Marshal(map[string]any{
				"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
				"model_filter": []string{"claude-*"},
			})
			req := httptest.NewRequest(http.MethodPut, "/api/v1/providers/anthropic/key", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Host = host
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("host %q: status = %d, body = %s, want 200", host, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPostVerify_InsecureTransportRefusedWhenKeyPresent(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	ph.secureCookies = false
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	resp := doJSON(t, r, http.MethodPost, "/api/v1/providers/anthropic/verify", map[string]any{
		"key": "sk-ant-abcdefghijklmnopqrstuvwx",
	})
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", resp.Code, resp.Body.String())
	}
	if gw.getVerifyCalls() != 0 {
		t.Fatalf("expected no gateway call before the transport check, got %d", gw.getVerifyCalls())
	}
}

func TestPostVerify_NoKeySkipsTransportCheck(t *testing.T) {
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	ph.secureCookies = false
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	// No key submitted (verify with the already-installed credential) -- no
	// credential crosses the wire, so the transport check must not apply,
	// even over plain HTTP to a non-loopback host.
	resp := doJSON(t, r, http.MethodPost, "/api/v1/providers/anthropic/verify", map[string]any{})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", resp.Code, resp.Body.String())
	}
}

// ---- DeleteKey: point-of-no-return ordering and never-fail-the-removal ----

// TestDeleteKey_KubeFailure_ModelsStayEnabledAndNoAudit is item 1's proving
// test: with disable_models=true and an enabled model present, a Secret
// PATCH failure must still refuse 503, leave the model enabled (disabling
// never runs -- the PATCH is the point of no return), and never record an
// audit row.
func TestDeleteKey_KubeFailure_ModelsStayEnabledAndNoAudit(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertVendorModel(t, ctx, pool, "claude-3-opus", "anthropic", true)

	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	fk.removeSecretErr = kube.ErrForbidden
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{
		"confirm":        "anthropic",
		"disable_models": true,
	})
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", resp.Code, resp.Body.String())
	}

	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM models WHERE model_id = 'claude-3-opus'`).Scan(&enabled); err != nil {
		t.Fatalf("read model: %v", err)
	}
	if !enabled {
		t.Fatal("expected the model to remain enabled -- the kube PATCH is the point of no return, disabling must never run before it succeeds")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM activity_events WHERE event_type = $1`, string(activity.ProviderKeyRemoved)).Scan(&count); err != nil {
		t.Fatalf("count activity events: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no provider.key_removed audit row, got %d", count)
	}
}

// TestDeleteKey_DisableModelsFailure_WarnsInsteadOfFailing covers "never a
// failed removal": the Secret PATCH succeeds, but disabling the
// now-orphaned models fails (even after its retry) -- the response is still
// 200, names the still-enabled models in a warning, and disabled_models is
// omitted (nothing was actually disabled).
func TestDeleteKey_DisableModelsFailure_WarnsInsteadOfFailing(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	insertVendorModel(t, ctx, pool, "boom-model", "anthropic", true)

	// A trigger that fails only the UPDATE...enabled=false path, so the
	// initial SELECT (checking which models are enabled) still succeeds.
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_fail_model_disable() RETURNS trigger AS $$
		BEGIN
			IF NEW.enabled = false THEN
				RAISE EXCEPTION 'simulated disable failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		CREATE TRIGGER test_fail_model_disable_trigger
		BEFORE UPDATE ON models
		FOR EACH ROW EXECUTE FUNCTION test_fail_model_disable();
	`); err != nil {
		t.Fatalf("install failing-disable trigger: %v", err)
	}

	gw := newFakeProviderGateway(t)
	ph, fk, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{
		"confirm":        "anthropic",
		"disable_models": true,
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (never a failed removal)", resp.Code, resp.Body.String())
	}
	if _, ok := fk.secrets["anthropic-api-key"]; ok {
		t.Fatal("expected the secret key to have been removed regardless of the disable failure")
	}

	var got deleteKeyResponseBody
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.DisabledModels) != 0 {
		t.Fatalf("expected disabled_models to be empty (nothing was actually disabled), got %v", got.DisabledModels)
	}
	found := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "boom-model") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning naming boom-model as still enabled, got %v", got.Warnings)
	}

	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM models WHERE model_id = 'boom-model'`).Scan(&enabled); err != nil {
		t.Fatalf("read model: %v", err)
	}
	if !enabled {
		t.Fatal("expected boom-model to still be enabled since disabling it failed")
	}

	// The removal itself still succeeded and was audited.
	if n := countActivityEventsContaining(t, ctx, pool, string(activity.ProviderKeyRemoved), "anthropic"); n != 1 {
		t.Fatalf("expected exactly one provider.key_removed event, got %d", n)
	}
}

// ---- DeleteKey: removing_until (item 2) ----

func TestDeleteKey_SetsRemovingUntilAndClearsInstallingUntil(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	until := time.Now().Add(3 * time.Minute)
	insertVendorSettings(t, ctx, pool, "anthropic", []string{"claude-*"}, time.Now(), &until)

	gw := newFakeProviderGateway(t)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodDelete, "/api/v1/providers/anthropic/key", map[string]any{"confirm": "anthropic"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var installingUntil, removingUntil *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT installing_until, removing_until FROM vendor_provider_settings WHERE vendor = 'anthropic'`,
	).Scan(&installingUntil, &removingUntil); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if installingUntil != nil {
		t.Fatalf("expected installing_until cleared, got %v", installingUntil)
	}
	if removingUntil == nil {
		t.Fatal("expected removing_until to be set")
	}
	if removingUntil.Before(time.Now().Add(4*time.Minute)) || removingUntil.After(time.Now().Add(6*time.Minute)) {
		t.Fatalf("removing_until = %v, want ~5 minutes out", removingUntil)
	}
}

func TestPutKey_ClearsRemovingUntil(t *testing.T) {
	pool := setupTestDB(t)
	ctx := context.Background()
	removingUntil := time.Now().Add(3 * time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, removing_until)
		VALUES ('anthropic', $1, now(), $2)
	`, []string{"claude-*"}, removingUntil); err != nil {
		t.Fatalf("seed removing_until: %v", err)
	}

	gw := newFakeProviderGateway(t)
	gw.verifyBody = map[string]any{"reachable": []string{"claude-3-opus"}, "admitted": []string{"claude-3-opus"}}
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, ctx, pool))

	resp := doJSON(t, r, http.MethodPut, "/api/v1/providers/anthropic/key", map[string]any{
		"key":          "sk-ant-abcdefghijklmnopqrstuvwx",
		"model_filter": []string{"claude-*"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var got *time.Time
	if err := pool.QueryRow(ctx, `SELECT removing_until FROM vendor_provider_settings WHERE vendor = 'anthropic'`).Scan(&got); err != nil {
		t.Fatalf("read removing_until: %v", err)
	}
	if got != nil {
		t.Fatalf("expected removing_until cleared by a reinstall, got %v", got)
	}
}

// ---- Malformed JSON body never echoes a byte of it (item 7) ----

// TestInvalidJSON_NeverEchoesSentinelKey covers all four
// c.ShouldBindJSON error paths: a request body that IS a raw, bare pasted
// vendor key (not wrapped in JSON at all) draws a json.SyntaxError from
// encoding/json whose own message quotes the offending character -- the
// first character of the sentinel here. The response must carry the fixed
// invalidJSONMessage instead, never a character of the submitted body.
func TestInvalidJSON_NeverEchoesSentinelKey(t *testing.T) {
	const sentinel = "SENTINELRAWBODYKEYABCDEFGHIJKLMNOPQRSTUVWXYZ"
	pool := setupTestDB(t)
	gw := newFakeProviderGateway(t)
	ph, _, _ := newReadyProvidersHandler(t, pool, gw)
	r := newProvidersTestRouter(ph, newTestUser(t, context.Background(), pool))

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/providers/anthropic/verify"},
		{http.MethodPut, "/api/v1/providers/anthropic/key"},
		{http.MethodPut, "/api/v1/providers/anthropic/filter"},
		{http.MethodDelete, "/api/v1/providers/anthropic/key"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(sentinel))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, body = %s, want 400", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), sentinel[:1]) && strings.Contains(rec.Body.String(), "character") {
			t.Errorf("%s %s: body echoed a character of the raw body: %s", tc.method, tc.path, rec.Body.String())
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		errObj, _ := body["error"].(map[string]any)
		if msg, _ := errObj["message"].(string); msg != invalidJSONMessage {
			t.Errorf("%s %s: message = %q, want the fixed invalidJSONMessage", tc.method, tc.path, msg)
		}
		if strings.Contains(rec.Body.String(), sentinel) {
			t.Errorf("%s %s: body leaked the sentinel outright: %s", tc.method, tc.path, rec.Body.String())
		}
	}
}
