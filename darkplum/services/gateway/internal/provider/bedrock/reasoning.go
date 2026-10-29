package bedrock

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// bedrockThinkingMaxTokens is the highest max_tokens this provider will raise
// a request to for extended thinking. Converse offers no per-model output
// limit at discovery, and the smallest limit among thinking-capable Claude
// models on Bedrock (Opus 4 / 4.1) is 32000; staying under it keeps a raised
// request valid on all of them. A caller's own larger max_tokens is never
// lowered.
const bedrockThinkingMaxTokens = 32000

// claudeFamily matches an Anthropic Claude model id as Bedrock spells it,
// including regional / global inference-profile prefixes
// ("us.anthropic.claude-sonnet-4-5-20250929-v1:0",
// "global.anthropic.claude-opus-4-1-...").
var claudeNewFamily = regexp.MustCompile(`(?:^|[./])anthropic\.claude-(?:sonnet|opus|haiku)-(\d+)`)

// The "/" anchor lets a foundation-model ARN
// (arn:aws:bedrock:...:foundation-model/anthropic.claude-...) match. An
// application inference profile ARN carries an opaque id, so it does not match
// and the parameter is dropped (recorded).
//
// supportsThinking reports whether a Bedrock model id is an Anthropic Claude
// model that takes extended thinking with budget_tokens: Claude 3.7 Sonnet and
// the Claude 4 generation onward (family-then-version naming, major >= 4). major >= 4 assumes every
// Claude 4+ model accepts thinking type enabled with budget_tokens.
// Converse discovery carries no capability data, so this is decided from the
// id; anything it does not recognise (Claude 3 / 3.5, every non-Anthropic
// model) is unsupported and the parameter is dropped, never sent.
func supportsThinking(modelID string) bool {
	id := strings.ToLower(modelID)
	if strings.Contains(id, "anthropic.claude-3-7-sonnet") {
		return true
	}
	m := claudeNewFamily.FindStringSubmatch(id)
	if m == nil {
		return false
	}
	major, err := strconv.Atoi(m[1])
	return err == nil && major >= 4
}

// PlanReasoning implements provider.ReasoningCapable (see
// anthropic.Provider.PlanReasoning); the model is decided by supportsThinking.
func (p *BedrockProvider) PlanReasoning(model, effort string, maxTokens int) provider.ReasoningPlan {
	budget := provider.ThinkingBudget(effort)
	if budget == 0 {
		return provider.ReasoningPlan{Mode: provider.ReasoningThinking}
	}
	if !supportsThinking(model) {
		return provider.ReasoningPlan{}
	}
	visible := maxTokens
	if visible <= 0 {
		visible = defaultMaxTokens
	}
	max, budget, ok := provider.PlanThinking(visible, budget, bedrockThinkingMaxTokens)
	if !ok {
		return provider.ReasoningPlan{}
	}
	return provider.ReasoningPlan{Mode: provider.ReasoningThinking, Budget: budget, MaxTokens: max}
}

// additionalModelRequestFields is the Converse additionalModelRequestFields
// document for req: {"thinking":{"type":"enabled","budget_tokens":N}} when
// extended thinking is on, nil otherwise. A budget that does not fit under
// max_tokens is not sent (Anthropic would 400 it).
func additionalModelRequestFields(req provider.ChatRequest) document.Interface {
	if !thinkingOn(req) {
		return nil
	}
	return document.NewLazyDocument(map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": req.ThinkingBudget},
	})
}

func thinkingOn(req provider.ChatRequest) bool {
	max := req.MaxTokens
	if max <= 0 {
		max = defaultMaxTokens
	}
	return req.ThinkingBudget > 0 && req.ThinkingBudget < max
}
