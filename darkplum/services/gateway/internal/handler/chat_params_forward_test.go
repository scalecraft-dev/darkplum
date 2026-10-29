package handler

// Phase A2 of #3: parameters a backend can honour are forwarded (typed, only
// when non-default), the rest keep their A1 behaviour, and `user` reaches a
// backend only in the form its ParamCaps().UserID allows.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/policy"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/anthropic"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/openai"
	"github.com/scalecraft-dev/darkplum/gateway/internal/routing"
	"github.com/scalecraft-dev/darkplum/gateway/internal/userid"
)

const testEmail = "alice@example.com"

func mountSecret(t *testing.T, key string) string {
	t.Helper()
	dir := t.TempDir()
	if key != "" {
		if err := os.WriteFile(filepath.Join(dir, userid.KeyFile), []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestForwardedParams_TypedAndOnlyWhenNonDefault(t *testing.T) {
	for _, be := range allBackends() {
		t.Run(be.name, func(t *testing.T) {
			// Defaults and absence: nothing set on the provider request.
			h, prov := paramsHandler(t, be)
			body := baseChatBody("m")
			body["frequency_penalty"] = 0
			body["presence_penalty"] = 0
			body["seed"] = nil
			body["store"] = false
			body["parallel_tool_calls"] = true
			body["tools"] = []map[string]any{weatherTool}
			if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			r := prov.lastReq
			if r.Seed != nil || r.FrequencyPenalty != nil || r.PresencePenalty != nil || r.ParallelToolCalls != nil || r.Logprobs || r.LogitBias != nil || r.User != "" {
				t.Errorf("default values were forwarded: %+v", r)
			}
			if !r.NoStore {
				t.Error("store:false must reach the provider (NoStore)")
			}

			// Non-default values.
			h, prov = paramsHandler(t, be)
			body = baseChatBody("m")
			body["seed"] = 42
			body["frequency_penalty"] = 0.5
			body["presence_penalty"] = -0.5
			body["tools"] = []map[string]any{weatherTool}
			body["parallel_tool_calls"] = false
			body["user"] = "u-1"
			body["top_k"] = 40
			body["min_p"] = 0.1
			body["repetition_penalty"] = 1.2
			body["guided_json"] = map[string]any{"type": "object"}
			body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
			if be.caps.Logprobs {
				body["logprobs"] = true
				body["top_logprobs"] = 3
			}
			if be.caps.LogitBias {
				body["logit_bias"] = map[string]float64{"50256": -100}
			}
			w, _ := doRawChat(t, h, body)
			if !be.caps.SerialToolCalls {
				assertParamRefused(t, w.Code, w.Body.Bytes(), prov, "parallel_tool_calls")
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			r = prov.lastReq
			if r.Seed == nil || *r.Seed != 42 {
				t.Errorf("Seed = %v", r.Seed)
			}
			if r.FrequencyPenalty == nil || *r.FrequencyPenalty != 0.5 || r.PresencePenalty == nil || *r.PresencePenalty != -0.5 {
				t.Errorf("penalties = %v / %v", r.FrequencyPenalty, r.PresencePenalty)
			}
			if r.ParallelToolCalls == nil || *r.ParallelToolCalls {
				t.Errorf("ParallelToolCalls = %v, want false", r.ParallelToolCalls)
			}
			if r.VLLM.TopK == nil || *r.VLLM.TopK != 40 || r.VLLM.MinP == nil || r.VLLM.RepetitionPenalty == nil ||
				string(r.VLLM.GuidedJSON) != `{"type":"object"}` || string(r.VLLM.ChatTemplateKwargs) != `{"enable_thinking":false}` {
				t.Errorf("VLLM extensions = %+v", r.VLLM)
			}
			if be.caps.Logprobs && (!r.Logprobs || r.TopLogprobs == nil || *r.TopLogprobs != 3) {
				t.Errorf("logprobs = %v / %v", r.Logprobs, r.TopLogprobs)
			}
			if be.caps.LogitBias && r.LogitBias["50256"] != -100 {
				t.Errorf("LogitBias = %v", r.LogitBias)
			}
		})
	}
}

func TestForwardedParams_MalformedValuesRefusedWithZeroProviderCalls(t *testing.T) {
	cases := []struct {
		name, param string
		mutate      func(map[string]any)
	}{
		{"top_logprobs without logprobs", "top_logprobs", func(b map[string]any) { b["top_logprobs"] = 2 }},
		{"top_logprobs with logprobs false", "top_logprobs", func(b map[string]any) { b["logprobs"] = false; b["top_logprobs"] = 2 }},
		{"top_logprobs > 20", "top_logprobs", func(b map[string]any) { b["logprobs"] = true; b["top_logprobs"] = 21 }},
		{"logit_bias out of range", "logit_bias", func(b map[string]any) { b["logit_bias"] = map[string]int{"1": 101} }},
		{"frequency_penalty out of range", "frequency_penalty", func(b map[string]any) { b["frequency_penalty"] = 2.5 }},
		{"presence_penalty out of range", "presence_penalty", func(b map[string]any) { b["presence_penalty"] = -3 }},
		{"min_p out of range", "min_p", func(b map[string]any) { b["min_p"] = 1.5 }},
		{"repetition_penalty not positive", "repetition_penalty", func(b map[string]any) { b["repetition_penalty"] = 0 }},
		{"logit_bias key not a token id", "logit_bias", func(b map[string]any) { b["logit_bias"] = map[string]int{"abc": 1} }},
		{"logit_bias too many entries", "logit_bias", func(b map[string]any) {
			m := map[string]int{}
			for i := 0; i < 301; i++ {
				m[strconv.Itoa(i)] = 1
			}
			b["logit_bias"] = m
		}},
		{"chat_template_kwargs reserved key", "chat_template_kwargs", func(b map[string]any) { b["chat_template_kwargs"] = map[string]any{"messages": []any{}} }},
		{"chat_template_kwargs unknown key", "chat_template_kwargs", func(b map[string]any) { b["chat_template_kwargs"] = map[string]any{"system_prompt": "x"} }},
		{"chat_template_kwargs long string", "chat_template_kwargs", func(b map[string]any) {
			b["chat_template_kwargs"] = map[string]any{"reasoning_effort": strings.Repeat("a", 33)}
		}},
		{"chat_template_kwargs too big", "chat_template_kwargs", func(b map[string]any) {
			b["chat_template_kwargs"] = map[string]any{"enable_thinking": true, "thinking": strings.Repeat("a", 5000)}
		}},
		{"chat_template_kwargs array", "chat_template_kwargs", func(b map[string]any) { b["chat_template_kwargs"] = []int{1} }},
		{"guided_json too big", "guided_json", func(b map[string]any) { b["guided_json"] = map[string]any{"d": strings.Repeat("a", 70000)} }},
		{"chat_template_kwargs not an object", "chat_template_kwargs", func(b map[string]any) { b["chat_template_kwargs"] = "x" }},
	}
	for _, c := range cases {
		for _, be := range allBackends() {
			t.Run(c.name+"/"+be.name, func(t *testing.T) {
				h, prov := paramsHandler(t, be)
				body := baseChatBody("m")
				c.mutate(body)
				w, _ := doRawChat(t, h, body)
				if w.Code != http.StatusBadRequest || prov.calls != 0 {
					t.Fatalf("status=%d calls=%d, want 400 and 0; body=%s", w.Code, prov.calls, w.Body.String())
				}
				var e openAIError
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				if e.Error.Param != c.param || e.Error.Code != "invalid_request" {
					t.Errorf("error = %+v, want invalid_request / %s", e.Error, c.param)
				}
			})
		}
	}
}

func TestUser_ReachesEachBackendOnlyInItsPermittedForm(t *testing.T) {
	dir := mountSecret(t, "org-secret-org-secret")
	for _, be := range allBackends() {
		t.Run(be.name, func(t *testing.T) {
			h, prov := paramsHandler(t, be)
			h.SetUserPseudonymizer(userid.New(dir))
			body := baseChatBody("m")
			body["user"] = testEmail
			if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			got := prov.lastReq.User
			switch be.caps.UserID {
			case provider.UserIDRaw:
				if be.name != "vllm" {
					t.Errorf("%s must not receive the raw user", be.name)
				}
				if got != testEmail {
					t.Errorf("User = %q, want the raw value", got)
				}
			case provider.UserIDHMAC:
				want, _ := userid.New(dir).Pseudonym(testEmail)
				if got != want || !strings.HasPrefix(got, "k1:") {
					t.Errorf("User = %q, want %q", got, want)
				}
				// The raw value is nowhere in what the provider is handed.
				dump, _ := json.Marshal(prov.lastReq)
				if strings.Contains(string(dump), testEmail) || strings.Contains(string(dump), "alice") {
					t.Errorf("provider request carries the raw identifier: %s", dump)
				}
			default:
				if got != "" {
					t.Errorf("User = %q, want none", got)
				}
			}
		})
	}
}

func TestUser_HMACBackendWithoutUsableSecretOmitsTheFieldNeverRaw(t *testing.T) {
	for name, setup := range map[string]func(h *ChatHandler){
		"no pseudonymizer wired": func(h *ChatHandler) {},
		"secret not mounted":     func(h *ChatHandler) { h.SetUserPseudonymizer(userid.New(mountSecret(t, ""))) },
		"secret too short":       func(h *ChatHandler) { h.SetUserPseudonymizer(userid.New(mountSecret(t, "short"))) },
	} {
		for _, be := range allBackends() {
			if be.caps.UserID != provider.UserIDHMAC {
				continue
			}
			t.Run(name+"/"+be.name, func(t *testing.T) {
				h, prov := paramsHandler(t, be)
				setup(h)
				body := baseChatBody("m")
				body["user"] = testEmail
				if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK {
					t.Fatalf("a missing secret must not fail the request: %d %s", w.Code, w.Body.String())
				}
				if prov.lastReq.User != "" {
					t.Errorf("User = %q, want omitted", prov.lastReq.User)
				}
			})
		}
	}
}

// Different orgs (different secrets) never produce the same identifier for the
// same person, and one org's value is stable across requests.
func TestUser_PseudonymIsPerOrgAndStable(t *testing.T) {
	be := backendPersona{"anthropic", "anthropic", routing.ClassExternalAPI, (&anthropic.Provider{}).ParamCaps()}
	send := func(secret string) string {
		h, prov := paramsHandler(t, be)
		h.SetUserPseudonymizer(userid.New(mountSecret(t, secret)))
		body := baseChatBody("m")
		body["user"] = testEmail
		doRawChat(t, h, body)
		return prov.lastReq.User
	}
	a1, a2, b := send("org-a-secret-org-a"), send("org-a-secret-org-a"), send("org-b-secret-org-b")
	if a1 == "" || a1 != a2 || a1 == b {
		t.Errorf("a1=%q a2=%q b=%q", a1, a2, b)
	}
}

type stubCreds struct{}

func (stubCreds) Key() (string, uint64, error) { return "sk-ant-test", 1, nil }

// End to end through a real Anthropic provider over HTTP: the person's email
// is in the request the caller sent and nowhere in the body Anthropic gets;
// metadata.user_id carries the k1: pseudonym. Without a secret there is no
// metadata at all.
func TestAnthropicWire_UserIsPseudonymisedNeverRaw(t *testing.T) {
	run := func(t *testing.T, dir string) string {
		t.Helper()
		var outbound string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			outbound = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		t.Cleanup(srv.Close)
		prov := anthropic.New(stubCreds{}, anthropic.Config{DefaultMaxTokens: 64, BaseURL: srv.URL})
		h := newTestHandler(
			fakeProviders{"anthropic": prov},
			map[string]routing.Route{"claude-haiku-4-5": {ProviderKey: "anthropic", Kind: "anthropic", Class: routing.ClassExternalAPI}},
			policy.Snapshot{Enabled: map[string]bool{"claude-haiku-4-5": true}, Residency: policy.ResidencyAny},
			true,
		)
		h.SetUserPseudonymizer(userid.New(dir))
		body := baseChatBody("claude-haiku-4-5")
		body["user"] = testEmail
		body["seed"] = 5 // no Anthropic equivalent: must not appear
		w, _ := doRawChat(t, h, body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		return outbound
	}

	with := run(t, mountSecret(t, "org-secret-org-secret"))
	want, _ := userid.New(mountSecret(t, "org-secret-org-secret")).Pseudonym(testEmail)
	if !strings.Contains(with, `"user_id":"`+want+`"`) {
		t.Errorf("outbound body lacks metadata.user_id=%s: %s", want, with)
	}
	if strings.Contains(with, testEmail) || strings.Contains(with, "alice") {
		t.Errorf("outbound body carries the raw identifier: %s", with)
	}
	if strings.Contains(with, `"seed"`) {
		t.Errorf("seed reached Anthropic: %s", with)
	}

	without := run(t, mountSecret(t, ""))
	if strings.Contains(without, "metadata") || strings.Contains(without, testEmail) {
		t.Errorf("no secret: outbound body = %s, want no metadata and no raw user", without)
	}
}

// OpenAI is outside the customer's boundary: over real HTTP, the outbound body
// has the same k1: pseudonym Anthropic would get and never the raw email; with
// no secret the field is absent.
func TestOpenAIWire_UserIsPseudonymisedNeverRaw(t *testing.T) {
	run := func(t *testing.T, dir string) string {
		t.Helper()
		var outbound string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			outbound = string(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}))
		t.Cleanup(srv.Close)
		prov := openai.New(stubOpenAICreds{}, openai.Config{BaseURL: srv.URL})
		h := newTestHandler(
			fakeProviders{"openai": prov},
			map[string]routing.Route{"gpt-5": {ProviderKey: "openai", Kind: "openai", Class: routing.ClassExternalAPI}},
			policy.Snapshot{Enabled: map[string]bool{"gpt-5": true}, Residency: policy.ResidencyAny},
			true,
		)
		h.SetUserPseudonymizer(userid.New(dir))
		body := baseChatBody("gpt-5")
		body["user"] = testEmail
		if w, _ := doRawChat(t, h, body); w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		return outbound
	}
	dir := mountSecret(t, "org-secret-org-secret")
	with := run(t, dir)
	want, _ := userid.New(dir).Pseudonym(testEmail)
	if !strings.Contains(with, `"user":"`+want+`"`) {
		t.Errorf("outbound body lacks user=%s: %s", want, with)
	}
	if strings.Contains(with, testEmail) || strings.Contains(with, "alice") {
		t.Errorf("outbound body carries the raw identifier: %s", with)
	}
	// Same person, same value as Anthropic gets.
	anth := backendPersona{"anthropic", "anthropic", routing.ClassExternalAPI, (&anthropic.Provider{}).ParamCaps()}
	h, prov := paramsHandler(t, anth)
	h.SetUserPseudonymizer(userid.New(dir))
	body := baseChatBody("m")
	body["user"] = testEmail
	doRawChat(t, h, body)
	if prov.lastReq.User != want {
		t.Errorf("anthropic got %q, openai got %q", prov.lastReq.User, want)
	}
	if without := run(t, mountSecret(t, "")); strings.Contains(without, `"user":`) || strings.Contains(without, testEmail) {
		t.Errorf("no secret: outbound body = %s, want no user field", without)
	}
}

type stubOpenAICreds struct{}

func (stubOpenAICreds) Key() (string, uint64, error) { return "sk-test", 1, nil }

// logprobs a provider returns come back in OpenAI's shape on the response.
func TestLogprobsReturnedToTheCaller(t *testing.T) {
	const lp = `{"content":[{"token":"ok","logprob":-0.5,"bytes":[111,107],"top_logprobs":[]}],"refusal":null}`
	body, err := provider.BuildCompletionJSON(&provider.ChatResponse{ID: "x", Model: "m", Content: "ok", FinishReason: "stop", Logprobs: json.RawMessage(lp)})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Logprobs json.RawMessage `json:"logprobs"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(body, &got)
	var a, b any
	_ = json.Unmarshal(got.Choices[0].Logprobs, &a)
	_ = json.Unmarshal([]byte(lp), &b)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Errorf("logprobs = %s", got.Choices[0].Logprobs)
	}
}
