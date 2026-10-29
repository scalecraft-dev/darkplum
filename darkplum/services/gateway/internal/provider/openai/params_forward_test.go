package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Phase A2: the parameters this backend forwards are on the wire under their
// OpenAI names with the caller's value, and absent otherwise.

func wireBody(t *testing.T, req provider.ChatRequest, stream bool) map[string]any {
	t.Helper()
	b, err := buildRequestBody(req, stream, droppedParams{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildRequestBody_ForwardedParameters(t *testing.T) {
	seed := int64(42)
	fp, pp := 0.5, -0.25
	top := 3
	no := false
	req := basicRequest("gpt-5")
	req.Seed = &seed
	req.FrequencyPenalty = &fp
	req.PresencePenalty = &pp
	req.Logprobs = true
	req.TopLogprobs = &top
	req.LogitBias = map[string]float64{"50256": -100}
	req.Tools = []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "f"}}}
	req.ParallelToolCalls = &no
	req.NoStore = true
	req.User = "user-123"

	body := wireBody(t, req, false)
	want := map[string]any{
		"seed": float64(42), "frequency_penalty": 0.5, "presence_penalty": -0.25,
		"logprobs": true, "top_logprobs": float64(3),
		"parallel_tool_calls": false, "store": false, "user": "user-123",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v (%T), want %v", k, body[k], body[k], v)
		}
	}
	if lb, _ := body["logit_bias"].(map[string]any); lb["50256"] != float64(-100) {
		t.Errorf("logit_bias = %v", body["logit_bias"])
	}
}

func TestBuildRequestBody_ForwardedParametersAbsentWhenNotSent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body := wireBody(t, basicRequest("gpt-5"), stream)
		for _, k := range []string{"seed", "frequency_penalty", "presence_penalty", "logprobs", "top_logprobs",
			"logit_bias", "parallel_tool_calls", "store", "user", "top_k", "min_p", "repetition_penalty", "guided_json", "chat_template_kwargs"} {
			if _, has := body[k]; has {
				t.Errorf("stream=%v: %s present on a request that did not set it", stream, k)
			}
		}
	}
}

// OpenAI rejects parallel_tool_calls on a request without tools, and
// top_logprobs without logprobs: neither is sent in those shapes.
func TestBuildRequestBody_DependentFieldsNeedTheirParent(t *testing.T) {
	no := false
	top := 2
	req := basicRequest("gpt-5")
	req.ParallelToolCalls = &no
	req.TopLogprobs = &top
	body := wireBody(t, req, false)
	for _, k := range []string{"parallel_tool_calls", "top_logprobs", "logprobs"} {
		if _, has := body[k]; has {
			t.Errorf("%s sent without its parent field", k)
		}
	}
}

// vLLM extensions are not OpenAI parameters and never reach OpenAI.
func TestBuildRequestBody_VLLMExtensionsNeverSent(t *testing.T) {
	k := 5
	req := basicRequest("gpt-5")
	req.VLLM = provider.VLLMExtensions{TopK: &k, GuidedJSON: json.RawMessage(`{"type":"object"}`)}
	body := wireBody(t, req, false)
	for _, name := range []string{"top_k", "guided_json"} {
		if _, has := body[name]; has {
			t.Errorf("%s sent to OpenAI", name)
		}
	}
}

// The adaptive sampling retry must never swallow a forwarded parameter.
func TestBuildRequestBody_DropNeverRemovesForwardedParameters(t *testing.T) {
	seed := int64(1)
	req := basicRequest("gpt-5")
	req.Seed = &seed
	req.User = "u"
	b, _ := buildRequestBody(req, false, droppedParams{temperature: true, topP: true, stop: true})
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	if body["seed"] == nil || body["user"] == nil {
		t.Errorf("retry dropped a forwarded field: %v", body)
	}
}

func TestComplete_400OnForwardedParameterNamesItAndIsNotRetried(t *testing.T) {
	for _, param := range []string{"seed", "frequency_penalty", "presence_penalty", "logprobs", "top_logprobs", "logit_bias", "parallel_tool_calls", "store", "user"} {
		t.Run(param, func(t *testing.T) {
			srv := newFakeOpenAIServer(t)
			srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
				func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
					writeVendorError(w, http.StatusBadRequest, "invalid_request_error", "unsupported value acct org-9", param)
				},
			}
			p := newTestProvider(t, srv, nil, Config{})
			seed := int64(1)
			req := basicRequest("gpt-5")
			req.Seed = &seed
			_, err := p.Complete(context.Background(), req)
			apiErr, ok := err.(*APIError)
			if !ok {
				t.Fatalf("err = %v, want APIError", err)
			}
			if srv.callCount() != 1 {
				t.Errorf("calls = %d, want 1", srv.callCount())
			}
			body := string(apiErr.ResponseBody())
			if !strings.Contains(body, `"param":"`+param+`"`) || strings.Contains(body, "org-9") {
				t.Errorf("body = %s", body)
			}
		})
	}
}

const sampleLogprobs = `{"content":[{"token":"Hi","logprob":-0.1,"bytes":[72,105],"top_logprobs":[{"token":"Hi","logprob":-0.1,"bytes":[72,105]},{"token":"Hello","logprob":-2.3,"bytes":null}]}],"refusal":null}`

func TestComplete_RelaysLogprobsInOpenAIShape(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	var sent map[string]any
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
			sent = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":` + sampleLogprobs + `,"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		},
	}
	p := newTestProvider(t, srv, nil, Config{})
	top := 2
	req := basicRequest("gpt-5")
	req.Logprobs = true
	req.TopLogprobs = &top
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if sent["logprobs"] != true || sent["top_logprobs"] != float64(2) {
		t.Errorf("request body = %v", sent)
	}
	out, _ := provider.BuildCompletionJSON(resp)
	var got struct {
		Choices []struct {
			Logprobs json.RawMessage `json:"logprobs"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(out, &got)
	if !jsonEqual(t, got.Choices[0].Logprobs, sampleLogprobs) {
		t.Errorf("logprobs = %s, want %s", got.Choices[0].Logprobs, sampleLogprobs)
	}
}

func TestComplete_NoLogprobsKeyWhenVendorSentNone(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":null,"finish_reason":"stop"}]}`))
		},
	}
	resp, err := newTestProvider(t, srv, nil, Config{}).Complete(context.Background(), basicRequest("gpt-5"))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := provider.BuildCompletionJSON(resp)
	if strings.Contains(string(out), "logprobs") {
		t.Errorf("logprobs key present on a response without them: %s", out)
	}
}

func TestStream_RelaysLogprobsOnTheDelta(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			frames := []string{
				`{"id":"c1","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"logprobs":` + sampleLogprobs + `,"finish_reason":null}]}`,
				`{"id":"c1","model":"gpt-5","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}`,
				`{"id":"c1","model":"gpt-5","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			}
			for _, f := range frames {
				_, _ = w.Write([]byte("data: " + f + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		},
	}
	p := newTestProvider(t, srv, nil, Config{})
	req := basicRequest("gpt-5")
	req.Logprobs = true
	var buf bytes.Buffer
	if _, err := p.Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta    struct{ Content string } `json:"delta"`
				Logprobs json.RawMessage          `json:"logprobs"`
			} `json:"choices"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk)
		if len(chunk.Choices) == 1 && chunk.Choices[0].Delta.Content == "Hi" {
			found = jsonEqual(t, chunk.Choices[0].Logprobs, sampleLogprobs)
		}
	}
	if !found {
		t.Errorf("content chunk with logprobs not found in stream:\n%s", buf.String())
	}
}

func jsonEqual(t *testing.T, a json.RawMessage, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// A delta carrying tool-call fragments and logprobs keeps its logprobs, on the
// first chunk emitted for it and not duplicated.
func TestStream_ToolCallDeltaKeepsLogprobs(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			frames := []string{
				`{"id":"c1","model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}},{"index":1,"id":"call_2","type":"function","function":{"name":"g","arguments":"{}"}}]},"logprobs":` + sampleLogprobs + `,"finish_reason":null}]}`,
				`{"id":"c1","model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			}
			for _, f := range frames {
				_, _ = w.Write([]byte("data: " + f + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		},
	}
	req := basicRequest("gpt-5")
	req.Logprobs = true
	var buf bytes.Buffer
	if _, err := newTestProvider(t, srv, nil, Config{}).Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), `"logprobs":{"content"`); n != 1 {
		t.Errorf("logprobs appear %d times, want exactly once:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), `"tool_calls"`) {
		t.Errorf("tool-call fragments lost:\n%s", buf.String())
	}
}
