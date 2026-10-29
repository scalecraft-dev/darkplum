package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Phase A2 on the vLLM/OpenAI-compatible backend: forwarded parameters and
// the vLLM extensions are on the wire under their names, absent otherwise;
// logprobs come back in OpenAI's shape on every path.

const compatLogprobs = `{"content":[{"token":"Hi","logprob":-0.1,"bytes":[72,105],"top_logprobs":[{"token":"Hi","logprob":-0.1,"bytes":[72,105]}]}],"refusal":null}`

// paramServer answers every request with respond and records the decoded
// request body.
func paramServer(t *testing.T, respond func(w http.ResponseWriter)) (*httptest.Server, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(raw, &got)
		respond(w)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func okJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buildMockCompletion("c1", "llama", "Hi", 1, 1))
}

func TestWire_ForwardedParameters(t *testing.T) {
	srv, got := paramServer(t, okJSON)
	p := newTestProvider(t, srv.URL)

	seed := int64(7)
	fp, pp := 0.3, 0.6
	top := 4
	k := 40
	minp, rep := 0.05, 1.1
	req := testRequest()
	req.Seed = &seed
	req.FrequencyPenalty = &fp
	req.PresencePenalty = &pp
	req.Logprobs = true
	req.TopLogprobs = &top
	req.LogitBias = map[string]float64{"1": 5}
	req.User = "u-1"
	req.VLLM = provider.VLLMExtensions{
		TopK: &k, MinP: &minp, RepetitionPenalty: &rep,
		GuidedJSON:         json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		ChatTemplateKwargs: json.RawMessage(`{"enable_thinking":false}`),
	}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b := *got
	scalars := map[string]any{
		"seed": float64(7), "frequency_penalty": 0.3, "presence_penalty": 0.6,
		"logprobs": true, "top_logprobs": float64(4), "user": "u-1",
		"top_k": float64(40), "min_p": 0.05, "repetition_penalty": 1.1,
	}
	for name, want := range scalars {
		if b[name] != want {
			t.Errorf("%s = %v, want %v", name, b[name], want)
		}
	}
	if lb, _ := b["logit_bias"].(map[string]any); lb["1"] != float64(5) {
		t.Errorf("logit_bias = %v", b["logit_bias"])
	}
	if g, _ := b["guided_json"].(map[string]any); g["type"] != "object" {
		t.Errorf("guided_json = %v", b["guided_json"])
	}
	if c, _ := b["chat_template_kwargs"].(map[string]any); c["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs = %v", b["chat_template_kwargs"])
	}
	// parallel_tool_calls is never forwarded to vLLM (it ignores it).
	if _, has := b["parallel_tool_calls"]; has {
		t.Error("parallel_tool_calls must not be forwarded")
	}
}

func TestWire_NothingExtraWhenNotSent(t *testing.T) {
	for _, stream := range []bool{false, true} {
		srv, got := paramServer(t, okJSON)
		p := newTestProvider(t, srv.URL)
		req := testRequest()
		req.Stream = stream
		if stream {
			var buf bytes.Buffer
			if _, err := p.Stream(context.Background(), req, &buf); err != nil {
				t.Fatal(err)
			}
		} else if _, err := p.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"seed", "frequency_penalty", "presence_penalty", "logprobs", "top_logprobs", "logit_bias",
			"parallel_tool_calls", "store", "user", "top_k", "min_p", "repetition_penalty", "guided_json", "chat_template_kwargs"} {
			if _, has := (*got)[name]; has {
				t.Errorf("stream=%v: %s present on a request that did not set it", stream, name)
			}
		}
	}
}

func TestComplete_RelaysLogprobs(t *testing.T) {
	srv, _ := paramServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"llama","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":` + compatLogprobs + `,"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	req := testRequest()
	req.Logprobs = true
	resp, err := newTestProvider(t, srv.URL).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := provider.BuildCompletionJSON(resp)
	var got struct {
		Choices []struct {
			Logprobs json.RawMessage `json:"logprobs"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(out, &got)
	if !sameJSON(got.Choices[0].Logprobs, compatLogprobs) {
		t.Errorf("logprobs = %s", got.Choices[0].Logprobs)
	}
}

func TestStream_LogprobsPassThroughSSE(t *testing.T) {
	srv, _ := paramServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"c1","model":"llama","choices":[{"index":0,"delta":{"content":"Hi"},"logprobs":` + compatLogprobs + `,"finish_reason":null}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"c1","model":"llama","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	req := testRequest()
	req.Stream = true
	req.Logprobs = true
	var buf bytes.Buffer
	if _, err := newTestProvider(t, srv.URL).Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"logprobs":`+compatLogprobs) {
		t.Errorf("logprobs not relayed on the stream:\n%s", buf.String())
	}
}

// An upstream that ignores stream:true and answers with plain JSON is adapted
// to SSE; its logprobs must survive the adaptation.
func TestStream_AdaptedJSONUpstreamKeepsLogprobs(t *testing.T) {
	srv, _ := paramServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"llama","choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"logprobs":` + compatLogprobs + `,"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	req := testRequest()
	req.Stream = true
	req.Logprobs = true
	var buf bytes.Buffer
	if _, err := newTestProvider(t, srv.URL).Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var c struct {
			Choices []struct {
				Delta    struct{ Content string } `json:"delta"`
				Logprobs json.RawMessage          `json:"logprobs"`
			} `json:"choices"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c)
		if len(c.Choices) == 1 && c.Choices[0].Delta.Content == "Hi" && sameJSON(c.Choices[0].Logprobs, compatLogprobs) {
			found = true
		}
	}
	if !found {
		t.Errorf("adapted stream lost the logprobs:\n%s", buf.String())
	}
}

func sameJSON(a json.RawMessage, b string) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
