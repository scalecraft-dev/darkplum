package router

import (
	"context"
	"strings"

	"github.com/scalecraft/darkplum/gateway/internal/policy"
)

// RulesClassifier matches a routed alias's ordered routes against Input by
// literal, case-folded substring keywords and three reserved builtin
// signals. No regex, no I/O, no per-request failure mode: a RulesClassifier
// call is always synchronous and always produces a Decision (never
// ErrAborted).
type RulesClassifier struct{}

// NewRulesClassifier returns a RulesClassifier. It has no state.
func NewRulesClassifier() *RulesClassifier {
	return &RulesClassifier{}
}

// Classify implements Classifier by delegating to the pure DecideRules.
func (RulesClassifier) Classify(_ context.Context, alias policy.AliasDef, in Input) (Decision, error) {
	return DecideRules(alias, in), nil
}

var _ Classifier = RulesClassifier{}

// DecideRules is the pure rules-classification decision, exposed directly so
// ordering, builtin signals, case folding, and the no-match default can be
// tested without going through the Classifier interface.
//
// Routes are checked in alias.Routes order (model_alias_routes.position
// ascending — "lowest position wins"). Within a route, keywords are checked
// in list order; the first route with any matching keyword or builtin signal
// wins outright — no further routes are considered. No match at all falls
// back to alias.DefaultRoute with ReasonRulesDefault.
func DecideRules(alias policy.AliasDef, in Input) Decision {
	lowerText := strings.ToLower(in.Text)

	for _, route := range alias.Routes {
		for _, kw := range route.Keywords {
			if matchesKeyword(kw, in, lowerText) {
				return Decision{RouteName: route.Name, Reason: "rules:" + route.Name + ":" + kw}
			}
		}
	}

	return Decision{RouteName: alias.DefaultRoute, Reason: ReasonRulesDefault}
}

// matchesKeyword reports whether kw (already case-folded by the policy
// loader) matches the current request: one of the three reserved builtin
// tokens matches its corresponding signal; anything else is a literal,
// case-folded substring match against lowerText (the case-folded form of
// in.Text).
func matchesKeyword(kw string, in Input, lowerText string) bool {
	switch kw {
	case BuiltinCode:
		// A code fence or a unified-diff header. Checked against the
		// truncated text (in.Text), not the case-folded copy — fences and
		// diff markers are case-sensitive syntax.
		return strings.Contains(in.Text, "```") ||
			strings.Contains(in.Text, "diff --git") ||
			strings.Contains(in.Text, "@@")
	case BuiltinLong:
		// Checked against the ORIGINAL (untruncated) length: in.Text is
		// always capped at Truncate's max, so checking len(in.Text) here
		// would never observe a turn long enough to matter.
		return in.OriginalLen > builtinLongThreshold
	case BuiltinMultimodal:
		return in.HasNonText
	default:
		return strings.Contains(lowerText, kw)
	}
}
