package main

// TestRouteInventoryMatchesGolden walks buildRouter's actual gin route table
// and diffs it, in both directions, against docs/api-routes.json's "api"
// entries — see DESIGN4's Parity section. This is the enforcement mechanism
// behind that file: a route added to cmd/main.go without a matching golden
// entry fails this test, and a stale golden entry for a route that no longer
// exists fails it too.
//
// Scope note: buildRouter's OIDC-enabled branch (GET /auth/login,
// GET /auth/callback) additively registers two auth routes that are
// mutually exclusive, at runtime, with local auth's POST /auth/setup (see
// cmd/main.go's comment on why setup is never exposed alongside OIDC). This
// test exercises BOTH branches — a second buildRouter call with a real
// OIDCProvider backed by a fake OIDC discovery server — and takes the union
// of both route tables, since docs/api-routes.json documents every route the
// api can ever serve, not just one deployment's configuration.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
	"github.com/scalecraft-dev/darkplum/api/internal/auth"
	"github.com/scalecraft-dev/darkplum/api/internal/billing"
	"github.com/scalecraft-dev/darkplum/api/internal/db"
	"github.com/scalecraft-dev/darkplum/api/internal/handler"
	"github.com/scalecraft-dev/darkplum/api/internal/kube"
)

// routeInventoryEntry mirrors one element of docs/api-routes.json.
type routeInventoryEntry struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Auth    string `json:"auth"`
	Surface string `json:"surface"`
	Reason  string `json:"reason,omitempty"`
}

func loadGoldenRoutes(t *testing.T) []routeInventoryEntry {
	t.Helper()
	// cmd/ -> api/ -> services/ -> darkplum/ -> docs/api-routes.json
	data, err := os.ReadFile("../../../docs/api-routes.json")
	if err != nil {
		t.Fatalf("read docs/api-routes.json: %v", err)
	}
	var entries []routeInventoryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("decode docs/api-routes.json: %v", err)
	}
	return entries
}

// routeInventoryTestDB creates a throwaway, uniquely-named schema against
// TEST_DATABASE_URL, mirroring internal/handler/testdb_test.go's
// setupTestDB (unexported there, so duplicated here for the cmd package).
func routeInventoryTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB integration test")
	}

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	schema := "test_routeinv_" + hex.EncodeToString(suffix)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA "%s"`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := db.RunMigrations(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(dropCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, schema))
		pool.Close()
	})
	return pool
}

// fakeOIDCDiscoveryServer serves just enough of the OIDC discovery/JWKS
// surface for auth.NewOIDCProvider to succeed, so the route-inventory test
// can exercise the OIDC-enabled route branch without a real IdP.
func fakeOIDCDiscoveryServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
			"userinfo_endpoint":      srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	})
	return srv
}

// buildRouteSet builds a router (buildRouter) and returns its route table as
// a set of "METHOD path" strings, using gin's own path syntax (":id") so it
// compares directly against the golden's path templates.
func buildRouteSet(t *testing.T, d routerDeps) map[string]bool {
	t.Helper()
	r := buildRouter(d)
	set := make(map[string]bool)
	for _, ri := range r.Routes() {
		set[ri.Method+" "+ri.Path] = true
	}
	return set
}

func TestRouteInventoryMatchesGolden(t *testing.T) {
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

	baseDeps := routerDeps{
		pool: pool, authCfg: authCfg, sessionMW: sessionMW, localProvider: localProvider,
		users: users, keys: keys, chat: chat, activityH: activityH, routing: routing,
		policy: policy, modelHandler: modelHandler, conversations: conversations, providers: providers, prices: prices, settings: handler.NewSettingsHandler(pool, activityRecorder),
		billing: handler.NewBillingHandler(pool, activityRecorder, billing.NewService(&billing.Store{DB: pool}, billing.KeyReader{}, nil, nil, nil), kube.NewWriter(), true),
	}

	// Branch 1: local auth only (oidcProvider nil) — the default install.
	actual := buildRouteSet(t, baseDeps)

	// Branch 2: OIDC enabled — additively registers GET /auth/login and
	// GET /auth/callback (and swaps POST /auth/setup out). See this test's
	// doc comment for why both are unioned into one actual set.
	oidcSrv := fakeOIDCDiscoveryServer(t)
	oidcCfg := &auth.Config{
		Secret:       authCfg.Secret,
		SecureCookie: true,
		OIDC: &auth.OIDCConfig{
			Issuer:       oidcSrv.URL,
			ClientID:     "test-client",
			ClientSecret: "test-secret",
			RedirectURL:  "http://localhost/auth/callback",
		},
	}
	oidcProvider, err := auth.NewOIDCProvider(context.Background(), oidcCfg, pool, activityRecorder)
	if err != nil {
		t.Fatalf("construct fake OIDC provider: %v", err)
	}
	oidcDeps := baseDeps
	oidcDeps.authCfg = oidcCfg
	oidcDeps.oidcProvider = oidcProvider
	for k := range buildRouteSet(t, oidcDeps) {
		actual[k] = true
	}

	golden := loadGoldenRoutes(t)
	wantAPI := make(map[string]routeInventoryEntry)
	for _, e := range golden {
		if e.Service != "api" {
			continue
		}
		key := e.Method + " " + normalizeGinPath(e.Path)
		wantAPI[key] = e
		if e.Surface != "ui" && e.Surface != "machine" {
			t.Errorf("golden entry %s %s has invalid surface %q", e.Method, e.Path, e.Surface)
		}
		if e.Surface == "machine" && e.Reason == "" {
			t.Errorf("golden entry %s %s is surface=machine but has no reason", e.Method, e.Path)
		}
	}

	var missingFromGolden, missingFromRouter []string
	for k := range actual {
		if _, ok := wantAPI[k]; !ok {
			missingFromGolden = append(missingFromGolden, k)
		}
	}
	for k := range wantAPI {
		if !actual[k] {
			missingFromRouter = append(missingFromRouter, k)
		}
	}
	sort.Strings(missingFromGolden)
	sort.Strings(missingFromRouter)

	if len(missingFromGolden) > 0 {
		t.Errorf("routes registered in cmd/main.go but missing from docs/api-routes.json: %v", missingFromGolden)
	}
	if len(missingFromRouter) > 0 {
		t.Errorf("routes in docs/api-routes.json (service=api) that cmd/main.go no longer registers: %v", missingFromRouter)
	}
}

// ginParamRe matches a gin path parameter segment (":name") so
// normalizeGinPath can pass golden paths through unchanged -- the golden
// file already uses gin's own ":id"/":alias" syntax, so this is currently an
// identity function, kept as a named step (rather than comparing raw
// strings inline) so a future golden format change has one place to adapt.
var ginParamRe = regexp.MustCompile(`:[A-Za-z_][A-Za-z0-9_]*`)

func normalizeGinPath(p string) string {
	return ginParamRe.ReplaceAllStringFunc(p, func(s string) string { return s })
}
