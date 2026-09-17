package config

import (
	"fmt"
	"net"
	"net/url"
)

// loopbackNets is the set of CIDR ranges considered loopback.
var loopbackNets = mustParseCIDRs([]string{
	"127.0.0.0/8",
	"::1/128",
})

// linkLocalNets is the set of CIDR ranges considered link-local.
var linkLocalNets = mustParseCIDRs([]string{
	"169.254.0.0/16",
	"fe80::/10",
})

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("config: invalid built-in CIDR %q: %v", cidr, err))
		}
		nets = append(nets, ipnet)
	}
	return nets
}

// ValidateUpstreamURL checks that rawURL is a valid upstream for an
// openai-compatible provider. It rejects loopback and link-local addresses
// unless devMode is true.
//
// This validation runs at startup so the gateway fails fast with a clear error
// rather than crash-looping silently when a misconfigured URL is deployed.
func ValidateUpstreamURL(rawURL string, devMode bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid upstream URL %q: %w", rawURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("upstream URL %q must include scheme and host", rawURL)
	}

	hostname := u.Hostname()

	// "localhost" resolves to loopback — treat it directly without DNS.
	if hostname == "localhost" {
		if devMode {
			return nil
		}
		return fmt.Errorf(
			"upstream URL %q resolves to loopback (localhost); "+
				"set DEV_MODE=true to allow loopback upstreams in development",
			rawURL,
		)
	}

	ip := net.ParseIP(hostname)
	if ip == nil {
		// Non-IP hostname that isn't "localhost" — we cannot resolve it at
		// startup without DNS, so we allow it. In-cluster service names like
		// "vllm-inference.darkplum-inference.svc.cluster.local" are safe.
		return nil
	}

	for _, ipnet := range loopbackNets {
		if ipnet.Contains(ip) {
			if devMode {
				return nil
			}
			return fmt.Errorf(
				"upstream URL %q uses a loopback address (%s); "+
					"set DEV_MODE=true to allow loopback upstreams in development",
				rawURL, ip,
			)
		}
	}

	for _, ipnet := range linkLocalNets {
		if ipnet.Contains(ip) {
			if devMode {
				return nil
			}
			return fmt.Errorf(
				"upstream URL %q uses a link-local address (%s); "+
					"set DEV_MODE=true to allow link-local upstreams in development",
				rawURL, ip,
			)
		}
	}

	return nil
}
