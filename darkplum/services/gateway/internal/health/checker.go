// Package health provides upstream reachability checks for the /health endpoint.
//
// Design contract:
//   - Results are NEVER used to flip the pod to unready. The pod stays ready as
//     long as the process is alive. Upstream status is observability only.
//   - Probes are cached (defaultCacheTTL) so a busy health-check loop cannot
//     flood upstreams with requests.
//   - Each probe uses a short timeout (defaultProbeTimeout) so a hung upstream
//     cannot block the /health response.
package health

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// defaultProbeTimeout caps how long a single upstream probe may take.
	// Must be well under the caller's total health-response budget (~2s).
	defaultProbeTimeout = 1500 * time.Millisecond

	// defaultCacheTTL is how long a probe result is reused before re-probing.
	// 10s is short enough to surface failures quickly, long enough to avoid
	// hammering upstreams on frequent readiness probe intervals.
	defaultCacheTTL = 10 * time.Second
)

// UpstreamStatus is the result of a single upstream probe.
type UpstreamStatus string

const (
	// StatusOK means the upstream is reachable and has at least one model available.
	StatusOK UpstreamStatus = "ok"
	// StatusUnreachable means the upstream did not respond (TCP failure, non-2xx, timeout).
	StatusUnreachable UpstreamStatus = "unreachable"
	// StatusNoModels means the upstream is reachable (HTTP 2xx) but returned an
	// empty model list. This is "degraded" — inference requests will fall through
	// to the default provider or fail with a routing error. Check that the model
	// is loaded and the upstream /v1/models endpoint is populated.
	StatusNoModels UpstreamStatus = "degraded"
)

// upstream holds configuration and cached state for one upstream.
type upstream struct {
	name     string
	probeURL string // GET this URL to check reachability (e.g. http://host:port/v1/models)
}

// cachedResult holds a probe result and its expiry time.
type cachedResult struct {
	status    UpstreamStatus
	expiresAt time.Time
}

// Checker probes a set of registered upstreams and returns their last known
// status. Results are cached for defaultCacheTTL.
type Checker struct {
	upstreams []upstream
	client    *http.Client

	mu       sync.Mutex
	cache    map[string]cachedResult
	cacheTTL time.Duration
}

// NewChecker creates a Checker with the given named upstream probe URLs.
// Each entry is a (name, probeURL) pair — the name appears in the /health
// response body; the probeURL is called with GET to verify reachability.
func NewChecker(entries []Entry) *Checker {
	upstreams := make([]upstream, 0, len(entries))
	for _, e := range entries {
		upstreams = append(upstreams, upstream{name: e.Name, probeURL: e.ProbeURL})
	}
	return &Checker{
		upstreams: upstreams,
		client: &http.Client{
			// No redirects, no connection reuse needed for probe requests.
			Timeout: defaultProbeTimeout,
		},
		cache:    make(map[string]cachedResult),
		cacheTTL: defaultCacheTTL,
	}
}

// Entry is a (name, probeURL) pair used to register an upstream with NewChecker.
type Entry struct {
	// Name is the key that appears in the /health response body (e.g. "vllm-default").
	Name string
	// ProbeURL is a URL that should return HTTP 200 when the upstream is reachable.
	// Typically the upstream's GET /v1/models endpoint.
	ProbeURL string
}

// Check returns the current status of all registered upstreams. Results are
// served from cache when fresh; stale entries trigger a fresh probe.
//
// Probes run concurrently. The overall call is bounded by defaultProbeTimeout
// plus scheduling overhead — well under the ~2s health-response budget.
func (c *Checker) Check(ctx context.Context) map[string]UpstreamStatus {
	if len(c.upstreams) == 0 {
		return map[string]UpstreamStatus{}
	}

	type result struct {
		name   string
		status UpstreamStatus
	}

	resultCh := make(chan result, len(c.upstreams))

	for _, u := range c.upstreams {
		u := u // capture loop variable

		// Check cache first (under lock).
		c.mu.Lock()
		cached, ok := c.cache[u.name]
		c.mu.Unlock()

		if ok && time.Now().Before(cached.expiresAt) {
			// Cache hit — no probe needed.
			resultCh <- result{name: u.name, status: cached.status}
			continue
		}

		// Cache miss or expired — probe concurrently.
		go func() {
			status := c.probe(ctx, u.probeURL)

			c.mu.Lock()
			c.cache[u.name] = cachedResult{
				status:    status,
				expiresAt: time.Now().Add(c.cacheTTL),
			}
			c.mu.Unlock()

			resultCh <- result{name: u.name, status: status}
		}()
	}

	out := make(map[string]UpstreamStatus, len(c.upstreams))
	for range c.upstreams {
		r := <-resultCh
		out[r.name] = r.status
	}
	return out
}

// probe performs a single HTTP GET against probeURL (expected to be the
// upstream's /v1/models endpoint).
//
// Return values:
//   - StatusUnreachable: TCP failure, timeout, or non-2xx HTTP status.
//   - StatusNoModels: 2xx response but the decoded model list is empty. This
//     means the upstream process is alive but has not loaded any models yet.
//     Inference requests routed to this upstream will fail or fall through to
//     the default provider.
//   - StatusOK: 2xx response with at least one model in the list (or the body
//     could not be decoded as a model list — we give the benefit of the doubt
//     to avoid false degraded alerts for non-standard /v1/models responses).
func (c *Checker) probe(ctx context.Context, probeURL string) UpstreamStatus {
	// Use a child context capped at the probe timeout so a slow upstream cannot
	// block beyond our budget even if the caller's ctx has a longer deadline.
	probeCtx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return StatusUnreachable
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return StatusUnreachable
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return StatusUnreachable
	}

	// Upstream is reachable. Attempt to decode the model list to distinguish
	// "up and has models" (ok) from "up but empty" (degraded). Cap the read at
	// 64 KiB — we only need the model list, not a full response body.
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		// Can't read body — treat as ok (reachable is the important signal).
		return StatusOK
	}

	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bodyBytes, &modelsResp); err != nil {
		// Body did not decode as a model list — non-standard upstream or probe
		// URL that returned plain 200. Don't penalise; report ok.
		return StatusOK
	}

	if len(modelsResp.Data) == 0 {
		return StatusNoModels
	}
	return StatusOK
}
