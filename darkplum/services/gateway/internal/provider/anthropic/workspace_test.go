package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// headerRecorder is a standalone server that records the
// anthropic-workspace-id header of every request, per path.
type headerRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	seen map[string][]string // path -> header values ("<absent>" when missing)
}

func newHeaderRecorder(t *testing.T) *headerRecorder {
	t.Helper()
	h := &headerRecorder{seen: map[string][]string{}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := "<absent>"
		if vals, ok := r.Header["Anthropic-Workspace-Id"]; ok {
			v = vals[0]
		}
		h.mu.Lock()
		h.seen[r.URL.Path] = append(h.seen[r.URL.Path], v)
		h.mu.Unlock()
		reqBody, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/models":
			modelsPage([]map[string]any{modelInfo("claude-haiku-4-5", 8192)})(w)
		default:
			if strings.Contains(string(reqBody), `"stream":true`) {
				sseCompletion(w, "msg_1", "claude-haiku-4-5", []string{"hi"}, "end_turn", 1, 1)
				return
			}
			jsonCompletion(w, 200, "msg_1", "claude-haiku-4-5", "hi", "end_turn", 1, 1)
		}
	}))
	t.Cleanup(h.Server.Close)
	return h
}

func (h *headerRecorder) only(t *testing.T, path, want string) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	got := h.seen[path]
	if len(got) == 0 {
		t.Fatalf("no request seen on %s", path)
	}
	for _, g := range got {
		if g != want {
			t.Errorf("%s: anthropic-workspace-id = %q, want %q (all: %v)", path, g, want, got)
		}
	}
}

func TestWorkspaceHeader_SentOnEveryRequestWhenSet(t *testing.T) {
	h := newHeaderRecorder(t)
	ws := "wrkspc_01AbC-d_9"
	p := New(&fakeCreds{key: "sk-ant-k", version: 1}, Config{
		DefaultMaxTokens: 100, BaseURL: h.URL, WorkspaceID: func() string { return ws },
	})
	ctx := context.Background()

	if _, err := p.Complete(ctx, basicRequest("claude-haiku-4-5")); err != nil {
		t.Fatalf("complete: %v", err)
	}
	var sb strings.Builder
	if _, err := p.Stream(ctx, basicRequest("claude-haiku-4-5"), &sb); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if _, err := p.DiscoverModels(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if _, err := p.VerifyKey(ctx, "sk-ant-candidate", ws); err != nil {
		t.Fatalf("verify: %v", err)
	}
	h.only(t, "/v1/messages", ws)
	h.only(t, "/v1/models", ws)
	if n := len(h.seen["/v1/messages"]); n != 2 {
		t.Errorf("expected messages + stream requests, saw %d", n)
	}
	if n := len(h.seen["/v1/models"]); n != 2 {
		t.Errorf("expected discovery + verify requests, saw %d", n)
	}
}

func TestWorkspaceHeader_AbsentWhenNotSet(t *testing.T) {
	h := newHeaderRecorder(t)
	for name, fn := range map[string]func() string{"nil_func": nil, "empty": func() string { return "" }} {
		t.Run(name, func(t *testing.T) {
			h.mu.Lock()
			h.seen = map[string][]string{}
			h.mu.Unlock()
			p := New(&fakeCreds{key: "sk-ant-k", version: 1}, Config{DefaultMaxTokens: 100, BaseURL: h.URL, WorkspaceID: fn})
			ctx := context.Background()
			if _, err := p.Complete(ctx, basicRequest("claude-haiku-4-5")); err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			if _, err := p.Stream(ctx, basicRequest("claude-haiku-4-5"), &sb); err != nil {
				t.Fatal(err)
			}
			if _, err := p.DiscoverModels(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := p.VerifyKey(ctx, "sk-ant-candidate", ""); err != nil {
				t.Fatal(err)
			}
			h.only(t, "/v1/messages", "<absent>")
			h.only(t, "/v1/models", "<absent>")
		})
	}
}

// The effective workspace id is read per request: changing it must take effect
// without rebuilding the provider or its cached SDK client.
func TestWorkspaceHeader_FollowsLiveValue(t *testing.T) {
	h := newHeaderRecorder(t)
	cur := ""
	p := New(&fakeCreds{key: "sk-ant-k", version: 1}, Config{
		DefaultMaxTokens: 100, BaseURL: h.URL, WorkspaceID: func() string { return cur },
	})
	ctx := context.Background()
	for _, want := range []string{"<absent>", "wrkspc_a", "<absent>"} {
		cur = strings.TrimPrefix(want, "<absent>")
		h.mu.Lock()
		h.seen = map[string][]string{}
		h.mu.Unlock()
		if _, err := p.Complete(ctx, basicRequest("claude-haiku-4-5")); err != nil {
			t.Fatal(err)
		}
		h.only(t, "/v1/messages", want)
	}
}

const wsRequiredMsg = "This API key is not scoped to a workspace, so this request must include the anthropic-workspace-id header with the ID of the workspace to use."

func TestWorkspaceRequired_ClassifiedFromParsedSDKError(t *testing.T) {
	srv := newFakeAnthropicServer(t)
	srv.modelsResponse = nil
	srv.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVendorError(w, 400, "invalid_request_error", wsRequiredMsg+" SENTINEL-acct-4242")
	})
	p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})

	_, err := p.VerifyKey(context.Background(), "sk-ant-x", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || !apiErr.WorkspaceRequired {
		t.Fatalf("expected WorkspaceRequired on the translated error, got %#v", err)
	}
	if strings.Contains(string(apiErr.Body), "SENTINEL") || strings.Contains(string(apiErr.Body), "anthropic-workspace-id") {
		t.Errorf("vendor text leaked into the translated body: %s", apiErr.Body)
	}

	_, derr := p.DiscoverModels(context.Background())
	if !IsWorkspaceRequiredError(derr) {
		t.Errorf("raw discovery error must classify as workspace-required: %v", derr)
	}
}

func TestWorkspaceRequired_OtherBadRequestsAreNot(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		msg    string
	}{
		"other_400":         {400, "max_tokens: Field required"},
		"marker_on_non_400": {403, wsRequiredMsg},
		"marker_on_5xx":     {500, wsRequiredMsg},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeAnthropicServer(t)
			srv.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeVendorError(w, tc.status, "invalid_request_error", tc.msg)
			})
			p := newTestProvider(t, srv, nil, Config{DefaultMaxTokens: 100})
			_, err := p.VerifyKey(context.Background(), "sk-ant-x", "")
			var apiErr *APIError
			if !asAPIError(err, &apiErr) {
				t.Fatalf("expected an APIError, got %v", err)
			}
			if apiErr.WorkspaceRequired {
				t.Error("must not be workspace-required")
			}
		})
	}
}

func asAPIError(err error, target **APIError) bool { return errors.As(err, target) }
