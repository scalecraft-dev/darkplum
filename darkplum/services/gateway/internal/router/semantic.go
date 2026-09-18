package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
)

// healthProbeCacheTTL bounds how often SemanticRouterClassifier.HealthStatus
// re-probes the configured URL for /health — a cheap cached probe, not a
// per-health-call round trip (see the amendment: "do not probe it on every
// health call").
const healthProbeCacheTTL = 30 * time.Second

// SemanticRouterClassifier calls an in-cluster vLLM Semantic Router
// (https://vllm-sr.ai) classification API to pick a route: CPU-only intent
// classification, not an LLM inference — the caller never writes a second
// (purpose="routing") audit row for this classifier kind; router_reason and
// latency on the main row are the only audit trail.
//
// Request/response shape: POST {url}/api/v1/classify/intent with
// {"text": "<truncated last user turn>"}, expecting
// {"classification": {"category": "...", "confidence": 0.0-1.0}}. This
// mirrors the v0.2 API docs (https://vllm-sr.ai/docs/v0.2/api/classification/)
// as best captured without live access to them; the wire shape is isolated
// entirely in classifyIntent/semanticClassifyResponse so it is a one-file
// change to adjust if the deployed version differs.
type SemanticRouterClassifier struct {
	// url is SEMANTIC_ROUTER_URL with any trailing slash trimmed. Empty
	// means unconfigured: every Classify call degrades to default_route with
	// ReasonFallbackClassifierInvalid, and HealthStatus reports
	// "unconfigured" without ever dialling anything.
	url           string
	minConfidence float64
	client        *http.Client
	br            *breaker

	healthMu       sync.Mutex
	healthStatus   string
	healthExpireAt time.Time
}

// NewSemanticRouterClassifier constructs a SemanticRouterClassifier. url is
// SEMANTIC_ROUTER_URL (empty disables it — see url doc above); minConfidence
// is SEMANTIC_ROUTER_MIN_CONFIDENCE (default 0.6); timeout/breakerFailures/
// breakerOpenDuration are the CLASSIFIER_TIMEOUT_MS/
// CLASSIFIER_BREAKER_FAILURES/CLASSIFIER_BREAKER_OPEN_SECONDS knobs.
func NewSemanticRouterClassifier(url string, minConfidence float64, timeout time.Duration, breakerFailures int, breakerOpenDuration time.Duration) *SemanticRouterClassifier {
	return &SemanticRouterClassifier{
		url:           strings.TrimRight(url, "/"),
		minConfidence: minConfidence,
		client:        &http.Client{Timeout: timeout},
		br:            newBreaker("semantic router", breakerFailures, breakerOpenDuration),
	}
}

// Classify implements Classifier.
func (s *SemanticRouterClassifier) Classify(ctx context.Context, alias policy.AliasDef, in Input) (Decision, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return Decision{}, ErrAborted
	}
	if s.url == "" {
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackClassifierInvalid}, nil
	}
	if s.br.open() {
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackCircuitOpen}, nil
	}

	category, confidence, callErr := s.classifyIntent(ctx, in.Text)
	decision, abortErr := decideFromSemanticResult(alias, category, confidence, s.minConfidence, callErr)
	if abortErr != nil {
		return decision, abortErr
	}
	s.br.recordOutcome(callErr)
	return decision, nil
}

// Status reports the semantic router classifier's circuit-breaker state for
// /health.
func (s *SemanticRouterClassifier) Status() (breakerOpen bool, consecutiveFailures int) {
	return s.br.status()
}

// HealthStatus reports "unconfigured" (SEMANTIC_ROUTER_URL unset),
// "configured" (a recent probe succeeded, or none has run yet), or
// "unreachable" (the most recent cached probe failed). Cached for
// healthProbeCacheTTL so /health never triggers a network call on every
// request.
func (s *SemanticRouterClassifier) HealthStatus(ctx context.Context) string {
	if s.url == "" {
		return "unconfigured"
	}

	s.healthMu.Lock()
	if time.Now().Before(s.healthExpireAt) {
		status := s.healthStatus
		s.healthMu.Unlock()
		return status
	}
	s.healthMu.Unlock()

	status := "configured"
	if !s.probeReachable(ctx) {
		status = "unreachable"
	}

	s.healthMu.Lock()
	s.healthStatus = status
	s.healthExpireAt = time.Now().Add(healthProbeCacheTTL)
	s.healthMu.Unlock()
	return status
}

// probeReachable makes a lightweight best-effort request to confirm the
// semantic router service is reachable at all — any HTTP response, even a
// non-2xx one, counts as reachable ("configured"); only a transport-level
// failure (dial/timeout/DNS) counts as "unreachable".
func (s *SemanticRouterClassifier) probeReachable(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, s.url+"/", nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// semanticClassifyRequest is the request body for POST
// {url}/api/v1/classify/intent.
type semanticClassifyRequest struct {
	Text string `json:"text"`
}

// semanticClassifyResponse is the expected response shape. See
// SemanticRouterClassifier's doc comment for the assumption this is built
// against.
type semanticClassifyResponse struct {
	Classification struct {
		Category   string  `json:"category"`
		Confidence float64 `json:"confidence"`
	} `json:"classification"`
}

// classifyIntent performs the classification HTTP call, isolating the wire
// shape (request encoding, response decoding) from the breaker/fallback
// logic in Classify so it is a small, self-contained thing to adjust and to
// test with httptest.
func (s *SemanticRouterClassifier) classifyIntent(ctx context.Context, text string) (category string, confidence float64, err error) {
	body, err := json.Marshal(semanticClassifyRequest{Text: text})
	if err != nil {
		return "", 0, fmt.Errorf("semantic router: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/api/v1/classify/intent", bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("semantic router: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		// Preserves context.DeadlineExceeded/context.Canceled via net/http's
		// *url.Error wrapping — errors.Is still matches through it.
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("semantic router: classify returned %d", resp.StatusCode)
	}

	var out semanticClassifyResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("semantic router: decode response: %w", err)
	}
	return out.Classification.Category, out.Classification.Confidence, nil
}

// decideFromSemanticResult maps the outcome of one classification call
// (category/confidence, or the error classifyIntent returned) to a Decision.
// Pure — no I/O — so timeout/low-confidence/unknown-category/canceled/
// success behaviour is directly testable with fabricated inputs, without
// httptest.
func decideFromSemanticResult(alias policy.AliasDef, category string, confidence, minConfidence float64, callErr error) (Decision, error) {
	if errors.Is(callErr, context.Canceled) {
		return Decision{}, ErrAborted
	}
	if callErr != nil {
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackTimeout}, nil
	}
	if confidence < minConfidence {
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackLowConfidence}, nil
	}
	if name, ok := matchRouteName(alias.Routes, category); ok {
		return Decision{RouteName: name, Reason: "semantic:" + name}, nil
	}
	return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackUnknownCategory}, nil
}

var _ Classifier = (*SemanticRouterClassifier)(nil)
