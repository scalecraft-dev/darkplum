package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all runtime configuration for the gateway, loaded from environment variables.
type Config struct {
	Port         string
	VLLMUpstream string
	DatabaseURL  string

	BedrockEnabled bool
	BedrockRegion  string

	LMStudioEnabled  bool
	LMStudioUpstream string

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

	devMode := false
	if v := os.Getenv("DEV_MODE"); v != "" {
		var err error
		devMode, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("DEV_MODE must be a boolean (true/false/1/0), got %q", v)
		}
	}

	return &Config{
		Port:             port,
		VLLMUpstream:     upstream,
		DatabaseURL:      dbURL,
		BedrockEnabled:   bedrockEnabled,
		BedrockRegion:    bedrockRegion,
		LMStudioEnabled:  lmStudioEnabled,
		LMStudioUpstream: lmStudioUpstream,
		DevMode:          devMode,
	}, nil
}
