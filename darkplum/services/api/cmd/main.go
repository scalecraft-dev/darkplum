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

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/auth"
	"github.com/scalecraft/darkplum/api/internal/db"
	"github.com/scalecraft/darkplum/api/internal/handler"
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

		ticker := time.NewTicker(modelSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				modelHandler.SyncModels(ctx)
			}
		}
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

	// Router.
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestID())
	r.Use(requestLogger())
	r.Use(corsMiddleware())
	r.Use(csrfProtection())

	// Handlers.
	users := handler.NewUserHandler(pool, activityRecorder, authCfg.Pepper)
	keys := handler.NewAPIKeyHandler(pool, activityRecorder)
	chat := handler.NewChatHandler(gatewayURL, serviceKey)
	activityH := handler.NewActivityHandler(pool)
	routing := handler.NewRoutingHandler(pool, activityRecorder)
	routing.SetAvailabilityWindow(modelAvailabilityWindowFromEnv())
	conversations := handler.NewConversationHandler(pool, activityRecorder)

	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Auth config — always available so the frontend can detect auth mode.
	r.GET("/auth/config", func(c *gin.Context) {
		resp := gin.H{
			"local": true,
			"oidc":  gin.H{"enabled": authCfg.OIDC != nil},
		}
		var count int
		if err := pool.QueryRow(c.Request.Context(),
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
	authGroup.POST("/login", loginRL, localProvider.Login)
	authGroup.POST("/change-password", localProvider.ChangePassword)
	// First-run admin creation is only reachable before OIDC is configured; an
	// OIDC deployment provisions its first admin via SSO + a role grant, so the
	// unauthenticated setup endpoint is never exposed alongside OIDC.
	if oidcProvider == nil {
		authGroup.POST("/setup", loginRL, localProvider.Setup)
	}
	authGroup.GET("/me", localProvider.HandleMe)
	if oidcProvider != nil {
		authGroup.GET("/login", loginRL, oidcProvider.Login)
		authGroup.GET("/callback", loginRL, oidcProvider.Callback)
		authGroup.POST("/logout", oidcProvider.Logout)
	} else {
		authGroup.POST("/logout", localProvider.Logout)
	}

	// API routes.
	v1 := r.Group("/api/v1")
	v1.Use(sessionMW)
	{
		v1.GET("/users", users.ListUsers)
		v1.GET("/api-keys", keys.ListAPIKeys)

		// Conversation history: session-authenticated but explicitly NOT
		// admin-only -- see the privacy comment atop
		// internal/handler/conversations.go. Every handler scopes by the
		// session user_id itself.
		v1.GET("/conversations", conversations.ListConversations)
		v1.POST("/conversations", conversations.CreateConversation)
		v1.GET("/conversations/:id", conversations.GetConversation)
		v1.PATCH("/conversations/:id", conversations.PatchConversation)
		v1.POST("/conversations/:id/messages", conversations.AppendMessages)
		v1.DELETE("/conversations/:id", conversations.DeleteConversation)
		v1.DELETE("/conversations/:id/messages", conversations.TruncateMessages)

		adminOnly := v1.Group("")
		adminOnly.Use(auth.AdminRequired())
		{
			adminOnly.POST("/users", users.CreateUser)
			adminOnly.DELETE("/users/:id", users.DeleteUser)
			adminOnly.PATCH("/users/:id/role", users.UpdateRole)
			adminOnly.PATCH("/users/:id/password", users.ResetPassword)

			adminOnly.POST("/api-keys", keys.CreateAPIKey)
			adminOnly.DELETE("/api-keys/:id", keys.RevokeAPIKey)

			adminOnly.GET("/models", modelHandler.ListAdminModels)
			adminOnly.PATCH("/models/:id", modelHandler.ToggleModel)

			adminOnly.GET("/model-aliases", routing.ListAliases)
			adminOnly.PUT("/model-aliases/:alias", routing.PutAlias)
			adminOnly.DELETE("/model-aliases/:alias", routing.DeleteAlias)

			adminOnly.GET("/routing-policy", routing.GetRoutingPolicy)
			adminOnly.PUT("/routing-policy", routing.PutRoutingPolicy)

			adminOnly.GET("/activity", activityH.ListActivity)
			adminOnly.GET("/activity/export", activityH.ExportActivity)
			adminOnly.GET("/usage", activityH.ListUsage)
			adminOnly.GET("/usage/export", activityH.ExportUsage)
			adminOnly.GET("/usage/stats", activityH.UsageStats)
			adminOnly.GET("/usage/summary", activityH.UsageSummary)
			adminOnly.GET("/usage/user-summary", activityH.UserSummary)
			adminOnly.GET("/usage/key-summary", activityH.KeySummary)
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
	r.GET("/v1/models", apiOrSession(sessionMW), modelHandler.ListModels)
	r.POST("/v1/chat/completions", apiOrSession(sessionMW), chat.ProxyChat)

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

// modelSyncIntervalFromEnv reads MODEL_SYNC_INTERVAL_SECONDS from the
// environment and applies parseModelSyncInterval.
func modelSyncIntervalFromEnv() time.Duration {
	return parseModelSyncInterval(os.Getenv("MODEL_SYNC_INTERVAL_SECONDS"))
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
		ExposeHeaders:    []string{"Content-Type", "X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	return cors.New(cfg)
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
