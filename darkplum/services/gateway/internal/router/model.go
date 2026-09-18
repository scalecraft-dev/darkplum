package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/bedrock"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

// providerLookup is the subset of *provider.Registry ModelClassifier needs.
// Defined locally (rather than importing handler's identical interface) to
// avoid a router<->handler import cycle; *provider.Registry satisfies both
// structurally.
type providerLookup interface {
	Get(key string) (provider.Provider, bool)
}

// routeSnapshotter is the subset of *routing.Manager ModelClassifier needs,
// for the same reason as providerLookup.
type routeSnapshotter interface {
	Snapshot() *routing.Table
}

// ModelClassifier asks a self-hosted LLM, via the provider registry, to name
// a route for the request — the Arch-Router prompt protocol (see
// buildArchRouterMessages): a system instruction, the candidate routes as a
// JSON array of {name, description}, and the conversation as a JSON array of
// {role, content}, expecting a JSON {"route": "<name>"} response.
//
// classifier_model_id is re-validated on every single call (classifierValid)
// against the CURRENT routing table and policy snapshot — an admin can
// disable or un-self-host the classifier model at any time, and every
// in-flight routed-alias request must observe that immediately, not just at
// alias-save time.
type ModelClassifier struct {
	registry providerLookup
	routes   routeSnapshotter
	policy   policy.Store
	timeout  time.Duration
	br       *breaker
}

// NewModelClassifier constructs a ModelClassifier. timeout is
// CLASSIFIER_TIMEOUT_MS; breakerFailures/breakerOpenDuration are
// CLASSIFIER_BREAKER_FAILURES/CLASSIFIER_BREAKER_OPEN_SECONDS (validated at
// config load: timeout in [50ms,2s], failures >= 1, open duration >= 5s).
func NewModelClassifier(registry providerLookup, routes routeSnapshotter, pol policy.Store, timeout time.Duration, breakerFailures int, breakerOpenDuration time.Duration) *ModelClassifier {
	return &ModelClassifier{
		registry: registry,
		routes:   routes,
		policy:   pol,
		timeout:  timeout,
		br:       newBreaker("model classifier", breakerFailures, breakerOpenDuration),
	}
}

// Classify implements Classifier.
func (m *ModelClassifier) Classify(ctx context.Context, alias policy.AliasDef, in Input) (Decision, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return Decision{}, ErrAborted
	}

	if m.br.open() {
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackCircuitOpen}, nil
	}

	table := m.routes.Snapshot()
	snap, loaded := m.policy.Current()
	if !loaded || !classifierValid(table, snap, alias.ClassifierModel) {
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackClassifierInvalid}, nil
	}
	route, _ := table.Lookup(alias.ClassifierModel) // ok already established by classifierValid
	prov, ok := m.registry.Get(route.ProviderKey)
	if !ok {
		// Should not happen if classifierValid passed (a routable model's
		// provider is always registered before it can appear in the route
		// table — see routing.Manager), but fail into the same bucket rather
		// than dispatch nowhere.
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackClassifierInvalid}, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	temperature := 0.0
	req := provider.ChatRequest{
		Model:       alias.ClassifierModel,
		Messages:    buildArchRouterMessages(alias, in.Text),
		MaxTokens:   8,
		Temperature: &temperature,
	}

	start := time.Now()
	resp, callErr := prov.Complete(callCtx, req)
	latency := int(time.Since(start).Milliseconds())

	var rawOutput string
	if resp != nil {
		rawOutput = resp.Content
	}

	decision, abortErr := decideFromModelResult(alias, rawOutput, callErr)
	if abortErr != nil {
		// Canceled: do not touch the breaker (a client hanging up is not
		// evidence the classifier itself is unhealthy) and do not attach a
		// ModelCallInfo — the caller must not dispatch or audit this attempt.
		return decision, abortErr
	}
	m.br.recordOutcome(callErr)

	info := &ModelCallInfo{
		Model:      alias.ClassifierModel,
		Provider:   providerKind(prov),
		Region:     providerRegion(prov),
		LatencyMS:  latency,
		StatusCode: callStatusCode(callErr),
	}
	if resp != nil {
		pt, ct := resp.PromptTokens, resp.CompletionTokens
		info.PromptTokens = &pt
		info.CompletionTokens = &ct
	}
	decision.ModelCall = info
	return decision, nil
}

// Status reports the model classifier's circuit-breaker state for /health.
func (m *ModelClassifier) Status() (breakerOpen bool, consecutiveFailures int) {
	return m.br.status()
}

var _ Classifier = (*ModelClassifier)(nil)

// classifierValid is the per-request validity check a model classifier must
// satisfy: routes.Snapshot().Lookup(id).Class == self_hosted AND
// snap.Enabled[id]. Pure (table/snap are already-fetched snapshots), so it is
// directly testable without a fake registry or provider.
func classifierValid(table *routing.Table, snap policy.Snapshot, modelID string) bool {
	if modelID == "" {
		return false
	}
	route, ok := table.Lookup(modelID)
	if !ok {
		return false
	}
	return route.Class == routing.ClassSelfHosted && snap.Enabled[modelID]
}

// decideFromModelResult maps the outcome of one classifier model call (its
// raw text output, or the error prov.Complete returned) to a Decision. Pure —
// no I/O, no clock, no provider — so timeout/unparseable/canceled/success
// behaviour is directly testable with fabricated (rawOutput, callErr) pairs.
func decideFromModelResult(alias policy.AliasDef, rawOutput string, callErr error) (Decision, error) {
	if errors.Is(callErr, context.Canceled) {
		return Decision{}, ErrAborted
	}
	if callErr != nil {
		// DeadlineExceeded and any other transport failure share this
		// bucket: the closed reason vocabulary has no separate term for a
		// hard infrastructure error, only for its most common cause, and the
		// effect on the request is identical either way (fall back to
		// default_route). See router.go's ReasonFallbackTimeout doc.
		return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackTimeout}, nil
	}

	if routeField, ok := parseArchRouterOutput(rawOutput); ok {
		if name, matched := matchRouteName(alias.Routes, routeField); matched {
			return Decision{RouteName: name, Reason: "model:" + name}, nil
		}
	}
	return Decision{RouteName: alias.DefaultRoute, Reason: ReasonFallbackUnparseable}, nil
}

// archRouterSystemPrompt is the system instruction half of the Arch-Router
// prompt protocol (see buildArchRouterMessages). Works with any self-hosted
// instruct model pointed at classifier_model_id — Arch-Router-1.5B itself is
// under a non-commercial licence and must not be bundled; an operator
// deploys it (or an alternative instruct model) under their own licence.
const archRouterSystemPrompt = `You are a request router. You will be given a list of available routes, each with a name and a description, and a conversation. Choose the single route whose description best matches what the conversation needs. Respond with ONLY a JSON object of the form {"route": "<name>"} using one of the given route names exactly. If none of the routes clearly apply, respond with {"route": "other"}. Do not include any other text.`

// routeDoc is one entry of the JSON array of routes sent to the classifier
// model, per the Arch-Router prompt protocol.
type routeDoc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// conversationTurn is one entry of the JSON array of conversation turns sent
// to the classifier model, per the Arch-Router prompt protocol.
type conversationTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildArchRouterMessages builds the classifier model prompt: a system
// instruction, then a user message embedding the candidate routes and the
// (already truncated) conversation as the two JSON arrays the Arch-Router
// prompt protocol expects.
func buildArchRouterMessages(alias policy.AliasDef, text string) []provider.Message {
	docs := make([]routeDoc, 0, len(alias.Routes))
	for _, r := range alias.Routes {
		docs = append(docs, routeDoc{Name: r.Name, Description: r.Description})
	}
	routesJSON, _ := json.Marshal(docs)
	conversationJSON, _ := json.Marshal([]conversationTurn{{Role: "user", Content: text}})

	userContent := fmt.Sprintf("<routes>\n%s\n</routes>\n\n<conversation>\n%s\n</conversation>", routesJSON, conversationJSON)

	return []provider.Message{
		{Role: "system", Content: archRouterSystemPrompt},
		{Role: "user", Content: userContent},
	}
}

// parseArchRouterOutput extracts the "route" field from the first complete
// JSON object found in raw model output, tolerating any surrounding text
// (e.g. a stray leading/trailing token) a small instruct model might emit
// around the object the prompt asked for. Returns "" and false if no
// balanced JSON object with a non-empty route field is found.
func parseArchRouterOutput(raw string) (string, bool) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var out struct {
					Route string `json:"route"`
				}
				if err := json.Unmarshal([]byte(raw[start:i+1]), &out); err != nil {
					return "", false
				}
				return out.Route, out.Route != ""
			}
		}
	}
	return "", false
}

// matchRouteName finds the route in routes whose Name matches name (e.g. the
// classifier model's "route" field, or the semantic router's "category"),
// returning the route's canonical-case Name. It first tries an exact
// case-insensitive match (strings.EqualFold, unchanged behaviour for callers
// that only differ by case); if that fails, it falls back to comparing
// normalizeRouteToken of both sides, so a decision like "computer science" or
// "Computer_Science" still matches a route literally named
// "computer-science". "other" (the Arch-Router protocol's explicit no-match
// sentinel) will not match any real route name under either comparison and
// falls through to the caller's unparseable/unknown fallback, same as any
// other unmatched string.
func matchRouteName(routes []policy.RouteDef, name string) (string, bool) {
	for _, r := range routes {
		if strings.EqualFold(r.Name, name) {
			return r.Name, true
		}
	}

	normalized := normalizeRouteToken(name)
	for _, r := range routes {
		if normalizeRouteToken(r.Name) == normalized {
			return r.Name, true
		}
	}
	return "", false
}

// providerKind mirrors handler.ChatHandler's provider-kind resolution
// (Kind() when available, Name() fallback for bedrock/mock) so the
// purpose="routing" audit row's provider column matches the same convention
// as the main purpose="inference" row.
func providerKind(p provider.Provider) string {
	if kp, ok := p.(interface{ Kind() string }); ok {
		return kp.Kind()
	}
	return p.Name()
}

// providerRegion mirrors handler.ChatHandler's Bedrock region resolution.
func providerRegion(p provider.Provider) string {
	if bp, ok := p.(*bedrock.BedrockProvider); ok {
		return bp.Region()
	}
	return ""
}

// callStatusCode maps a provider.Complete outcome to an HTTP-shaped status
// for the purpose="routing" audit row: 200 on success, 504 for a budget
// timeout, 502 for any other call failure. callErr is never context.Canceled
// here — Classify returns before recording an outcome in that case.
func callStatusCode(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}
