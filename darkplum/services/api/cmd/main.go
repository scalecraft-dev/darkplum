package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/auth"
	"github.com/scalecraft/darkplum/api/internal/billing"
	"github.com/scalecraft/darkplum/api/internal/db"
	"github.com/scalecraft/darkplum/api/internal/handler"
	"github.com/scalecraft/darkplum/api/internal/kube"
	"github.com/scalecraft/darkplum/api/internal/pricing"
)

func main() {
	// Structured JSON logging — log level from env, default info.
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})))

	// Operator subcommands (run by the darkplum CLI as one-shot Jobs) branch
	// before the server boots and do NOT initialize OIDC, so they work even when
	// SSO config is broken and run() would fail closed.
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := runAdmin(os.Args[2:]); err != nil {
			slog.Error("admin command failed", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runAdmin implements `darkplum-api admin create --email <e> --name <n>` with
// the password read from stdin only (never an argv flag).
func runAdmin(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: darkplum-api admin create --email <email> --name <name>  (password on stdin)")
	}
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	email := fs.String("email", "", "admin email address")
	name := fs.String("name", "", "admin display name")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *email == "" || *name == "" {
		return errors.New("--email and --name are required")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	// The bootstrap Job injects the password via ADMIN_PASSWORD (from a
	// short-lived Secret); fall back to stdin for interactive/manual use. The
	// password is never an argv flag.
	password := os.Getenv("ADMIN_PASSWORD")
	if password == "" {
		pwBytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read password from stdin: %w", err)
		}
		password = strings.TrimRight(string(pwBytes), "\r\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	if err := db.RunMigrations(ctx, pool); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	userID, err := auth.CreateLocalAdmin(ctx, pool, activity.NewRecorder(pool), *email, *name, password)
	if err != nil {
		return err
	}
	slog.Info("local admin created", "id", userID, "email", *email)
	fmt.Printf("created admin %s (id %s)\n", *email, userID)
	return nil
}

func run() error {
	port := envOr("WEB_PORT", "8080")
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	gatewayURL := envOr("GATEWAY_URL", "http://darkplum-gateway.darkplum-system.svc.cluster.local:8080")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := db.NewPool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	if err := db.RunMigrations(ctx, pool); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	if err := db.EnsurePartitions(ctx, pool); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}

	// Auth configuration must load before BootstrapServiceKey — the service
	// key is derived from a secret (SERVICE_KEY_SECRET if set, else
	// SESSION_SIGNING_KEY) rather than randomly generated, so the secret must
	// be in hand first.
	authCfg, err := auth.LoadConfig()
	if err != nil {
		return fmt.Errorf("load auth config: %w", err)
	}

	serviceKeySecret, serviceKeySecretSource, err := db.SelectServiceKeySecret(
		os.Getenv("SERVICE_KEY_SECRET"), authCfg.Secret,
	)
	if err != nil {
		return fmt.Errorf("select service key secret: %w", err)
	}
	if serviceKeySecretSource == db.ServiceKeySecretSourceFallback {
		slog.Info("service key secret: SERVICE_KEY_SECRET not set, falling back to SESSION_SIGNING_KEY " +
			"(set SERVICE_KEY_SECRET to rotate the gateway credential independently of user sessions)")
	} else {
		slog.Info("service key secret: using SERVICE_KEY_SECRET")
	}

	serviceKey, err := db.BootstrapServiceKey(ctx, pool, serviceKeySecret)
	if err != nil {
		return fmt.Errorf("bootstrap service key: %w", err)
	}

	activityRecorder := activity.NewRecorder(pool)

	// Model sync — run once at startup, then on a periodic ticker. This is
	// constructed early so SyncModels can run before the HTTP server accepts
	// traffic. The handler is also registered on the router below.
	modelSyncInterval := modelSyncIntervalFromEnv()
	modelHandler := handler.NewModelHandler(gatewayURL, serviceKey, pool)
	modelHandler.SetAvailabilityWindow(modelAvailabilityWindowFromEnv())
	modelHandler.SetSecureCookies(authCfg.SecureCookie)
	go func() {
		delay := 2 * time.Second
		for i := 0; i < 20; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			modelHandler.SyncModels(ctx)
			var count int
			_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM models").Scan(&count)
			if count > 0 {
				slog.Info("model sync: initial sync complete", "models", count)
				break
			}
			slog.Info("model sync: gateway not ready, retrying", "attempt", i+1, "next_delay", delay*2)
			if delay < 30*time.Second {
				delay *= 2
			}
		}

		runModelSyncTicker(ctx, modelHandler, modelSyncInterval)
	}()

	// Local password auth is always available. OIDC is additive when configured.
	localProvider := auth.NewLocalProvider(authCfg, pool, activityRecorder)
	sessionMW := auth.SessionRequired(pool, authCfg.Secret, authCfg.SecureCookie)
	slog.Info("local authentication enabled")

	var oidcProvider *auth.OIDCProvider
	if authCfg.OIDC != nil {
		oidcProvider, err = auth.NewOIDCProvider(ctx, authCfg, pool, activityRecorder)
		if err != nil {
			return fmt.Errorf("init OIDC provider: %w", err)
		}
		slog.Info("OIDC authentication enabled")
	}

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				auth.CleanupExpiredSessions(context.Background(), pool)
			}
		}
	}()

	// Handlers.
	users := handler.NewUserHandler(pool, activityRecorder, authCfg.Pepper)
	keys := handler.NewAPIKeyHandler(pool, activityRecorder)
	chat := handler.NewChatHandler(gatewayURL, serviceKey)
	activityH := handler.NewActivityHandler(pool)
	routing := handler.NewRoutingHandler(pool, activityRecorder, gatewayURL, serviceKey)
	routing.SetAvailabilityWindow(modelAvailabilityWindowFromEnv())
	policy := handler.NewPolicyHandler(pool, activityRecorder, gatewayURL, serviceKey)
	conversations := handler.NewConversationHandler(pool, activityRecorder)
	providers := handler.NewProvidersHandler(pool, activityRecorder, gatewayURL, serviceKey, kube.NewWriter(), modelHandler, authCfg.SecureCookie)

	prices := handler.NewPricingHandler(pool, activityRecorder)

	// Vendor usage/cost reconciliation (separate billing keys; see
	// internal/billing). The poller starts after the price seed below.
	billingSvc := billing.NewService(&billing.Store{DB: pool},
		billing.KeyReader{Dir: envOr("BILLING_KEYS_DIR", "/var/run/darkplum/billing-keys")},
		map[string]billing.Vendor{
			billing.Anthropic: billing.NewAnthropicClient(billing.AnthropicBaseURL, nil),
			billing.OpenAI:    billing.NewOpenAIClient(billing.OpenAIBaseURL, nil),
		}, nil, nil)
	billingH := handler.NewBillingHandler(pool, activityRecorder, billingSvc, kube.NewWriter(), authCfg.SecureCookie)
	if n, err := pricing.Seed(ctx, pool); err != nil {
		slog.Error("seed model prices failed; spend will be unpriced until fixed", "err", err)
	} else if n > 0 {
		slog.Info("seeded model prices", "rows", n)
	}

	go func() {
		// Bedrock spend comes from Cost Explorer via the default credential
		// chain; failing to build the client just leaves Bedrock unconfigured.
		if src, err := billing.NewAWSBedrockSource(ctx); err != nil {
			slog.Warn("billing: AWS config unavailable; Bedrock reconciliation disabled")
		} else {
			billingSvc.Bedrock = src
		}
		billingSvc.Run(ctx)
	}()

	r := buildRouter(routerDeps{
		pool:          pool,
		authCfg:       authCfg,
		sessionMW:     sessionMW,
		localProvider: localProvider,
		oidcProvider:  oidcProvider,
		users:         users,
		keys:          keys,
		chat:          chat,
		activityH:     activityH,
		routing:       routing,
		policy:        policy,
		modelHandler:  modelHandler,
		conversations: conversations,
		providers:     providers,
		prices:        prices,
		billing:       billingH,
	})

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server starting", "port", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	return <-errCh
}

// routerDeps is every handler/config buildRouter needs to register routes.
// Pulled out of run() so cmd's route-inventory test (main_test.go) can build
// the exact same route table run() serves, against a throwaway test
// database, and diff it against docs/api-routes.json without booting a full
// server or a real gateway.
type routerDeps struct {
	pool          *pgxpool.Pool
	authCfg       *auth.Config
	sessionMW     gin.HandlerFunc
	localProvider *auth.LocalProvider
	oidcProvider  *auth.OIDCProvider
	users         *handler.UserHandler
	keys          *handler.APIKeyHandler
	chat          *handler.ChatHandler
	activityH     *handler.ActivityHandler
	routing       *handler.RoutingHandler
	policy        *handler.PolicyHandler
	modelHandler  *handler.ModelHandler
	conversations *handler.ConversationHandler
	providers     *handler.ProvidersHandler
	prices        *handler.PricingHandler
	billing       *handler.BillingHandler
}

// buildRouter registers every route the api serves. See routerDeps' doc
// comment for why this is a standalone function rather than inlined in run().
func buildRouter(d routerDeps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestID())
	r.Use(requestLogger())
	r.Use(corsMiddleware())
	// Registered before csrfProtection (and before session auth): a Bearer
	// credential on a vendor-key route must be refused outright, not merely
	// left to fall through to session auth, because Bearer requests skip
	// csrfProtection's X-Requested-With check below (see that function) --
	// letting a Bearer-carrying request reach these routes at all would
	// reopen the CSRF hole PLAN-UI's security ruling closed.
	r.Use(rejectBearerOnProviderRoutes())
	r.Use(csrfProtection())

	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Auth config — always available so the frontend can detect auth mode.
	r.GET("/auth/config", func(c *gin.Context) {
		resp := gin.H{
			"local": true,
			"oidc":  gin.H{"enabled": d.authCfg.OIDC != nil},
		}
		var count int
		if err := d.pool.QueryRow(c.Request.Context(),
			`SELECT COUNT(*) FROM users WHERE password_hash IS NOT NULL`).Scan(&count); err == nil {
			resp["setup_required"] = count == 0
		}
		c.JSON(http.StatusOK, resp)
	})

	// Auth routes. Local password auth (POST /login) is always registered; OIDC
	// (GET /login redirect + callback) is additive. /me and /logout are
	// provider-agnostic via the shared session layer.
	loginRL := auth.LoginRateLimit(0.2, 5) // 1 attempt / 5s sustained, burst 5
	authGroup := r.Group("/auth")
	authGroup.POST("/login", loginRL, d.localProvider.Login)
	authGroup.POST("/change-password", d.localProvider.ChangePassword)
	// First-run admin creation is only reachable before OIDC is configured; an
	// OIDC deployment provisions its first admin via SSO + a role grant, so the
	// unauthenticated setup endpoint is never exposed alongside OIDC.
	if d.oidcProvider == nil {
		authGroup.POST("/setup", loginRL, d.localProvider.Setup)
	}
	authGroup.GET("/me", d.localProvider.HandleMe)
	if d.oidcProvider != nil {
		authGroup.GET("/login", loginRL, d.oidcProvider.Login)
		authGroup.GET("/callback", loginRL, d.oidcProvider.Callback)
		authGroup.POST("/logout", d.oidcProvider.Logout)
	} else {
		authGroup.POST("/logout", d.localProvider.Logout)
	}

	// API routes.
	v1 := r.Group("/api/v1")
	v1.Use(d.sessionMW)
	{
		v1.GET("/users", d.users.ListUsers)
		v1.GET("/api-keys", d.keys.ListAPIKeys)

		// Conversation history: session-authenticated but explicitly NOT
		// admin-only -- see the privacy comment atop
		// internal/handler/conversations.go. Every handler scopes by the
		// session user_id itself.
		v1.GET("/conversations", d.conversations.ListConversations)
		v1.POST("/conversations", d.conversations.CreateConversation)
		v1.GET("/conversations/:id", d.conversations.GetConversation)
		v1.PATCH("/conversations/:id", d.conversations.PatchConversation)
		v1.POST("/conversations/:id/messages", d.conversations.AppendMessages)
		v1.DELETE("/conversations/:id", d.conversations.DeleteConversation)
		v1.DELETE("/conversations/:id/messages", d.conversations.TruncateMessages)

		// Any session user's own effective routing policy — no "stored" field
		// (that's admin-only, via the equivalent .../routing-policy/effective
		// endpoints below), just what they can actually reach right now.
		v1.GET("/me/routing-policy/effective", d.policy.GetMyPolicyEffective)

		adminOnly := v1.Group("")
		adminOnly.Use(auth.AdminRequired())
		{
			adminOnly.POST("/users", d.users.CreateUser)
			adminOnly.DELETE("/users/:id", d.users.DeleteUser)
			adminOnly.PATCH("/users/:id/role", d.users.UpdateRole)
			adminOnly.PATCH("/users/:id/password", d.users.ResetPassword)
			adminOnly.GET("/users/:id/routing-policy", d.policy.GetUserPolicy)
			adminOnly.PUT("/users/:id/routing-policy", d.policy.PutUserPolicy)
			adminOnly.DELETE("/users/:id/routing-policy", d.policy.DeleteUserPolicy)
			adminOnly.GET("/users/:id/routing-policy/effective", d.policy.GetUserPolicyEffective)
			adminOnly.POST("/users/:id/routing-policy/preview", d.policy.PreviewUserPolicy)

			adminOnly.POST("/api-keys", d.keys.CreateAPIKey)
			adminOnly.DELETE("/api-keys/:id", d.keys.RevokeAPIKey)
			adminOnly.GET("/api-keys/:id/routing-policy", d.policy.GetAPIKeyPolicy)
			adminOnly.PUT("/api-keys/:id/routing-policy", d.policy.PutAPIKeyPolicy)
			adminOnly.DELETE("/api-keys/:id/routing-policy", d.policy.DeleteAPIKeyPolicy)
			adminOnly.GET("/api-keys/:id/routing-policy/effective", d.policy.GetAPIKeyPolicyEffective)
			adminOnly.POST("/api-keys/:id/routing-policy/preview", d.policy.PreviewAPIKeyPolicy)

			adminOnly.GET("/models", d.modelHandler.ListAdminModels)
			adminOnly.PATCH("/models/:id", d.modelHandler.ToggleModel)

			adminOnly.GET("/model-aliases", d.routing.ListAliases)
			adminOnly.PUT("/model-aliases/:alias", d.routing.PutAlias)
			adminOnly.DELETE("/model-aliases/:alias", d.routing.DeleteAlias)
			adminOnly.POST("/model-aliases/:alias/test", d.routing.TestAlias)

			adminOnly.GET("/routing-policy", d.routing.GetRoutingPolicy)
			adminOnly.PUT("/routing-policy", d.routing.PutRoutingPolicy)
			adminOnly.GET("/routing-policy/principals", d.policy.ListPrincipalPolicies)

			adminOnly.GET("/activity", d.activityH.ListActivity)
			adminOnly.GET("/activity/export", d.activityH.ExportActivity)
			adminOnly.GET("/usage", d.activityH.ListUsage)
			adminOnly.GET("/usage/export", d.activityH.ExportUsage)
			adminOnly.GET("/usage/stats", d.activityH.UsageStats)
			adminOnly.GET("/usage/summary", d.activityH.UsageSummary)
			adminOnly.GET("/usage/user-summary", d.activityH.UserSummary)
			adminOnly.GET("/usage/key-summary", d.activityH.KeySummary)
			adminOnly.GET("/usage/spend", d.activityH.UsageSpend)

			// Versioned price book behind the spend estimates. PUT with an id
			// closes the old row and inserts a new one; DELETE retires.
			adminOnly.GET("/model-prices", d.prices.ListPrices)
			adminOnly.PUT("/model-prices", d.prices.PutPrice)
			adminOnly.DELETE("/model-prices/:id", d.prices.DeletePrice)

			// Vendor key install/rotate/remove (PLAN-UI). Rate-limited on
			// every route that calls out to the vendor through the gateway's
			// verify (verify itself, install/rotate, and change-models,
			// which all invoke the same gateway verify path): 5/min per
			// acting admin, PLUS a process-wide cap of 20/hr (see
			// providerProcessRL below), so a mistyped key or filter retried
			// in a loop can't hammer the vendor's own rate limits. DELETE
			// never calls the vendor (it only removes the Secret key) and is
			// typed-confirmation-gated instead.
			providerActorRL := auth.KeyedRateLimit(5.0/60.0, 5, func(c *gin.Context) string { return c.GetString("user_id") })
			// providerProcessRL is keyed by the constant "org" -- despite the
			// name, this is NOT an org-scoped limit (there is no org_id
			// anywhere in this request path to key on): it is one shared
			// bucket for the entire api PROCESS, deliberately, and only
			// behaves like a whole-deployment limit because this handler
			// assumes replicas: 1 (see k8s/base/api/deployment.yaml). Running
			// more than one api replica would give each pod its own
			// independent 20/hr bucket -- i.e. an effective org-wide limit of
			// 20*N/hr -- since auth.KeyedRateLimit's state is in-process, not
			// shared (e.g. via Postgres or Redis). Revisit this the day the
			// api deployment ever scales beyond one replica.
			providerProcessRL := auth.KeyedRateLimit(20.0/3600.0, 20, func(c *gin.Context) string { return "org" })
			adminOnly.POST("/providers/:vendor/verify", providerActorRL, providerProcessRL, d.providers.PostVerify)
			adminOnly.PUT("/providers/:vendor/key", providerActorRL, providerProcessRL, d.providers.PutKey)
			adminOnly.PUT("/providers/:vendor/filter", providerActorRL, providerProcessRL, d.providers.PutFilter)
			adminOnly.DELETE("/providers/:vendor/key", d.providers.DeleteKey)

			// Vendor billing (admin-scope) keys for usage/cost reconciliation:
			// same limiters as the inference-key routes (verify calls the
			// vendor); DELETE only edits the Secret.
			adminOnly.POST("/providers/:vendor/billing-key/verify", providerActorRL, providerProcessRL, d.billing.PostVerify)
			adminOnly.PUT("/providers/:vendor/billing-key", providerActorRL, providerProcessRL, d.billing.PutKey)
			adminOnly.DELETE("/providers/:vendor/billing-key", d.billing.DeleteKey)
			adminOnly.GET("/usage/reconciliation", d.billing.GetReconciliation)
			// 1/min process-wide: vendors ask for polling no faster than that.
			adminOnly.POST("/usage/reconciliation/refresh",
				auth.KeyedRateLimit(1.0/60.0, 1, func(c *gin.Context) string { return "org" }), d.billing.PostRefresh)
		}
	}

	// Chat and model routes accept either session auth (browser) or Bearer
	// API key (curl, Cursor, SDK clients). When a Bearer token is present the
	// request is forwarded to the gateway which validates the key — we skip
	// session auth so API-key callers don't need a cookie.
	apiOrSession := func(sessionMW gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			if strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") {
				// Bearer (API key) requests are forwarded to the gateway, which
				// validates the key. The API layer does not re-validate it here.
				c.Next()
				return
			}
			// No Bearer token — a valid browser session is required. There is no
			// unauthenticated fallback for session callers.
			sessionMW(c)
		}
	}
	r.GET("/v1/models", apiOrSession(d.sessionMW), d.modelHandler.ListModels)
	r.POST("/v1/chat/completions", apiOrSession(d.sessionMW), d.chat.ProxyChat)

	return r
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// modelSyncMinInterval is the floor for MODEL_SYNC_INTERVAL_SECONDS: below
// this, polling the gateway's /v1/models on every tick stops being a
// meaningful cost/freshness trade-off and starts being a self-inflicted load
// problem.
const modelSyncMinInterval = 10 * time.Second

// modelSyncDefaultInterval matches the gateway's default discovery interval
// (DISCOVERY_INTERVAL_SECONDS=60) so the admin UI doesn't lag behind what the
// gateway itself already knows.
const modelSyncDefaultInterval = 60 * time.Second

// providerInstallSyncInterval is the sync loop's tick interval while any
// vendor has a live installing_until (PLAN-UI v2.1) -- fast enough that the
// admin's "Installing" progress card and admitted-models list reflect the
// gateway's post-install state within a few ticks, not a full
// MODEL_SYNC_INTERVAL_SECONDS cycle.
const providerInstallSyncInterval = 5 * time.Second

// modelSyncIntervalFromEnv reads MODEL_SYNC_INTERVAL_SECONDS from the
// environment and applies parseModelSyncInterval.
func modelSyncIntervalFromEnv() time.Duration {
	return parseModelSyncInterval(os.Getenv("MODEL_SYNC_INTERVAL_SECONDS"))
}

// runModelSyncTicker runs the periodic model sync loop: SyncModels on every
// tick of syncInterval, dropping to providerInstallSyncInterval while
// modelHandler.AnyInstalling() is true (PLAN-UI v2.1) and reverting once it
// isn't -- plus an immediate, out-of-band SyncModels whenever
// modelHandler.Kick() fires (see ModelHandler.KickCh's doc comment).
//
// The Kick case exists because otherwise the loop only notices
// AnyInstalling() went true on its own next natural tick: with the default
// 60s syncInterval, a vendor key install right after a tick fired could sit
// up to ~60s before the loop ever ran a sync to pick up installing_until and
// switch to the fast cadence, even though providers.go's own
// syncModelsBounded already ran ONE immediate sync for that specific
// request. Kick closes that gap for every subsequent tick, not just the
// first one.
//
// Blocks until ctx is done, then returns -- pulled out of run() as its own
// function so it's callable directly from a test with a short-lived ctx and
// a fake gateway, without booting the whole server.
func runModelSyncTicker(ctx context.Context, modelHandler *handler.ModelHandler, syncInterval time.Duration) {
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	currentInterval := syncInterval

	sync := func() {
		modelHandler.SyncModels(ctx)
		want := syncInterval
		if modelHandler.AnyInstalling() {
			want = providerInstallSyncInterval
		}
		if want != currentInterval {
			ticker.Reset(want)
			currentInterval = want
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sync()
		case <-modelHandler.KickCh():
			sync()
		}
	}
}

// parseModelSyncInterval defaults to modelSyncDefaultInterval and enforces
// modelSyncMinInterval as a floor. An empty or unparseable value falls back
// to the default rather than failing startup — this is a polling cadence,
// not a required secret. Pulled out as a pure function (raw string in,
// time.Duration out) so the default/min/parse-failure paths are unit-
// testable without touching the environment.
func parseModelSyncInterval(raw string) time.Duration {
	if raw == "" {
		return modelSyncDefaultInterval
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("MODEL_SYNC_INTERVAL_SECONDS is not a valid integer, using default",
			"value", raw, "default_seconds", int(modelSyncDefaultInterval.Seconds()))
		return modelSyncDefaultInterval
	}
	interval := time.Duration(secs) * time.Second
	if interval < modelSyncMinInterval {
		slog.Warn("MODEL_SYNC_INTERVAL_SECONDS is below the minimum, clamping",
			"requested_seconds", secs, "min_seconds", int(modelSyncMinInterval.Seconds()))
		return modelSyncMinInterval
	}
	return interval
}

// modelAvailabilityWindowMinInterval is the floor for
// MODEL_AVAILABILITY_WINDOW_SECONDS: below this, a single missed
// MODEL_SYNC_INTERVAL_SECONDS tick would flap a model's availability off and
// on for no operational reason.
const modelAvailabilityWindowMinInterval = 30 * time.Second

// modelAvailabilityWindowDefaultInterval mirrors handler.modelAvailabilityWindow
// (kept in sync by hand — the handler package's constant is unexported and
// this is the operator-facing default for the same setting, so there's
// nothing to import). See handler.modelAvailabilityWindow's doc comment for
// why 2 minutes.
const modelAvailabilityWindowDefaultInterval = 2 * time.Minute

// modelAvailabilityWindowFromEnv reads MODEL_AVAILABILITY_WINDOW_SECONDS from
// the environment and applies parseModelAvailabilityWindow.
func modelAvailabilityWindowFromEnv() time.Duration {
	return parseModelAvailabilityWindow(os.Getenv("MODEL_AVAILABILITY_WINDOW_SECONDS"))
}

// parseModelAvailabilityWindow defaults to modelAvailabilityWindowDefaultInterval
// and enforces modelAvailabilityWindowMinInterval as a floor. An empty or
// unparseable value falls back to the default rather than failing startup,
// matching parseModelSyncInterval's contract. Pulled out as a pure function
// so the default/min/parse-failure paths are unit-testable without touching
// the environment.
func parseModelAvailabilityWindow(raw string) time.Duration {
	if raw == "" {
		return modelAvailabilityWindowDefaultInterval
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("MODEL_AVAILABILITY_WINDOW_SECONDS is not a valid integer, using default",
			"value", raw, "default_seconds", int(modelAvailabilityWindowDefaultInterval.Seconds()))
		return modelAvailabilityWindowDefaultInterval
	}
	window := time.Duration(secs) * time.Second
	if window < modelAvailabilityWindowMinInterval {
		slog.Warn("MODEL_AVAILABILITY_WINDOW_SECONDS is below the minimum, clamping",
			"requested_seconds", secs, "min_seconds", int(modelAvailabilityWindowMinInterval.Seconds()))
		return modelAvailabilityWindowMinInterval
	}
	return window
}

// requestID attaches a unique request ID to the context and response header.
// It honours an incoming X-Request-Id header so distributed traces are
// preserved when the web service calls the gateway.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if id == "" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err == nil {
				id = hex.EncodeToString(b)
			}
		}
		c.Set("request_id", id)
		c.Header("X-Request-Id", id)
		c.Next()
	}
}

// requestLogger is a minimal structured request log middleware.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
		)
	}
}

func corsMiddleware() gin.HandlerFunc {
	origins := []string{"http://localhost:8080"}
	if v := os.Getenv("CORS_ALLOWED_ORIGINS"); v != "" {
		origins = strings.Split(v, ",")
		for i := range origins {
			origins[i] = strings.TrimSpace(origins[i])
		}
	}
	cfg := cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Request-Id", "X-Requested-With"},
		ExposeHeaders:    []string{"Content-Type", "X-Request-Id", "X-Darkplum-Model", "X-Darkplum-Route", "X-Darkplum-Router-Reason"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	return cors.New(cfg)
}

// providerRoutesPrefix is every route ProvidersHandler serves.
const providerRoutesPrefix = "/api/v1/providers/"

// reconRefreshPath triggers vendor report pulls; interactive admins only.
const reconRefreshPath = "/api/v1/usage/reconciliation/refresh"

// rejectBearerOnProviderRoutes refuses any request under
// providerRoutesPrefix that carries an Authorization: Bearer header with
// 403, before session auth, CSRF, or anything else in the chain runs (see
// buildRouter's registration order). These routes install a key into the
// cluster Secret and must only ever be reachable by an interactive admin
// session with CSRF protection intact -- a Bearer credential exists
// specifically so an SDK/script can skip that protection on the
// OpenAI-compatible surface (apiOrSession, csrfProtection), which must never
// extend to this one.
func rejectBearerOnProviderRoutes() gin.HandlerFunc {
	return func(c *gin.Context) {
		if (strings.HasPrefix(c.Request.URL.Path, providerRoutesPrefix) || c.Request.URL.Path == reconRefreshPath) &&
			strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") {
			// Structured {code,message} like every other error these routes
			// return (internal/handler/providers.go's apiErrorBody) -- the
			// web engineer's UI reads error.message verbatim regardless of
			// which layer produced the refusal.
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"code":    "bearer_not_allowed",
					"message": "vendor key routes do not accept API-key or bearer credentials; sign in as an admin",
				},
			})
			return
		}
		c.Next()
	}
}

func csrfProtection() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case "POST", "PUT", "PATCH", "DELETE":
			if c.GetHeader("X-Requested-With") == "" &&
				c.Request.URL.Path != "/health" &&
				!strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "missing required header"})
				return
			}
		}
		c.Next()
	}
}
