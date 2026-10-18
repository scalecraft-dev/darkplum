package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// normalizedRequest is the result of normalizeMessages: a hoisted system
// prompt (may be empty) plus a strictly alternating user/assistant turn
// sequence ready to hand to the Anthropic SDK.
type normalizedRequest struct {
	System string
	Turns  []turn
}

// turn is one normalized user or assistant turn. Content is the text part;
// ToolUses is set on assistant turns, ToolResults on user turns.
type turn struct {
	Role        string
	Content     string
	ToolUses    []toolUse
	ToolResults []toolResult
}

type toolUse struct {
	ID    string
	Name  string
	Input json.RawMessage // always a JSON object
}

type toolResult struct {
	ToolUseID string
	Content   string
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
//  4. Tool calling: an assistant turn with tool_calls is kept even when its
//     text is empty, and carries one parsed tool_use per call (arguments that
//     are not a JSON object are rejected as invalid_request). A "tool" turn
//     becomes a tool_result on a USER turn; consecutive tool turns and the
//     user text that follows them merge into ONE user turn, tool_results
//     first (the Messages API requires every tool_result for the preceding
//     assistant tool_use blocks to lead the next user message). Any other role
//     is rejected (invalid_request).
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
	turns := make([]turn, 0, len(messages))

	for i, m := range messages {
		switch m.Role {
		case "system":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			systemParts = append(systemParts, m.Content)
		case "user":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			turns = append(turns, turn{Role: "user", Content: m.Content})
		case "assistant":
			t := turn{Role: "assistant", Content: m.Content}
			for j, tc := range m.ToolCalls {
				input, err := parseToolArguments(tc.Function.Arguments)
				if err != nil {
					return normalizedRequest{}, newInvalidRequestError(
						fmt.Sprintf("messages[%d].tool_calls[%d].function.arguments: %s", i, j, err),
					)
				}
				t.ToolUses = append(t.ToolUses, toolUse{ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			if strings.TrimSpace(t.Content) == "" && len(t.ToolUses) == 0 {
				continue
			}
			turns = append(turns, t)
		case "tool":
			turns = append(turns, turn{
				Role:        "user",
				ToolResults: []toolResult{{ToolUseID: m.ToolCallID, Content: m.Content}},
			})
		default:
			return normalizedRequest{}, newInvalidRequestError(
				"message role " + quote(m.Role) + " is not supported",
			)
		}
	}

	merged := make([]turn, 0, len(turns))
	for _, t := range turns {
		if n := len(merged); n > 0 && merged[n-1].Role == t.Role {
			prev := &merged[n-1]
			switch {
			case strings.TrimSpace(t.Content) == "":
			case strings.TrimSpace(prev.Content) == "":
				prev.Content = t.Content
			default:
				prev.Content = prev.Content + "\n\n" + t.Content
			}
			prev.ToolUses = append(prev.ToolUses, t.ToolUses...)
			prev.ToolResults = append(prev.ToolResults, t.ToolResults...)
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

// parseToolArguments parses a tool call's JSON arguments string into the
// object a tool_use block's input must be. Empty arguments mean no arguments.
func parseToolArguments(args string) (json.RawMessage, error) {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
		return nil, errors.New("must be a JSON object")
	}
	return json.RawMessage(args), nil
}
