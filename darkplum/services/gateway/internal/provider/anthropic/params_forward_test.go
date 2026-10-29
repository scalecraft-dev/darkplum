package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// Phase A2 on Anthropic: parallel_tool_calls:false becomes
// disable_parallel_tool_use; `user` becomes metadata.user_id exactly as the
// handler supplied it (already pseudonymised); seed, penalties, logprobs and
// the vLLM extensions have no Messages API equivalent and never reach the wire.

func TestParallelToolCallsFalse_DisablesParallelToolUse(t *testing.T) {
	no, yes := false, true
	weather := provider.Tool{Type: "function", Function: provider.ToolFunction{Name: "get_weather"}}
	cases := []struct {
		name     string
		choice   string
		parallel *bool
		want     string // tool_choice JSON, "" = omitted
	}{
		{"false, no tool_choice -> auto + disable", "", &no, `{"type":"auto","disable_parallel_tool_use":true}`},
		{"false, auto", `"auto"`, &no, `{"type":"auto","disable_parallel_tool_use":true}`},
		{"false, required", `"required"`, &no, `{"type":"any","disable_parallel_tool_use":true}`},
		{"false, named", `{"type":"function","function":{"name":"get_weather"}}`, &no, `{"type":"tool","name":"get_weather","disable_parallel_tool_use":true}`},
		{"false, none is a no-op", `"none"`, &no, `{"type":"none"}`},
		{"true is the default, nothing sent", "", &yes, ""},
		{"absent, nothing sent", "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := basicRequest("claude-haiku-4-5")
			req.Tools = []provider.Tool{weather}
			req.ParallelToolCalls = tc.parallel
			if tc.choice != "" {
				req.ToolChoice = json.RawMessage(tc.choice)
			}
			body := wireBody(t, req)
			if tc.want == "" {
				if v, ok := body["tool_choice"]; ok {
					t.Errorf("tool_choice = %v, want omitted", v)
				}
				return
			}
			if got, want := body["tool_choice"], mustJSON(t, tc.want); !reflect.DeepEqual(got, want) {
				t.Errorf("tool_choice = %v, want %v", got, want)
			}
		})
	}
}

func TestParallelToolCallsFalse_WithoutToolsIsANoOp(t *testing.T) {
	no := false
	req := basicRequest("claude-haiku-4-5")
	req.ParallelToolCalls = &no
	body := wireBody(t, req)
	if _, ok := body["tool_choice"]; ok {
		t.Errorf("tool_choice sent without tools: %v", body["tool_choice"])
	}
	if _, ok := body["tools"]; ok {
		t.Errorf("tools sent: %v", body["tools"])
	}
}

func TestUser_BecomesMetadataUserIDVerbatim(t *testing.T) {
	req := basicRequest("claude-haiku-4-5")
	req.User = "k1:abc123"
	body := wireBody(t, req)
	md, _ := body["metadata"].(map[string]any)
	if md["user_id"] != "k1:abc123" {
		t.Errorf("metadata = %v, want user_id k1:abc123", body["metadata"])
	}

	if _, has := wireBody(t, basicRequest("claude-haiku-4-5"))["metadata"]; has {
		t.Error("metadata sent without a user")
	}
}

func TestInapplicableParametersNeverReachTheWire(t *testing.T) {
	seed := int64(3)
	fp := 0.5
	top := 2
	k := 10
	req := basicRequest("claude-haiku-4-5")
	req.Seed = &seed
	req.FrequencyPenalty = &fp
	req.PresencePenalty = &fp
	req.Logprobs = true
	req.TopLogprobs = &top
	req.LogitBias = map[string]float64{"1": 1}
	req.NoStore = true
	req.VLLM = provider.VLLMExtensions{TopK: &k, GuidedJSON: json.RawMessage(`{}`)}
	body := wireBody(t, req)
	for _, name := range []string{"seed", "frequency_penalty", "presence_penalty", "logprobs", "top_logprobs", "logit_bias",
		"store", "user", "top_k", "guided_json", "chat_template_kwargs", "min_p", "repetition_penalty"} {
		if _, has := body[name]; has {
			t.Errorf("%s reached Anthropic", name)
		}
	}
}

// End to end over HTTP, stream and non-stream: the email the caller sent is
// nowhere in the outbound body when the handler hands the provider the
// pseudonym, and the body carries exactly that pseudonym.
func TestUserOnTheWire_NoRawIdentifier(t *testing.T) {
	for _, stream := range []bool{false, true} {
		srv := newFakeAnthropicServer(t)
		var raw []byte
		srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
			func(w http.ResponseWriter, r *http.Request, body map[string]any) {
				raw, _ = json.Marshal(body)
				if stream {
					sseCompletion(w, "msg_1", "claude-haiku-4-5", []string{"hi"}, "end_turn", 1, 1)
					return
				}
				jsonCompletion(w, http.StatusOK, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
			},
		}
		p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
		req := basicRequest("claude-haiku-4-5")
		req.User = "k1:deadbeef"
		var err error
		if stream {
			_, err = p.Stream(context.Background(), req, &bytes.Buffer{})
		} else {
			_, err = p.Complete(context.Background(), req)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"user_id":"k1:deadbeef"`) {
			t.Errorf("stream=%v: body = %s, want metadata.user_id", stream, raw)
		}
		if strings.Contains(string(raw), "@") {
			t.Errorf("stream=%v: body contains an identifier: %s", stream, raw)
		}
	}
}
