package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Client-executed tool calling, in the OpenAI chat-completions shape.
//
// The caller sends `tools` (function definitions) and optionally
// `tool_choice`; the model may answer with `tool_calls` on an assistant turn
// (finish_reason "tool_calls"); the caller runs the tool and sends a
// `role: tool` turn carrying the result and the matching tool_call_id. The
// gateway never executes anything: it translates these turns to and from each
// backend's native shape and relays the model's calls back verbatim.
//
// A backend advertises support through ToolCapable. A request carrying tools
// for a backend that lacks it is refused 400 unsupported_parameter AFTER route
// resolution (the handler knows the backend only then), never silently
// stripped.

// Tool is one entry of the request's `tools` array. Only "function" tools
// exist in the OpenAI shape today; the handler refuses any other type.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function a Tool describes. Parameters is the JSON
// Schema object (or null/absent for a no-argument tool), kept raw so a
// provider can hand it to its own API unchanged.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolCall is one call the model asked for, on an assistant turn (request
// history and response alike). Arguments is the raw JSON-encoded argument
// object as a string, exactly as OpenAI carries it.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction names the function and carries its JSON arguments string.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCapable is implemented by a provider that translates tools, tool_calls
// and tool-result turns for its backend. A provider that does not implement
// it, or returns false, never receives a request with tools: the handler
// refuses first.
type ToolCapable interface {
	SupportsTools() bool
}

// SupportsTools reports whether p can take a request carrying tools.
func SupportsTools(p Provider) bool {
	tc, ok := p.(ToolCapable)
	return ok && tc.SupportsTools()
}

// ToolCallDelta is one streamed fragment of a tool call in the OpenAI
// chat.completion.chunk shape: the first fragment for an index carries id,
// type and the function name (with empty arguments); later fragments for the
// same index carry only argument text to append.
type ToolCallDelta struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function *ToolCallFunctionDelta `json:"function,omitempty"`
}

// ToolCallFunctionDelta is the function part of a ToolCallDelta.
type ToolCallFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// NewToolCallStartDelta is the first fragment for a call: id, type, name and
// empty arguments, which is what OpenAI SDKs key a new call on.
func NewToolCallStartDelta(index int, id, name string) ToolCallDelta {
	return ToolCallDelta{Index: index, ID: id, Type: "function", Function: &ToolCallFunctionDelta{Name: name, Arguments: ""}}
}

// NewToolCallArgumentsDelta is a later fragment appending argument text.
func NewToolCallArgumentsDelta(index int, arguments string) ToolCallDelta {
	return ToolCallDelta{Index: index, Function: &ToolCallFunctionDelta{Arguments: arguments}}
}

// ToolChoiceKind classifies a request's tool_choice: "" (absent), "none",
// "auto", "required", or "function" (a named function, returned in name).
// ok is false when the value is none of the shapes OpenAI accepts.
func ToolChoiceKind(raw json.RawMessage) (kind, name string, ok bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", "", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none", "auto", "required":
			return s, "", true
		}
		return "", "", false
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
		return "", "", false
	}
	return "function", obj.Function.Name, true
}

// toolNameRE is OpenAI's rule for function names, which is also the
// strictest of the backends'.
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// maxTools bounds a request's tools array. OpenAI's own limit is 128.
const maxTools = 128

// ValidateTools checks a request's tools, tool_choice and the tool-related
// turns of its messages for the shape every backend needs, and returns a
// caller-readable message (with the offending parameter name) when they are
// malformed. It does not decide whether the backend supports tools; see
// ToolCapable for that.
func ValidateTools(tools []Tool, toolChoice json.RawMessage, messages []Message) (param, msg string) {
	if len(tools) > maxTools {
		return "tools", fmt.Sprintf("tools: at most %d tools per request", maxTools)
	}
	names := make(map[string]bool, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			return "tools", fmt.Sprintf("tools[%d].type: only \"function\" is supported", i)
		}
		if !toolNameRE.MatchString(t.Function.Name) {
			return "tools", fmt.Sprintf("tools[%d].function.name: must be 1-64 characters of letters, digits, _ or -", i)
		}
		if names[t.Function.Name] {
			return "tools", fmt.Sprintf("tools[%d].function.name: duplicate name %q", i, t.Function.Name)
		}
		names[t.Function.Name] = true
		if p := strings.TrimSpace(string(t.Function.Parameters)); p != "" && p != "null" {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(t.Function.Parameters, &obj); err != nil {
				return "tools", fmt.Sprintf("tools[%d].function.parameters: must be a JSON Schema object", i)
			}
		}
	}

	kind, name, ok := ToolChoiceKind(toolChoice)
	if !ok {
		return "tool_choice", `tool_choice: must be "none", "auto", "required" or {"type":"function","function":{"name":...}}`
	}
	if kind != "" && kind != "none" && len(tools) == 0 {
		return "tool_choice", "tool_choice: set without any tools"
	}
	if kind == "function" && !names[name] {
		return "tool_choice", fmt.Sprintf("tool_choice: function %q is not in tools", name)
	}

	// Tool turns in the history must pair up exactly: every assistant
	// tool_calls entry has a unique id, carries a JSON-object arguments
	// string, and is answered by exactly one role:tool turn before the
	// conversation moves on; every role:tool turn answers one of the calls
	// in the assistant turn immediately preceding its run of tool turns.
	// Each backend rejects a violation in its own words (and some only after
	// a 502-shaped round trip); refuse here, once, with the field named.
	seen := map[string]bool{}
	pending := map[string]int{} // call ids of the last assistant turn not yet answered
	pendingFrom := -1
	for i, m := range messages {
		if m.Role != "tool" && len(pending) > 0 {
			return "messages", fmt.Sprintf("messages[%d]: assistant tool_calls at messages[%d] are not all answered by tool turns (%d unanswered)", i, pendingFrom, len(pending))
		}
		switch m.Role {
		case "assistant":
			for j, tc := range m.ToolCalls {
				if tc.ID == "" {
					return "messages", fmt.Sprintf("messages[%d].tool_calls[%d].id: required", i, j)
				}
				if seen[tc.ID] {
					return "messages", fmt.Sprintf("messages[%d].tool_calls[%d].id: duplicate id %q", i, j, tc.ID)
				}
				if tc.Type != "" && tc.Type != "function" {
					return "messages", fmt.Sprintf("messages[%d].tool_calls[%d].type: only \"function\" is supported", i, j)
				}
				if tc.Function.Name == "" {
					return "messages", fmt.Sprintf("messages[%d].tool_calls[%d].function.name: required", i, j)
				}
				if !isJSONObjectOrEmpty(tc.Function.Arguments) {
					return "messages", fmt.Sprintf("messages[%d].tool_calls[%d].function.arguments: must be a JSON object encoded as a string", i, j)
				}
				seen[tc.ID] = true
				pending[tc.ID] = i
				pendingFrom = i
			}
		case "tool":
			if m.ToolCallID == "" {
				return "messages", fmt.Sprintf("messages[%d].tool_call_id: required on a tool turn", i)
			}
			if _, ok := pending[m.ToolCallID]; !ok {
				if seen[m.ToolCallID] {
					return "messages", fmt.Sprintf("messages[%d].tool_call_id: %q is already answered or belongs to an earlier assistant turn", i, m.ToolCallID)
				}
				return "messages", fmt.Sprintf("messages[%d].tool_call_id: %q does not answer the preceding assistant tool_calls", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	// A trailing assistant tool_calls turn with no answers yet is also a
	// malformed request: the caller is asking the model to continue
	// without the results it asked for.
	if len(pending) > 0 {
		return "messages", fmt.Sprintf("assistant tool_calls at messages[%d] are not all answered by tool turns (%d unanswered)", pendingFrom, len(pending))
	}
	return "", ""
}

// isJSONObjectOrEmpty accepts "" (normalised to {} by providers) and any
// JSON object; a provider never has to re-check arguments after this.
func isJSONObjectOrEmpty(args string) bool {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return true
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal([]byte(trimmed), &obj) == nil && obj != nil || trimmed == "{}"
}

// HasToolTurns reports whether the history carries tool activity (an
// assistant tool_calls turn or a role:tool turn) -- which a backend without
// tool support cannot take even when the request carries no tools array.
func HasToolTurns(messages []Message) bool {
	for _, m := range messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}
