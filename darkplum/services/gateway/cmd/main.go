package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
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
	"github.com/scalecraft/darkplum/gateway/internal/proxy"
	"github.com/scalecraft/darkplum/gateway/internal/recorder"
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

	var rp *httputil.ReverseProxy
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

		rp, err = proxy.New(cfg.VLLMUpstream)
		if err != nil {
			log.Fatalf("proxy: failed to parse upstream URL %q: %v", cfg.VLLMUpstream, err)
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

	routeMgr := routing.NewManager(sources)
	routeMgr.Run(ctx, time.Duration(cfg.DiscoveryIntervalSeconds)*time.Second)
	log.Printf("gateway: initial discovery complete, %d models routable (refreshing every %ds)",
		len(routeMgr.Snapshot().IDs()), cfg.DiscoveryIntervalSeconds)

	// Policy store: models.enabled, model_aliases, routing_policy. Fails
	// closed (503 policy_unavailable) until the first successful load — which
	// may not happen until the api runs migration 010 against a freshly
	// onboarded database — then keeps serving the last-good snapshot on later
	// refresh failures.
	polStore := policy.NewPgStore(dbPool)
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

	chatHandler := handler.NewChatHandler(reg, routeMgr, polStore)

	rec := recorder.New(dbPool)

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	// /health is the liveness AND readiness probe endpoint.
	//
	// It always returns 200 as long as the process is healthy — upstream
	// reachability is OBSERVABILITY, not a kill switch. A down lmstudio or vllm
	// upstream must never flip the pod to unready and take out Bedrock traffic.
	//
	// The response body carries a per-upstream status map so operators and
	// dashboards can see which upstreams are reachable without tailing pod logs.
	// Probe results are cached (10s TTL) so frequent readiness checks don't
	// hammer the upstream /v1/models endpoints.
	//
	// TODO(follow-up): increment an upstream_error_total counter here once a
	// metrics infrastructure (e.g. prometheus/expvar) is wired in. Currently no
	// metrics infra exists in the gateway, so we don't invent one now.
	router.GET("/health", func(c *gin.Context) {
		upstreams := upstreamChecker.Check(c.Request.Context())

		// Convert to map[string]string for JSON serialisation.
		upstreamBody := make(map[string]string, len(upstreams))
		for name, status := range upstreams {
			upstreamBody[name] = string(status)
		}

		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"upstreams": upstreamBody,
		})
	})

	v1 := router.Group("/v1")
	v1.Use(middleware.APIKey(dbPool))
	v1.Use(middleware.Recording(rec))

	// Chat completions are handled by ChatHandler, which enforces model
	// routing (alias resolve -> route lookup -> enabled -> residency ->
	// dispatch) using the live routing.Table and policy.Snapshot. GET
	// /v1/models is served by HandleListModels, which lists every discovered
	// model with its backend_class. All other /v1/* GET paths are forwarded
	// to the vLLM upstream via the reverse proxy.
	//
	// NOTE: gin/httprouter does not allow registering both a static segment
	// (/models) and a catch-all (/*path) under the same parent. The wildcard
	// handler below checks c.Param("path") and dispatches to HandleListModels
	// for the /models path so that both routing requirements are satisfied
	// without a route conflict panic.
	v1.POST("/chat/completions", chatHandler.HandleChatCompletions)
	proxyHandler := func(c *gin.Context) {
		if rp == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no upstream configured"})
			return
		}
		rp.ServeHTTP(c.Writer, c.Request)
	}
	v1.GET("/*path", func(c *gin.Context) {
		if c.Param("path") == "/models" {
			chatHandler.HandleListModels(c)
			return
		}
		proxyHandler(c)
	})
	v1.DELETE("/*path", proxyHandler)
	v1.PUT("/*path", proxyHandler)
	v1.PATCH("/*path", proxyHandler)

	addr := ":" + cfg.Port
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
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
