package anthropic

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func TestComplete_CacheTokensSplitAndPromptTotalUnchanged(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5",
				"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,
				"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":30,"cache_read_input_tokens":60}}`)
		},
	}
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 4096})
	resp, err := p.Complete(context.Background(), basicRequest("claude-haiku-4-5"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100 (input+cache_read+cache_creation, unchanged)", resp.PromptTokens)
	}
	if resp.CacheReadTokens != 60 || resp.CacheCreationTokens != 30 || resp.CompletionTokens != 5 {
		t.Errorf("read=%d create=%d completion=%d, want 60/30/5", resp.CacheReadTokens, resp.CacheCreationTokens, resp.CompletionTokens)
	}
}

func TestStream_CacheTokensFromMessageStartAndDelta(t *testing.T) {
	cases := []struct {
		name         string
		deltaUsage   string
		wantPrompt   int
		wantRead     int
		wantCreation int
	}{
		{"cache fields in message_start only", `{"output_tokens":4}`, 100, 60, 30},
		{"cumulative cache fields in message_delta", `{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":30,"cache_read_input_tokens":60}`, 100, 60, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeAnthropicServer(t)
			srv.messagesResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
				func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w,
						"event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_s","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_creation_input_tokens":30,"cache_read_input_tokens":60}}}`+"\n\n"+
							"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n"+
							"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n"+
							"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`+"\n\n"+
							"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":`+tc.deltaUsage+`}`+"\n\n"+
							"event: message_stop\ndata: "+`{"type":"message_stop"}`+"\n\n")
				},
			}
			p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
			var buf bytes.Buffer
			stats, err := p.Stream(context.Background(), basicRequest("claude-haiku-4-5"), &buf)
			if err != nil {
				t.Fatal(err)
			}
			if stats.PromptTokens != tc.wantPrompt || stats.CompletionTokens != 4 ||
				stats.CacheReadTokens != tc.wantRead || stats.CacheCreationTokens != tc.wantCreation {
				t.Errorf("prompt=%d completion=%d read=%d create=%d, want %d/4/%d/%d",
					stats.PromptTokens, stats.CompletionTokens, stats.CacheReadTokens, stats.CacheCreationTokens,
					tc.wantPrompt, tc.wantRead, tc.wantCreation)
			}
		})
	}
}
