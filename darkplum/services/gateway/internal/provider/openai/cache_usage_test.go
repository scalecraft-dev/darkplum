package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func TestComplete_CachedTokensParsed(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","model":"gpt-5-mini",
				"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":64}}}`)
		},
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c2","model":"gpt-5-mini",
				"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	resp, err := p.Complete(context.Background(), basicRequest("gpt-5-mini"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.PromptTokens != 100 || resp.CompletionTokens != 7 || resp.CacheReadTokens != 64 || resp.CacheCreationTokens != 0 {
		t.Errorf("got prompt=%d completion=%d read=%d create=%d, want 100/7/64/0",
			resp.PromptTokens, resp.CompletionTokens, resp.CacheReadTokens, resp.CacheCreationTokens)
	}

	resp, err = p.Complete(context.Background(), basicRequest("gpt-5-mini"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.PromptTokens != 10 || resp.CacheReadTokens != 0 {
		t.Errorf("no details: prompt=%d read=%d, want 10/0", resp.PromptTokens, resp.CacheReadTokens)
	}
}

func TestStream_CachedTokensParsedFromFinalUsageChunk(t *testing.T) {
	srv := newFakeOpenAIServer(t)
	srv.completionResponses = []func(w http.ResponseWriter, r *http.Request, body map[string]any){
		func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"s1","model":"gpt-5-mini","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"s1","model":"gpt-5-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
				`data: {"id":"s1","model":"gpt-5-mini","choices":[],"usage":{"prompt_tokens":200,"completion_tokens":9,"total_tokens":209,"prompt_tokens_details":{"cached_tokens":128}}}`+"\n\n"+
				"data: [DONE]\n\n")
		},
	}
	p := newTestProvider(t, srv, nil, Config{})

	var buf bytes.Buffer
	stats, err := p.Stream(context.Background(), basicRequest("gpt-5-mini"), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PromptTokens != 200 || stats.CompletionTokens != 9 || stats.CacheReadTokens != 128 || stats.CacheCreationTokens != 0 {
		t.Errorf("got prompt=%d completion=%d read=%d create=%d, want 200/9/128/0",
			stats.PromptTokens, stats.CompletionTokens, stats.CacheReadTokens, stats.CacheCreationTokens)
	}
}
