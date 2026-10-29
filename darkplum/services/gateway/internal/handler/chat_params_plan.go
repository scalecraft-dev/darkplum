package handler

// The chat parameter contract (GitHub #3, Phase B). chatparams.Rows() is the
// only place a parameter's treatment per backend is written down; the code
// here classifies the value the caller sent, looks the row up and acts on it.
// docs/chat-params.json is that registry rendered, pinned by a test.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft-dev/darkplum/gateway/internal/chatparams"
	"github.com/scalecraft-dev/darkplum/gateway/internal/middleware"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// droppedParamsHeader names every parameter the gateway accepted but did not
// pass on as sent. Comma-separated; an entry is the bare parameter name when
// the parameter was dropped, and name=action for an adjustment (max_tokens=
// raised: extended thinking needed more room). Nested keys are written
// parent.child. Names only, never the values sent (an unknown key's name is caller-chosen). Set before the first response byte, so it is
// present on a stream too.
const droppedParamsHeader = "X-Darkplum-Dropped-Params"

// maxUnknownKeysRecorded bounds how many distinct unknown request keys are
// named individually; the rest are counted. A caller controls these names.
const maxUnknownKeysRecorded = 20

// paramsModeStrict is principal_routing_policies.params_mode 'strict' as
// carried on the policy snapshot; anything else is lenient.
const paramsModeStrict = "strict"

// paramRefusal is a 400 unsupported_parameter decided by the registry.
type paramRefusal struct {
	Param, Reason string
	// Anywhere: the same on every backend (decided before routing).
	Anywhere bool
}

// paramPlan is what the registry says to do with one request on one backend.
type paramPlan struct {
	// fwd holds the parameters whose row is forward or translate for this
	// request's value; only these may reach the provider request.
	fwd map[string]bool
	// acts are the drop-recorded parameters (default mode semantics).
	acts []paramAction
	// refuse is the first contract refusal in registry order.
	refuse *paramRefusal
}

func (p paramPlan) forwards(param string) bool { return p.fwd[param] }

// valueClass names the registry "when" a non-null value falls in.
func valueClass(param string, v json.RawMessage, req *chatCompletionRequest) string {
	nd := func(b bool) string {
		if b {
			return chatparams.WhenNonDefault
		}
		return chatparams.WhenDefault
	}
	switch param {
	case "functions":
		return nd(len(req.Functions) > 0)
	case "function_call":
		return nd(isSetAndNotNone(v))
	case "response_format":
		if req.ResponseFormat == nil {
			return "text"
		}
		switch req.ResponseFormat.Type {
		case "", "text":
			return "text"
		case "json_object", "json_schema":
			return req.ResponseFormat.Type
		}
		return "unknown-type"
	case "messages":
		for _, m := range req.Messages {
			if m.HasNonTextContent {
				return "non-text-content"
			}
		}
		return "text"
	case "stream_options":
		return "object"
	case "max_completion_tokens":
		if _, conflict := effectiveMaxTokens(*req); conflict {
			return "conflicts-with-max_tokens"
		}
	case "n":
		return nd(req.N != nil && *req.N != 1)
	case "frequency_penalty":
		return nd(req.FrequencyPenalty != nil && *req.FrequencyPenalty != 0)
	case "presence_penalty":
		return nd(req.PresencePenalty != nil && *req.PresencePenalty != 0)
	case "logit_bias":
		return nd(len(req.LogitBias) > 0)
	case "logprobs":
		if req.Logprobs != nil && *req.Logprobs {
			return "true"
		}
		return chatparams.WhenDefault
	case "top_logprobs":
		return nd(req.TopLogprobs != nil && *req.TopLogprobs != 0)
	case "tools":
		if len(req.Tools) > 0 {
			return "non-empty"
		}
		return chatparams.WhenDefault
	case "tool_choice":
		if kind, _, _ := provider.ToolChoiceKind(v); kind == "required" || kind == "function" {
			return "required-or-named"
		}
		return "auto-or-none"
	case "parallel_tool_calls":
		if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
			return chatparams.WhenDefault
		}
		if len(req.Tools) > 0 {
			return "false-with-tools"
		}
		return "false-without-tools"
	case "store":
		if req.Store != nil && *req.Store {
			return "true"
		}
		return chatparams.WhenDefault
	case "metadata":
		var m map[string]json.RawMessage
		return nd(json.Unmarshal(v, &m) != nil || len(m) > 0)
	case "service_tier":
		var s string
		return nd(json.Unmarshal(v, &s) != nil || (s != "auto" && s != "default"))
	case "modalities":
		var l []string
		return nd(json.Unmarshal(v, &l) != nil || len(l) != 1 || l[0] != "text")
	case "verbosity":
		var s string
		return nd(json.Unmarshal(v, &s) != nil || s != "medium")
	case "stream_options.include_usage":
		var b bool
		if json.Unmarshal(v, &b) == nil && b {
			return "true"
		}
		return chatparams.WhenDefault
	}
	return chatparams.WhenAny
}

// nestedObject decodes raw[parent] as an object; ok is false when absent,
// null or not an object.
func nestedObject(raw map[string]json.RawMessage, parent string) (map[string]json.RawMessage, bool) {
	v, ok := raw[parent]
	if !ok || isNullJSON(v) {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil {
		return nil, false
	}
	return m, true
}

func isNullJSON(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// presentValue returns the value for a registry row's parameter when the
// caller sent it non-null.
func presentValue(raw map[string]json.RawMessage, param string) (json.RawMessage, bool) {
	if i := strings.IndexByte(param, '.'); i >= 0 {
		obj, ok := nestedObject(raw, param[:i])
		if !ok {
			return nil, false
		}
		v, ok := obj[param[i+1:]]
		if !ok || isNullJSON(v) {
			return nil, false
		}
		return v, true
	}
	v, ok := raw[param]
	if !ok || isNullJSON(v) {
		return nil, false
	}
	return v, true
}

// matchingRows calls fn for the row (registry order) that applies to each
// non-null parameter the caller sent. Dynamic rows are decided by their own
// planners and are not visited.
func matchingRows(raw map[string]json.RawMessage, req *chatCompletionRequest, fn func(chatparams.Row)) {
	for _, row := range chatparams.Rows() {
		if row.Dynamic {
			continue
		}
		v, ok := presentValue(raw, row.Param)
		if !ok {
			continue
		}
		if valueClass(row.Param, v, req) != row.When {
			continue
		}
		fn(row)
	}
}

// earlyRefusal is the contract refusal that is the same on every backend and
// is reported before the model is resolved (functions, function_call,
// structured output, non-text content, conflicting token limits).
func earlyRefusal(raw map[string]json.RawMessage, req *chatCompletionRequest) *paramRefusal {
	var out *paramRefusal
	matchingRows(raw, req, func(row chatparams.Row) {
		if out != nil || !row.Early || !row.RefusedEverywhere() {
			return
		}
		out = &paramRefusal{Param: row.RefusalName(), Reason: row.Reason, Anywhere: true}
	})
	return out
}

// planParams applies the registry to the request for kind's backend.
func planParams(raw map[string]json.RawMessage, req *chatCompletionRequest, kind string) paramPlan {
	backend, _ := chatparams.BackendOfKind(kind)
	plan := paramPlan{fwd: map[string]bool{}}
	matchingRows(raw, req, func(row chatparams.Row) {
		switch row.For(backend) {
		case chatparams.Forward, chatparams.Translate:
			plan.fwd[row.Param] = true
		case chatparams.DropRecorded:
			plan.acts = append(plan.acts, paramAction{row.Param, actionDropped})
		case chatparams.ContractRefuse:
			if plan.refuse == nil {
				plan.refuse = &paramRefusal{Param: row.RefusalName(), Reason: row.Reason}
			}
		}
	})

	// The catch-all: a key the registry does not name is dropped and recorded
	// under its own (sanitised) name. Top-level keys first, then the nested
	// keys of the containers the registry knows.
	var unknown []string
	for k, v := range raw {
		if !chatparams.TopLevel(k) && !isNullJSON(v) {
			unknown = append(unknown, k)
		}
	}
	for _, parent := range nestedParents {
		obj, ok := nestedObject(raw, parent)
		if !ok {
			continue
		}
		for k, v := range obj {
			if !chatparams.ParamKnown(parent+"."+k) && !isNullJSON(v) {
				unknown = append(unknown, parent+"."+k)
			}
		}
	}
	sort.Strings(unknown)
	for i, k := range unknown {
		if i == maxUnknownKeysRecorded {
			plan.acts = append(plan.acts, paramAction{"unknown_keys", fmt.Sprintf("more_%d", len(unknown)-i)})
			break
		}
		plan.acts = append(plan.acts, paramAction{sanitizeParamName(k), actionDropped})
	}
	return plan
}

// nestedParents lists the parameters whose own keys are registry rows.
var nestedParents = func() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range chatparams.Rows() {
		if i := strings.IndexByte(r.Param, '.'); i >= 0 && !seen[r.Param[:i]] {
			seen[r.Param[:i]] = true
			out = append(out, r.Param[:i])
		}
	}
	return out
}()

// sanitizeParamName makes a caller-chosen key safe for a response header and a
// text column: ASCII letters, digits and _ . - [ ] only, at most 64 bytes.
func sanitizeParamName(k string) string {
	var b strings.Builder
	for _, r := range k {
		if b.Len() >= 64 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-', r == '[', r == ']':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// paramEntries renders the seam for the header and the dropped_params column:
// in order, each entry once; a dropped parameter by name, anything else as
// name=action.
func paramEntries(acts []paramAction) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range acts {
		e := a.Param
		if a.Action != actionDropped {
			e = a.Param + "=" + a.Action
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// droppedNames is the parameters among acts that were dropped (not merely
// adjusted).
func droppedNames(acts []paramAction) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range acts {
		if a.Action == actionDropped && !seen[a.Param] {
			seen[a.Param] = true
			out = append(out, a.Param)
		}
	}
	return out
}

// DropNotifier is told, once per request, which parameters were dropped and
// for whom, so it can write the first-time audit event. It must not block.
type DropNotifier interface {
	NoteDrops(principalKind, principalID, backend, model string, params []string)
}

// SetDropNotifier installs the first-drop audit notifier; nil disables it.
func (h *ChatHandler) SetDropNotifier(n DropNotifier) { h.dropNotifier = n }

// dropPrincipal is who a dropped parameter is attributed to: the user for
// webchat session traffic (the api proxies it under its service key with
// X-Darkplum-User-Id), otherwise the key.
func dropPrincipal(c *gin.Context) (kind, id string) {
	if c.GetString(middleware.ContextKeyType) == middleware.KeyTypeService {
		if uid := c.GetHeader("X-Darkplum-User-Id"); isUUID(uid) {
			return "user", uid
		}
		return "service_key", c.GetString(middleware.ContextKeyID)
	}
	return "api_key", c.GetString(middleware.ContextKeyID)
}

// publishParamActions is the single exit for everything the gateway changed
// about the caller's parameters: the seam, the response header (before the
// first byte), the usage-log entries, and the first-drop audit notification.
func (h *ChatHandler) publishParamActions(c *gin.Context, acts []paramAction, backend, model string) {
	recordParamActions(c, acts)
	v, _ := c.Get(ctxKeyDroppedParams)
	all, _ := v.([]paramAction)
	entries := paramEntries(all)
	if len(entries) == 0 {
		return
	}
	c.Header(droppedParamsHeader, strings.Join(entries, ","))
	c.Set(middleware.ContextDroppedParams, entries)
	if h.dropNotifier != nil {
		if names := droppedNames(all); len(names) > 0 {
			kind, id := dropPrincipal(c)
			if id != "" {
				h.dropNotifier.NoteDrops(kind, id, backend, model, names)
			}
		}
	}
}

// strictRefusal is the 400 an organisation in strict mode gets for the first
// dropped parameter.
func strictRefusal(acts []paramAction) *paramRefusal {
	for _, a := range acts {
		if a.Action == actionDropped {
			return &paramRefusal{
				Param:  a.Param,
				Reason: "this backend does not honour it and this organisation's parameter mode is strict, so it is refused instead of dropped",
			}
		}
	}
	return nil
}

// requestKeys are the JSON names chatCompletionRequest decodes. Go matches
// them case-insensitively; the registry does not.
var requestKeys = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(chatCompletionRequest{})
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}()

// nonCanonicalKeys returns the keys of raw that the struct decoder would
// match to a request field only by ignoring case ("Temperature").
func nonCanonicalKeys(raw map[string]json.RawMessage) []string {
	var out []string
	for k := range raw {
		if requestKeys[k] {
			continue
		}
		for known := range requestKeys {
			if strings.EqualFold(k, known) {
				out = append(out, k)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
