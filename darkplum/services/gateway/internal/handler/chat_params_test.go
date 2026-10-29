package handler

// Tests for the per-backend parameter contract (GitHub #3, phase A1): a
// parameter the resolved backend cannot honour is refused 400
// unsupported_parameter naming the field, with zero provider calls; a
// parameter at its default, or one the backend supports, passes. Capabilities
// come from the real providers' own ParamCaps, so a provider that changes
// what it forwards moves these tests with it.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/bedrock"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openaicompat"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
)

type backendPersona struct {
	name  string
	kind  string
	class string
	caps  provider.ParamCaps
}

// allBackends is every real backend with the ParamCaps it reports today.
func allBackends() []backendPersona {
	return []backendPersona{
		{"openai", "openai", routing.ClassExternalAPI, (&openai.Provider{}).ParamCaps()},
		{"vllm", "vllm", routing.ClassSelfHosted, (&openaicompat.OpenAICompatProvider{}).ParamCaps()},
		{"anthropic", "anthropic", routing.ClassExternalAPI, (&anthropic.Provider{}).ParamCaps()},
		{"bedrock", "bedrock", routing.ClassCloudManaged, (&bedrock.BedrockProvider{}).ParamCaps()},
	}
}

func paramsHandler(t *testing.T, b backendPersona) (*ChatHandler, *capturingProvider) {
	t.Helper()
	caps := b.caps
	tp := &toolCapturingProvider{capturingProvider{name: b.kind + "-default", kind: b.kind, caps: &caps}}
	prov := &tp.capturingProvider
	h := newTestHandler(
		fakeProviders{prov.name: tp},
		map[string]routing.Route{"m": {ProviderKey: prov.name, Kind: b.kind, Class: b.class}},
		policy.Snapshot{Enabled: map[string]bool{"m": true}, Residency: policy.ResidencyAny},
		true,
	)
	return h, prov
}

func assertParamRefused(t *testing.T, code int, raw []byte, prov *capturingProvider, wantParam string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", code, raw)
	}
	var e openAIError
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Error.Code != "unsupported_parameter" || e.Error.Param != wantParam {
		t.Errorf("error = code %q param %q, want unsupported_parameter / %q; body=%s", e.Error.Code, e.Error.Param, wantParam, raw)
	}
	if prov.calls != 0 {
		t.Errorf("provider called %d times on a refusal, want 0", prov.calls)
	}
}

func TestChatParams_PerBackendContract(t *testing.T) {
	tools := []map[string]any{weatherTool}
	named := map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}

	type tc struct {
		name   string
		mutate func(map[string]any)
		// refusedOn maps backend name to the param its refusal names; a
		// backend absent from the map must accept (200).
		refusedOn map[string]string
	}
	all := func(param string) map[string]string {
		return map[string]string{"openai": param, "vllm": param, "anthropic": param, "bedrock": param}
	}
	cases := []tc{
		{"n=2", func(b map[string]any) { b["n"] = 2 }, all("n")},
		{"n=1", func(b map[string]any) { b["n"] = 1 }, nil},
		{"n=null", func(b map[string]any) { b["n"] = nil }, nil},
		{"logprobs=true", func(b map[string]any) { b["logprobs"] = true }, all("logprobs")},
		{"logprobs=false", func(b map[string]any) { b["logprobs"] = false }, nil},
		{"logprobs=null", func(b map[string]any) { b["logprobs"] = nil }, nil},
		{"logit_bias set", func(b map[string]any) { b["logit_bias"] = map[string]int{"50256": -100} }, all("logit_bias")},
		{"logit_bias empty", func(b map[string]any) { b["logit_bias"] = map[string]int{} }, nil},
		{"logit_bias=null", func(b map[string]any) { b["logit_bias"] = nil }, nil},
		{"stop list", func(b map[string]any) { b["stop"] = []string{"END"} }, nil},
		{"stop string", func(b map[string]any) { b["stop"] = "END" }, nil},
		{"response_format text", func(b map[string]any) { b["response_format"] = map[string]any{"type": "text"} }, nil},
		{"response_format json_object", func(b map[string]any) { b["response_format"] = map[string]any{"type": "json_object"} },
			map[string]string{"anthropic": "response_format", "bedrock": "response_format"}},
		{"tool_choice required", func(b map[string]any) { b["tools"] = tools; b["tool_choice"] = "required" }, nil},
		{"tool_choice named", func(b map[string]any) { b["tools"] = tools; b["tool_choice"] = named }, nil},
		{"tool_choice auto", func(b map[string]any) { b["tools"] = tools; b["tool_choice"] = "auto" }, nil},
		{"parallel_tool_calls=false with tools", func(b map[string]any) { b["tools"] = tools; b["parallel_tool_calls"] = false }, all("parallel_tool_calls")},
		{"parallel_tool_calls=true with tools", func(b map[string]any) { b["tools"] = tools; b["parallel_tool_calls"] = true }, nil},
		{"parallel_tool_calls=false without tools", func(b map[string]any) { b["parallel_tool_calls"] = false }, nil},
	}
	for _, c := range cases {
		for _, be := range allBackends() {
			t.Run(c.name+"/"+be.name, func(t *testing.T) {
				// Both paths: the contract is mode- and stream-independent.
				for _, stream := range []bool{false, true} {
					h, prov := paramsHandler(t, be)
					body := baseChatBody("m")
					body["stream"] = stream
					c.mutate(body)
					w, _ := doRawChat(t, h, body)
					if want, refused := c.refusedOn[be.name]; refused {
						assertParamRefused(t, w.Code, w.Body.Bytes(), prov, want)
						continue
					}
					if w.Code != http.StatusOK {
						t.Fatalf("stream=%v: status = %d, want 200; body=%s", stream, w.Code, w.Body.String())
					}
					if prov.calls != 1 {
						t.Errorf("stream=%v: provider calls = %d, want 1", stream, prov.calls)
					}
				}
			})
		}
	}
}

// A backend that reports no capability for stop / forced tool_choice refuses
// them: the refusal path exists for every parameter, not only the ones every
// current backend happens to lack.
func TestChatParams_CapabilityGatesStopAndForcedToolChoice(t *testing.T) {
	bare := backendPersona{"bare", "vllm", routing.ClassSelfHosted, provider.ParamCaps{}}

	h, prov := paramsHandler(t, bare)
	body := baseChatBody("m")
	body["stop"] = []string{"END"}
	w, _ := doRawChat(t, h, body)
	assertParamRefused(t, w.Code, w.Body.Bytes(), prov, "stop")

	for _, choice := range []any{"required", map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}} {
		h, prov = paramsHandler(t, bare)
		body = baseChatBody("m")
		body["tools"] = []map[string]any{weatherTool}
		body["tool_choice"] = choice
		w, _ = doRawChat(t, h, body)
		assertParamRefused(t, w.Code, w.Body.Bytes(), prov, "tool_choice")
	}
}

func TestChatParams_JSONObjectReachesJSONModeBackendsOnly(t *testing.T) {
	for _, be := range allBackends() {
		t.Run(be.name, func(t *testing.T) {
			h, prov := paramsHandler(t, be)
			body := baseChatBody("m")
			body["response_format"] = map[string]any{"type": "json_object"}
			w, _ := doRawChat(t, h, body)
			if !be.caps.JSONMode {
				assertParamRefused(t, w.Code, w.Body.Bytes(), prov, "response_format")
				return
			}
			if w.Code != http.StatusOK || !prov.lastReq.JSONMode {
				t.Errorf("status=%d JSONMode=%v, want 200 and JSONMode forwarded", w.Code, prov.lastReq.JSONMode)
			}

			// text and absent never set JSONMode.
			h, prov = paramsHandler(t, be)
			body = baseChatBody("m")
			body["response_format"] = map[string]any{"type": "text"}
			doRawChat(t, h, body)
			if prov.lastReq.JSONMode {
				t.Error("response_format text must not set JSONMode")
			}
		})
	}
}

func TestChatParams_JSONSchemaStillRefusedOnEveryBackend(t *testing.T) {
	for _, be := range allBackends() {
		t.Run(be.name, func(t *testing.T) {
			h, prov := paramsHandler(t, be)
			body := baseChatBody("m")
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x"}}
			w, _ := doRawChat(t, h, body)
			assertParamRefused(t, w.Code, w.Body.Bytes(), prov, "response_format")
		})
	}
}

func TestChatParams_MaxCompletionTokens(t *testing.T) {
	for _, be := range allBackends() {
		t.Run(be.name, func(t *testing.T) {
			// max_completion_tokens alone maps to MaxTokens.
			h, prov := paramsHandler(t, be)
			body := baseChatBody("m")
			body["max_completion_tokens"] = 77
			if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || prov.lastReq.MaxTokens != 77 {
				t.Errorf("status=%d MaxTokens=%d, want 200 and 77", w.Code, prov.lastReq.MaxTokens)
			}

			// max_tokens alone is unchanged.
			h, prov = paramsHandler(t, be)
			body = baseChatBody("m")
			body["max_tokens"] = 55
			if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || prov.lastReq.MaxTokens != 55 {
				t.Errorf("status=%d MaxTokens=%d, want 200 and 55", w.Code, prov.lastReq.MaxTokens)
			}

			// Both, equal: one limit, accepted.
			h, prov = paramsHandler(t, be)
			body = baseChatBody("m")
			body["max_tokens"] = 64
			body["max_completion_tokens"] = 64
			if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || prov.lastReq.MaxTokens != 64 {
				t.Errorf("status=%d MaxTokens=%d, want 200 and 64", w.Code, prov.lastReq.MaxTokens)
			}

			// Both, different: refused naming max_completion_tokens, nothing dispatched.
			h, prov = paramsHandler(t, be)
			body = baseChatBody("m")
			body["max_tokens"] = 64
			body["max_completion_tokens"] = 128
			w, _ := doRawChat(t, h, body)
			assertParamRefused(t, w.Code, w.Body.Bytes(), prov, "max_completion_tokens")
		})
	}
}

func TestChatParams_StreamOptions(t *testing.T) {
	for _, be := range allBackends() {
		t.Run(be.name, func(t *testing.T) {
			// include_usage on a stream reaches the provider.
			h, prov := paramsHandler(t, be)
			body := baseChatBody("m")
			body["stream"] = true
			body["stream_options"] = map[string]any{"include_usage": true}
			if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || !prov.lastReq.IncludeUsage {
				t.Errorf("status=%d IncludeUsage=%v, want 200 and true", w.Code, prov.lastReq.IncludeUsage)
			}

			// A stream that did not ask keeps IncludeUsage off.
			for _, opts := range []any{nil, map[string]any{"include_usage": false}} {
				h, prov = paramsHandler(t, be)
				body = baseChatBody("m")
				body["stream"] = true
				body["stream_options"] = opts
				if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK || prov.lastReq.IncludeUsage {
					t.Errorf("opts=%v: status=%d IncludeUsage=%v, want 200 and false", opts, w.Code, prov.lastReq.IncludeUsage)
				}
			}

			// stream_options without stream:true is a 400, like OpenAI.
			for _, stream := range []any{nil, false} {
				h, prov = paramsHandler(t, be)
				body = baseChatBody("m")
				if stream != nil {
					body["stream"] = stream
				}
				body["stream_options"] = map[string]any{"include_usage": true}
				w, _ := doRawChat(t, h, body)
				if w.Code != http.StatusBadRequest || prov.calls != 0 {
					t.Fatalf("stream=%v: status=%d calls=%d, want 400 and 0 calls; body=%s", stream, w.Code, prov.calls, w.Body.String())
				}
				var e openAIError
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				if e.Error.Param != "stream_options" || e.Error.Type != "invalid_request_error" {
					t.Errorf("error = %+v, want param stream_options / invalid_request_error", e.Error)
				}
			}

			// stream_options:null without stream is the same as absent.
			h, prov = paramsHandler(t, be)
			body = baseChatBody("m")
			body["stream_options"] = nil
			if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK {
				t.Errorf("null stream_options without stream: status=%d, want 200", w.Code)
			}
		})
	}
}

// The combination the OpenAI SDKs send by default must pass on every backend,
// stream and non-stream, with exactly one provider call.
func TestChatParams_SDKDefaultsAcceptedTogether(t *testing.T) {
	for _, be := range allBackends() {
		for _, stream := range []bool{false, true} {
			h, prov := paramsHandler(t, be)
			body := baseChatBody("m")
			body["stream"] = stream
			body["n"] = 1
			body["logprobs"] = false
			body["logit_bias"] = map[string]int{}
			body["stream_options"] = nil
			body["parallel_tool_calls"] = true
			body["tools"] = []map[string]any{weatherTool}
			w, _ := doRawChat(t, h, body)
			if w.Code != http.StatusOK || prov.calls != 1 {
				t.Errorf("%s stream=%v: status=%d calls=%d, want 200 and 1; body=%s", be.name, stream, w.Code, prov.calls, w.Body.String())
			}
		}
	}
}
