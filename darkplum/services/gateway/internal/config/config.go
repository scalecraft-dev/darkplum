package config

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
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

	bedrockModelFilter, err := parseGlobList(os.Getenv("BEDROCK_MODEL_FILTER"))
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
	}, nil
}

// parseGlobList splits a comma-separated list of path.Match glob patterns and
// validates each one compiles, so a typo in BEDROCK_MODEL_FILTER fails at
// startup instead of silently matching nothing at request time.
func parseGlobList(raw string) ([]string, error) {
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
			return nil, fmt.Errorf("BEDROCK_MODEL_FILTER: invalid glob %q: %w", p, err)
		}
		patterns = append(patterns, p)
	}
	return patterns, nil
}
