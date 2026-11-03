package openaicompat_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComplete_PassesThroughCachedTokens(t *testing.T) {
	bodies := []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":50,"completion_tokens":3,"total_tokens":53,"prompt_tokens_details":{"cached_tokens":32}}}`,
		`{"id":"c2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":50,"completion_tokens":3,"total_tokens":53}}`,
	}
	want := []int{32, 0}
	for i, body := range bodies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		p := newTestProvider(t, srv.URL)
		resp, err := p.Complete(context.Background(), testRequest())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.PromptTokens != 50 || resp.CacheReadTokens != want[i] || resp.CacheCreationTokens != 0 {
			t.Errorf("case %d: prompt=%d read=%d create=%d, want 50/%d/0", i, resp.PromptTokens, resp.CacheReadTokens, resp.CacheCreationTokens, want[i])
		}
	}
}

func TestStream_PassesThroughCachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"s","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"s","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":3,"total_tokens":53,"prompt_tokens_details":{"cached_tokens":40}}}`+"\n\n"+
			"data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL)
	stats, err := p.Stream(context.Background(), testRequest(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PromptTokens != 50 || stats.CacheReadTokens != 40 || stats.CacheCreationTokens != 0 {
		t.Errorf("prompt=%d read=%d create=%d, want 50/40/0", stats.PromptTokens, stats.CacheReadTokens, stats.CacheCreationTokens)
	}
}

// A vendor body that closes cleanly with neither a finish_reason nor [DONE] is a reply
// cut short. A vendor may leave out [DONE], so a finish_reason alone is a whole reply.
func TestStream_EndedEarlyIsAnError(t *testing.T) {
	const words = `data: {"id":"s","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"
	for name, tc := range map[string]struct {
		body    string
		wantErr bool
	}{
		"neither":            {words, true},
		"[DONE] only":        {words + "data: [DONE]\n\n", false},
		"finish_reason only": {words + `data: {"id":"s","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n", false},
		"no space, [DONE]":   {"data:{}\n\ndata:[DONE]\n\n", false},
		"no space, finish":   {`data:{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := newTestProvider(t, srv.URL)
			if _, err := p.Stream(context.Background(), testRequest(), io.Discard); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// include_usage is answered whichever way the vendor spells the terminator.
func TestStream_IncludeUsageWithNoSpaceBeforeDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data:{"id":"s","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\ndata:[DONE]\n\n")
	}))
	defer srv.Close()
	p := newTestProvider(t, srv.URL)
	req := testRequest()
	req.IncludeUsage = true
	var buf strings.Builder
	if _, err := p.Stream(context.Background(), req, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"usage"`) {
		t.Errorf("no usage chunk: %q", buf.String())
	}
}
