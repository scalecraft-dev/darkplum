package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/gateway/internal/config"
	"github.com/scalecraft/darkplum/gateway/internal/handler"
	"github.com/scalecraft/darkplum/gateway/internal/health"
	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/mock"
	"github.com/scalecraft/darkplum/gateway/internal/provider/openaicompat"
	"github.com/scalecraft/darkplum/gateway/internal/recorder"
	"github.com/scalecraft/darkplum/gateway/internal/router"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dbPool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: failed to create pool: %v", err)
	}
	defer dbPool.Close()

	if err := dbPool.Ping(ctx); err != nil {
		log.Fatalf("database: ping failed: %v", err)
	}

	// reg is the dispatch-time provider registry: routing.Route.ProviderKey ->
	// provider.Provider. Entries for statically-configured backends (mock,
	// vllm-default, extra vLLMs, LM Studio) are set once below; the Bedrock
	// entry is set lazily by routing.NewBedrockSource's onReady callback the
	// first time its client construction succeeds (which may be on a later
	// discovery tick, not necessarily at startup).
	reg := provider.NewRegistry()

	// sources drives routing.Manager's periodic discovery. Registration order
	// is significant: it is the tie-break among self-hosted sources that
	// claim the same model id (self_hosted always beats cloud_managed
	// regardless of position).
	var sources []routing.Source

	mockMode := cfg.VLLMUpstream == "mock"

	if mockMode {
		mockProv := mock.New()
		reg.Set(mockProv.Name(), mockProv)
		sources = append(sources, routing.NewMockSource(mockProv.Name(), mockProv.Models()))
		log.Println("gateway: mock mode enabled — no upstream required")
	} else {
		// Validate the vLLM upstream URL before attempting to connect.
		if err := config.ValidateUpstreamURL(cfg.VLLMUpstream, cfg.DevMode); err != nil {
			log.Fatalf("vllm: upstream validation failed: %v", err)
		}

		vllmProv, err := openaicompat.New("vllm-default", "vllm", cfg.VLLMUpstream)
		if err != nil {
			log.Fatalf("vllm: failed to create provider: %v", err)
		}
		reg.Set("vllm-default", vllmProv)
		sources = append(sources, routing.NewHTTPModelSource("vllm-default", "vllm", routing.ClassSelfHosted, "vllm-default", cfg.VLLMUpstream))
	}

	// Extra named vLLM upstreams (VLLM_UPSTREAMS). Each is its own routing
	// source and dispatch target, independent of whether the primary
	// VLLM_UPSTREAM is mock or real.
	for _, u := range cfg.VLLMUpstreams {
		instanceName := "vllm-" + u.Name
		p, err := openaicompat.New(instanceName, "vllm", u.URL)
		if err != nil {
			log.Fatalf("vllm(%s): failed to create provider: %v", u.Name, err)
		}
		reg.Set(instanceName, p)
		sources = append(sources, routing.NewHTTPModelSource(instanceName, "vllm", routing.ClassSelfHosted, instanceName, u.URL))
	}

	// Build LM Studio provider if enabled.
	if cfg.LMStudioEnabled {
		if err := config.ValidateUpstreamURL(cfg.LMStudioUpstream, cfg.DevMode); err != nil {
			log.Fatalf("lmstudio: upstream validation failed: %v", err)
		}

		lmsProv, err := openaicompat.New("lmstudio-dev", "lmstudio", cfg.LMStudioUpstream)
		if err != nil {
			log.Fatalf("lmstudio: failed to create provider: %v", err)
		}
		reg.Set("lmstudio-dev", lmsProv)
		sources = append(sources, routing.NewHTTPModelSource("lmstudio-dev", "lmstudio", routing.ClassSelfHosted, "lmstudio-dev", cfg.LMStudioUpstream))
	}

	// Bedrock. Unlike the other backends, provider construction happens
	// lazily inside the Source's Discover closure so that a failed init (e.g.
	// expired SSO creds, VPC endpoint not yet reachable) is retried on every
	// later discovery tick instead of leaving Bedrock unavailable until the
	// pod restarts. onReady registers the constructed client in reg the
	// moment it exists, strictly before any model from it can appear in the
	// route table.
	if cfg.BedrockEnabled {
		onReady := func(p provider.Provider) {
			reg.Set("bedrock", p)
			log.Printf("bedrock: client ready (region=%s)", cfg.BedrockRegion)
		}
		sources = append(sources, routing.NewBedrockSource(cfg.BedrockRegion, cfg.BedrockModelFilter, onReady))
	}

	sourceDescriptions := make([]string, 0, len(sources))
	for _, s := range sources {
		sourceDescriptions = append(sourceDescriptions, fmt.Sprintf("%s(%s/%s)", s.Name, s.Kind, s.Class))
	}
	log.Printf("gateway: routing sources configured: %s", strings.Join(sourceDescriptions, ", "))

	routeMgr := routing.NewManager(sources, time.Duration(cfg.RouteLastGoodTTLSeconds)*time.Second)
	routeMgr.Run(ctx, time.Duration(cfg.DiscoveryIntervalSeconds)*time.Second)
	log.Printf("gateway: initial discovery complete, %d models routable (refreshing every %ds, last-good TTL %ds)",
		len(routeMgr.Snapshot().IDs()), cfg.DiscoveryIntervalSeconds, cfg.RouteLastGoodTTLSeconds)

	// Policy store: models.enabled, model_aliases, routing_policy. Fails
	// closed (503 policy_unavailable) until the first successful load — which
	// may not happen until the api runs migration 010 against a freshly
	// onboarded database — then keeps serving the last-good snapshot on later
	// refresh failures, up to POLICY_MAX_STALENESS_SECONDS.
	polStore := policy.NewPgStore(dbPool, time.Duration(cfg.PolicyMaxStalenessSeconds)*time.Second)
	polStore.Run(ctx, time.Duration(cfg.PolicyRefreshSeconds)*time.Second)
	if _, loaded := polStore.Current(); loaded {
		log.Printf("policy: initial load succeeded (refreshing every %ds)", cfg.PolicyRefreshSeconds)
	} else {
		log.Printf("policy: WARNING initial load failed — chat completions will 503 policy_unavailable until a refresh succeeds (retrying every %ds)", cfg.PolicyRefreshSeconds)
	}

	// Build upstream health entries for observability.
	// Only openai-compatible HTTP upstreams are probed (vllm, lmstudio). Mock and
	// Bedrock are excluded: mock has no HTTP endpoint, and Bedrock reachability is
	// governed by IAM/VPC — a failed /v1/models probe there would be meaningless.
	// The checker result is included in the /health response body but NEVER used
	// to flip the pod to unready.
	var healthEntries []health.Entry
	if !mockMode {
		healthEntries = append(healthEntries, health.Entry{
			Name:     "vllm-default",
			ProbeURL: cfg.VLLMUpstream + "/v1/models",
		})
	}
	for _, u := range cfg.VLLMUpstreams {
		healthEntries = append(healthEntries, health.Entry{
			Name:     "vllm-" + u.Name,
			ProbeURL: u.URL + "/v1/models",
		})
	}
	if cfg.LMStudioEnabled {
		healthEntries = append(healthEntries, health.Entry{
			Name:     "lmstudio-dev",
			ProbeURL: cfg.LMStudioUpstream + "/v1/models",
		})
	}
	upstreamChecker := health.NewChecker(healthEntries)

	rec := recorder.New(dbPool)

	// Routed-alias classifiers. rulesClassifier is stateless; modelClassifier
	// and semanticClassifier each own a circuit breaker over
	// CLASSIFIER_TIMEOUT_MS/CLASSIFIER_BREAKER_FAILURES/
	// CLASSIFIER_BREAKER_OPEN_SECONDS, surfaced on /health below.
	// SemanticRouterURL empty (SEMANTIC_ROUTER_URL unset) is a supported,
	// permanent "unconfigured" state — every semantic_router alias just
	// degrades to default_route until an operator sets it.
	rulesClassifier := router.NewRulesClassifier()
	classifierTimeout := time.Duration(cfg.ClassifierTimeoutMS) * time.Millisecond
	classifierBreakerOpen := time.Duration(cfg.ClassifierBreakerOpenSeconds) * time.Second
	modelClassifier := router.NewModelClassifier(reg, routeMgr, polStore, classifierTimeout, cfg.ClassifierBreakerFailures, classifierBreakerOpen)
	semanticClassifier := router.NewSemanticRouterClassifier(cfg.SemanticRouterURL, cfg.SemanticRouterMinConfidence, classifierTimeout, cfg.ClassifierBreakerFailures, classifierBreakerOpen)
	log.Printf("gateway: routed-alias classifiers configured: timeout=%s breaker_failures=%d breaker_open=%s semantic_router_url=%q semantic_router_min_confidence=%.2f",
		classifierTimeout, cfg.ClassifierBreakerFailures, classifierBreakerOpen, cfg.SemanticRouterURL, cfg.SemanticRouterMinConfidence)

	chatHandler := handler.NewChatHandler(reg, routeMgr, polStore, rulesClassifier, modelClassifier, semanticClassifier, rec)

	gin.SetMode(gin.ReleaseMode)
	healthHandler := newHealthHandler(upstreamChecker, routeMgr, polStore, modelClassifier, semanticClassifier)
	httpRouter := buildRouter(chatHandler, middleware.APIKey(dbPool), middleware.Recording(rec), healthHandler)

	addr := ":" + cfg.Port
	srv := &http.Server{
		Addr:         addr,
		Handler:      httpRouter,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming responses can run for minutes
		IdleTimeout:  120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("gateway: listening on %s, vllm=%s, lmstudio=%v, bedrock=%v (region=%s)",
			addr, cfg.VLLMUpstream, cfg.LMStudioEnabled, cfg.BedrockEnabled, cfg.BedrockRegion)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Println("gateway: shutdown signal received")
	case err := <-errCh:
		log.Fatalf("gateway: server exited: %v", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("gateway: graceful shutdown error: %v", err)
	}

	rec.Drain()
	log.Println("gateway: recorder drained, exiting")
}

// buildRouter assembles the full gin.Engine: /health plus the /v1 surface.
// Extracted from main so tests can exercise the real route table — including
// the unknown-endpoint 404 contract — via httptest without a live database or
// upstream. apiKeyMW/recordingMW are gin.HandlerFunc rather than concrete
// dependencies so tests can substitute no-op fakes for the auth/audit
// machinery, which already has its own package-level tests.
func buildRouter(chatHandler *handler.ChatHandler, apiKeyMW, recordingMW, healthHandler gin.HandlerFunc) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Logger())
	engine.Use(gin.Recovery())

	engine.GET("/health", healthHandler)

	v1 := engine.Group("/v1")
	v1.Use(apiKeyMW)

	// GET /v1/models and POST /v1/routing/dryrun are authenticated but not
	// wrapped in recordingMW: recordingMW builds and records exactly one
	// purpose="inference" InferenceEvent per POST, which is right for
	// /chat/completions but wrong for a dryrun (it never dispatches to any
	// model, so there is no "inference" to record — HandleRoutingDryrun
	// records its own, single, purpose="routing_dryrun" row directly,
	// only when a model classifier call actually happened). GET /v1/models
	// is unaffected either way (Recording is a POST-only middleware) but is
	// kept out of the recorded group for the same reason: it never produces
	// an inference_events row.
	v1.GET("/models", chatHandler.HandleListModels)
	v1.POST("/routing/dryrun", chatHandler.HandleRoutingDryrun)

	// Only three endpoints are served under /v1. There is no reverse-proxy
	// passthrough: the previous behaviour forwarded every other
	// GET/DELETE/PUT/PATCH /v1/* request straight to the default vLLM
	// upstream with no policy check — an open allowlist onto a third party's
	// route table that also leaked GET /v1/models/{id} metadata for arbitrary
	// ids. Every other /v1/* path, and every other method on these three
	// paths, falls through to the NoRoute handler below.
	recorded := v1.Group("")
	recorded.Use(recordingMW)
	recorded.POST("/chat/completions", chatHandler.HandleChatCompletions)

	engine.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			respondUnknownEndpoint(c)
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})

	return engine
}

// respondUnknownEndpoint writes the OpenAI-compatible 404 for any /v1/* path
// or method combination other than GET /v1/models and POST
// /v1/chat/completions. This also fixes POST /v1/embeddings and
// /v1/completions, which previously fell through to gin's bare (non-JSON) 404
// since no route matched them at all.
func respondUnknownEndpoint(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("unknown endpoint: %s %s", c.Request.Method, c.Request.URL.Path),
			"type":    "invalid_request_error",
			"code":    "unknown_endpoint",
		},
	})
}

// classifierBreakerStatus is the subset of *router.ModelClassifier that
// newHealthHandler needs. Defined as an interface so tests can substitute a
// fake without constructing a real classifier/registry/routes/policy chain.
type classifierBreakerStatus interface {
	Status() (breakerOpen bool, consecutiveFailures int)
}

// semanticRouterHealthStatus is the subset of *router.SemanticRouterClassifier
// that newHealthHandler needs, for the same reason as classifierBreakerStatus.
type semanticRouterHealthStatus interface {
	HealthStatus(ctx context.Context) string
}

// newHealthHandler builds the /health handler. /health is the liveness AND
// readiness probe endpoint: it always returns 200 as long as the process is
// healthy — upstream reachability, routing source state, policy staleness,
// and classifier breaker/reachability state are OBSERVABILITY, never a kill
// switch. A down lmstudio/vllm upstream, an expired routing source, a stale
// policy snapshot, an open classifier breaker, or an unreachable semantic
// router must never flip the pod to unready and take out unrelated traffic
// (e.g. a deployment that doesn't use routed aliases at all).
//
// The response is unauthenticated, so routing source status reports model
// COUNTS only (never model ids or secrets), and a discovery failure is
// surfaced as last_error_kind — a small fixed classification (see
// routing.classifyError) — never the raw error string. The raw error
// (which, for an HTTP discovery failure, is an *url.Error whose message
// embeds the full upstream URL) is only ever written to the gateway's own
// log by routing.Manager.Refresh. Similarly, semanticClassifier.HealthStatus
// is a cached, rate-limited probe (see SemanticRouterClassifier) — this
// handler never triggers a network call itself.
func newHealthHandler(upstreamChecker *health.Checker, routeMgr *routing.Manager, polStore *policy.PgStore, modelClassifier classifierBreakerStatus, semanticClassifier semanticRouterHealthStatus) gin.HandlerFunc {
	return func(c *gin.Context) {
		upstreams := upstreamChecker.Check(c.Request.Context())
		upstreamBody := make(map[string]string, len(upstreams))
		for name, status := range upstreams {
			upstreamBody[name] = string(status)
		}

		routingStatus := routeMgr.Status()
		sourcesBody := make(map[string]gin.H, len(routingStatus.Sources))
		for name, s := range routingStatus.Sources {
			sourcesBody[name] = gin.H{
				"models":                   s.Models,
				"last_success_seconds_ago": s.LastSuccessSecondsAgo,
				"last_error_kind":          nullableString(s.LastErrorKind),
			}
		}

		policyStatus := polStore.Status()

		breakerOpen, consecutiveFailures := modelClassifier.Status()

		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"upstreams": upstreamBody,
			"routing": gin.H{
				"models":  routingStatus.Models,
				"sources": sourcesBody,
				"classifier": gin.H{
					"breaker_open":         breakerOpen,
					"consecutive_failures": consecutiveFailures,
					"semantic_router":      semanticClassifier.HealthStatus(c.Request.Context()),
				},
			},
			"policy": gin.H{
				"loaded":      policyStatus.Loaded,
				"age_seconds": policyStatus.AgeSeconds,
				"stale":       policyStatus.Stale,
			},
		})
	}
}

// nullableString returns nil for an empty string so it serialises as JSON
// null rather than "" (used for /health's last_error field).
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
