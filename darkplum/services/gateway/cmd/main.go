package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/gateway/internal/config"
	"github.com/scalecraft/darkplum/gateway/internal/handler"
	"github.com/scalecraft/darkplum/gateway/internal/health"
	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/bedrock"
	"github.com/scalecraft/darkplum/gateway/internal/provider/mock"
	"github.com/scalecraft/darkplum/gateway/internal/provider/openaicompat"
	"github.com/scalecraft/darkplum/gateway/internal/proxy"
	"github.com/scalecraft/darkplum/gateway/internal/recorder"
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

	// modelRoute maps each discovered model ID to the instance name of the
	// openai-compatible provider that serves it. Built at startup from the
	// /v1/models discovery call to each upstream. DB-free: no per-request lookups.
	modelRoute := make(map[string]string)

	// openaiProviders holds all openai-compatible instances in registration order.
	// The first element is always the default provider.
	var openaiProviders []provider.Provider

	var defaultProv provider.Provider
	var rp *httputil.ReverseProxy
	mockMode := cfg.VLLMUpstream == "mock"

	if mockMode {
		mockProv := mock.New()
		defaultProv = mockProv
		for _, id := range mockProv.Models() {
			if _, exists := modelRoute[id]; exists {
				log.Printf("gateway: model ID %q already registered, keeping first-registered winner", id)
			} else {
				modelRoute[id] = mockProv.Name()
			}
		}
		openaiProviders = append(openaiProviders, mockProv)
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
		defaultProv = vllmProv
		openaiProviders = append(openaiProviders, vllmProv)

		vllmModels := fetchOpenAICompatModels(ctx, cfg.VLLMUpstream, "vllm-default")
		if len(vllmModels) == 0 {
			log.Printf("WARNING: vllm-default: discovered 0 models from %s — upstream may be unreachable; check NetworkPolicy egress rules and probe /health", cfg.VLLMUpstream)
		} else {
			log.Printf("vllm: discovered %d models from %s", len(vllmModels), cfg.VLLMUpstream)
		}
		for _, id := range vllmModels {
			if _, exists := modelRoute[id]; exists {
				log.Printf("gateway: model ID %q already registered (vllm-default), keeping first-registered winner", id)
			} else {
				modelRoute[id] = "vllm-default"
			}
		}
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
		openaiProviders = append(openaiProviders, lmsProv)

		lmsModels := fetchOpenAICompatModels(ctx, cfg.LMStudioUpstream, "lmstudio-dev")
		if len(lmsModels) == 0 {
			log.Printf("WARNING: lmstudio-dev: discovered 0 models from %s — upstream may be unreachable; check NetworkPolicy egress rules and probe /health", cfg.LMStudioUpstream)
		} else {
			log.Printf("lmstudio: discovered %d models from %s", len(lmsModels), cfg.LMStudioUpstream)
		}
		for _, id := range lmsModels {
			if _, exists := modelRoute[id]; exists {
				log.Printf("gateway: model ID %q already registered (lmstudio-dev conflicts), keeping first-registered winner", id)
			} else {
				modelRoute[id] = "lmstudio-dev"
			}
		}
	}

	var bedrockProv provider.Provider
	bedrockModels := make(map[string]bool)
	if cfg.BedrockEnabled {
		bp, err := bedrock.New(cfg.BedrockRegion)
		if err != nil {
			// Non-fatal: treat as "no Bedrock models available this run". Mirrors
			// the vLLM/LM Studio pattern where an unreachable upstream at startup
			// logs a warning and continues rather than crashing the entire gateway.
			log.Printf("bedrock: WARNING — provider init failed, Bedrock models unavailable until restart: %v", err)
		} else {
			discovered, discErr := bp.DiscoverModels(ctx)
			if discErr != nil {
				// Non-fatal: expired SSO creds, VPC endpoint not yet reachable, etc.
				// bedrockProv stays nil, bedrockModels stays empty — no Bedrock routes
				// are registered. All other providers (vllm, lmstudio, mock) are
				// unaffected. /health continues to serve 200.
				log.Printf("bedrock: WARNING — model discovery failed, Bedrock models unavailable until restart: %v", discErr)
			} else {
				bedrockProv = bp
				bedrockModels = discovered
				log.Printf("bedrock: discovered %d models in %s", len(bedrockModels), cfg.BedrockRegion)
			}
		}
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
	if cfg.LMStudioEnabled {
		healthEntries = append(healthEntries, health.Entry{
			Name:     "lmstudio-dev",
			ProbeURL: cfg.LMStudioUpstream + "/v1/models",
		})
	}
	upstreamChecker := health.NewChecker(healthEntries)

	chatHandler := handler.NewChatHandler(
		defaultProv,
		bedrockProv,
		bedrockModels,
		openaiProviders,
		modelRoute,
	)

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

	// Chat completions are handled by ChatHandler, which selects the backend
	// provider based on the discovered Bedrock model set and model-route table.
	// GET /v1/models is served by HandleListModels, which combines openai-compat
	// and Bedrock model lists. All other /v1/* GET paths are forwarded to the
	// vLLM upstream via the reverse proxy.
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

// fetchOpenAICompatModels calls the upstream's GET /v1/models and returns the
// list of model IDs. Errors are logged and an empty slice is returned so an
// upstream startup delay does not prevent the gateway from starting.
// instanceName is used only for log messages.
func fetchOpenAICompatModels(ctx context.Context, upstreamURL, instanceName string) []string {
	target := upstreamURL + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		log.Printf("%s: build models request failed: %v", instanceName, err)
		return nil
	}

	// Use a DialContext with a 5s TCP connect timeout so a NetworkPolicy-dropped
	// SYN fails in ~5s rather than after the OS default (~120s). A generous
	// overall client Timeout is retained so slow-responding upstreams don't hang
	// startup indefinitely either.
	discoveryDialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: discoveryDialer.DialContext,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("%s: models request failed: %v", instanceName, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("%s: models request returned %d", instanceName, resp.StatusCode)
		return nil
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("%s: decode models response failed: %v", instanceName, err)
		return nil
	}

	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}
