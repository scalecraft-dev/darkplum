package handler

// The chat parameter contract (GitHub #3, Phase B): for EVERY row of the
// registry (internal/chatparams, pinned to docs/chat-params.json by its own
// test) and every backend, the handler does what the row says -- in default
// mode and in an organisation's strict mode:
//
//	forward / translate  reaches the provider request (and, for the three
//	                     HTTP backends, the wire)
//	accept-default       silent: no header, no record, not forwarded
//	drop-recorded        absent from the provider request and the wire,
//	                     named in X-Darkplum-Dropped-Params and in the entries
//	                     the Recording middleware writes to
//	                     inference_events.dropped_params; strict mode: 400
//	contract-refuse      400 unsupported_parameter naming the field, zero
//	                     provider calls, in both modes
//
// A row with no sample here fails the test, so a new parameter cannot be added
// to the registry without being exercised on every backend.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/chatparams"
	"github.com/scalecraft-dev/darkplum/gateway/internal/middleware"
	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openaicompat"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

// fakePseudo is a UserPseudonymizer whose secret is always present.
type fakePseudo struct{}

func (fakePseudo) Pseudonym(user string) (string, bool) { return "k1:PSEUDO", user != "" }

// thinkingStubPlan stands in for a Claude model that supports extended
// thinking (the real providers decide this from discovery / the model id).
func thinkingStubPlan(_ string, effort string, maxTokens int) provider.ReasoningPlan {
	b := provider.ThinkingBudget(effort)
	if b == 0 {
		return provider.ReasoningPlan{Mode: provider.ReasoningThinking}
	}
	mt := maxTokens
	if mt <= b {
		mt = maxTokens + b
	}
	return provider.ReasoningPlan{Mode: provider.ReasoningThinking, Budget: b, MaxTokens: mt}
}

func contractHandler(t *testing.T, be backendPersona, strict bool) (*ChatHandler, *reasoningProvider) {
	t.Helper()
	caps := be.caps
	rp := &reasoningProvider{toolCapturingProvider: toolCapturingProvider{capturingProvider{name: be.kind + "-default", kind: be.kind, caps: &caps}}}
	switch be.name {
	case "openai":
		rp.plan = (&openai.Provider{}).PlanReasoning
	case "anthropic", "bedrock":
		rp.plan = thinkingStubPlan
	default:
		rp.plan = func(string, string, int) provider.ReasoningPlan { return provider.ReasoningPlan{} }
	}
	snap := policy.Snapshot{Enabled: map[string]bool{"m": true}, Residency: policy.ResidencyAny}
	if strict {
		snap.ParamsMode = "strict"
	}
	h := newTestHandler(
		fakeProviders{rp.name: rp},
		map[string]routing.Route{"m": {ProviderKey: rp.name, Kind: be.kind, Class: be.class}},
		snap, true,
	)
	h.SetUserPseudonymizer(fakePseudo{})
	return h, rp
}

// paramSample is one row's request and how to see its effect.
type paramSample struct {
	// body adds the row's parameter(s) to the base body for backend be.
	body func(b map[string]any, be backendPersona)
	// wantParam is the field a refusal names when it is not the row's own.
	wantParam string
	// seen reports whether the value reached the provider request. nil: the
	// row has no seam representation (accepted/refused only).
	seen func(r provider.ChatRequest) bool
	// needle is text that appears in the upstream JSON body exactly when the
	// value is on the wire; wire overrides it per backend name.
	needle string
	wire   map[string]string
	// stream sends the request as a stream instead.
	stream bool
	// skipWire excludes the row from the wire test (dynamic rows need the
	// provider to have discovered a model; stream-only rows).
	skipWire bool
}

func set(kv ...any) func(map[string]any, backendPersona) {
	return func(b map[string]any, _ backendPersona) {
		for i := 0; i < len(kv); i += 2 {
			b[kv[i].(string)] = kv[i+1]
		}
	}
}

func str(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

var (
	tools1 = []map[string]any{weatherTool}
	named  = map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
)

func k(param, when string) string { return param + " | " + when }

// samples has one entry per registry row.
var samples = map[string]paramSample{
	k("functions", "non-default"):        {body: set("functions", []map[string]any{{"name": "f"}}), wantParam: "functions"},
	k("functions", "default"):            {body: set("functions", []any{})},
	k("function_call", "non-default"):    {body: set("function_call", "auto"), wantParam: "function_call"},
	k("function_call", "default"):        {body: set("function_call", "none")},
	k("response_format", "json_schema"):  {body: set("response_format", map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x"}}), wantParam: "response_format"},
	k("response_format", "unknown-type"): {body: set("response_format", map[string]any{"type": "xml"}), wantParam: "response_format"},
	k("messages", "non-text-content"): {
		body:      set("messages", []map[string]any{{"role": "user", "content": []map[string]any{{"type": "image_url", "image_url": map[string]any{"url": "http://x/y.png"}}}}}),
		wantParam: "messages[].content",
	},
	k("model", "any"):             {body: set(), seen: func(r provider.ChatRequest) bool { return r.Model == "m" }, skipWire: true},
	k("messages", "text"):         {body: set(), seen: func(r provider.ChatRequest) bool { return len(r.Messages) == 1 }, needle: "hello"},
	k("stream", "any"):            {body: set("stream", true), stream: true, seen: func(r provider.ChatRequest) bool { return r.Stream }, skipWire: true},
	k("stream_options", "object"): {body: set("stream", true, "stream_options", map[string]any{}), stream: true, skipWire: true},
	k("stream_options.include_usage", "true"): {
		body: set("stream", true, "stream_options", map[string]any{"include_usage": true}), stream: true,
		seen: func(r provider.ChatRequest) bool { return r.IncludeUsage }, skipWire: true,
	},
	k("stream_options.include_usage", "default"): {
		body: set("stream", true, "stream_options", map[string]any{"include_usage": false}), stream: true,
		seen: func(r provider.ChatRequest) bool { return r.IncludeUsage }, skipWire: true,
	},
	k("stream_options.include_obfuscation", "any"): {
		body: set("stream", true, "stream_options", map[string]any{"include_obfuscation": true}), stream: true, skipWire: true,
	},
	k("max_tokens", "any"): {body: set("max_tokens", 777), seen: func(r provider.ChatRequest) bool { return r.MaxTokens == 777 }, needle: "777"},
	k("max_tokens", "with-thinking"): {
		// Where extended thinking applies the value sent is larger; elsewhere it is the caller's.
		body: func(b map[string]any, be backendPersona) {
			b["max_tokens"] = 777
			if be.name == "anthropic" || be.name == "bedrock" {
				b["reasoning_effort"] = "low"
			}
		},
		seen: func(r provider.ChatRequest) bool { return r.MaxTokens >= 777 }, skipWire: true,
	},
	k("max_completion_tokens", "any"):                       {body: set("max_completion_tokens", 555), seen: func(r provider.ChatRequest) bool { return r.MaxTokens == 555 }, needle: "555"},
	k("max_completion_tokens", "conflicts-with-max_tokens"): {body: set("max_tokens", 10, "max_completion_tokens", 20), wantParam: "max_completion_tokens"},
	k("temperature", "any"):                                 {body: set("temperature", 0.63), seen: func(r provider.ChatRequest) bool { return str(r.Temperature) == 0.63 }, needle: "0.63"},
	k("temperature", "with-thinking"): {
		body: func(b map[string]any, be backendPersona) {
			b["temperature"] = 0.63
			if be.name == "anthropic" || be.name == "bedrock" {
				b["reasoning_effort"] = "low"
			}
		},
		seen: func(r provider.ChatRequest) bool { return str(r.Temperature) == 0.63 }, skipWire: true,
	},
	k("top_p", "any"): {body: set("top_p", 0.71), seen: func(r provider.ChatRequest) bool { return str(r.TopP) == 0.71 }, needle: "0.71"},
	k("top_p", "with-thinking"): {
		body: func(b map[string]any, be backendPersona) {
			b["top_p"] = 0.5
			if be.name == "anthropic" || be.name == "bedrock" {
				b["reasoning_effort"] = "low"
			}
		},
		seen: func(r provider.ChatRequest) bool { return str(r.TopP) == 0.5 }, skipWire: true,
	},
	k("seed", "any"):                      {body: set("seed", 4242), seen: func(r provider.ChatRequest) bool { return r.Seed != nil && *r.Seed == 4242 }, needle: "4242"},
	k("frequency_penalty", "default"):     {body: set("frequency_penalty", 0), seen: func(r provider.ChatRequest) bool { return r.FrequencyPenalty != nil }, needle: "frequency_penalty"},
	k("frequency_penalty", "non-default"): {body: set("frequency_penalty", 0.37), seen: func(r provider.ChatRequest) bool { return str(r.FrequencyPenalty) == 0.37 }, needle: "0.37"},
	k("presence_penalty", "default"):      {body: set("presence_penalty", 0), seen: func(r provider.ChatRequest) bool { return r.PresencePenalty != nil }, needle: "presence_penalty"},
	k("presence_penalty", "non-default"):  {body: set("presence_penalty", 0.41), seen: func(r provider.ChatRequest) bool { return str(r.PresencePenalty) == 0.41 }, needle: "0.41"},
	k("n", "default"):                     {body: set("n", 1)},
	k("n", "non-default"):                 {body: set("n", 3), wantParam: "n"},
	k("stop", "any"):                      {body: set("stop", []string{"ENDSEQ"}), seen: func(r provider.ChatRequest) bool { return len(r.Stop) == 1 && r.Stop[0] == "ENDSEQ" }, needle: "ENDSEQ"},
	k("logit_bias", "default"):            {body: set("logit_bias", map[string]int{}), seen: func(r provider.ChatRequest) bool { return len(r.LogitBias) > 0 }, needle: "logit_bias"},
	k("logit_bias", "non-default"):        {body: set("logit_bias", map[string]int{"50256": -100}), seen: func(r provider.ChatRequest) bool { return r.LogitBias["50256"] == -100 }, needle: "50256"},
	k("logprobs", "default"):              {body: set("logprobs", false), seen: func(r provider.ChatRequest) bool { return r.Logprobs }, needle: `"logprobs":`},
	k("logprobs", "true"):                 {body: set("logprobs", true), seen: func(r provider.ChatRequest) bool { return r.Logprobs }, needle: `"logprobs":true`},
	k("top_logprobs", "default"):          {body: set("top_logprobs", 0), seen: func(r provider.ChatRequest) bool { return r.TopLogprobs != nil }, needle: "top_logprobs"},
	k("top_logprobs", "non-default"): {
		// top_logprobs needs logprobs: true, which a backend without logprobs refuses first.
		body: set("logprobs", true, "top_logprobs", 3), wantParam: "logprobs",
		seen: func(r provider.ChatRequest) bool { return r.TopLogprobs != nil && *r.TopLogprobs == 3 }, needle: `"top_logprobs":3`,
	},
	k("tools", "default"):   {body: set("tools", []any{}), seen: func(r provider.ChatRequest) bool { return len(r.Tools) > 0 }, needle: "get_weather"},
	k("tools", "non-empty"): {body: set("tools", tools1), seen: func(r provider.ChatRequest) bool { return len(r.Tools) == 1 }, needle: "get_weather"},
	k("tool_choice", "auto-or-none"): {
		body: set("tools", tools1, "tool_choice", "auto"),
		seen: func(r provider.ChatRequest) bool {
			kd, _, _ := provider.ToolChoiceKind(r.ToolChoice)
			return kd == "auto"
		}, skipWire: true,
	},
	k("tool_choice", "required-or-named"): {
		body: set("tools", tools1, "tool_choice", named), wantParam: "tool_choice",
		seen: func(r provider.ChatRequest) bool {
			kd, _, _ := provider.ToolChoiceKind(r.ToolChoice)
			return kd == "function"
		}, skipWire: true,
	},
	k("parallel_tool_calls", "default"): {body: set("tools", tools1, "parallel_tool_calls", true), seen: func(r provider.ChatRequest) bool { return r.ParallelToolCalls != nil }, skipWire: true},
	k("parallel_tool_calls", "false-with-tools"): {
		body: set("tools", tools1, "parallel_tool_calls", false), wantParam: "parallel_tool_calls",
		seen: func(r provider.ChatRequest) bool { return r.ParallelToolCalls != nil && !*r.ParallelToolCalls }, skipWire: true,
	},
	k("parallel_tool_calls", "false-without-tools"): {body: set("parallel_tool_calls", false), seen: func(r provider.ChatRequest) bool { return r.ParallelToolCalls != nil }, needle: "parallel_tool_calls"},
	k("response_format", "text"):                    {body: set("response_format", map[string]any{"type": "text"}), seen: func(r provider.ChatRequest) bool { return r.JSONMode }, needle: "response_format"},
	k("response_format", "json_object"): {
		body: set("response_format", map[string]any{"type": "json_object"}), wantParam: "response_format",
		seen: func(r provider.ChatRequest) bool { return r.JSONMode }, needle: "json_object",
	},
	k("response_format.type", "any"): {body: set("response_format", map[string]any{"type": "text"}), skipWire: true},
	k("response_format.json_schema", "any"): {
		body:   set("response_format", map[string]any{"type": "text", "json_schema": map[string]any{"name": "SCHEMAX"}}),
		needle: "SCHEMAX",
	},
	k("user", "any"): {
		body: set("user", "u-1"),
		seen: func(r provider.ChatRequest) bool { return r.User != "" }, skipWire: true,
	},
	k("store", "default"):              {body: set("store", false), seen: func(r provider.ChatRequest) bool { return r.NoStore }, needle: `"store":false`},
	k("store", "true"):                 {body: set("store", true), wantParam: "store"},
	k("reasoning_effort", "none"):      {body: set("reasoning_effort", "none"), seen: func(r provider.ChatRequest) bool { return r.ReasoningEffort != "" || r.ThinkingBudget > 0 }, skipWire: true},
	k("reasoning_effort", "other"):     {body: set("reasoning_effort", "medium"), seen: func(r provider.ChatRequest) bool { return r.ReasoningEffort != "" || r.ThinkingBudget > 0 }, skipWire: true},
	k("metadata", "default"):           {body: set("metadata", map[string]any{})},
	k("metadata", "non-default"):       {body: set("metadata", map[string]any{"trace": "TRACEX"}), needle: "TRACEX"},
	k("service_tier", "default"):       {body: set("service_tier", "auto")},
	k("service_tier", "non-default"):   {body: set("service_tier", "flex"), needle: "flex"},
	k("modalities", "default"):         {body: set("modalities", []string{"text"})},
	k("modalities", "non-default"):     {body: set("modalities", []string{"text", "audio"}), needle: "modalities"},
	k("audio", "any"):                  {body: set("audio", map[string]any{"voice": "VOICEX", "format": "wav"}), needle: "VOICEX"},
	k("prediction", "any"):             {body: set("prediction", map[string]any{"type": "content", "content": "PREDX"}), needle: "PREDX"},
	k("web_search_options", "any"):     {body: set("web_search_options", map[string]any{"search_context_size": "SEARCHX"}), needle: "SEARCHX", wantParam: "web_search_options"},
	k("verbosity", "default"):          {body: set("verbosity", "medium")},
	k("verbosity", "non-default"):      {body: set("verbosity", "low"), needle: "verbosity"},
	k("safety_identifier", "any"):      {body: set("safety_identifier", "SAFEX"), needle: "SAFEX"},
	k("prompt_cache_key", "any"):       {body: set("prompt_cache_key", "CACHEX"), needle: "CACHEX"},
	k("prompt_cache_retention", "any"): {body: set("prompt_cache_retention", "RETX"), needle: "RETX"},
	k("top_k", "any"):                  {body: set("top_k", 40), seen: func(r provider.ChatRequest) bool { return r.VLLM.TopK != nil && *r.VLLM.TopK == 40 }, needle: `"top_k":40`},
	k("min_p", "any"):                  {body: set("min_p", 0.17), seen: func(r provider.ChatRequest) bool { return str(r.VLLM.MinP) == 0.17 }, needle: `"min_p":0.17`},
	k("repetition_penalty", "any"):     {body: set("repetition_penalty", 1.23), seen: func(r provider.ChatRequest) bool { return str(r.VLLM.RepetitionPenalty) == 1.23 }, needle: `"repetition_penalty":1.23`},
	k("guided_json", "any"):            {body: set("guided_json", map[string]any{"type": "object", "title": "GJX"}), seen: func(r provider.ChatRequest) bool { return len(r.VLLM.GuidedJSON) > 0 }, needle: "GJX"},
	k("chat_template_kwargs", "any"):   {body: set("chat_template_kwargs", map[string]any{"enable_thinking": false}), seen: func(r provider.ChatRequest) bool { return len(r.VLLM.ChatTemplateKwargs) > 0 }, needle: "enable_thinking"},
}

func entriesOf(w *httptest.ResponseRecorder) []string {
	h := w.Header().Get(droppedParamsHeader)
	if h == "" {
		return nil
	}
	return strings.Split(h, ",")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestChatParamsContract_EveryRowOnEveryBackend(t *testing.T) {
	// Coverage both ways: no row without a sample, no sample without a row.
	known := map[string]bool{}
	for _, row := range chatparams.Rows() {
		known[k(row.Param, row.When)] = true
		if _, ok := samples[k(row.Param, row.When)]; !ok {
			t.Errorf("registry row %q has no sample in chat_params_contract_test.go", k(row.Param, row.When))
		}
	}
	for key := range samples {
		if !known[key] {
			t.Errorf("sample %q has no registry row", key)
		}
	}

	exercised := 0
	for _, row := range chatparams.Rows() {
		smp, ok := samples[k(row.Param, row.When)]
		if !ok {
			continue
		}
		for _, be := range allBackends() {
			be := be
			reg, _ := chatparams.BackendOfKind(be.kind)
			disp := row.For(reg)
			for _, strict := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/%s/strict=%v", k(row.Param, row.When), be.name, disp, strict)
				t.Run(name, func(t *testing.T) {
					exercised++
					h, prov := contractHandler(t, be, strict)
					body := baseChatBody("m")
					smp.body(body, be)
					w, c := doRawChat(t, h, body)

					effective := disp
					if strict && disp == chatparams.DropRecorded {
						effective = chatparams.ContractRefuse
					}
					want := smp.wantParam
					if want == "" {
						want = row.RefusalName()
					}
					if effective == chatparams.ContractRefuse && disp == chatparams.DropRecorded {
						want = row.Param // strict mode names the dropped parameter
					}

					switch effective {
					case chatparams.ContractRefuse:
						assertParamRefused(t, w.Code, w.Body.Bytes(), &prov.capturingProvider, want)
						if len(entriesOf(w)) != 0 {
							t.Errorf("a refusal carries a dropped-params header: %v", entriesOf(w))
						}
					case chatparams.DropRecorded:
						if w.Code != http.StatusOK {
							t.Fatalf("status %d: %s", w.Code, w.Body.String())
						}
						if !contains(entriesOf(w), row.Param) {
							t.Errorf("header %q does not name %s", w.Header().Get(droppedParamsHeader), row.Param)
						}
						if v, _ := c.Get(middleware.ContextDroppedParams); !contains(asStrings(v), row.Param) {
							t.Errorf("usage-log entries %v do not name %s", v, row.Param)
						}
						if smp.seen != nil && smp.seen(prov.lastReq) {
							t.Errorf("a dropped parameter reached the provider request: %+v", prov.lastReq)
						}
					case chatparams.AcceptDefault:
						if w.Code != http.StatusOK {
							t.Fatalf("status %d: %s", w.Code, w.Body.String())
						}
						if len(entriesOf(w)) != 0 {
							t.Errorf("an accepted default was recorded: %v", entriesOf(w))
						}
						if _, has := c.Get(middleware.ContextDroppedParams); has {
							t.Error("an accepted default was written to the usage log entries")
						}
						// A default is never forwarded as a value, except where the
						// backend's own row says forward for that value (store:false).
						if smp.seen != nil && smp.seen(prov.lastReq) {
							t.Errorf("an accept-default value was forwarded: %+v", prov.lastReq)
						}
					case chatparams.Forward, chatparams.Translate:
						if w.Code != http.StatusOK {
							t.Fatalf("status %d: %s", w.Code, w.Body.String())
						}
						if contains(entriesOf(w), row.Param) {
							t.Errorf("a %s parameter is reported dropped: %q", effective, w.Header().Get(droppedParamsHeader))
						}
						if smp.seen != nil && !smp.seen(prov.lastReq) {
							t.Errorf("%s: value did not reach the provider request: %+v", effective, prov.lastReq)
						}
					}
				})
			}
		}
	}
	if exercised < len(chatparams.Rows())*len(allBackends())*2 {
		t.Errorf("exercised %d cases, want %d", exercised, len(chatparams.Rows())*len(allBackends())*2)
	}
}

func asStrings(v any) []string {
	s, _ := v.([]string)
	return s
}

// ---------------------------------------------------------------------------
// Wire level: the same rows through the REAL openai, openaicompat and
// anthropic providers against a recording upstream. Bedrock has no HTTP
// seam; it is covered at the provider request above and by its provider
// package's own wire tests.
// ---------------------------------------------------------------------------

type wireUpstream struct {
	srv  *httptest.Server
	body []byte
	hits int
}

func newWireUpstream(t *testing.T, reply string) *wireUpstream {
	t.Helper()
	u := &wireUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits++
		u.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

const (
	openAIReply    = `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	anthropicReply = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
)

func wireHandler(t *testing.T, be backendPersona, strict bool) (*ChatHandler, *wireUpstream) {
	t.Helper()
	var prov provider.Provider
	var up *wireUpstream
	switch be.name {
	case "openai", "anthropic":
		key := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(key, []byte("sk-test"), 0o600); err != nil {
			t.Fatal(err)
		}
		if be.name == "openai" {
			up = newWireUpstream(t, openAIReply)
			prov = openai.New(credsource.New(key), openai.Config{BaseURL: up.srv.URL})
		} else {
			up = newWireUpstream(t, anthropicReply)
			prov = anthropic.New(credsource.New(key), anthropic.Config{BaseURL: up.srv.URL})
		}
	case "vllm":
		up = newWireUpstream(t, openAIReply)
		p, err := openaicompat.New("vllm-default", "vllm", up.srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		prov = p
	}
	snap := policy.Snapshot{Enabled: map[string]bool{"m": true}, Residency: policy.ResidencyAny}
	if strict {
		snap.ParamsMode = "strict"
	}
	h := newTestHandler(fakeProviders{"p": prov},
		map[string]routing.Route{"m": {ProviderKey: "p", Kind: be.kind, Class: be.class}}, snap, true)
	h.SetUserPseudonymizer(fakePseudo{})
	return h, up
}

func TestChatParamsContract_WireForEveryRow(t *testing.T) {
	checked := 0
	for _, row := range chatparams.Rows() {
		smp := samples[k(row.Param, row.When)]
		if row.Dynamic || smp.skipWire || smp.needle == "" && len(smp.wire) == 0 {
			continue
		}
		for _, be := range allBackends() {
			if be.name == "bedrock" {
				continue
			}
			be := be
			reg, _ := chatparams.BackendOfKind(be.kind)
			disp := row.For(reg)
			needle := smp.needle
			if n, ok := smp.wire[be.name]; ok {
				needle = n
			}
			for _, strict := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%s/strict=%v", k(row.Param, row.When), be.name, disp, strict), func(t *testing.T) {
					checked++
					h, up := wireHandler(t, be, strict)
					body := baseChatBody("m")
					smp.body(body, be)
					w, _ := doRawChat(t, h, body)
					onWire := strings.Contains(string(up.body), needle)
					switch {
					case disp == chatparams.ContractRefuse || strict && disp == chatparams.DropRecorded:
						if w.Code != http.StatusBadRequest || up.hits != 0 {
							t.Errorf("status %d, upstream hits %d; want 400 and no upstream call", w.Code, up.hits)
						}
					case disp == chatparams.Forward || disp == chatparams.Translate:
						if w.Code != http.StatusOK || !onWire {
							t.Errorf("status %d, %q on the wire = %v; want 200 and on the wire. body: %s", w.Code, needle, onWire, up.body)
						}
					default: // drop-recorded (lenient) and accept-default
						if w.Code != http.StatusOK {
							t.Fatalf("status %d: %s", w.Code, w.Body.String())
						}
						if onWire {
							t.Errorf("%q reached the wire although the row is %s. body: %s", needle, disp, up.body)
						}
					}
				})
			}
		}
	}
	if checked == 0 {
		t.Fatal("no wire cases ran")
	}
}

// ---------------------------------------------------------------------------
// The registry and the providers must agree: ParamCaps is the fail-closed
// backstop the handler keeps, so a registry row that promises what a provider
// says it cannot do (or the reverse) is a bug in one of them.
// ---------------------------------------------------------------------------

func TestChatParamsContract_RegistryAgreesWithProviderCaps(t *testing.T) {
	forwards := func(d chatparams.Disposition) bool { return d == chatparams.Forward || d == chatparams.Translate }
	for _, be := range allBackends() {
		reg, _ := chatparams.BackendOfKind(be.kind)
		cell := func(param, when string) chatparams.Disposition {
			row, ok := chatparams.Lookup(param, when)
			if !ok {
				t.Fatalf("no row %s|%s", param, when)
			}
			return row.For(reg)
		}
		caps := be.caps
		check := func(name string, capOn bool, d chatparams.Disposition) {
			if capOn != forwards(d) {
				t.Errorf("%s: %s ParamCaps says %v, registry says %s", be.name, name, capOn, d)
			}
		}
		check("JSONMode", caps.JSONMode, cell("response_format", "json_object"))
		check("Stop", caps.Stop, cell("stop", "any"))
		check("Logprobs", caps.Logprobs, cell("logprobs", "true"))
		check("LogitBias", caps.LogitBias, cell("logit_bias", "non-default"))
		check("ForcedToolChoice", caps.ForcedToolChoice, cell("tool_choice", "required-or-named"))
		check("SerialToolCalls", caps.SerialToolCalls, cell("parallel_tool_calls", "false-with-tools"))
		switch caps.UserID {
		case provider.UserIDRaw:
			if cell("user", "any") != chatparams.Forward {
				t.Errorf("%s: UserIDRaw but the registry does not say forward", be.name)
			}
		case provider.UserIDHMAC:
			if cell("user", "any") != chatparams.Translate {
				t.Errorf("%s: UserIDHMAC but the registry does not say translate", be.name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Dynamic rows: what the handler decides per model, and that strict mode and
// the seam see it.
// ---------------------------------------------------------------------------

func marshalReq(r provider.ChatRequest) string {
	b, _ := json.Marshal(r)
	return string(bytes.ToLower(b))
}

func TestChatParamsContract_ThinkingSideEffectsAreRowsToo(t *testing.T) {
	for _, be := range allBackends() {
		if be.name != "anthropic" && be.name != "bedrock" {
			continue
		}
		h, prov := contractHandler(t, be, false)
		body := baseChatBody("m")
		body["reasoning_effort"] = "low"
		body["temperature"] = 0.63
		body["top_p"] = 0.5
		body["max_tokens"] = 100
		w, _ := doRawChat(t, h, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		got := entriesOf(w)
		for _, want := range []string{"max_tokens=raised", "temperature", "top_p"} {
			if !contains(got, want) {
				t.Errorf("%s: header %v does not name %s", be.name, got, want)
			}
		}
		if prov.lastReq.Temperature != nil || prov.lastReq.TopP != nil || prov.lastReq.MaxTokens <= 100 {
			t.Errorf("%s: %s", be.name, marshalReq(prov.lastReq))
		}

		// Strict mode: the drop is a refusal; the raise alone is not.
		hs, provS := contractHandler(t, be, true)
		w, _ = doRawChat(t, hs, body)
		assertParamRefused(t, w.Code, w.Body.Bytes(), &provS.capturingProvider, "temperature")
		body2 := baseChatBody("m")
		body2["reasoning_effort"] = "low"
		body2["max_tokens"] = 100
		w, _ = doRawChat(t, hs, body2)
		if w.Code != http.StatusOK || !contains(entriesOf(w), "max_tokens=raised") {
			t.Errorf("%s strict: raising max_tokens must still be allowed and reported: %d %v", be.name, w.Code, entriesOf(w))
		}
	}
}
