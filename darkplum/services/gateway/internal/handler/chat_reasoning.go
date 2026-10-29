package handler

import (
	"fmt"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// ctxKeyDroppedParams is the gin context key holding the []paramAction of
// request parameters the gateway accepted but did not pass through as sent
// (dropped, or altered such as max_tokens raised). It is the seam a later
// phase turns into a response header and a usage-log column; today it is only
// set, never rendered. Always written through recordParamActions.
const ctxKeyDroppedParams = "dropped_params"

// paramAction is one entry on the seam. Action is "dropped" or "raised".
type paramAction struct {
	Param, Action string
}

const (
	actionDropped = "dropped"
	actionRaised  = "raised"
)

// recordParamActions appends to the request's seam; it never overwrites what
// an earlier step recorded.
func recordParamActions(c *gin.Context, acts []paramAction) {
	if len(acts) == 0 {
		return
	}
	prev, _ := c.Get(ctxKeyDroppedParams)
	existing, _ := prev.([]paramAction)
	c.Set(ctxKeyDroppedParams, append(append([]paramAction(nil), existing...), acts...))
}

// reasoningDecision is what the handler does with reasoning_effort (and the
// sampling parameters extended thinking forbids) for one request.
type reasoningDecision struct {
	// Effort is the value to forward verbatim (OpenAI); "" otherwise.
	Effort string
	// Budget > 0 turns extended thinking on; MaxTokens is the max_tokens to
	// send with it. ClearTemperature / ClearTopP say the caller's value must
	// not reach the backend.
	Budget    int
	MaxTokens int
	// Dropped names every parameter accepted but not honoured, in a fixed
	// order.
	Dropped []paramAction
	// RefuseParam/RefuseReason: a contract refusal (400
	// unsupported_parameter).
	RefuseParam, RefuseReason string

	clearTemperature, clearTopP bool
}

// historyHasToolTurn reports whether the conversation already contains an
// assistant tool call or a tool result. Anthropic requires the signed
// thinking blocks of such a turn to be passed back with it; an OpenAI caller
// never sends them, so any turn following a tool call runs with thinking off.
func historyHasToolTurn(msgs []provider.Message) bool {
	for _, m := range msgs {
		if m.Role == "tool" || m.Role == "function" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// forcedToolChoice reports tool_choice "required" or a named function with
// tools present.
func forcedToolChoice(req chatCompletionRequest) bool {
	if len(req.Tools) == 0 {
		return false
	}
	kind, _, _ := provider.ToolChoiceKind(req.ToolChoice)
	return kind == "required" || kind == "function"
}

// planReasoning decides reasoning_effort for the resolved backend and model.
//
//   - openai forwards it.
//   - Anthropic and Bedrock-served Claude translate it to extended thinking
//     when the MODEL supports it (the provider decides per model); otherwise
//     it is dropped and recorded.
//   - every other backend drops it and records it.
//   - "none" asks for no reasoning: nothing is sent and nothing is recorded as
//     dropped, except that OpenAI is told "none" because there it is a real
//     setting.
//   - With thinking on: temperature is dropped (recorded), top_p below 0.95 is
//     dropped (recorded), top_k is dropped (recorded) -- the Messages API
//     rejects all three alongside thinking.
//   - A forced tool_choice cannot be combined with thinking: that is a
//     contract refusal naming reasoning_effort, because dropping the
//     reasoning would hand back a different answer than asked for.
//   - A history that already contains a tool call/result turns thinking off
//     (recorded).
func planReasoning(req chatCompletionRequest, prov provider.Provider, model string, maxTokens int) reasoningDecision {
	var d reasoningDecision
	if req.ReasoningEffort == nil {
		return d
	}
	effort := *req.ReasoningEffort
	plan := provider.PlanReasoning(prov, model, effort, maxTokens)

	switch plan.Mode {
	case provider.ReasoningForward:
		d.Effort = effort
	case provider.ReasoningThinking:
		if effort == "none" || plan.Budget == 0 {
			return d
		}
		switch {
		case historyHasToolTurn(req.Messages):
			d.Dropped = append(d.Dropped, paramAction{"reasoning_effort", actionDropped})
		case forcedToolChoice(req):
			d.RefuseParam = "reasoning_effort"
			d.RefuseReason = "extended thinking cannot be combined with a required or named tool_choice; use tool_choice auto or none, or omit reasoning_effort"
		default:
			d.Budget, d.MaxTokens = plan.Budget, plan.MaxTokens
			if plan.MaxTokens != maxTokens {
				d.Dropped = append(d.Dropped, paramAction{"max_tokens", actionRaised})
			}
			if req.Temperature != nil {
				d.clearTemperature = true
				d.Dropped = append(d.Dropped, paramAction{"temperature", actionDropped})
			}
			if req.TopP != nil && *req.TopP < 0.95 {
				d.clearTopP = true
				d.Dropped = append(d.Dropped, paramAction{"top_p", actionDropped})
			}
			if req.TopK != nil {
				d.Dropped = append(d.Dropped, paramAction{"top_k", actionDropped})
			}
		}
	default:
		if effort != "none" {
			d.Dropped = append(d.Dropped, paramAction{"reasoning_effort", actionDropped})
		}
	}
	return d
}

// apply puts the decision onto the outbound request.
func (d reasoningDecision) apply(r *provider.ChatRequest) {
	r.ReasoningEffort = d.Effort
	if d.Budget > 0 {
		r.ThinkingBudget = d.Budget
		r.MaxTokens = d.MaxTokens
	}
	if d.clearTemperature {
		r.Temperature = nil
	}
	if d.clearTopP {
		r.TopP = nil
	}
}

// invalidReasoningEffort is the 400 invalid_request text for a
// reasoning_effort OpenAI does not define.
func invalidReasoningEffort(v string) string {
	return fmt.Sprintf("reasoning_effort %q is not one of none, minimal, low, medium, high, xhigh", v)
}
