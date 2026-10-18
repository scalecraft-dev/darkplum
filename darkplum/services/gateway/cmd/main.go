package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/gateway/internal/config"
	"github.com/scalecraft-dev/darkplum/gateway/internal/handler"
	"github.com/scalecraft-dev/darkplum/gateway/internal/health"
	"github.com/scalecraft-dev/darkplum/gateway/internal/middleware"
	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/mock"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openaicompat"
	"github.com/scalecraft-dev/darkplum/gateway/internal/recorder"
	"github.com/scalecraft-dev/darkplum/gateway/internal/router"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
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
	// vllm-default, extra vLLMs, LM Studio, anthropic) are set once below;
	// the Bedrock entry is set lazily by routing.NewBedrockSource's onReady
	// callback the first time its client construction succeeds (which may be
	// on a later discovery tick, not necessarily at startup).
	reg := provider.NewRegistry()

	// providerStatus backs GET /v1/models' `providers` array (service-key +
	// X-Darkplum-Classes: external_api callers only). Seeded with both known
	// vendor names now — before either provider's Source is constructed
	// below — so the response always lists both (see
	// provider.NewStatusRegistry) even on a build that has wired in neither.
	// Constructed here (rather than just before NewChatHandler, as originally
	// scaffolded) because routing.NewAnthropicSource needs it to report its
	// own state on every discovery tick.
	providerStatus := provider.NewStatusRegistry("anthropic", "openai")

	// Policy store: models.enabled, model_aliases, routing_policy, and (since
	// migration 017) vendor_provider_settings. Fails closed (503
	// policy_unavailable) until the first successful load — which may not
	// happen until the api runs migration 010 against a freshly onboarded
	// database — then keeps serving the last-good snapshot on later refresh
	// failures, up to POLICY_MAX_STALENESS_SECONDS. Constructed here, before
	// the routing sources below, because NewAnthropicSource/NewOpenAISource's
	// filter functions read the live snapshot's VendorFilters on every
	// discovery tick (see routing.NewVendorFilterFunc).
	polStore := policy.NewPgStore(dbPool, time.Duration(cfg.PolicyMaxStalenessSeconds)*time.Second)
	polStore.Run(ctx, time.Duration(cfg.PolicyRefreshSeconds)*time.Second)
	if _, loaded := polStore.Current(); loaded {
		log.Printf("policy: initial load succeeded (refreshing every %ds)", cfg.PolicyRefreshSeconds)
	} else {
		log.Printf("policy: WARNING initial load failed — chat completions will 503 policy_unavailable until a refresh succeeds (retrying every %ds)", cfg.PolicyRefreshSeconds)
	}

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

	// Anthropic. Unlike Bedrock, provider construction never fails at
	// startup (it wraps a file-backed credsource.Source, not a
	// network-dependent SDK config load), so it is built and registered
	// unconditionally — there is no ANTHROPIC_ENABLED flag. Whether it ever
	// contributes a routable model depends entirely on whether the key file
	// is present, the filter is set, and discovery succeeds, all of which
	// anthropicCreds/anthropicProv/NewAnthropicSource re-check on every
	// discovery tick without a restart. A gateway with the
	// darkplum-provider-keys Secret never mounted at all runs exactly as
	// before: not_configured, zero models, zero vendor HTTP calls.
	anthropicKeyPath := filepath.Join(cfg.ProviderKeysDir, "anthropic-api-key")
	anthropicCreds := credsource.New(anthropicKeyPath)
	// The filter func is built first: it also resolves the effective
	// workspace id (same last-writer-wins row), which the provider reads per
	// request.
	anthropicFilter := routing.NewVendorFilterFunc("anthropic", cfg.AnthropicModelFilter, cfg.AnthropicModelFilterSetAt, cfg.AnthropicWorkspaceID, policyVendorFilterLookup{polStore})
	anthropicProv := anthropic.New(anthropicCreds, anthropic.Config{
		DefaultMaxTokens: cfg.AnthropicDefaultMaxTokens,
		BaseURL:          cfg.AnthropicBaseURL,
		Status:           providerStatus,
		WorkspaceID:      func() string { return routing.EffectiveWorkspaceID(anthropicFilter) },
	})
	reg.Set("anthropic", anthropicProv)
	sources = append(sources, routing.NewAnthropicSource(anthropicCreds, anthropicProv, anthropicFilter, providerStatus))

	// OpenAI. Same unconditional-construction reasoning as Anthropic above —
	// there is no OPENAI_ENABLED flag either; whether it ever contributes a
	// routable model depends entirely on the key file, the filter, and
	// discovery, all re-checked on every tick without a restart.
	openAIKeyPath := filepath.Join(cfg.ProviderKeysDir, "openai-api-key")
	openAICreds := credsource.New(openAIKeyPath)
	openAIProv := openai.New(openAICreds, openai.Config{
		BaseURL: cfg.OpenAIBaseURL,
		Status:  providerStatus,
	})
	reg.Set("openai", openAIProv)
	openAIFilter := routing.NewVendorFilterFunc("openai", cfg.OpenAIModelFilter, cfg.OpenAIModelFilterSetAt, "", policyVendorFilterLookup{polStore})
	sources = append(sources, routing.NewOpenAISource(openAICreds, openAIProv, openAIFilter, providerStatus))

	sourceDescriptions := make([]string, 0, len(sources))
	for _, s := range sources {
		sourceDescriptions = append(sourceDescriptions, fmt.Sprintf("%s(%s/%s)", s.Name, s.Kind, s.Class))
	}
	log.Printf("gateway: routing sources configured: %s", strings.Join(sourceDescriptions, ", "))

	routeMgr := routing.NewManager(sources, time.Duration(cfg.RouteLastGoodTTLSeconds)*time.Second)
	routeMgr.Run(ctx, time.Duration(cfg.DiscoveryIntervalSeconds)*time.Second)
	log.Printf("gateway: initial discovery complete, %d models routable (refreshing every %ds, last-good TTL %ds)",
		len(routeMgr.Snapshot().IDs()), cfg.DiscoveryIntervalSeconds, cfg.RouteLastGoodTTLSeconds)

	// Build upstream health entries for observability.
	// Only openai-compatible HTTP upstreams are probed (vllm, lmstudio). Mock,
	// Bedrock, Anthropic, and OpenAI are excluded: mock has no HTTP endpoint,
	// Bedrock reachability is governed by IAM/VPC, and each vendor's own
	// discovery-tick state (not_configured/filter_missing/auth_failed/
	// unreachable/ok) is already surfaced richly through providerStatus's
	// `providers` block on GET /v1/models — a bare up/down health.Checker
	// probe here would tell an operator strictly less. The checker result is
	// included in the /health response body but NEVER used to flip the pod
	// to unready.
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

	// Routed-alias classifiers. rulesClassifier is stateless; semanticClassifier
	// owns a circuit breaker over CLASSIFIER_TIMEOUT_MS/
	// CLASSIFIER_BREAKER_FAILURES/CLASSIFIER_BREAKER_OPEN_SECONDS, surfaced on
	// /health below. SemanticRouterURL empty (SEMANTIC_ROUTER_URL unset) is a
	// supported, permanent "unconfigured" state — every semantic_router alias
	// just degrades to default_route until an operator sets it.
	rulesClassifier := router.NewRulesClassifier()
	classifierTimeout := time.Duration(cfg.ClassifierTimeoutMS) * time.Millisecond
	classifierBreakerOpen := time.Duration(cfg.ClassifierBreakerOpenSeconds) * time.Second
	semanticClassifier := router.NewSemanticRouterClassifier(cfg.SemanticRouterURL, cfg.SemanticRouterMinConfidence, classifierTimeout, cfg.ClassifierBreakerFailures, classifierBreakerOpen)
	log.Printf("gateway: routed-alias classifiers configured: timeout=%s breaker_failures=%d breaker_open=%s semantic_router_url=%q semantic_router_min_confidence=%.2f",
		classifierTimeout, cfg.ClassifierBreakerFailures, classifierBreakerOpen, cfg.SemanticRouterURL, cfg.SemanticRouterMinConfidence)

	// providerStatus was constructed earlier (before the routing sources) —
	// routing.NewAnthropicSource and routing.NewOpenAISource both needed it,
	// each reporting into it from its own discovery Source on every tick.
	chatHandler := handler.NewChatHandler(reg, routeMgr, polStore, rulesClassifier, semanticClassifier, providerStatus)
	chatHandler.SetPolicyRefresher(polStore.Refresh)

	// providersHandler serves the two machine-only routes the api calls (with
	// the service key) to verify and activate a vendor key an admin installs
	// from Admin > Models — see internal/handler/providers.go. Built from the
	// SAME anthropicCreds/anthropicProv/openAICreds/openAIProv instances wired
	// into the vendor's routing.Source above, so "verify" and what
	// routing.Manager's own discovery tick catalogues can never diverge. ctx
	// is the gateway's root context, so its per-vendor refresh-hint loops stop
	// cleanly on shutdown.
	providersHandler := handler.NewProvidersHandler(ctx, routeMgr, map[string]handler.ProviderVendorConfig{
		"anthropic": {
			Verifier:            anthropicProv,
			Creds:               anthropicCreds,
			Recheck:             anthropicCreds,
			EnvVar:              "ANTHROPIC_MODEL_FILTER",
			IsAuthError:         anthropic.IsAuthError,
			IsWorkspaceRequired: anthropic.IsWorkspaceRequiredError,
		},
		"openai": {
			Verifier:    openAIProv,
			Creds:       openAICreds,
			Recheck:     openAICreds,
			EnvVar:      "OPENAI_MODEL_FILTER",
			IsAuthError: openai.IsAuthError,
			PostFilter:  openai.FilterChatModels,
		},
	})

	gin.SetMode(gin.ReleaseMode)
	healthHandler := newHealthHandler(upstreamChecker, routeMgr, polStore, semanticClassifier, providerStatus, bedrockRetentionFn(reg))
	httpRouter := buildRouter(chatHandler, providersHandler, middleware.APIKey(dbPool), middleware.Recording(rec), healthHandler)

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
func buildRouter(chatHandler *handler.ChatHandler, providersHandler *handler.ProvidersHandler, apiKeyMW, recordingMW, healthHandler gin.HandlerFunc) *gin.Engine {
	engine := gin.New()
	// Same rule as the api: only in-cluster hops may speak for a client via
	// X-Forwarded-For. The api proxy sets it to the caller's address, so the
	// inference audit row records the person, not the api pod.
	if err := engine.SetTrustedProxies([]string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7"}); err != nil {
		panic(fmt.Sprintf("set trusted proxies: %v", err))
	}
	engine.Use(gin.Logger())
	engine.Use(gin.Recovery())

	engine.GET("/health", healthHandler)

	v1 := engine.Group("/v1")
	v1.Use(apiKeyMW)

	// GET /v1/models, POST /v1/routing/dryrun, GET/POST /v1/policy/effective
	// and POST /v1/policy/impact are authenticated but not wrapped in recordingMW:
	// recordingMW builds and records exactly one purpose="inference"
	// InferenceEvent per POST, which is right for /chat/completions but wrong
	// for a dryrun (it never dispatches to any model, and records nothing
	// itself — see HandleRoutingDryrun) or an effective-policy lookup/preview
	// (a metadata read, not an inference — POST /v1/policy/effective never
	// dispatches to a model either, it only re-runs the pure Effective()
	// merge). GET /v1/models and GET /v1/policy/effective are unaffected
	// either way (Recording is a POST-only middleware) but are kept out of
	// the recorded group for the same reason: neither ever produces an
	// inference_events row.
	v1.GET("/models", chatHandler.HandleListModels)
	v1.POST("/routing/dryrun", chatHandler.HandleRoutingDryrun)
	v1.GET("/policy/effective", chatHandler.HandleEffectivePolicy)
	v1.POST("/policy/effective", chatHandler.HandleEffectivePolicyOverride)
	v1.POST("/policy/impact", chatHandler.HandleEffectivePolicyImpact)
	// Service-key-only "reload policy now": the api calls it after every
	// policy-shaped write so the snapshot never lags an admin's save by a
	// refresh interval. Not recorded (no model is dispatched).
	v1.POST("/policy/refresh", chatHandler.HandlePolicyRefresh)

	// POST /v1/providers/:vendor/verify and .../refresh are likewise
	// service-key-only, dispatch to no model, and produce no
	// inference_events row — kept out of the recorded group for the same
	// reason as the five routes above. Their request bodies (a candidate
	// vendor key) must never be logged: recordingMW never wraps them, and
	// gin.Logger() above only logs method/path/status/latency, never a body.
	v1.POST("/providers/:vendor/verify", providersHandler.HandleVerify)
	v1.POST("/providers/:vendor/refresh", providersHandler.HandleRefresh)

	// Only eight endpoints are served under /v1. There is no reverse-proxy
	// passthrough: the previous behaviour forwarded every other
	// GET/DELETE/PUT/PATCH /v1/* request straight to the default vLLM
	// upstream with no policy check — an open allowlist onto a third party's
	// route table that also leaked GET /v1/models/{id} metadata for arbitrary
	// ids. Every other /v1/* path, and every other method on these eight
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

// semanticRouterHealthStatus is the subset of *router.SemanticRouterClassifier
// that newHealthHandler needs. Defined as an interface so tests can
// substitute a fake without constructing a real classifier/registry/routes
// chain: its own circuit-breaker state (Status) alongside its reachability
// probe (HealthStatus) — /health must surface both.
type semanticRouterHealthStatus interface {
	HealthStatus(ctx context.Context) string
	Status() (breakerOpen bool, consecutiveFailures int)
}

// policyHealthStatus is the subset of *policy.PgStore that newHealthHandler
// needs. Defined as an interface (mirroring semanticRouterHealthStatus above)
// so tests can substitute a fake Snapshot/Status directly — including one
// carrying PrincipalCounts — without going through PgStore's real load
// machinery, which requires a live pool or the policy package's own
// unexported test seams.
type policyHealthStatus interface {
	Status() policy.Status
	Current() (policy.Snapshot, bool)
}

// bedrockRetentionFn adapts the provider registry to /health's
// bedrock_data_retention field. The Bedrock provider registers lazily, so the
// lookup happens per request; before it exists the mode is "unknown".
func bedrockRetentionFn(reg *provider.Registry) func() (string, *time.Time) {
	return func() (string, *time.Time) {
		if p, ok := reg.Get("bedrock"); ok {
			if r, ok := p.(provider.DataRetentionReporter); ok {
				return r.AccountDataRetention()
			}
		}
		return provider.DataRetentionModeUnknown, nil
	}
}

// vendorProviderHealthStatus is the subset of *provider.StatusRegistry that
// newHealthHandler needs — just Snapshot, mirroring the same interface
// internal/handler/chat.go's providerStatusSource already uses for GET
// /v1/models' `providers` array. Defined here (rather than importing that
// unexported handler-package type) so tests can substitute a fake without a
// real registry.
type vendorProviderHealthStatus interface {
	Snapshot() []provider.ProviderStatus
}

// healthProviderStatus is /health's OWN reduced view of one vendor's status
// — name, configured, state, model_count, last_sync_at ONLY. /health is
// unauthenticated, so unlike GET /v1/models' `providers` array (service key
// + X-Darkplum-Classes: external_api only), it must never expose a filter
// pattern, filter_source, or env_filter_set_at: a filter glob can itself be
// sensitive naming detail (e.g. an internal model codename), and there is no
// operational reason an unauthenticated liveness probe needs it. Built by
// reducing provider.ProviderStatus field-by-field rather than embedding it,
// so a future field added to ProviderStatus does not silently leak onto this
// endpoint by default.
type healthProviderStatus struct {
	Name       string     `json:"name"`
	Configured bool       `json:"configured"`
	State      string     `json:"state"`
	ModelCount int        `json:"model_count"`
	LastSyncAt *time.Time `json:"last_sync_at"`
}

// reduceProviderStatusForHealth converts a full ProviderStatus into
// /health's reduced DTO. Never nil (mirrors StatusRegistry.Snapshot's own
// "never nil" contract) so the JSON array is always `[]`, never `null`.
func reduceProviderStatusForHealth(full []provider.ProviderStatus) []healthProviderStatus {
	out := make([]healthProviderStatus, 0, len(full))
	for _, p := range full {
		out = append(out, healthProviderStatus{
			Name:       p.Name,
			Configured: p.Configured,
			State:      p.State,
			ModelCount: p.ModelCount,
			LastSyncAt: p.LastSyncAt,
		})
	}
	return out
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
//
// providerStatus (may be nil, e.g. in a test that doesn't care about it)
// backs a read-only `providers` array — the SAME closed-vocabulary shape
// (name, configured, state, model_count, last_sync_at) GET /v1/models
// already exposes to a service key that sent the classes header — added here
// so an operator can see vendor discovery state (not_configured/
// filter_missing/auth_failed/filter_matched_nothing/unreachable/ok) on the
// unauthenticated liveness probe without needing a service key. Like every
// other field on this endpoint, it NEVER carries a model id or a filter
// pattern (ProviderStatus itself has no room for either), and it NEVER
// affects the response status: a vendor in auth_failed or unreachable state
// is still just an observability fact, not a reason to fail liveness/
// readiness.
func newHealthHandler(upstreamChecker *health.Checker, routeMgr *routing.Manager, polStore policyHealthStatus, semanticClassifier semanticRouterHealthStatus, providerStatus vendorProviderHealthStatus, bedrockRetention func() (string, *time.Time)) gin.HandlerFunc {
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

		// principals reports how many explicit user/api_key policy rows are
		// currently loaded (policy.Snapshot.PrincipalCounts) — zero for both
		// when no snapshot has ever loaded, or before migration 015 exists on
		// the customer database (the 42P01-tolerance path still produces a
		// loaded Snapshot with zero explicit principal rows).
		principals := gin.H{"users": 0, "api_keys": 0}
		if snap, ok := polStore.Current(); ok {
			principals = gin.H{"users": snap.PrincipalCounts.Users, "api_keys": snap.PrincipalCounts.APIKeys}
		}

		srBreakerOpen, srConsecutiveFailures := semanticClassifier.Status()
		srHealthStatus := semanticClassifier.HealthStatus(c.Request.Context())

		// providers is read-only diagnostics — see this handler's doc
		// comment and healthProviderStatus's: this is the REDUCED DTO, never
		// the full provider.ProviderStatus GET /v1/models exposes to a
		// service key, so filter/filter_source/env_filter_set_at never reach
		// this unauthenticated endpoint. nil providerStatus (e.g. a test that
		// doesn't care about it) or an empty registry both marshal as `[]`,
		// never `null`.
		providers := []healthProviderStatus{}
		if providerStatus != nil {
			providers = reduceProviderStatusForHealth(providerStatus.Snapshot())
		}

		// bedrock_data_retention: closed vocabulary only (the mode name and a
		// timestamp). "unknown" until the account mode has been read once.
		drMode, drCheckedAt := provider.DataRetentionModeUnknown, (*time.Time)(nil)
		if bedrockRetention != nil {
			drMode, drCheckedAt = bedrockRetention()
		}

		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"upstreams": upstreamBody,
			"routing": gin.H{
				"models":                 routingStatus.Models,
				"bedrock_data_retention": gin.H{"mode": drMode, "checked_at": drCheckedAt},
				"sources":                sourcesBody,
				"classifier": gin.H{
					"semantic_router": gin.H{
						"status":               srHealthStatus,
						"breaker_open":         srBreakerOpen,
						"consecutive_failures": srConsecutiveFailures,
					},
				},
			},
			"policy": gin.H{
				"loaded":      policyStatus.Loaded,
				"age_seconds": policyStatus.AgeSeconds,
				"stale":       policyStatus.Stale,
				"principals":  principals,
			},
			"providers": providers,
		})
	}
}

// policyVendorFilterLookup adapts *policy.PgStore to
// routing.NewVendorFilterFunc's vendorFilterPolicySource seam. Defined here
// (rather than in internal/routing) because internal/policy already imports
// internal/routing (for residency/backend-class checks — see
// policy/effective.go), so routing importing policy directly would cycle;
// this small adapter is the one place that needs to know both shapes.
type policyVendorFilterLookup struct {
	store *policy.PgStore
}

func (p policyVendorFilterLookup) VendorFilter(vendor string) (filter []string, setAt time.Time, workspaceID string, ok bool) {
	snap, loaded := p.store.Current()
	if !loaded {
		return nil, time.Time{}, "", false
	}
	row, ok := snap.VendorFilters[vendor]
	return row.Filter, row.SetAt, row.WorkspaceID, ok
}

// nullableString returns nil for an empty string so it serialises as JSON
// null rather than "" (used for /health's last_error field).
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
