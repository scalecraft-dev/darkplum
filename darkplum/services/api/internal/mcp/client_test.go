package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeServer is a tiny MCP server: initialize, tools/list, tools/call (an
// "echo" tool), answering as JSON or as SSE depending on sse.
func fakeServer(t *testing.T, sse bool) (*httptest.Server, *[]string) {
	t.Helper()
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json, text/event-stream" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %s", body)
		}
		methods = append(methods, req.Method)
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			result = map[string]any{"protocolVersion": ProtocolVersion, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Errorf("session id not echoed")
			}
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "Echoes", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "boom" {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "it broke"}}, "isError": true}
			} else {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo:" + string(p.Arguments)}, {"type": "image"}}}
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			// A notification first, then the response, as servers do.
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "data: %s\n\n", resp)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &methods
}

func policyFor(srv *httptest.Server) *HostPolicy {
	u, _ := url.Parse(srv.URL)
	return &HostPolicy{Allowed: []string{u.Hostname()}}
}

func TestClient_RoundTrip(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			srv, methods := fakeServer(t, sse)
			c := &Client{URL: srv.URL, Policy: policyFor(srv), HTTP: srv.Client()}
			ctx := context.Background()
			if err := c.Initialize(ctx); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if c.ServerName() != "fake" || c.SessionID() != "sess-1" {
				t.Errorf("server=%q session=%q", c.ServerName(), c.SessionID())
			}
			tools, err := c.ListTools(ctx)
			if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
				t.Fatalf("tools = %+v, err = %v", tools, err)
			}
			res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"a":1}`))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError || res.Text() != "echo:{\"a\":1}\n[image]" {
				t.Errorf("result = %+v text=%q", res, res.Text())
			}
			res, err = c.CallTool(ctx, "boom", nil)
			if err != nil || !res.IsError || res.Text() != "it broke" {
				t.Errorf("error result = %+v, err = %v", res, err)
			}
			want := []string{"initialize", "notifications/initialized", "tools/list", "tools/call", "tools/call"}
			if strings.Join(*methods, ",") != strings.Join(want, ",") {
				t.Errorf("methods = %v", *methods)
			}
		})
	}
}

func TestClient_PolicyRefusesBeforeConnecting(t *testing.T) {
	srv, methods := fakeServer(t, false)
	c := &Client{URL: srv.URL, Policy: &HostPolicy{Allowed: []string{"other.example"}}, HTTP: srv.Client()}
	if err := c.Initialize(context.Background()); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("err = %v, want ErrNotAllowed", err)
	}
	if len(*methods) != 0 {
		t.Errorf("server was contacted: %v", *methods)
	}
	c = &Client{URL: srv.URL, HTTP: srv.Client()} // nil policy
	if err := c.Initialize(context.Background()); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("nil policy: err = %v", err)
	}
}

func TestClient_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no such method"}}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, Policy: policyFor(srv), HTTP: srv.Client()}
	err := c.Initialize(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Code != -32601 {
		t.Fatalf("err = %v", err)
	}
}

func TestHostPolicy_Allows(t *testing.T) {
	p := &HostPolicy{Allowed: []string{"mcp.corp.example", "*.svc.cluster.local", "localhost"}}
	for host, want := range map[string]bool{
		"mcp.corp.example":                true,
		"MCP.corp.example":                true,
		"evil-mcp.corp.example":           false,
		"tools.default.svc.cluster.local": true,
		"svc.cluster.local":               false,
		"localhost":                       true,
		"169.254.169.254":                 false,
	} {
		if got := p.Allows(host); got != want {
			t.Errorf("Allows(%q) = %v, want %v", host, got, want)
		}
	}
	if err := p.Check("ftp://mcp.corp.example/x"); err == nil {
		t.Error("ftp scheme accepted")
	}
	if err := p.Check("https://mcp.corp.example/mcp"); err != nil {
		t.Errorf("allowed host refused: %v", err)
	}
	if err := (&HostPolicy{}).Check("https://mcp.corp.example/mcp"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("empty policy allowed a host: %v", err)
	}
}

func TestHostPolicy_TransportRefusesLoopbackUnlessLiteral(t *testing.T) {
	// Allowed by wildcard only: resolves to loopback -> refused at dial time.
	p := &HostPolicy{Allowed: []string{"*.localhost"}}
	tr := p.Transport()
	req, _ := http.NewRequest(http.MethodGet, "http://foo.localhost:1/", nil)
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "reserved address") {
		t.Errorf("wildcard loopback: err = %v", err)
	}
	// The literal is allowed: the dial proceeds (and fails on the closed port).
	p = &HostPolicy{Allowed: []string{"localhost"}}
	req, _ = http.NewRequest(http.MethodGet, "http://localhost:1/", nil)
	if _, err := p.Transport().RoundTrip(req); err == nil || strings.Contains(err.Error(), "reserved address") {
		t.Errorf("literal loopback: err = %v", err)
	}
}

func TestClient_RedirectsAreNotFollowed(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	tu, _ := url.Parse(target.URL)
	c := &Client{URL: srv.URL, Policy: &HostPolicy{Allowed: []string{u.Hostname(), tu.Hostname()}}, HTTP: srv.Client()}
	err := c.Initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Errorf("err = %v, want an HTTP 307 refusal", err)
	}
	if hits != 0 {
		t.Errorf("redirect target was contacted %d times", hits)
	}
}
