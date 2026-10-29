package provider

import "strings"

// ReasoningMode is what a backend does with the OpenAI `reasoning_effort`
// parameter for ONE model. It is decided per model, not per backend: the same
// Anthropic key serves models that take extended thinking and models that
// reject it.
type ReasoningMode int

const (
	// ReasoningNone: the model has no equivalent. The handler drops the
	// parameter and records the drop on the dropped-params seam.
	ReasoningNone ReasoningMode = iota
	// ReasoningForward: forwarded verbatim (OpenAI).
	ReasoningForward
	// ReasoningThinking: translated to Anthropic extended thinking
	// ({"type":"enabled","budget_tokens":N}), directly or via Bedrock.
	ReasoningThinking
)

// Thinking budgets for each reasoning_effort level, in tokens. Anthropic's
// minimum budget is 1024 and the budget must be below max_tokens.
const (
	ThinkingBudgetLow    = 1024
	ThinkingBudgetMedium = 4096
	ThinkingBudgetHigh   = 16384
)

// ValidReasoningEffort reports whether e is a reasoning_effort value OpenAI
// defines.
func ValidReasoningEffort(e string) bool {
	switch e {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	}
	return false
}

// ThinkingBudget maps a reasoning_effort to an extended-thinking budget.
// "none" is 0: thinking stays off (the Anthropic default), which is what the
// caller asked for, not a drop. "minimal" is the smallest budget Anthropic
// accepts (same as low); "xhigh" has no bigger budget than high.
func ThinkingBudget(effort string) int {
	switch strings.ToLower(effort) {
	case "minimal", "low":
		return ThinkingBudgetLow
	case "medium":
		return ThinkingBudgetMedium
	case "high", "xhigh":
		return ThinkingBudgetHigh
	}
	return 0
}

// ReasoningPlan is a backend's answer for one (model, effort, max_tokens).
type ReasoningPlan struct {
	Mode ReasoningMode
	// Budget and MaxTokens are set for ReasoningThinking only: the
	// budget_tokens to send and the max_tokens to send with it
	// (MaxTokens > Budget, clamped to the model's own output limit).
	Budget    int
	MaxTokens int
}

// ReasoningCapable is implemented by a provider that can honour (forward or
// translate) reasoning_effort. maxTokens is the caller's effective value, 0
// meaning "the backend default". A provider that does not implement it gets
// ReasoningNone.
type ReasoningCapable interface {
	PlanReasoning(model, effort string, maxTokens int) ReasoningPlan
}

// PlanReasoning returns p's plan, ReasoningNone when p reports none.
func PlanReasoning(p Provider, model, effort string, maxTokens int) ReasoningPlan {
	if rc, ok := p.(ReasoningCapable); ok {
		return rc.PlanReasoning(model, effort, maxTokens)
	}
	return ReasoningPlan{}
}

// PlanThinking is the shared max_tokens arithmetic for the two Anthropic
// paths. visible is the caller's effective max_tokens (already defaulted);
// ceiling is the model's output limit, 0 when unknown. max_tokens is ALWAYS
// visible+budget, capped at the ceiling: budget_tokens is a target, not a
// reservation, so the response may use all of it. A visible value already at
// or above the ceiling is never lowered. If what remains leaves no room for
// the minimum budget, ok is false and thinking must be treated as
// unsupported.
func PlanThinking(visible, budget, ceiling int) (maxTokens, newBudget int, ok bool) {
	maxTokens = visible + budget
	if ceiling > 0 && maxTokens > ceiling {
		maxTokens = ceiling
		if maxTokens < visible {
			maxTokens = visible
		}
	}
	newBudget = budget
	if newBudget >= maxTokens {
		newBudget = maxTokens - 1
	}
	if newBudget < ThinkingBudgetLow {
		return 0, 0, false
	}
	return maxTokens, newBudget, true
}
