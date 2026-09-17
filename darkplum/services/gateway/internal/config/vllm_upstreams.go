package config

import (
	"fmt"
	"regexp"
	"strings"
)

// reservedUpstreamName is the instance name reserved for the primary
// VLLM_UPSTREAM ("vllm-default"'s bare name); an extra upstream cannot claim it.
const reservedUpstreamName = "default"

var vllmUpstreamNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// VLLMUpstream is one extra named vLLM instance configured via VLLM_UPSTREAMS.
type VLLMUpstream struct {
	Name string
	URL  string
}

// parseVLLMUpstreams parses VLLM_UPSTREAMS="name=url,name2=url2" into a list
// of extra vLLM upstreams beyond the primary VLLM_UPSTREAM. Each name must
// match vllmUpstreamNameRE, must be unique, and must not be "default"
// (reserved for the primary vllm-default instance). Each URL is validated
// with ValidateUpstreamURL so extra upstreams get the same loopback/link-local
// production guard as the primary one.
func parseVLLMUpstreams(raw string, devMode bool) ([]VLLMUpstream, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	seen := make(map[string]bool)
	var upstreams []VLLMUpstream
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("VLLM_UPSTREAMS: entry %q must be in name=url form", pair)
		}
		name := strings.TrimSpace(parts[0])
		url := strings.TrimSpace(parts[1])

		if !vllmUpstreamNameRE.MatchString(name) {
			return nil, fmt.Errorf("VLLM_UPSTREAMS: invalid name %q (must match %s)", name, vllmUpstreamNameRE.String())
		}
		if name == reservedUpstreamName {
			return nil, fmt.Errorf("VLLM_UPSTREAMS: name %q is reserved for the primary VLLM_UPSTREAM", reservedUpstreamName)
		}
		if seen[name] {
			return nil, fmt.Errorf("VLLM_UPSTREAMS: duplicate name %q", name)
		}
		seen[name] = true

		if url == "" {
			return nil, fmt.Errorf("VLLM_UPSTREAMS: entry %q has an empty URL", pair)
		}
		if err := ValidateUpstreamURL(url, devMode); err != nil {
			return nil, fmt.Errorf("VLLM_UPSTREAMS: %w", err)
		}

		upstreams = append(upstreams, VLLMUpstream{Name: name, URL: url})
	}
	return upstreams, nil
}
