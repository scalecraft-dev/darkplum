package anthropic

import (
	"strings"

	"github.com/scalecraft/darkplum/gateway/internal/provider"
)

// normalizedRequest is the result of normalizeMessages: a hoisted system
// prompt (may be empty) plus a strictly alternating user/assistant turn
// sequence ready to hand to the Anthropic SDK.
type normalizedRequest struct {
	System string
	Turns  []provider.Message
}

// normalizeMessages converts Darkplum's flat OpenAI-shaped message list into
// the shape the Messages API requires: a single system string plus a
// messages array that must start with a user turn and never end with an
// assistant turn.
//
// Rules (pure, deterministic, table-tested):
//
//  1. Every message with role "system" is hoisted out, in the order
//     encountered, and joined with a blank line into System — regardless of
//     where it appeared relative to the user/assistant turns.
//  2. Among the remaining turns, any whose content is empty after trimming
//     whitespace is dropped.
//  3. Consecutive turns of the same role are merged, joining their content
//     with a blank line — mirroring the Messages API's own documented
//     behaviour ("Consecutive user or assistant turns ... will be combined
//     into a single turn"), so a merge Darkplum performs and a merge the API
//     would have performed anyway never disagree about what the model reads.
//  4. Any role other than "system", "user", or "assistant" is rejected
//     (invalid_request) — request-shape honesty for tool/function-result
//     turns is enforced earlier, at the handler (chat.go's
//     unsupportedRequestParam), so this should never actually fire in
//     practice; it exists so an unrecognised role fails loudly here too
//     rather than being silently sent to the vendor as some other role.
//  5. The turn sequence must be non-empty and start with "user". A leading
//     assistant turn is rejected (invalid_request) rather than silently
//     dropped or given a synthetic user turn: darkplum-api always prepends a
//     system prompt but never a canned assistant greeting into conversation
//     history (a new conversation's message list starts empty; the first
//     wire turn is always the caller's own user message — see
//     services/api/internal/handler/chat.go and
//     services/web/src/lib/conversations.ts), so this rule cannot break
//     webchat; it can only catch a malformed direct API-key request.
//  6. The turn sequence must not end with "assistant" — a trailing assistant
//     turn is a prefill request, which current Claude models reject, and
//     darkplum-api/webchat never sends one (toWireMessages sends exactly the
//     stored history plus the new user turn, never a placeholder assistant
//     turn — see services/web/src/pages/ChatPage.tsx). Rejected here with the
//     same invalid_request shape rather than left for the vendor to reject
//     with a raw error message that might not be safe to relay verbatim.
func normalizeMessages(messages []provider.Message) (normalizedRequest, error) {
	var systemParts []string
	turns := make([]provider.Message, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			systemParts = append(systemParts, m.Content)
		case "user", "assistant":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			turns = append(turns, m)
		default:
			return normalizedRequest{}, newInvalidRequestError(
				"message role " + quote(m.Role) + " is not supported",
			)
		}
	}

	merged := make([]provider.Message, 0, len(turns))
	for _, t := range turns {
		if n := len(merged); n > 0 && merged[n-1].Role == t.Role {
			merged[n-1].Content = merged[n-1].Content + "\n\n" + t.Content
			continue
		}
		merged = append(merged, t)
	}

	if len(merged) == 0 {
		return normalizedRequest{}, newInvalidRequestError("at least one user or assistant message is required")
	}
	if merged[0].Role != "user" {
		return normalizedRequest{}, newInvalidRequestError("conversation must start with a user message")
	}
	if merged[len(merged)-1].Role != "user" {
		return normalizedRequest{}, newInvalidRequestError("conversation must not end with an assistant message (prefill is not supported)")
	}

	return normalizedRequest{
		System: strings.Join(systemParts, "\n\n"),
		Turns:  merged,
	}, nil
}

// quote wraps s in double quotes for a readable, unambiguous error message
// (avoids pulling in fmt.Sprintf("%q", ...) at every call site).
func quote(s string) string {
	return `"` + s + `"`
}
