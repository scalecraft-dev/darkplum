package provider

import (
	"net"
	"net/http"
	"time"
)

// NewVendorHTTPClient builds the per-provider http.Client shared by every
// direct vendor API provider (anthropic, openai): routed through the
// process's normal proxy configuration, with sane dial/TLS/response-header
// timeouts, but NO overall client Timeout — a streaming completion can
// legitimately run for minutes, and each provider's own idle watchdog
// (streamIdleTimeout) already guards against a genuinely wedged connection.
//
// Originally implemented inside internal/provider/anthropic as an unexported
// newHTTPClient function; extracted here, behaviour-preserving, once
// internal/provider/openai needed the identical construction.
func NewVendorHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
		},
	}
}
