package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// apiClient talks to the install through the Forwarder, the way the web UI
// does. Cookies are kept by hand rather than in a net/http/cookiejar: the api
// marks them Secure, and a jar will not send a Secure cookie over the plain
// http tunnel. They live in memory only and are never printed.
type apiClient struct {
	fwd     Forwarder
	http    *http.Client
	base    string
	cookies map[string]string
	// reconnect is called when the tunnel has died under a request; it opens a
	// new one (at most once per run) and returns its base URL.
	reconnect func(ctx context.Context) (string, error)
}

// errForwardLost means the tunnel died and could not be reopened.
var errForwardLost = errors.New("the connection to the install closed")

type apiResponse struct {
	status int
	body   []byte
	header http.Header
}

// errMessage is the refusal text in either error shape the api sends:
// {"error":"..."} or {"error":{"message":"...","code":"..."}}.
func (r apiResponse) errMessage() (msg, code string) {
	var flat struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(r.body, &flat) == nil && flat.Error != "" {
		return flat.Error, ""
	}
	var nested struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(r.body, &nested) == nil && nested.Error.Message != "" {
		return nested.Error.Message, nested.Error.Code
	}
	return http.StatusText(r.status), ""
}

// do sends one request. retry says whether it may be re-sent on a new tunnel
// when the old one died mid-request (false for anything that is not safe to
// repeat, such as creating the first admin).
func (a *apiClient) do(ctx context.Context, method, path string, body any, retry bool) (apiResponse, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return apiResponse{}, err
		}
	}
	resp, err := a.send(ctx, method, path, payload)
	if err == nil || ctx.Err() != nil || !a.dead() {
		return resp, err
	}
	if a.reconnect == nil {
		return resp, errForwardLost
	}
	base, rerr := a.reconnect(ctx)
	if rerr != nil {
		return resp, fmt.Errorf("%w: %v", errForwardLost, rerr)
	}
	a.base = base
	if !retry {
		return resp, errForwardLost
	}
	return a.send(ctx, method, path, payload)
}

func (a *apiClient) dead() bool {
	select {
	case <-a.fwd.Done():
		return true
	default:
		return false
	}
}

func (a *apiClient) send(ctx context.Context, method, path string, payload []byte) (apiResponse, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return apiResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		// csrfProtection refuses a cookie-authenticated write without it.
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	if c := a.cookieHeader(); c != "" {
		req.Header.Set("Cookie", c)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return apiResponse{}, err
	}
	a.keepCookies(resp)
	return apiResponse{status: resp.StatusCode, body: b, header: resp.Header}, nil
}

func (a *apiClient) keepCookies(resp *http.Response) {
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(a.cookies, c.Name)
			continue
		}
		a.cookies[c.Name] = c.Value
	}
}

// logout ends the CLI's session, if it has one. Best effort, bounded, on its
// own context (the run's may already be cancelled), and never reopens a tunnel.
func (a *apiClient) logout() {
	if len(a.cookies) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = a.send(ctx, http.MethodPost, "/auth/logout", []byte("{}"))
	a.cookies = map[string]string{}
}

func (a *apiClient) cookieHeader() string {
	names := make([]string, 0, len(a.cookies))
	for n := range a.cookies {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + "=" + a.cookies[n]
	}
	return strings.Join(parts, "; ")
}

// authConfig is GET /auth/config. SetupRequired is nil when the api could not
// count users (it omits the field rather than guess).
type authConfig struct {
	SetupRequired *bool `json:"setup_required"`
	OIDC          struct {
		Enabled bool `json:"enabled"`
	} `json:"oidc"`
}

// adminModel is one row of GET /api/v1/models (the admin list, bare-array
// shape: no X-Darkplum-Classes header is sent).
type adminModel struct {
	ID           string  `json:"id"`
	ModelID      string  `json:"model_id"`
	Provider     string  `json:"provider"`
	DisplayName  *string `json:"display_name"`
	Enabled      bool    `json:"enabled"`
	BackendClass string  `json:"backend_class"`
	Available    bool    `json:"available"`
	Availability *struct {
		State string `json:"state"`
	} `json:"availability"`
}

func (m adminModel) label() string {
	if m.DisplayName != nil && *m.DisplayName != "" {
		return *m.DisplayName
	}
	return m.ModelID
}

func (m adminModel) selfHosted() bool { return m.BackendClass == "self_hosted" }

func (m adminModel) needsOptIn() bool {
	return m.Availability != nil && m.Availability.State == "needs_aws_opt_in"
}

// ready: the gateway has seen it recently and nothing is known to block it.
func (m adminModel) ready() bool { return m.Available && !m.needsOptIn() }

func (m adminModel) kind() string {
	switch {
	case m.selfHosted():
		return "self-hosted"
	case m.Provider == "bedrock":
		return "Bedrock"
	default:
		return m.Provider
	}
}

func (m adminModel) state() string {
	switch {
	case m.needsOptIn():
		return "needs AWS opt-in"
	case !m.Available:
		return "not reachable right now"
	default:
		return "ready"
	}
}

func (a *apiClient) models(ctx context.Context) ([]adminModel, error) {
	r, err := a.do(ctx, http.MethodGet, "/api/v1/models", nil, true)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK {
		msg, _ := r.errMessage()
		return nil, fmt.Errorf("listing models: %d %s", r.status, msg)
	}
	var ms []adminModel
	if err := json.Unmarshal(r.body, &ms); err != nil {
		return nil, fmt.Errorf("listing models: unexpected response")
	}
	return ms, nil
}

func (a *apiClient) enable(ctx context.Context, id string) error {
	r, err := a.do(ctx, http.MethodPatch, "/api/v1/models/"+id, map[string]bool{"enabled": true}, true)
	if err != nil {
		return err
	}
	if r.status != http.StatusOK {
		msg, _ := r.errMessage()
		return fmt.Errorf("%d %s", r.status, msg)
	}
	return nil
}

// chatResult is one non-streamed answer.
type chatResult struct {
	reply   string
	latency time.Duration
}

func (a *apiClient) chat(ctx context.Context, model string, now func() time.Time) (chatResult, apiResponse, error) {
	body := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "Say hello in one short sentence."}},
	}
	start := now()
	// Not replayed on a new tunnel: the first send may have reached the model.
	r, err := a.do(ctx, http.MethodPost, "/v1/chat/completions", body, false)
	if err != nil {
		return chatResult{}, r, err
	}
	res := chatResult{latency: now().Sub(start)}
	if r.status != http.StatusOK {
		return res, r, nil
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || len(out.Choices) == 0 {
		return res, r, errors.New("the answer was not a chat completion")
	}
	res.reply = strings.Join(strings.Fields(out.Choices[0].Message.Content), " ")
	return res, r, nil
}
