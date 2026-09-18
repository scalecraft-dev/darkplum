// Package router decides, for one request against a routed alias
// (policy.AliasKindRouted), which of the alias's candidate routes should
// handle it. This is the "which model should answer this?" decision from a
// routed alias's ordered [{route_name, target_model_id, keywords}] list plus
// a default_route — never a retry or a swap to a different model once a
// route is chosen: handler/chat.go still runs the resolved target through
// the unchanged route/enabled/residency chain, and a forbidden pick is
// refused rather than silently redirected.
//
// Two Classifier implementations exist:
//   - RulesClassifier: ordered keyword/builtin-signal matching, no I/O.
//   - SemanticRouterClassifier: calls an in-cluster vLLM Semantic Router
//     classification API (CPU-only, not an LLM inference).
package router

import (
	"context"
	"errors"
	"strings"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
)

// Input bundles everything a Classifier needs to decide a route for one
// request.
type Input struct {
	// Text is the text to classify: the last user turn, system prompt
	// excluded, truncated via Truncate before being handed to a Classifier.
	// Both RulesClassifier's keyword matching and
	// SemanticRouterClassifier's calls operate on this value.
	Text string
	// OriginalLen is the rune length of the untruncated last-user-turn text.
	// Used for the builtin:long signal (text > 8000 chars) — Text itself is
	// always capped at Truncate's max, so checking len(Text) there would
	// never see past it.
	OriginalLen int
	// HasNonText is true when the last user turn's original content was an
	// OpenAI content-block array containing at least one non-"text" block
	// (e.g. image_url). Used for the builtin:multimodal signal — Text itself
	// has already had non-text blocks dropped by
	// provider.Message.UnmarshalJSON, so this is the one bit that flattening
	// would otherwise lose.
	HasNonText bool
}

// Decision is the result of classifying one request against a routed alias.
type Decision struct {
	// RouteName names either one of the alias's routes or its DefaultRoute.
	// Always resolvable via AliasDef.Route for any alias actually present in
	// a loaded policy.Snapshot (see policy.buildAliases's drop-if-
	// inconsistent rule).
	RouteName string
	// Reason is a one-line, closed-vocabulary explanation of why RouteName
	// was chosen. See the Reason* and Builtin* constants for every value a
	// Classifier in this package can produce.
	Reason string
}

// Classifier decides a route for one request against a routed alias. Classify
// never fails permanently — every non-abort outcome is a valid Decision
// (falling back to alias.DefaultRoute when the classifier can't produce a
// confident pick). The only error Classify ever returns is ErrAborted, when
// ctx was canceled — the caller must not dispatch to any route at all in
// that case.
type Classifier interface {
	Classify(ctx context.Context, alias policy.AliasDef, in Input) (Decision, error)
}

// ErrAborted is returned by Classify when ctx is canceled (the caller
// disconnected, or an upstream deadline fired) mid-classification. It is
// distinct from every fallback Decision: a fallback still names a route to
// dispatch to; ErrAborted means the handler must not dispatch anywhere.
var ErrAborted = errors.New("router: classification aborted, do not dispatch")

// Reason vocabulary. Every Classifier in this package produces exactly one
// of these (rules match/default reasons are built with fmt.Sprintf — see
// rules.go — but always start with "rules:"; semantic-router success
// reasons start with "semantic:").
const (
	// ReasonRulesDefault is used by RulesClassifier when no route's keywords
	// or builtin signals matched.
	ReasonRulesDefault = "rules:default"

	// ReasonFallbackClassifierInvalid is used by SemanticRouterClassifier
	// when the per-request validity check fails: SEMANTIC_ROUTER_URL is
	// unset.
	ReasonFallbackClassifierInvalid = "fallback:classifier_invalid"

	// ReasonFallbackTimeout is used by SemanticRouterClassifier when the call
	// did not produce a usable response within the configured budget (a
	// context.DeadlineExceeded) — and, since the closed vocabulary has no
	// separate bucket for a hard transport failure, for any other
	// non-canceled call error too: from the request's perspective the effect
	// (no answer, fall back to default_route) is identical either way.
	ReasonFallbackTimeout = "fallback:timeout"

	// ReasonFallbackCircuitOpen is used when the classifier's circuit
	// breaker is currently open (CLASSIFIER_BREAKER_FAILURES consecutive
	// failures within the current window) — the call is skipped entirely.
	ReasonFallbackCircuitOpen = "fallback:circuit_open"

	// ReasonFallbackLowConfidence is used by SemanticRouterClassifier when
	// the classification API returns a confidence below
	// SEMANTIC_ROUTER_MIN_CONFIDENCE.
	ReasonFallbackLowConfidence = "fallback:low_confidence"

	// ReasonFallbackUnknownCategory is used by SemanticRouterClassifier when
	// the returned category does not case-insensitively match any of the
	// alias's route names.
	ReasonFallbackUnknownCategory = "fallback:unknown_category"
)

// Reserved rules keywords bindable to a route (see rules.go). An admin binds
// one of these instead of a literal substring to route on a signal that
// isn't a literal keyword match.
const (
	BuiltinCode       = "builtin:code"
	BuiltinLong       = "builtin:long"
	BuiltinMultimodal = "builtin:multimodal"
)

// builtinLongThreshold is the character count above which BuiltinLong
// matches (Input.OriginalLen > builtinLongThreshold).
const builtinLongThreshold = 8000

// matchRouteName finds the route in routes whose Name matches name (e.g. the
// semantic router's "category"), returning the route's canonical-case Name.
// It first tries an exact case-insensitive match (strings.EqualFold,
// unchanged behaviour for callers that only differ by case); if that fails,
// it falls back to comparing normalizeRouteToken of both sides, so a
// decision like "computer science" or "Computer_Science" still matches a
// route literally named "computer-science".
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

// normalizeRouteToken normalises a route name or a classifier-provided
// category/route field for loose matching (see matchRouteName): lowercased,
// leading/trailing whitespace trimmed, and every run of spaces and
// underscores collapsed to a single dash — so "Computer Science",
// "computer_science", and "computer-science" all normalise to the same
// token. Pure — no I/O — so it is directly table-testable.
func normalizeRouteToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))

	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		if r == ' ' || r == '_' || r == '-' {
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	return strings.TrimRight(b.String(), "-")
}
