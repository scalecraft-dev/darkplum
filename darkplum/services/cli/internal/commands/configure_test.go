package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

const (
	testContext    = "darkplum"
	testKubeconfig = "/work/darkplum/terraform/aws/.darkplum/kubeconfig"
	testPassword   = "correct-horse-battery"
	sessionToken   = "s3ss10n"
)

// fakeAPI is the install as the web service's front door shows it: the same
// routes, the CSRF header rule, and Secure cookies (which a cookiejar would
// refuse to send over http).
type fakeAPI struct {
	mu            sync.Mutex
	setupRequired *bool // nil: omitted, as when the api cannot count users
	oidc          bool
	setupConflict bool // another admin claims setup first
	users         map[string]string
	models        []map[string]any
	chatStatus    int
	chatBody      string
	notEnabled    int // model_not_enabled answers before a chat succeeds

	calls     []string
	csrfFails int
	chatBods  []map[string]any
}

func boolp(b bool) *bool { return &b }

func newFakeAPI() *fakeAPI {
	return &fakeAPI{setupRequired: boolp(true), users: map[string]string{}, chatStatus: 200,
		chatBody: `{"choices":[{"message":{"role":"assistant","content":"Hello there!"}}]}`}
}

func bedrockModel(id string, enabled bool) map[string]any {
	return map[string]any{"id": "uuid-" + id, "model_id": id, "provider": "bedrock", "backend_class": "cloud_managed",
		"enabled": enabled, "available": true, "availability": map[string]string{"state": "ok"}}
}

func (f *fakeAPI) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) setSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "darkplum_session", Value: sessionToken, Path: "/", Secure: true, HttpOnly: true, MaxAge: 3600})
}

func (f *fakeAPI) authed(r *http.Request) bool {
	c, err := r.Cookie("darkplum_session")
	return err == nil && c.Value == sessionToken
}

// handler serves one tunnel. When dead is set the tunnel drops every
// connection, as a port-forward does when the web pod goes away.
func (f *fakeAPI) handler(dead *atomic.Bool, onRequest func(string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		if onRequest != nil {
			onRequest(call)
		}
		if dead != nil && dead.Load() {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, call)
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") == "" {
			f.csrfFails++
			writeJSON(w, 403, map[string]string{"error": "missing required header"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case call == "GET /auth/config":
			resp := map[string]any{"local": true, "oidc": map[string]bool{"enabled": f.oidc}}
			if f.setupRequired != nil {
				resp["setup_required"] = *f.setupRequired
			}
			writeJSON(w, 200, resp)
		case call == "POST /auth/setup":
			if f.setupConflict || f.setupRequired == nil || !*f.setupRequired {
				writeJSON(w, 409, map[string]string{"error": "setup already completed"})
				return
			}
			f.users[body["email"].(string)] = body["password"].(string)
			f.setupRequired = boolp(false)
			f.setSession(w)
			writeJSON(w, 201, map[string]string{"email": body["email"].(string)})
		case call == "POST /auth/logout":
			http.SetCookie(w, &http.Cookie{Name: "darkplum_session", Value: "", Path: "/", MaxAge: -1})
			writeJSON(w, 200, map[string]string{"redirect": "/login"})
		case call == "POST /auth/login":
			if pw, ok := f.users[body["email"].(string)]; !ok || pw != body["password"] {
				writeJSON(w, 401, map[string]string{"error": "invalid credentials"})
				return
			}
			f.setSession(w)
			writeJSON(w, 200, map[string]string{"email": body["email"].(string)})
		case !f.authed(r):
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		case call == "GET /api/v1/models":
			writeJSON(w, 200, f.models)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/models/"):
			if body["enabled"] != true {
				writeJSON(w, 400, map[string]string{"error": "nothing to change"})
				return
			}
			for _, m := range f.models {
				if m["id"] == strings.TrimPrefix(r.URL.Path, "/api/v1/models/") {
					m["enabled"] = true
					writeJSON(w, 200, m)
					return
				}
			}
			writeJSON(w, 404, map[string]string{"error": "model not found"})
		case call == "POST /v1/chat/completions":
			f.chatBods = append(f.chatBods, body)
			if f.notEnabled > 0 {
				f.notEnabled--
				writeJSON(w, 403, map[string]any{"error": map[string]string{"message": "not enabled", "code": "model_not_enabled"}})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.chatStatus)
			_, _ = w.Write([]byte(f.chatBody))
		default:
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	})
}

// fakeFwd hands out successive tunnels to the fake api.
type fakeFwd struct {
	mu     sync.Mutex
	urls   []string
	opened int
	done   chan struct{}
	closed bool
}

func (f *fakeFwd) Open(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.opened >= len(f.urls) {
		return "", errors.New("error: unable to forward port")
	}
	f.done = make(chan struct{})
	f.opened++
	return f.urls[f.opened-1], nil
}

func (f *fakeFwd) Done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done
}

func (f *fakeFwd) Close() { f.mu.Lock(); f.closed = true; f.mu.Unlock() }

// kill ends the current tunnel.
func (f *fakeFwd) kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.done)
}

// fakeKube answers the kubectl reads configure makes and records every call.
type fakeKube struct {
	mu       sync.Mutex
	calls    [][]string
	deploys  string
	pods     string
	podsFunc func() string

	trusted      string // darkplum-web-trusted-proxies data, "" = absent
	applied      []map[string]any
	restarts     int
	deleted      []string
	oidcRedirect string
}

func (k *fakeKube) Run(stdin []byte, _ string, args ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, args)
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "get configmap "+trustedProxiesConfigMap):
		if k.trusted == "" {
			return nil, errors.New(`configmaps "darkplum-web-trusted-proxies" not found`)
		}
		return []byte(k.trusted), nil
	case strings.HasSuffix(joined, "apply -f -"):
		var m map[string]any
		if err := json.Unmarshal(stdin, &m); err != nil {
			return nil, err
		}
		k.applied = append(k.applied, m)
		if m["kind"] == "ConfigMap" {
			k.trusted = m["data"].(map[string]any)[trustedProxiesKey].(string)
		}
		return nil, nil
	case strings.Contains(joined, "delete configmap/"+trustedProxiesConfigMap):
		k.trusted = ""
		k.deleted = append(k.deleted, "configmap")
		return nil, nil
	case strings.Contains(joined, "delete networkpolicy/"+webIngressPolicy):
		k.deleted = append(k.deleted, "networkpolicy")
		return nil, nil
	case strings.Contains(joined, "rollout restart deployment/darkplum-web"):
		k.restarts++
		return nil, nil
	case strings.Contains(joined, "rollout status deployment/darkplum-web"):
		return nil, nil
	case strings.Contains(joined, "get secret darkplum-auth"):
		return []byte(base64.StdEncoding.EncodeToString([]byte(k.oidcRedirect))), nil
	case strings.Contains(joined, "get deploy"):
		return []byte(k.deploys), nil
	case strings.Contains(joined, "get pods"):
		if k.podsFunc != nil {
			return []byte(k.podsFunc()), nil
		}
		return []byte(k.pods), nil
	}
	return nil, fmt.Errorf("unexpected kubectl %s", joined)
}

type configureHarness struct {
	t      *testing.T
	api    *fakeAPI
	fwd    *fakeFwd
	kube   *fakeKube
	out    *bytes.Buffer
	env    *configureEnv
	clock  time.Time
	dead   []*atomic.Bool
	hook   func(string)
	resolv func() (clusterTarget, checkResult, error)

	web      webAddress
	tfOut    map[string]json.RawMessage
	tfErr    error
	probeErr error
	probed   []string
}

// newHarness starts n tunnels (httptest servers) onto one fake api.
func newHarness(t *testing.T, api *fakeAPI, tunnels int) *configureHarness {
	t.Helper()
	h := &configureHarness{t: t, api: api, fwd: &fakeFwd{}, out: &bytes.Buffer{},
		kube:  &fakeKube{deploys: "darkplum-api=1/1\ndarkplum-web=2/2\ndarkplum-gateway=1/1\n", pods: `{"items":[]}`},
		clock: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	for i := 0; i < tunnels; i++ {
		dead := &atomic.Bool{}
		h.dead = append(h.dead, dead)
		srv := httptest.NewServer(api.handler(dead, func(c string) {
			if h.hook != nil {
				h.hook(c)
			}
		}))
		t.Cleanup(srv.Close)
		h.fwd.urls = append(h.fwd.urls, srv.URL)
	}
	return h
}

// run executes configure with scripted line answers and secrets.
func (h *configureHarness) run(c *ConfigureCmd, answers string, secrets ...string) error {
	p := newIOPrompter(strings.NewReader(answers), h.out)
	if secrets != nil {
		p.secret = func() (string, error) {
			if len(secrets) == 0 {
				return "", errAborted
			}
			s := secrets[0]
			secrets = secrets[1:]
			return s, nil
		}
	}
	resolve := h.resolv
	if resolve == nil {
		resolve = func() (clusterTarget, checkResult, error) {
			return clusterTarget{Name: "darkplum", Region: "us-east-2", Kubeconfig: testKubeconfig, Context: testContext},
				checkResult{name: "cluster", status: checkPass, detail: "darkplum (us-east-2)"}, nil
		}
	}
	h.env = &configureEnv{
		out:         h.out,
		prompt:      p,
		interactive: !c.Yes,
		namespace:   "darkplum-system",
		resolve:     resolve,
		newKube: func(t clusterTarget, ns string) *kube.Client {
			return kube.ForTest(h.kube, ns, "--kubeconfig", t.Kubeconfig, "--context", t.Context)
		},
		newFwd: func(clusterTarget) Forwarder { return h.fwd },
		httpc:  &http.Client{Timeout: 5 * time.Second},
		now:    func() time.Time { return h.clock },
		sleep: func(ctx context.Context, d time.Duration) error {
			h.clock = h.clock.Add(d)
			return ctx.Err()
		},
		web:       h.web,
		tfOutputs: func() (map[string]json.RawMessage, error) { return h.tfOut, h.tfErr },
		probe:     func(u string) error { h.probed = append(h.probed, u); return h.probeErr },
	}
	err := c.execute(context.Background(), h.env)
	h.checkInvariants()
	return err
}

// checkInvariants holds for every run: the password never appears in the
// output, every kubectl call is pinned, writes carried the CSRF header, and
// the tunnel was closed.
func (h *configureHarness) checkInvariants() {
	h.t.Helper()
	if strings.Contains(h.out.String(), testPassword) {
		h.t.Errorf("the password appeared in the output:\n%s", h.out.String())
	}
	for _, c := range h.kube.calls {
		if len(c) < 4 || c[0] != "--kubeconfig" || c[1] != testKubeconfig || c[2] != "--context" || c[3] != testContext {
			h.t.Errorf("kubectl call not pinned to the install's private kubeconfig: %q", c)
		}
	}
	if h.api.csrfFails > 0 {
		h.t.Errorf("%d write(s) were sent without X-Requested-With", h.api.csrfFails)
	}
	if h.fwd.opened > 0 && !h.fwd.closed {
		h.t.Error("the tunnel was left open")
	}
	if m := regexp.MustCompile(`\bmake [a-z]`).FindString(h.out.String()); m != "" {
		h.t.Errorf("configure must not point at make (found %q):\n%s", m, h.out.String())
	}
}

func (h *configureHarness) contains(want ...string) {
	h.t.Helper()
	for _, w := range want {
		if !strings.Contains(h.out.String(), w) {
			h.t.Errorf("output lacks %q:\n%s", w, h.out.String())
		}
	}
}

func TestConfigureFirstRunBedrock(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{bedrockModel("us.anthropic.claude-sonnet", false)}
	h := newHarness(t, api, 1)
	closedBeforeLogout := false
	h.hook = func(call string) {
		if call == "POST /auth/logout" {
			h.fwd.mu.Lock()
			closedBeforeLogout = h.fwd.closed
			h.fwd.mu.Unlock()
		}
	}
	// Admin email, Name [default], models [default] — passwords typed twice.
	if err := h.run(&ConfigureCmd{}, "Jane.Doe@acme.com\n\n\n", testPassword, testPassword); err != nil {
		t.Fatalf("configure: %v\n%s", err, h.out.String())
	}
	h.contains("✓ first admin", "Jane.Doe@acme.com created", "? Name [Jane Doe]", "enabled now",
		"✓ test chat", "answered in", "Hello there!",
		"Open darkplum: http://localhost:", "Sign in as Jane.Doe@acme.com", "Ctrl-C closes it",
		"No team address yet: re-run ./bin/darkplum init to add a private HTTPS address")
	if got := api.users["Jane.Doe@acme.com"]; got != testPassword {
		t.Errorf("setup did not carry the password")
	}
	if api.models[0]["enabled"] != true {
		t.Error("the ready Bedrock model was not enabled by default")
	}
	if len(api.chatBods) != 1 || api.chatBods[0]["stream"] != false {
		t.Errorf("chat must be one non-streamed request, got %v", api.chatBods)
	}
	if n := api.count("POST /auth/logout"); n != 1 || closedBeforeLogout {
		t.Errorf("the CLI session must be logged out once, before the tunnel closes (logouts %d, closed first %v)", n, closedBeforeLogout)
	}
	if strings.Contains(h.out.String(), "terraform.tfvars") || strings.Contains(h.out.String(), "\r") {
		t.Errorf("normal output must not name files or redraw lines:\n%s", h.out.String())
	}
}

func TestConfigureIsIdempotent(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{bedrockModel("us.anthropic.claude-sonnet", false)}
	h := newHarness(t, api, 2)
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n\n\n", testPassword, testPassword); err != nil {
		t.Fatal(err)
	}
	patches := 0
	for _, c := range api.calls {
		if strings.HasPrefix(c, "PATCH ") {
			patches++
		}
	}
	h.out.Reset()
	h.fwd.opened = 0 // a second run opens its own tunnel
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
		t.Fatalf("second run: %v\n%s", err, h.out.String())
	}
	h.contains("✓ first admin", "already set up", "✓ sign in", "Bedrock · enabled", "✓ test chat")
	if n := api.count("POST /auth/setup"); n != 1 {
		t.Errorf("setup called %d times, want 1", n)
	}
	after := 0
	for _, c := range api.calls {
		if strings.HasPrefix(c, "PATCH ") {
			after++
		}
	}
	if after != patches {
		t.Errorf("second run changed models (%d PATCHes before, %d after)", patches, after)
	}
	if strings.Contains(h.out.String(), "Enable which models") {
		t.Error("nothing new to enable, so nothing should be asked")
	}
}

func TestConfigureAdminStates(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*fakeAPI)
		answers  string
		secrets  []string
		wantErr  bool
		want     []string
		notWant  []string
		setupCnt int
	}{
		{
			name:    "setup required",
			setup:   func(a *fakeAPI) {},
			answers: "a@acme.com\n\n\n", secrets: []string{testPassword, testPassword},
			want: []string{"✓ first admin", "a@acme.com created"}, setupCnt: 1,
		},
		{
			name:    "admin exists: sign in",
			setup:   func(a *fakeAPI) { a.setupRequired = boolp(false); a.users["a@acme.com"] = testPassword },
			answers: "a@acme.com\n", secrets: []string{testPassword},
			want: []string{"already set up", "✓ sign in"}, setupCnt: 0,
		},
		{
			name:    "setup_required missing: the api cannot read its database",
			setup:   func(a *fakeAPI) { a.setupRequired = nil },
			wantErr: true,
			want:    []string{"✗ api", "could not read its database"}, setupCnt: 0,
		},
		{
			name:    "409 after setup_required true: someone else claimed it",
			setup:   func(a *fakeAPI) { a.setupConflict = true },
			answers: "a@acme.com\n\n", secrets: []string{testPassword, testPassword},
			wantErr: true,
			want:    []string{"✗ first admin", "someone else created the first admin", "nothing was changed"},
			notWant: []string{"✓ first admin"}, setupCnt: 1,
		},
		{
			name:    "OIDC on: no setup, no models",
			setup:   func(a *fakeAPI) { a.oidc = true; a.setupRequired = nil },
			answers: "\n",
			want:    []string{"! first admin", "SSO is on", "Sign in with SSO"},
			notWant: []string{"Admin email"}, setupCnt: 0,
		},
		{
			name:    "401: wrong password three times",
			setup:   func(a *fakeAPI) { a.setupRequired = boolp(false); a.users["a@acme.com"] = "other-password" },
			answers: "a@acme.com\n", secrets: []string{testPassword, testPassword, testPassword},
			wantErr: true,
			want:    []string{"✗ sign in", "email or password is wrong", "admin create"}, setupCnt: 0,
		},
		{
			name:    "no terminal: the password is refused, nothing is sent",
			setup:   func(a *fakeAPI) {},
			answers: "a@acme.com\n\n", secrets: nil,
			wantErr: true,
			want:    []string{"✗ password", "needs a terminal"}, setupCnt: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			api.models = []map[string]any{bedrockModel("m", true)}
			tc.setup(api)
			h := newHarness(t, api, 1)
			err := h.run(&ConfigureCmd{}, tc.answers, tc.secrets...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tc.wantErr, h.out.String())
			}
			if err != nil && !IsReported(err) {
				t.Errorf("failure must be a reported ✗ row, got %v", err)
			}
			h.contains(tc.want...)
			for _, nw := range tc.notWant {
				if strings.Contains(h.out.String(), nw) {
					t.Errorf("output must not contain %q:\n%s", nw, h.out.String())
				}
			}
			if n := api.count("POST /auth/setup"); n != tc.setupCnt {
				t.Errorf("POST /auth/setup called %d times, want %d", n, tc.setupCnt)
			}
		})
	}
}

func TestConfigureWrongPasswordStopsAtThreeAttempts(t *testing.T) {
	api := newFakeAPI()
	api.setupRequired = boolp(false)
	api.users["a@acme.com"] = "other-password"
	h := newHarness(t, api, 1)
	_ = h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword, testPassword, testPassword, testPassword)
	if n := api.count("POST /auth/login"); n != 3 {
		t.Errorf("login attempted %d times, want 3 (the api rate-limits sign-ins)", n)
	}
}

func TestConfigureCSRFHeaderIsRequired(t *testing.T) {
	// The fake refuses a write without X-Requested-With, like the api's
	// csrfProtection; a client that forgot it would fail here.
	api := newFakeAPI()
	srv := httptest.NewServer(api.handler(nil, nil))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/auth/setup", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("fake must refuse a write without the header, got %d", resp.StatusCode)
	}
	api.csrfFails = 0
	c := &apiClient{fwd: &fakeFwd{done: make(chan struct{})}, http: srv.Client(), base: srv.URL, cookies: map[string]string{}}
	r, err := c.do(context.Background(), http.MethodPost, "/auth/setup",
		map[string]string{"email": "a@acme.com", "name": "A", "password": testPassword}, false)
	if err != nil || r.status != 201 {
		t.Fatalf("client write refused: %v %d", err, r.status)
	}
	if c.cookies["darkplum_session"] != sessionToken {
		t.Error("the Secure session cookie must be kept for the http tunnel")
	}
}

func TestConfigureReconnectsOnceWhenTheTunnelDies(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{bedrockModel("m", false)}
	h := newHarness(t, api, 2)
	var once sync.Once
	h.hook = func(call string) {
		if call == "GET /api/v1/models" {
			once.Do(func() { h.dead[0].Store(true); h.fwd.kill() })
		}
	}
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n\n\n", testPassword, testPassword); err != nil {
		t.Fatalf("configure: %v\n%s", err, h.out.String())
	}
	h.contains("! connection", "reconnecting", "✓ test chat", strings.Replace(h.fwd.urls[1], "127.0.0.1", "localhost", 1))
	if h.fwd.opened != 2 {
		t.Errorf("tunnel opened %d times, want 2", h.fwd.opened)
	}
}

func TestConfigureHoldOpenGivesUpAfterSecondDrop(t *testing.T) {
	api := newFakeAPI()
	h := newHarness(t, api, 2)
	out := &bytes.Buffer{}
	r := &configureRun{configureEnv: &configureEnv{out: out}, fwd: h.fwd, kc: kube.ForTest(h.kube, "")}
	base, _ := h.fwd.Open(context.Background())
	r.api = &apiClient{fwd: h.fwd, base: base}
	go func() {
		h.fwd.kill()
		for {
			h.fwd.mu.Lock()
			n := h.fwd.opened
			h.fwd.mu.Unlock()
			if n == 2 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		h.fwd.kill()
	}()
	err := r.holdOpen(context.Background())
	if err == nil || !IsReported(err) {
		t.Fatalf("second drop must end with a reported ✗, got %v", err)
	}
	for _, w := range []string{"reconnecting", "(new address)", "✗ connection", "re-run ./bin/darkplum configure"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, out.String())
		}
	}
}

func TestConfigureForwardFailsToOpen(t *testing.T) {
	api := newFakeAPI()
	h := newHarness(t, api, 0)
	err := h.run(&ConfigureCmd{}, "")
	if err == nil || !IsReported(err) {
		t.Fatalf("want a reported failure, got %v", err)
	}
	h.contains("✗ connection", "unable to forward port")
}

const podDownloading = `{"items":[{"metadata":{"creationTimestamp":"2026-10-30T11:00:00Z"},"status":{"phase":"Pending",
 "initContainerStatuses":[{"name":"model-download","state":{"running":{}}}],
 "containerStatuses":[{"name":"vllm","state":{"waiting":{"reason":"PodInitializing"}}}]}}]}`

const podReady = `{"items":[{"metadata":{"creationTimestamp":"2026-10-30T11:00:00Z"},"status":{"phase":"Running",
 "initContainerStatuses":[{"name":"model-download","state":{"terminated":{"exitCode":0}}}],
 "containerStatuses":[{"name":"vllm","ready":true,"state":{"running":{}}}]}}]}`

func TestConfigureSelfHostedLoadingDoesNotBlockBedrock(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{bedrockModel("us.anthropic.claude-sonnet", false)}
	h := newHarness(t, api, 1)
	h.kube.pods = podDownloading
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n\n\n", testPassword, testPassword); err != nil {
		t.Fatalf("configure: %v\n%s", err, h.out.String())
	}
	h.contains("! self-hosted model", "downloading the model weights", "re-run ./bin/darkplum configure", "✓ test chat")
	if strings.Contains(h.out.String(), "waiting for the self-hosted model") {
		t.Error("without --wait configure must not wait while a Bedrock model is ready")
	}
}

func TestConfigureWaitModelNeverReady(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{}
	h := newHarness(t, api, 1)
	h.kube.pods = podDownloading
	err := h.run(&ConfigureCmd{Wait: true, WaitTimeout: 10 * time.Minute}, "a@acme.com\n\n", testPassword, testPassword)
	if err == nil || !IsReported(err) {
		t.Fatalf("want a reported failure, got %v\n%s", err, h.out.String())
	}
	h.contains("waiting for the self-hosted model, up to 10 min", "self-hosted model: downloading the model weights",
		"✗ self-hosted model", "not ready after 10 min", "logs -f deploy/vllm-inference -c model-download", `eval "$(./bin/darkplum kubeconfig)" && kubectl`)
}

func TestConfigureWaitThenEnablesSelfHosted(t *testing.T) {
	api := newFakeAPI()
	h := newHarness(t, api, 1)
	polls := 0
	h.kube.podsFunc = func() string {
		polls++
		if polls < 3 {
			return podDownloading
		}
		api.mu.Lock()
		api.models = []map[string]any{{"id": "uuid-q", "model_id": "qwen", "provider": "vllm", "backend_class": "self_hosted",
			"enabled": false, "available": true, "availability": nil}}
		api.mu.Unlock()
		return podReady
	}
	// GPU-only: nothing ready, --yes takes "wait" as the default.
	if err := h.run(&ConfigureCmd{Yes: true}, "a@acme.com\n", testPassword, testPassword); err != nil {
		t.Fatalf("configure: %v\n%s", err, h.out.String())
	}
	h.contains("✓ qwen", "self-hosted · enabled", "✓ test chat", "qwen answered")
}

func TestConfigureChatRetriesPolicyLagThenNamesBedrock(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{bedrockModel("m", true)}
	api.notEnabled = 2
	h := newHarness(t, api, 1)
	api.setupRequired = boolp(false)
	api.users["a@acme.com"] = testPassword
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
		t.Fatalf("policy lag must be retried: %v\n%s", err, h.out.String())
	}
	h.contains("✓ test chat")

	api2 := newFakeAPI()
	api2.models = []map[string]any{bedrockModel("m", true)}
	api2.setupRequired = boolp(false)
	api2.users["a@acme.com"] = testPassword
	api2.chatStatus = 403
	api2.chatBody = `{"error":{"message":"AccessDeniedException","code":"upstream_error"}}`
	h2 := newHarness(t, api2, 1)
	err := h2.run(&ConfigureCmd{}, "a@acme.com\n", testPassword)
	if err == nil || !IsReported(err) {
		t.Fatalf("want a reported failure, got %v", err)
	}
	h2.contains("✗ test chat", "Bedrock refused", "model access in the Bedrock console", "Open darkplum:")
}

func TestConfigureRefusesUnresolvedCluster(t *testing.T) {
	api := newFakeAPI()
	h := newHarness(t, api, 1)
	h.resolv = func() (clusterTarget, checkResult, error) {
		return clusterTarget{}, checkResult{name: "cluster", status: checkFail, detail: "credentials are for account 999999999999, the cluster is in 123456789012",
			hint: "use the profile for the install's account"}, errors.New("x")
	}
	err := h.run(&ConfigureCmd{}, "")
	if err == nil || !IsReported(err) {
		t.Fatalf("want a reported refusal, got %v", err)
	}
	if len(h.kube.calls) != 0 || h.fwd.opened != 0 {
		t.Errorf("nothing may run against a cluster that did not resolve: %d kubectl calls, %d tunnels", len(h.kube.calls), h.fwd.opened)
	}
	h.contains("✗ cluster", "credentials are for account")
}

func TestVLLMStateFromPods(t *testing.T) {
	pod := func(status string) string {
		return `{"items":[{"metadata":{"creationTimestamp":"2026-10-30T11:00:00Z"},"status":` + status + `}]}`
	}
	cases := []struct {
		name, raw     string
		phase         string
		broken, ready bool
	}{
		{"no pod", `{"items":[]}`, "", false, false},
		{"downloading", podDownloading, "downloading the model weights", false, false},
		{"loading", pod(`{"phase":"Running","initContainerStatuses":[{"name":"model-download","state":{"terminated":{"exitCode":0}}}],"containerStatuses":[{"name":"vllm","ready":false,"state":{"running":{}}}]}`), "loading the model onto the GPU", false, false},
		{"ready", podReady, "ready", false, true},
		{"download failed", pod(`{"phase":"Pending","initContainerStatuses":[{"name":"model-download","restartCount":2,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}`), "the model download failed", true, false},
		{"oom", pod(`{"phase":"Running","initContainerStatuses":[{"name":"model-download","state":{"terminated":{"exitCode":0}}}],"containerStatuses":[{"name":"vllm","restartCount":1,"state":{"running":{}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}`), "vLLM ran out of memory loading the model", true, false},
		{"unschedulable", pod(`{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes are available: insufficient nvidia.com/gpu."}]}`), "cannot be placed on a GPU node: 0/3 nodes are available: insufficient nvidia.com/gpu.", true, false},
		{"ready after restarts and an OOM", pod(`{"phase":"Running","initContainerStatuses":[{"name":"model-download","state":{"terminated":{"exitCode":0}}}],"containerStatuses":[{"name":"vllm","ready":true,"restartCount":4,"state":{"running":{}},"lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}`), "ready", false, true},
		{"download succeeded after a failed attempt", pod(`{"phase":"Running","initContainerStatuses":[{"name":"model-download","restartCount":1,"state":{"terminated":{"exitCode":0}},"lastState":{"terminated":{"exitCode":1}}}],"containerStatuses":[{"name":"vllm","ready":false,"state":{"running":{}}}]}`), "loading the model onto the GPU", false, false},
		{"download retrying after a failed attempt", pod(`{"phase":"Pending","initContainerStatuses":[{"name":"model-download","restartCount":1,"state":{"running":{}},"lastState":{"terminated":{"exitCode":1}}}]}`), "the model download failed", true, false},
		{"restarts", pod(`{"phase":"Running","initContainerStatuses":[{"name":"model-download","state":{"terminated":{"exitCode":0}}}],"containerStatuses":[{"name":"vllm","restartCount":4,"state":{"running":{}}}]}`), "vLLM keeps restarting (4 restarts)", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := vllmStateFromPods([]byte(tc.raw))
			if s.phase != tc.phase || s.broken != tc.broken || s.ready != tc.ready {
				t.Errorf("got %+v", s)
			}
			if s.broken && !strings.Contains(s.hint, `eval "$(./bin/darkplum kubeconfig)" && kubectl -n darkplum-inference`) {
				t.Errorf("diagnostic must validate the install first: %q", s.hint)
			}
		})
	}
}

func TestPrompterSecretAndMultiSelect(t *testing.T) {
	var out bytes.Buffer
	p := newIOPrompter(strings.NewReader("\n1,3\n2-3\nnone\n9\n0-1\n2\n"), &out)
	opts := []Option{{Label: "a"}, {Label: "b"}, {Label: "c"}}
	want := [][]int{{0, 2}, {0, 2}, {1, 2}, {}, {1}}
	for i, w := range want {
		got, err := p.MultiSelect("pick", opts, []int{0, 2})
		if err != nil || fmt.Sprint(got) != fmt.Sprint(w) {
			t.Errorf("answer %d: got %v %v, want %v", i, got, err, w)
		}
	}
	if !strings.Contains(out.String(), "[1,3]") || !strings.Contains(out.String(), "enter numbers from 1 to 3") {
		t.Errorf("default or validation hint missing:\n%s", out.String())
	}
	if _, err := p.AskSecret("Password"); !errors.Is(err, errNoTerminal) {
		t.Errorf("AskSecret without a terminal = %v, want errNoTerminal", err)
	}
	p.secret = func() (string, error) { return "s3cret-value", nil }
	out.Reset()
	if v, _ := p.AskSecret("Password"); v != "s3cret-value" || strings.Contains(out.String(), "s3cret-value") {
		t.Errorf("AskSecret = %q, output %q", v, out.String())
	}
}

func TestNameFromEmail(t *testing.T) {
	for in, want := range map[string]string{"élodie.ångström@x.fr": "Élodie Ångström", "jane.doe@x.com": "Jane Doe", "ops_team+dp@x.io": "Ops Team", "a@b.co": "A"} {
		if got := nameFromEmail(in); got != want {
			t.Errorf("nameFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoginAndChatAreNotReplayedOnANewTunnel(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		api := newFakeAPI()
		h := newHarness(t, api, 2)
		h.dead[0].Store(true)
		base, _ := h.fwd.Open(context.Background())
		h.fwd.kill()
		c := &apiClient{fwd: h.fwd, http: &http.Client{Timeout: 5 * time.Second}, base: base, cookies: map[string]string{},
			reconnect: func(ctx context.Context) (string, error) { return h.fwd.Open(ctx) }}
		if _, _, err := c.chat(context.Background(), "m", time.Now); !errors.Is(err, errForwardLost) {
			t.Fatalf("err = %v, want errForwardLost", err)
		}
		if n := api.count("POST /v1/chat/completions"); n != 0 {
			t.Errorf("the chat was replayed on the new tunnel (%d calls)", n)
		}
	})
	t.Run("login", func(t *testing.T) {
		api := newFakeAPI()
		api.setupRequired = boolp(false)
		api.users["a@acme.com"] = testPassword
		h := newHarness(t, api, 2)
		var once sync.Once
		h.hook = func(call string) {
			if call == "POST /auth/login" {
				once.Do(func() { h.dead[0].Store(true); h.fwd.kill() })
			}
		}
		err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword)
		if err == nil || !IsReported(err) {
			t.Fatalf("want a reported failure, got %v\n%s", err, h.out.String())
		}
		if n := api.count("POST /auth/login"); n != 0 {
			t.Errorf("the sign-in was replayed on the new tunnel (%d calls)", n)
		}
		h.contains("✗ sign in")
	})
}

// loggingKubectl puts a kubectl on PATH that records every call, and an aws
// that fails (an expired session), so nothing real is ever reached.
func loggingKubectl(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "kubectl.log")
	for name, body := range map[string]string{
		"kubectl": "echo \"$@\" >> " + logPath + "\n",
		"aws":     "echo 'An error occurred (ExpiredToken)' >&2; exit 255\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestWriteCommandsFailClosed(t *testing.T) {
	g := &Globals{Namespace: "darkplum-system"}
	t.Run("sso with an install that does not resolve refuses", func(t *testing.T) {
		log := loggingKubectl(t)
		testInstall(t, nil)
		// an aws that cannot answer: the install cannot be validated
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "aws"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		err := (&SSOConfigureCmd{Issuer: "https://idp", ClientID: "c", RedirectURL: "https://x/cb", SecretStdin: true}).Run(g)
		if err == nil || !IsReported(err) {
			t.Fatalf("want a reported refusal, got %v", err)
		}
		if b, _ := os.ReadFile(log); len(b) != 0 {
			t.Errorf("kubectl was called after the refusal:\n%s", b)
		}
	})
	t.Run("no install settings and no --context refuses", func(t *testing.T) {
		log := loggingKubectl(t)
		t.Chdir(t.TempDir())
		for name, run := range map[string]func() error{
			"sso":    func() error { return (&SSOConfigureCmd{SecretStdin: true}).Run(g) },
			"admin":  func() error { return (&AdminCreateCmd{Email: "a@acme.com", Name: "A", PasswordStdin: true}).Run(g) },
			"status": func() error { return (&StatusCmd{}).Run(g) },
		} {
			if err := run(); err == nil || !IsReported(err) {
				t.Errorf("%s: want a reported refusal, got %v", name, err)
			}
		}
		if b, _ := os.ReadFile(log); len(b) != 0 {
			t.Errorf("kubectl was called (the current context must never be used):\n%s", b)
		}
	})
	t.Run("--context pins every call", func(t *testing.T) {
		t.Chdir(t.TempDir())
		old := newLocalKind
		defer func() { newLocalKind = old }()
		newLocalKind = func(ctx string) (runner.LocalKind, error) {
			return runner.LocalKind{}, errors.New("stub: pretend kind")
		}
		if _, err := installKube(io.Discard, "darkplum-system", "kind-darkplum-dev", ""); err == nil {
			t.Fatal("the stub refused; installKube must surface it")
		}
	})
}

func webHarness(t *testing.T) *configureHarness {
	api := newFakeAPI()
	api.setupRequired = boolp(false)
	api.users["a@acme.com"] = testPassword
	api.models = []map[string]any{bedrockModel("m", true)}
	h := newHarness(t, api, 1)
	h.web = webAddress{hostname: "darkplum.acme.com", allowed: []string{"10.0.0.0/16", "10.8.0.0/16"}}
	h.tfOut = map[string]json.RawMessage{"web_lb_private_ips": json.RawMessage(`["10.0.100.250","10.0.101.250"]`)}
	return h
}

func TestConfigureTrustsTheLoadBalancer(t *testing.T) {
	h := webHarness(t)
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	h.contains("✓ audit", "client addresses recorded (trusts only the load balancer at 10.0.100.250, 10.0.101.250)")
	if h.kube.trusted != "# Written by `darkplum configure` from the install's Terraform outputs\n# (web_lb_private_ips): the internal NLB's pinned addresses.\nset_real_ip_from 10.0.100.250/32;\nset_real_ip_from 10.0.101.250/32;\n" {
		t.Errorf("trusted-proxy list = %q", h.kube.trusted)
	}
	if h.kube.restarts != 1 {
		t.Errorf("web restarted %d times, want 1 (the list changed)", h.kube.restarts)
	}
	var policy map[string]any
	for _, m := range h.kube.applied {
		if m["kind"] == "NetworkPolicy" {
			policy = m
		}
	}
	b, _ := json.Marshal(policy)
	for _, want := range []string{`"name":"web-ingress"`, `{"ipBlock":{"cidr":"10.0.100.250/32"}}`, `{"ipBlock":{"cidr":"10.0.101.250/32"}}`, `"port":8443`, `{"ports":[{"port":80,"protocol":"TCP"}]}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("web-ingress policy lacks %s: %s", want, b)
		}
	}

	// Second run: same addresses, so nothing restarts.
	h.out.Reset()
	h.fwd.opened = 0
	h.fwd.urls = append(h.fwd.urls, h.fwd.urls[0])
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
		t.Fatal(err)
	}
	if h.kube.restarts != 1 {
		t.Errorf("an unchanged list must not restart web (restarts %d)", h.kube.restarts)
	}
	h.contains("✓ audit")
}

func TestConfigureAuditStepStates(t *testing.T) {
	t.Run("install without a private address: skipped", func(t *testing.T) {
		h := webHarness(t)
		h.web = webAddress{}
		if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
			t.Fatal(err)
		}
		for _, c := range h.kube.calls {
			if strings.Contains(strings.Join(c, " "), "configmap") || strings.Contains(strings.Join(c, " "), "apply") {
				t.Errorf("no private address, yet configure ran %q", c)
			}
		}
		if strings.Contains(h.out.String(), "audit") || len(h.probed) != 0 {
			t.Errorf("nothing about the address may be shown or probed:\n%s", h.out.String())
		}
	})
	t.Run("not deployed yet", func(t *testing.T) {
		h := webHarness(t)
		h.tfOut = map[string]json.RawMessage{"web_lb_private_ips": json.RawMessage(`[]`)}
		if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
			t.Fatal(err)
		}
		h.contains("! audit", "not deployed yet")
		if len(h.kube.applied) != 0 {
			t.Error("nothing may be applied without the load balancer's addresses")
		}
	})
	t.Run("outputs unreadable", func(t *testing.T) {
		h := webHarness(t)
		h.tfErr = errors.New("Error: No valid credential sources found")
		err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword)
		if err == nil || !IsReported(err) {
			t.Fatalf("want a reported problem, got %v", err)
		}
		h.contains("✗ audit", "cannot read the install's Terraform outputs")
	})
	for _, bad := range []string{`"10.0.0.1; include /etc/passwd"`, `"::ffff:10.0.0.1"`, `"8.8.8.8"`, `"fd00::1"`, `"junk"`} {
		t.Run("refused output "+bad, func(t *testing.T) {
			h := webHarness(t)
			h.tfOut = map[string]json.RawMessage{"web_lb_private_ips": json.RawMessage(`["10.0.100.250",` + bad + `]`)}
			_ = h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword)
			if len(h.kube.applied) != 0 {
				t.Errorf("%s must never reach nginx", bad)
			}
			h.contains("✗ audit", "want a private IPv4 address")
		})
	}
	t.Run("the address went away: stale trust is removed", func(t *testing.T) {
		h := webHarness(t)
		h.kube.trusted = renderTrustedProxies([]string{"10.0.100.250"})
		h.tfOut = map[string]json.RawMessage{"web_lb_private_ips": json.RawMessage(`[]`)}
		if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
			t.Fatal(err)
		}
		if strings.Join(h.kube.deleted, ",") != "configmap,networkpolicy" || h.kube.restarts != 1 {
			t.Errorf("deleted %v, restarts %d", h.kube.deleted, h.kube.restarts)
		}
		h.contains("removed the old load balancer trust list and web-ingress policy")
	})
}

func TestConfigureTeamAddress(t *testing.T) {
	h := webHarness(t)
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
		t.Fatal(err)
	}
	h.contains("Your team: https://darkplum.acme.com", "✓ reachable from this machine",
		"An admin adds teammates in Admin > Users & Keys (name, email, password); there is no invite email.")
	if len(h.probed) != 1 || h.probed[0] != "https://darkplum.acme.com" {
		t.Errorf("probed %v", h.probed)
	}

	h2 := webHarness(t)
	h2.probeErr = errors.New("dial tcp 10.0.100.250:443: i/o timeout")
	if err := h2.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
		t.Fatal(err)
	}
	h2.contains("not reachable from this machine — expected unless you are on a network in 10.0.0.0/16, 10.8.0.0/16")
}

func TestConfigureWarnsWhenSSORedirectIsElsewhere(t *testing.T) {
	for redirect, warn := range map[string]bool{
		"https://darkplum.acme.com/auth/callback": false,
		"http://localhost:8081/auth/callback":     true,
		"https://old.example.com/auth/callback":   true,
	} {
		h := webHarness(t)
		h.api.oidc = true
		h.api.setupRequired = nil
		h.kube.oidcRedirect = redirect
		if err := h.run(&ConfigureCmd{}, "\n"); err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(h.out.String(), "! sso")
		if got != warn {
			t.Errorf("redirect %s: warning shown = %v, want %v\n%s", redirect, got, warn, h.out.String())
		}
		if warn {
			h.contains("./bin/darkplum sso configure --redirect-url https://darkplum.acme.com/auth/callback")
		}
	}
}

func TestWebAddressFromSettings(t *testing.T) {
	tf := tfvarsInfo{Raw: parseTFVars("web_domain = \"acme.com\"\nvpc_cidr = \"10.20.0.0/16\"\n")}
	a := webAddressFrom(tf)
	if a.hostname != "darkplum.acme.com" || strings.Join(a.allowed, ",") != "10.20.0.0/16" {
		t.Errorf("%+v", a)
	}
	tf = tfvarsInfo{Raw: parseTFVars("web_domain = \"acme.com\"\nweb_hostname = \"chat.acme.com\"\nweb_allowed_cidrs = [\"10.8.0.0/16\"]\n")}
	if a := webAddressFrom(tf); a.hostname != "chat.acme.com" || strings.Join(a.allowed, ",") != "10.8.0.0/16" {
		t.Errorf("%+v", a)
	}
	if a := webAddressFrom(tfvarsInfo{Raw: map[string]string{}}); a.hostname != "" {
		t.Errorf("no web_domain must mean no address: %+v", a)
	}
}

func TestLBIPsAreCanonical(t *testing.T) {
	ips, err := lbIPs(map[string]json.RawMessage{"web_lb_private_ips": json.RawMessage(`["10.0.100.250","172.16.5.6","192.168.0.10"]`)})
	if err != nil || strings.Join(ips, ",") != "10.0.100.250,172.16.5.6,192.168.0.10" {
		t.Errorf("%v %v", ips, err)
	}
}

func TestConfigureAuditIdempotentWithTrailingWhitespace(t *testing.T) {
	h := webHarness(t)
	// What kubectl returns can differ from what was written only in
	// surrounding whitespace; that is not a change.
	h.kube.trusted = renderTrustedProxies([]string{"10.0.100.250", "10.0.101.250"}) + "\n\n"
	if err := h.run(&ConfigureCmd{}, "a@acme.com\n", testPassword); err != nil {
		t.Fatal(err)
	}
	if h.kube.restarts != 0 {
		t.Errorf("an unchanged list must not restart web (restarts %d)", h.kube.restarts)
	}
}
