// Package mcp is a minimal Model Context Protocol client over the streamable
// HTTP transport: enough to initialize a session, list a server's tools and
// call one. It is deliberately hand-written rather than a dependency so the
// api controls exactly what leaves the cluster (see HostPolicy) and how much
// comes back (bodies are capped).
//
// Transport (MCP 2025-03-26 "Streamable HTTP"): every message is a JSON-RPC
// 2.0 POST to the server URL with Accept: application/json, text/event-stream.
// The server answers a request either with one JSON body or with an SSE stream
// whose events carry JSON-RPC messages; the response to the request is the
// message whose id matches. A notification gets 202 and no body. The server
// may hand out an Mcp-Session-Id on initialize, echoed on every later call.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// MaxResponseBytes caps what is read from a server for one message. A tool
// result larger than this is a misbehaving server, not a bigger context.
const MaxResponseBytes = 4 << 20

// Tool is one entry of a server's tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ContentBlock is one block of a tools/call result's content.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Non-text blocks (image, audio, resource) are kept only by type and are
	// summarised as "[<type>]" in Text(); nothing binary is relayed.
}

// CallResult is what tools/call returned.
type CallResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Text flattens the result for a model: text blocks joined by newlines, any
// other block named by type, and structured content as JSON when there is no
// text at all.
func (r *CallResult) Text() string {
	var parts []string
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	if len(parts) == 0 && len(r.StructuredContent) > 0 {
		return string(r.StructuredContent)
	}
	return strings.Join(parts, "\n")
}

// Error is a JSON-RPC error the server returned.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp: server error %d: %s", e.Code, e.Message) }

// ErrNotAllowed is returned before any connection is made when HostPolicy
// refuses the server's host.
var ErrNotAllowed = errors.New("mcp: server host is not on the allowed list")

// Client talks to one MCP server. It is not safe for concurrent use; the
// chat loop uses one per server per request.
type Client struct {
	// URL is the server's streamable-HTTP endpoint.
	URL string
	// Headers are sent on every request (an Authorization header, typically).
	Headers map[string]string
	// HTTP is the client used; Dialer below is applied when HTTP is nil.
	HTTP *http.Client
	// Policy decides whether URL's host may be contacted at all. nil allows
	// nothing: a server is reachable only through an explicit policy.
	Policy *HostPolicy

	sessionID string
	nextID    atomic.Int64
	// http is the client built from Policy on first use (so one transport,
	// and its idle connections, serves every message of a session).
	http       *http.Client
	serverInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
}

// ServerName is what the server called itself on initialize ("" before).
func (c *Client) ServerName() string { return c.serverInfo.Name }

// SessionID is the Mcp-Session-Id the server assigned ("" if none).
func (c *Client) SessionID() string { return c.sessionID }

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *Error          `json:"error"`
	Method  string          `json:"method"` // set on server-initiated messages, which are ignored
}

// Initialize opens the session: initialize, then notifications/initialized.
func (c *Client) Initialize(ctx context.Context) error {
	if c.Policy == nil {
		return ErrNotAllowed
	}
	if err := c.Policy.Check(c.URL); err != nil {
		return err
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "darkplum", "version": "1"},
	}, &result); err != nil {
		return err
	}
	c.serverInfo = result.ServerInfo
	return c.notify(ctx, "notifications/initialized")
}

// ListTools returns the server's tools (first page; servers that paginate
// are followed up to 20 pages).
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &result); err != nil {
			return nil, err
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return all, nil
}

// CallTool runs one tool. A server-reported tool failure comes back as a
// CallResult with IsError set, not as a Go error: the model is meant to see
// it. A Go error means the call itself could not be made.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*CallResult, error) {
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = json.RawMessage("{}")
	}
	var result CallResult
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// noRedirects: an MCP endpoint is a fixed URL. A 3xx is answered as an
// error rather than re-sent, with the Authorization header, elsewhere.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		if c.HTTP.CheckRedirect == nil {
			c.HTTP.CheckRedirect = noRedirects
		}
		return c.HTTP
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second, Transport: c.Policy.Transport(), CheckRedirect: noRedirects}
	}
	return c.http
}

// Close releases the session's idle connections.
func (c *Client) Close() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func (c *Client) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (c *Client) notify(ctx context.Context, method string) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, body)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("mcp: %s: %w", method, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mcp: %s: server answered HTTP %d", method, resp.StatusCode)
	}
	return nil
}

// call sends one request and decodes the matching response's result into
// out.
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, body)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("mcp: %s: %w", method, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
		return fmt.Errorf("mcp: %s: server answered HTTP %d", method, resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, MaxResponseBytes+1)
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var msg *rpcResponse
	switch mediaType {
	case "text/event-stream":
		msg, err = readSSEResponse(limited, id)
	default:
		msg, err = readJSONResponse(limited, id)
	}
	if err != nil {
		return fmt.Errorf("mcp: %s: %w", method, err)
	}
	if msg.Error != nil {
		return msg.Error
	}
	if out != nil && len(msg.Result) > 0 {
		if err := json.Unmarshal(msg.Result, out); err != nil {
			return fmt.Errorf("mcp: %s: decode result: %w", method, err)
		}
	}
	return nil
}

func idMatches(raw json.RawMessage, want int64) bool {
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n == want
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == fmt.Sprint(want)
	}
	return false
}

func readJSONResponse(r io.Reader, id int64) (*rpcResponse, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBytes {
		return nil, fmt.Errorf("response larger than %d bytes", MaxResponseBytes)
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		var batch []rpcResponse
		if err := json.Unmarshal(data, &batch); err != nil {
			return nil, fmt.Errorf("decode batch: %w", err)
		}
		for i := range batch {
			if idMatches(batch[i].ID, id) {
				return &batch[i], nil
			}
		}
		return nil, errors.New("no response with the request's id in batch")
	}
	var msg rpcResponse
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if !idMatches(msg.ID, id) {
		return nil, errors.New("response id does not match the request")
	}
	return &msg, nil
}

// readSSEResponse reads events until one carries the response with the
// request's id. Other messages on the stream (notifications, server
// requests) are ignored.
func readSSEResponse(r io.Reader, id int64) (*rpcResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxResponseBytes)
	var data []string
	total := 0
	flush := func() (*rpcResponse, bool, error) {
		if len(data) == 0 {
			return nil, false, nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		var msg rpcResponse
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return nil, false, nil // not JSON-RPC; skip
		}
		if msg.Method != "" || !idMatches(msg.ID, id) {
			return nil, false, nil
		}
		return &msg, true, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line)
		if total > MaxResponseBytes {
			return nil, fmt.Errorf("stream larger than %d bytes", MaxResponseBytes)
		}
		switch {
		case line == "":
			msg, done, err := flush()
			if err != nil {
				return nil, err
			}
			if done {
				return msg, nil
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// event:, id:, retry:, comments -- irrelevant here.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if msg, done, _ := flush(); done {
		return msg, nil
	}
	return nil, errors.New("stream ended without a response to the request")
}

// ---------------------------------------------------------------------------
// Host policy
// ---------------------------------------------------------------------------

// HostPolicy says which server hosts the api may contact. Allowed holds
// hostname patterns an administrator wrote: an exact host ("mcp.corp.example")
// or a leading-wildcard suffix ("*.svc.cluster.local"). Nothing is reachable
// by default.
//
// Loopback, link-local (the cloud metadata service lives there) and
// unspecified addresses are refused at dial time even when DNS points an
// allowed name at them, unless the literal host itself is on the list -- so
// a developer can allow "localhost" on purpose, and nobody can reach
// 169.254.169.254 by accident.
type HostPolicy struct {
	Allowed []string
}

// Check refuses a URL whose scheme is not http(s) or whose host is not
// allowed.
func (p *HostPolicy) Check(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("mcp: invalid server url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("mcp: server url must be http or https")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("mcp: server url has no host")
	}
	if !p.Allows(host) {
		return ErrNotAllowed
	}
	return nil
}

// Allows reports whether host matches one of the patterns.
func (p *HostPolicy) Allows(host string) bool {
	if p == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pat := range p.Allowed {
		pat = strings.ToLower(strings.TrimSpace(pat))
		switch {
		case pat == "":
		case strings.HasPrefix(pat, "*."):
			suffix := pat[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		case pat == host:
			return true
		}
	}
	return false
}

// literalAllowed reports whether the host literal itself (not via a
// wildcard) is on the list: the opt-in for loopback/link-local targets.
func (p *HostPolicy) literalAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, pat := range p.Allowed {
		if strings.ToLower(strings.TrimSpace(pat)) == host {
			return true
		}
	}
	return false
}

// Transport returns an http.Transport whose dialer refuses loopback,
// link-local, unspecified and multicast destinations unless the host literal
// is explicitly allowed. Redirects are refused by the client (noRedirects),
// not here.
func (p *HostPolicy) Transport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: nil, // never through an environment proxy: the allowlist names the real host
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if !p.Allows(host) {
				return nil, ErrNotAllowed
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			literal := p.literalAllowed(host)
			for _, ip := range ips {
				if !literal && (ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsUnspecified() || ip.IP.IsMulticast()) {
					return nil, fmt.Errorf("mcp: %s resolves to a reserved address", host)
				}
			}
			// Dial the first address that was checked, not the name again,
			// so a resolver answer cannot change between check and dial.
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
	}
}
