package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// New constructs an httputil.ReverseProxy that forwards requests to upstream.
//
// The Director rewrites the incoming request's scheme and host to match the
// upstream URL and strips the Authorization header so the API key never reaches
// vLLM (which has no auth layer of its own inside the cluster).
//
// FlushInterval is set to -1 so the proxy flushes after every write, which is
// required for SSE / streaming responses from vLLM — without this, tokens
// accumulate in the response buffer and the client sees no output until the
// full completion finishes.
//
// The ErrorHandler returns 502 with an OpenAI-compatible error body. Note that
// this only fires for transport-level errors (dial failure, connection reset,
// etc.), not for upstream HTTP 4xx/5xx responses — those pass through as-is.
func New(upstream string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// Preserve the incoming path and query as-is; we're a transparent proxy.
			req.Host = target.Host
			// Strip Authorization — the key was already validated by middleware
			// and must not be forwarded to vLLM.
			req.Header.Del("Authorization")
			// Strip the internal user-attribution header — vLLM has no auth layer
			// and must not see any Darkplum-internal metadata.
			req.Header.Del("X-Darkplum-User-Id")
		},
		FlushInterval: -1, // flush immediately; required for streaming SSE
		Transport: &http.Transport{
			// Inherit default transport settings but add explicit timeouts
			// appropriate for long-running LLM completions.
			ResponseHeaderTimeout: 120 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			// OpenAI-compatible error format for transport-level failures.
			_, _ = w.Write([]byte(`{"error":{"message":"Model service unavailable","type":"server_error"}}`))
		},
	}

	return rp, nil
}
