package config

import (
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// Config holds all runtime configuration for the gateway, loaded from environment variables.
type Config struct {
	Port         string
	VLLMUpstream string
	DatabaseURL  string

	// VLLMUpstreams holds extra named vLLM instances beyond the primary
	// VLLM_UPSTREAM, parsed from VLLM_UPSTREAMS. Each participates in
	// discovery as its own routing.Source (instance name "vllm-<name>").
	VLLMUpstreams []VLLMUpstream

	BedrockEnabled bool
	BedrockRegion  string
	// BedrockModelFilter restricts which discovered Bedrock ids enter the
	// route table, as path.Match globs (e.g. "us.anthropic.*"). Empty means
	// no filter — every discovered id is routable.
	BedrockModelFilter []string

	LMStudioEnabled  bool
	LMStudioUpstream string

	// DiscoveryIntervalSeconds controls how often routing.Manager re-runs
	// discovery across all sources. DISCOVERY_INTERVAL_SECONDS, default 60.
	DiscoveryIntervalSeconds int
	// RouteLastGoodTTLSeconds caps how long a source's last-good model list
	// stays routable after its discovery starts failing. Once exceeded, the
	// source's models leave the route table (so requests get a clean 404
	// model_not_found instead of silently dialling a backend that has been
	// down for many discovery cycles). ROUTE_LAST_GOOD_TTL_SECONDS, default
	// 120 (>= 2 missed 60s discovery ticks); must be >= DiscoveryIntervalSeconds.
	//
	// This is the largest term in the "picker still offers a dead model"
	// window end to end: the api only hides a model from its picker once the
	// gateway stops listing it on GET /v1/models, and the gateway only stops
	// listing it once this TTL expires the source's last-good list. Lowering
	// it shortens how long a user can select a model that is actually down.
	RouteLastGoodTTLSeconds int
	// PolicyRefreshSeconds controls how often policy.PgStore reloads its
	// snapshot from Postgres. POLICY_REFRESH_SECONDS, default 5.
	PolicyRefreshSeconds int
	// PolicyMaxStalenessSeconds caps how old the last successful policy load
	// may be before the store fails closed (503 policy_unavailable) even
	// though a last-good snapshot is still cached in memory.
	// POLICY_MAX_STALENESS_SECONDS, default 900. 0/disabled is not allowed —
	// minimum 30.
	PolicyMaxStalenessSeconds int

	// DevMode relaxes startup validation that blocks loopback/link-local upstream
	// URLs. Set DEV_MODE=true when running locally against LM Studio or a local
	// vLLM instance. Must never be set in production.
	DevMode bool

	// ClassifierTimeoutMS bounds a routed alias's semantic-router classifier
	// call. CLASSIFIER_TIMEOUT_MS, default 500, clamped to [50, 2000].
	ClassifierTimeoutMS int
	// ClassifierBreakerFailures is the number of consecutive classifier call
	// failures that opens the circuit breaker. CLASSIFIER_BREAKER_FAILURES,
	// default 5, minimum 1.
	ClassifierBreakerFailures int
	// ClassifierBreakerOpenSeconds is how long the breaker stays open (the
	// classifier is skipped, falling back to default_route) once it trips.
	// CLASSIFIER_BREAKER_OPEN_SECONDS, default 30, minimum 5.
	ClassifierBreakerOpenSeconds int

	// SemanticRouterURL is the base URL of an in-cluster vLLM Semantic
	// Router deployment (e.g.
	// http://semantic-router.darkplum-system.svc.cluster.local:8080).
	// SEMANTIC_ROUTER_URL, empty by default — a routed alias using the
	// semantic_router classifier_kind degrades to default_route with
	// fallback:classifier_invalid until this is set.
	SemanticRouterURL string
	// SemanticRouterMinConfidence is the minimum classification confidence
	// the semantic router must report to use its picked category; below
	// this, the request falls back to default_route with
	// fallback:low_confidence. SEMANTIC_ROUTER_MIN_CONFIDENCE, default 0.6,
	// clamped to [0, 1].
	SemanticRouterMinConfidence float64

	// ProviderKeysDir is where the darkplum-provider-keys Secret is mounted
	// (files "anthropic-api-key", "openai-api-key"). PROVIDER_KEYS_DIR,
	// default /var/run/darkplum/provider-keys. A vendor whose key file
	// doesn't exist under this directory is simply not_configured — the
	// directory itself not existing at all (e.g. the Secret was never
	// created) is handled the same way, not as a startup error.
	ProviderKeysDir string
	// AnthropicModelFilter restricts which discovered Anthropic model ids
	// enter the route table, as path.Match globs. Unlike
	// BedrockModelFilter, an EMPTY filter means "no vendor models" (not
	// "every model") — see the plan's failure-isolation ruling — and an
	// INVALID glob is deliberately not validated here (see
	// parseVendorGlobList): a vendor filter typo must never be able to
	// block the gateway from starting; routing.NewAnthropicSource surfaces
	// it per-tick as a non-fatal filter_missing-equivalent discovery error
	// instead.
	AnthropicModelFilter []string
	// AnthropicModelFilterSetAt is the parsed ANTHROPIC_MODEL_FILTER_SET_AT
	// env value (RFC3339) — the "env" side of the last-writer-wins
	// comparison against a UI-installed vendor_provider_settings row's
	// filter_set_at (see routing.resolveVendorFilter). The zero time if
	// unset or unparseable: an unstamped env filter loses to ANY stamped DB
	// row, but still wins over no DB row at all.
	AnthropicModelFilterSetAt time.Time
	// AnthropicWorkspaceID is the ANTHROPIC_WORKSPACE_ID env fallback (sent as
	// the anthropic-workspace-id header), needed only for an API key created
	// at org level rather than inside a workspace. It rides with the
	// ANTHROPIC_MODEL_FILTER/_SET_AT pair under the same last-writer-wins
	// rule. "" if unset or malformed (^[A-Za-z0-9_-]{1,64}$): a bad value
	// never fails Load.
	AnthropicWorkspaceID string
	// AnthropicDefaultMaxTokens is used when a chat request doesn't set
	// max_tokens. ANTHROPIC_DEFAULT_MAX_TOKENS, default 8192.
	AnthropicDefaultMaxTokens int
	// AnthropicBaseURL overrides the Anthropic SDK's production endpoint —
	// tests and dev proxies only. ANTHROPIC_BASE_URL, empty by default (use
	// the SDK's own default). Must be https unless DevMode.
	AnthropicBaseURL string
	// OpenAIModelFilter restricts which discovered OpenAI model ids enter
	// the route table, as path.Match globs. Same "empty means no vendor
	// models" and "invalid glob is not validated here" semantics as
	// AnthropicModelFilter — see its doc comment.
	OpenAIModelFilter []string
	// OpenAIModelFilterSetAt is the parsed OPENAI_MODEL_FILTER_SET_AT env
	// value (RFC3339) — same semantics as AnthropicModelFilterSetAt.
	OpenAIModelFilterSetAt time.Time
	// OpenAIBaseURL overrides https://api.openai.com — tests and dev proxies
	// only. OPENAI_BASE_URL, empty by default. Must be https unless DevMode.
	OpenAIBaseURL string
}

// Load reads environment variables and returns a populated Config.
// DATABASE_URL is required; missing it is a fatal startup error.
func Load() (*Config, error) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required but not set")
	}

	port := os.Getenv("GATEWAY_PORT")
	if port == "" {
		port = "8080"
	}

	upstream := os.Getenv("VLLM_UPSTREAM")
	if upstream == "" {
		upstream = "http://vllm-inference.darkplum-inference.svc.cluster.local:8000"
	}

	// DevMode is computed before ValidateUpstreamURL is invoked below (for
	// VLLM_UPSTREAMS) since it governs whether loopback/link-local upstream
	// URLs are accepted.
	devMode := false
	if v := os.Getenv("DEV_MODE"); v != "" {
		var err error
		devMode, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("DEV_MODE must be a boolean (true/false/1/0), got %q", v)
		}
	}

	vllmUpstreams, err := parseVLLMUpstreams(os.Getenv("VLLM_UPSTREAMS"), devMode)
	if err != nil {
		return nil, err
	}

	bedrockEnabled := false
	if v := os.Getenv("BEDROCK_ENABLED"); v != "" {
		var err error
		bedrockEnabled, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("BEDROCK_ENABLED must be a boolean (true/false/1/0), got %q", v)
		}
	}

	bedrockRegion := os.Getenv("BEDROCK_REGION")
	if bedrockRegion == "" {
		bedrockRegion = "us-east-1"
	}

	bedrockModelFilter, err := parseGlobList("BEDROCK_MODEL_FILTER", os.Getenv("BEDROCK_MODEL_FILTER"))
	if err != nil {
		return nil, err
	}

	lmStudioEnabled := false
	if v := os.Getenv("LMSTUDIO_ENABLED"); v != "" {
		var err error
		lmStudioEnabled, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("LMSTUDIO_ENABLED must be a boolean (true/false/1/0), got %q", v)
		}
	}

	lmStudioUpstream := os.Getenv("LMSTUDIO_UPSTREAM")
	if lmStudioEnabled && lmStudioUpstream == "" {
		return nil, fmt.Errorf("LMSTUDIO_UPSTREAM is required when LMSTUDIO_ENABLED=true")
	}

	discoveryInterval := 60
	if v := os.Getenv("DISCOVERY_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("DISCOVERY_INTERVAL_SECONDS must be a positive integer, got %q", v)
		}
		discoveryInterval = n
	}

	policyRefresh := 5
	if v := os.Getenv("POLICY_REFRESH_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("POLICY_REFRESH_SECONDS must be a positive integer, got %q", v)
		}
		policyRefresh = n
	}

	// 0/disabled is deliberately not allowed: an unbounded staleness cap
	// means a policy snapshot loaded once, years ago, keeps being enforced
	// forever even through an extended Postgres outage. Minimum 30s keeps the
	// cap meaningfully larger than POLICY_REFRESH_SECONDS' default (5s) while
	// still bounding worst-case staleness to something an operator would
	// actually choose.
	policyMaxStaleness := 900
	if v := os.Getenv("POLICY_MAX_STALENESS_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 30 {
			return nil, fmt.Errorf("POLICY_MAX_STALENESS_SECONDS must be an integer >= 30 (0/disabled is not allowed), got %q", v)
		}
		policyMaxStaleness = n
	}

	// Minimum is the discovery interval: a TTL shorter than the interval that
	// refreshes it would expire a healthy, merely-not-yet-re-polled source on
	// every single tick.
	routeLastGoodTTL := 120
	if v := os.Getenv("ROUTE_LAST_GOOD_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("ROUTE_LAST_GOOD_TTL_SECONDS must be a positive integer, got %q", v)
		}
		routeLastGoodTTL = n
	}
	if routeLastGoodTTL < discoveryInterval {
		return nil, fmt.Errorf("ROUTE_LAST_GOOD_TTL_SECONDS (%d) must be >= DISCOVERY_INTERVAL_SECONDS (%d)", routeLastGoodTTL, discoveryInterval)
	}

	// classifierTimeoutMS/classifierBreakerFailures/
	// classifierBreakerOpenSeconds govern a routed alias's semantic-router
	// classifier (internal/router). Rejected (not clamped)
	// out of bounds, matching every other bounded knob in this file
	// (POLICY_MAX_STALENESS_SECONDS, ROUTE_LAST_GOOD_TTL_SECONDS): a
	// misconfigured env value should fail loudly at startup, not silently
	// run with a different value than the operator asked for.
	classifierTimeoutMS := 500
	if v := os.Getenv("CLASSIFIER_TIMEOUT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 50 || n > 2000 {
			return nil, fmt.Errorf("CLASSIFIER_TIMEOUT_MS must be an integer between 50 and 2000, got %q", v)
		}
		classifierTimeoutMS = n
	}

	classifierBreakerFailures := 5
	if v := os.Getenv("CLASSIFIER_BREAKER_FAILURES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("CLASSIFIER_BREAKER_FAILURES must be an integer >= 1, got %q", v)
		}
		classifierBreakerFailures = n
	}

	classifierBreakerOpenSeconds := 30
	if v := os.Getenv("CLASSIFIER_BREAKER_OPEN_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 {
			return nil, fmt.Errorf("CLASSIFIER_BREAKER_OPEN_SECONDS must be an integer >= 5, got %q", v)
		}
		classifierBreakerOpenSeconds = n
	}

	semanticRouterURL := strings.TrimSpace(os.Getenv("SEMANTIC_ROUTER_URL"))

	semanticRouterMinConfidence := 0.6
	if v := os.Getenv("SEMANTIC_ROUTER_MIN_CONFIDENCE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return nil, fmt.Errorf("SEMANTIC_ROUTER_MIN_CONFIDENCE must be a number between 0 and 1, got %q", v)
		}
		semanticRouterMinConfidence = f
	}

	providerKeysDir := os.Getenv("PROVIDER_KEYS_DIR")
	if providerKeysDir == "" {
		providerKeysDir = "/var/run/darkplum/provider-keys"
	}

	anthropicModelFilter := parseVendorGlobList(os.Getenv("ANTHROPIC_MODEL_FILTER"))
	anthropicModelFilterSetAt := parseVendorFilterSetAt(os.Getenv("ANTHROPIC_MODEL_FILTER_SET_AT"))

	anthropicWorkspaceID := strings.TrimSpace(os.Getenv("ANTHROPIC_WORKSPACE_ID"))
	if anthropicWorkspaceID != "" && !provider.ValidWorkspaceID(anthropicWorkspaceID) {
		log.Printf("config: WARNING ignoring malformed ANTHROPIC_WORKSPACE_ID (want ^[A-Za-z0-9_-]{1,64}$)")
		anthropicWorkspaceID = ""
	}

	anthropicDefaultMaxTokens := 8192
	if v := os.Getenv("ANTHROPIC_DEFAULT_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("ANTHROPIC_DEFAULT_MAX_TOKENS must be a positive integer, got %q", v)
		}
		anthropicDefaultMaxTokens = n
	}

	anthropicBaseURL := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	if err := provider.ValidateVendorBaseURL("ANTHROPIC_BASE_URL", anthropicBaseURL, devMode); err != nil {
		return nil, err
	}

	openAIModelFilter := parseVendorGlobList(os.Getenv("OPENAI_MODEL_FILTER"))
	openAIModelFilterSetAt := parseVendorFilterSetAt(os.Getenv("OPENAI_MODEL_FILTER_SET_AT"))

	openAIBaseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if err := provider.ValidateVendorBaseURL("OPENAI_BASE_URL", openAIBaseURL, devMode); err != nil {
		return nil, err
	}

	return &Config{
		Port:                      port,
		VLLMUpstream:              upstream,
		VLLMUpstreams:             vllmUpstreams,
		DatabaseURL:               dbURL,
		BedrockEnabled:            bedrockEnabled,
		BedrockRegion:             bedrockRegion,
		BedrockModelFilter:        bedrockModelFilter,
		LMStudioEnabled:           lmStudioEnabled,
		LMStudioUpstream:          lmStudioUpstream,
		DiscoveryIntervalSeconds:  discoveryInterval,
		PolicyRefreshSeconds:      policyRefresh,
		PolicyMaxStalenessSeconds: policyMaxStaleness,
		RouteLastGoodTTLSeconds:   routeLastGoodTTL,
		DevMode:                   devMode,

		ClassifierTimeoutMS:          classifierTimeoutMS,
		ClassifierBreakerFailures:    classifierBreakerFailures,
		ClassifierBreakerOpenSeconds: classifierBreakerOpenSeconds,
		SemanticRouterURL:            semanticRouterURL,
		SemanticRouterMinConfidence:  semanticRouterMinConfidence,

		ProviderKeysDir:           providerKeysDir,
		AnthropicModelFilter:      anthropicModelFilter,
		AnthropicModelFilterSetAt: anthropicModelFilterSetAt,
		AnthropicWorkspaceID:      anthropicWorkspaceID,
		AnthropicDefaultMaxTokens: anthropicDefaultMaxTokens,
		AnthropicBaseURL:          anthropicBaseURL,
		OpenAIModelFilter:         openAIModelFilter,
		OpenAIModelFilterSetAt:    openAIModelFilterSetAt,
		OpenAIBaseURL:             openAIBaseURL,
	}, nil
}

// parseVendorFilterSetAt parses raw (RFC3339) as a <VENDOR>_MODEL_FILTER_SET_AT
// env value. Empty or unparseable input returns the zero time rather than an
// error — like parseVendorGlobList, a vendor filter's env configuration must
// never be able to block the gateway from starting; an unstamped/malformed
// SET_AT simply loses every last-writer-wins comparison against a DB row
// (see routing.resolveVendorFilter).
func parseVendorFilterSetAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseVendorGlobList splits a comma-separated list of path.Match glob
// patterns WITHOUT validating that each one compiles — unlike parseGlobList
// (used by BEDROCK_MODEL_FILTER), which fails Load outright on a bad
// pattern. A vendor filter (ANTHROPIC_MODEL_FILTER, OPENAI_MODEL_FILTER)
// must never be able to block the gateway from starting at all: an invalid
// pattern here is instead caught per-tick by routing.ApplyGlobFilter, which
// returns a non-fatal discovery error that the vendor's routing Source
// reports through the provider status block, in a build that has otherwise
// started up normally and is still serving every self-hosted/cloud-managed
// model.
func parseVendorGlobList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var patterns []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		patterns = append(patterns, p)
	}
	return patterns
}

// parseGlobList splits a comma-separated list of path.Match glob patterns and
// validates each one compiles, so a typo in the source env var fails at
// startup instead of silently matching nothing at request time. name is the
// env var being parsed (e.g. "BEDROCK_MODEL_FILTER", "ANTHROPIC_MODEL_FILTER",
// "OPENAI_MODEL_FILTER") — named in the returned error only, so every caller
// of this shared parser gets an error pointing at the knob it actually needs
// to fix, not a hardcoded one.
func parseGlobList(name, raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var patterns []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%s: invalid glob %q: %w", name, p, err)
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
}
