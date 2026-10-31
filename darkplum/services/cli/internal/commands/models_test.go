package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	darkplum "github.com/scalecraft-dev/darkplum"
	"github.com/scalecraft-dev/darkplum/cli/internal/models"
)

// modelsCluster is the part of a cluster the models commands read and change.
type modelsCluster struct {
	*fakeCluster
	gateway    bool
	upstreams  string
	deploys    map[string]string // name -> JSON (inference namespace)
	svcIP      map[string]string
	nodes      string
	pods       string
	nodesErr   error
	setEnvErr  error
	deletedSvc []string
}

func newModelsCluster() *modelsCluster {
	m := &modelsCluster{fakeCluster: newFakeCluster(), gateway: true, deploys: map[string]string{}, svcIP: map[string]string{},
		nodes: `{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1"}}}]}`, pods: `{"items":[]}`}
	m.extra = m.handle
	return m
}

func (m *modelsCluster) handle(c call) (string, bool, error) {
	a := c.args
	line := c.line()
	inf := strings.HasPrefix(line, "-n darkplum-inference ")
	if inf {
		a = a[2:]
	}
	switch {
	case !inf && len(a) >= 2 && a[0] == "-n" && a[2] == "get" && a[3] == "deploy" && a[4] == "darkplum-gateway":
		if !m.gateway {
			return "", true, notFound("deployments", "darkplum-gateway")
		}
		if strings.Contains(line, "jsonpath") {
			return m.upstreams, true, nil
		}
		return "deployment.apps/darkplum-gateway", true, nil
	case strings.Contains(line, "set env deploy/darkplum-gateway VLLM_UPSTREAMS="):
		if m.setEnvErr != nil {
			return "", true, m.setEnvErr
		}
		m.upstreams = strings.TrimPrefix(a[len(a)-1], "VLLM_UPSTREAMS=")
		return "", true, nil
	case len(a) >= 2 && a[0] == "get" && a[1] == "nodes":
		return m.nodes, true, m.nodesErr
	case inf && a[0] == "get" && a[1] == "pods":
		return m.pods, true, nil
	case inf && a[0] == "get" && a[1] == "svc":
		ip, ok := m.svcIP[a[2]]
		if !ok {
			return "", true, notFound("services", a[2])
		}
		return ip, true, nil
	case inf && a[0] == "delete" && a[1] == "svc":
		delete(m.svcIP, a[2])
		m.deletedSvc = append(m.deletedSvc, a[2])
		return "", true, nil
	case inf && a[0] == "get" && a[1] == "deploy" && len(a) >= 3 && a[2] != "-l":
		d, ok := m.deploys[a[2]]
		if !ok {
			return "", true, notFound("deployments", a[2])
		}
		if len(a) > 4 && a[4] == "json" {
			return d, true, nil
		}
		return "deployment.apps/" + a[2], true, nil
	case inf && a[0] == "get" && a[1] == "deploy" && a[2] == "-l":
		var items []string
		for n, d := range m.deploys {
			if n != "vllm-inference" {
				items = append(items, d)
			}
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`, true, nil
	case inf && a[0] == "delete" && a[1] == "deploy,svc":
		return "", true, nil
	}
	return "", false, nil
}

func (m *modelsCluster) env(t *testing.T) (*modelsEnv, *bytes.Buffer) {
	t.Helper()
	k8s, err := fs.Sub(darkplum.Tree(), "k8s")
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	return &modelsEnv{out: out, kc: m.client(), k8s: k8s, httc: &http.Client{Timeout: 5 * time.Second}}, out
}

func TestModelsAddFlow(t *testing.T) {
	m := newModelsCluster()
	m.upstreams = "a=http://vllm-a.darkplum-inference.svc.cluster.local:8000"
	m.svcIP["vllm-qwen"] = "10.100.0.5" // an old ClusterIP Service
	e, out := m.env(t)
	err := e.add(modelsSpec("qwen", "Qwen/Qwen2.5-7B-Instruct", "abc123", "qwen-2.5-7b", 4096))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.deletedSvc) != 1 || m.deletedSvc[0] != "vllm-qwen" {
		t.Errorf("the ClusterIP Service was not recreated: %v", m.deletedSvc)
	}
	// order: headless delete, then apply, then register
	var iDel, iApply, iEnv, iRoll = -1, -1, -1, -1
	for i, l := range m.lines() {
		switch {
		case strings.Contains(l, "delete svc vllm-qwen"):
			iDel = i
		case l == "apply -f -":
			iApply = i
		case strings.Contains(l, "set env deploy/darkplum-gateway VLLM_UPSTREAMS="):
			iEnv = i
		case strings.Contains(l, "rollout status deploy/darkplum-gateway --timeout=120s"):
			iRoll = i
		}
	}
	if !(iDel >= 0 && iDel < iApply && iApply < iEnv && iEnv < iRoll) {
		t.Errorf("order wrong (delete %d, apply %d, env %d, rollout %d):\n%s", iDel, iApply, iEnv, iRoll, strings.Join(m.lines(), "\n"))
	}
	want := "a=http://vllm-a.darkplum-inference.svc.cluster.local:8000,qwen=http://vllm-qwen.darkplum-inference.svc.cluster.local:8000"
	if m.upstreams != want {
		t.Errorf("upstreams = %q", m.upstreams)
	}
	var applied string
	for _, c := range m.calls {
		if c.line() == "apply -f -" {
			applied = c.stdin
		}
	}
	var list struct{ Items []map[string]any }
	if err := json.Unmarshal([]byte(applied), &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("applied %q: %v", applied, err)
	}
	for _, want := range []string{"Qwen/Qwen2.5-7B-Instruct", "abc123", "qwen-2.5-7b", `"4096"`, "vllm-qwen"} {
		if !strings.Contains(applied, want) {
			t.Errorf("applied manifest lacks %s", want)
		}
	}
	if strings.Contains(applied, "PLACEHOLDER_") || strings.Contains(applied, "MODEL_INSTANCE") {
		t.Error("placeholders survived")
	}
	if !strings.Contains(out.String(), "Recreating Service vllm-qwen as headless (was ClusterIP 10.100.0.5)") ||
		!strings.Contains(out.String(), "Model download + load takes 15-25 minutes") {
		t.Errorf("out = %s", out)
	}
}

func modelsSpec(name, hf, rev, served string, maxLen int) models.Spec {
	return models.Spec{Name: name, HF: hf, Revision: rev, Served: served, MaxLen: maxLen}
}

func TestModelsAddRegistrationIsIdempotent(t *testing.T) {
	m := newModelsCluster()
	e, _ := m.env(t)
	for i := 0; i < 2; i++ {
		if err := e.add(modelsSpec("qwen", "Qwen/Q", "main", "", 0)); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, l := range m.lines() {
		if strings.Contains(l, "set env") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("set env ran %d times for the same model", n)
	}
}

func TestModelsAddGatewayMissingChangesNothing(t *testing.T) {
	m := newModelsCluster()
	m.gateway = false
	e, _ := m.env(t)
	err := e.add(modelsSpec("qwen", "Qwen/Q", "main", "", 0))
	if err == nil || !strings.Contains(err.Error(), "Run `darkplum deploy` first") {
		t.Errorf("err = %v", err)
	}
	if w := m.writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

func TestModelsAddInvalidSpecTouchesNothing(t *testing.T) {
	m := newModelsCluster()
	e, _ := m.env(t)
	for _, s := range []models.Spec{{Name: "default", HF: "a/b"}, {Name: "q", HF: "a/b|c"}, {Name: "q", HF: "a/b", Revision: "x y"}} {
		if err := e.add(s); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("calls = %v", m.lines())
	}
}

func TestModelsAddGPUWarning(t *testing.T) {
	m := newModelsCluster()
	m.pods = `{"items":[{"status":{"phase":"Running"},"spec":{"containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]}}]}`
	e, out := m.env(t)
	if err := e.add(modelsSpec("qwen", "Qwen/Q", "abc", "", 0)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING: no free GPU (1 of 1 allocated)") {
		t.Errorf("out = %s", out)
	}
	// an existing deployment is a redeploy, not a new GPU claim
	m = newModelsCluster()
	m.pods = `{"items":[{"status":{"phase":"Running"},"spec":{"containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]}}]}`
	m.deploys["vllm-qwen"] = "{}"
	e, out = m.env(t)
	if err := e.add(modelsSpec("qwen", "Qwen/Q", "abc", "", 0)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "WARNING") {
		t.Errorf("warned for an existing model: %s", out)
	}
	// the check failing is a note, never a failure
	m = newModelsCluster()
	m.nodesErr = errors.New("forbidden")
	e, out = m.env(t)
	if err := e.add(modelsSpec("qwen", "Qwen/Q", "main", "", 0)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "could not check for a free GPU") || !strings.Contains(out.String(), "NOTE: --revision main is a moving target") {
		t.Errorf("out = %s", out)
	}
}

func TestModelsAddRegistrationFailureIsAnError(t *testing.T) {
	m := newModelsCluster()
	m.setEnvErr = errors.New("conflict")
	e, _ := m.env(t)
	if err := e.add(modelsSpec("qwen", "Qwen/Q", "abc", "", 0)); err == nil || !strings.Contains(err.Error(), "VLLM_UPSTREAMS") {
		t.Errorf("err = %v", err)
	}
}

func TestModelsRemove(t *testing.T) {
	m := newModelsCluster()
	m.upstreams = "qwen2=http://vllm-qwen2.darkplum-inference.svc.cluster.local:8000,qwen=http://vllm-qwen.darkplum-inference.svc.cluster.local:8000"
	e, out := m.env(t)
	if err := e.remove("qwen"); err != nil {
		t.Fatal(err)
	}
	if m.upstreams != "qwen2=http://vllm-qwen2.darkplum-inference.svc.cluster.local:8000" {
		t.Errorf("upstreams = %q (a name prefix must not match)", m.upstreams)
	}
	lines := strings.Join(m.lines(), "\n")
	if strings.Index(lines, "set env") > strings.Index(lines, "delete deploy,svc -l darkplum.ai/model-instance=qwen --ignore-not-found") {
		t.Errorf("the deployment was deleted before it was unregistered:\n%s", lines)
	}
	if !strings.Contains(out.String(), "Removed vllm-qwen") {
		t.Errorf("out = %s", out)
	}
	// nothing registered: no gateway roll
	m = newModelsCluster()
	e, _ = m.env(t)
	if err := e.remove("qwen"); err != nil {
		t.Fatal(err)
	}
	for _, l := range m.lines() {
		if strings.Contains(l, "set env") || strings.Contains(l, "rollout") {
			t.Errorf("unexpected %s", l)
		}
	}
}

func TestModelsList(t *testing.T) {
	m := newModelsCluster()
	m.upstreams = "b=http://vllm-b:8000,a=http://vllm-a:8000"
	// HOME is the first init-container env, as in the real template: the model
	// must be found by name.
	m.deploys["vllm-qwen"] = `{"metadata":{"name":"vllm-qwen"},"spec":{"replicas":1,"template":{"spec":{"initContainers":[{"env":[{"name":"HOME","value":"/tmp"},{"name":"MODEL_NAME","value":"Qwen/Qwen2.5-7B-Instruct"}]}]}}},"status":{"readyReplicas":1}}`
	m.deploys["vllm-inference"] = `{}`
	e, out := m.env(t)
	if err := e.list(); err != nil {
		t.Fatal(err)
	}
	o := out.String()
	for _, want := range []string{"vllm-qwen", "1/1", "Qwen/Qwen2.5-7B-Instruct", "vllm-inference is the default deployment", "a=http://vllm-a:8000\nb=http://vllm-b:8000"} {
		if !strings.Contains(o, want) {
			t.Errorf("list lacks %q:\n%s", want, o)
		}
	}
	if strings.Contains(o, "/tmp") {
		t.Errorf("list shows HOME as the model:\n%s", o)
	}
	m = newModelsCluster()
	e, out = m.env(t)
	if err := e.list(); err != nil || !strings.Contains(out.String(), "(none beyond the default upstream)") {
		t.Errorf("empty list: %v %s", err, out)
	}
}

func TestModelsListReadFailureIsAnError(t *testing.T) {
	m := newModelsCluster()
	e, _ := m.env(t)
	m.extra = func(c call) (string, bool, error) {
		if strings.Contains(c.line(), "-l app.kubernetes.io/component=inference-backend") {
			return "", true, errors.New("Unauthorized")
		}
		return m.handle(c)
	}
	if err := e.list(); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("err = %v", err)
	}
}

// ---- models test ----------------------------------------------------------------

type tunnel struct {
	mu          sync.Mutex
	url         string
	openErr     error
	opened, cls int
	service     string
	port        int
	done        chan struct{}
}

func (f *tunnel) Open(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return f.url, f.openErr
}
func (f *tunnel) Done() <-chan struct{} { return f.done }
func (f *tunnel) Close()                { f.mu.Lock(); f.cls++; f.mu.Unlock() }

const vllmDeploy = `{"spec":{"template":{"spec":{"containers":[{"name":"vllm","args":["--model","/model","--served-model-name","qwen-2.5-7b","--port","8000"]}]}}}}`

func (m *modelsCluster) testEnv(t *testing.T, h http.HandlerFunc) (*modelsEnv, *tunnel, *bytes.Buffer, *http.Request, *[]byte) {
	t.Helper()
	var gotReq *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		gotBody = make([]byte, r.ContentLength)
		r.Body.Read(gotBody)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	e, out := m.env(t)
	tn := &tunnel{url: srv.URL, done: make(chan struct{})}
	e.fwd = func(svc string, port int) Forwarder { tn.service, tn.port = svc, port; return tn }
	return e, tn, out, gotReq, &gotBody
}

func TestModelsTestSuccess(t *testing.T) {
	m := newModelsCluster()
	m.deploys["vllm-qwen"] = vllmDeploy
	var body *[]byte
	var e *modelsEnv
	var tn *tunnel
	var out *bytes.Buffer
	e, tn, out, _, body = m.testEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"Hello there."}}]}`))
	})
	if err := e.test(context.Background(), "qwen"); err != nil {
		t.Fatal(err)
	}
	if tn.service != "vllm-qwen" || tn.port != 8000 || tn.opened != 1 || tn.cls != 1 {
		t.Errorf("tunnel = %+v", tn)
	}
	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil || req["model"] != "qwen-2.5-7b" || req["max_tokens"] != float64(50) {
		t.Errorf("request = %s", *body)
	}
	if !strings.Contains(out.String(), "SUCCESS: HTTP 200") || !strings.Contains(out.String(), "Response: Hello there.") ||
		!strings.Contains(out.String(), "gateway path is tested by `darkplum configure`") {
		t.Errorf("out = %s", out)
	}
}

func TestModelsTestFailuresAndTunnelAlwaysClosed(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"empty completion": {200, `{"choices":[{"message":{"content":"  "}}]}`, "FAIL: HTTP 200 but empty completion."},
		"no choices":       {200, `{}`, "FAIL: HTTP 200 but empty completion."},
		"still loading":    {503, `loading`, "vLLM may still be loading. Check: darkplum status"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := newModelsCluster()
			m.deploys["vllm-inference"] = vllmDeploy
			e, tn, out, _, _ := m.testEnv(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			err := e.test(context.Background(), "")
			if err == nil || !IsReported(err) {
				t.Errorf("err = %v", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("out = %s", out)
			}
			if tn.service != "vllm-inference" || tn.cls != 1 {
				t.Errorf("tunnel = %+v", tn)
			}
		})
	}
}

func TestModelsTestTunnelFailureAndNoServedName(t *testing.T) {
	m := newModelsCluster()
	m.deploys["vllm-inference"] = vllmDeploy
	e, tn, _, _, _ := m.testEnv(t, func(http.ResponseWriter, *http.Request) {})
	tn.openErr = errors.New("unable to listen")
	if err := e.test(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "unable to listen") {
		t.Errorf("err = %v", err)
	}
	if tn.cls != 1 {
		t.Error("tunnel left open after a failed Open")
	}
	m.deploys["vllm-inference"] = `{"spec":{"template":{"spec":{"containers":[{"name":"vllm","args":[]}]}}}}`
	if err := e.test(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "served-model-name") {
		t.Errorf("err = %v", err)
	}
}

func TestModelsTestBedrockOnlyAndMissingModel(t *testing.T) {
	m := newModelsCluster()
	e, tn, out, _, _ := m.testEnv(t, func(http.ResponseWriter, *http.Request) {})
	if err := e.test(context.Background(), ""); err != nil {
		t.Errorf("Bedrock-only must exit 0: %v", err)
	}
	if !strings.Contains(out.String(), "Bedrock-only install: there is no self-hosted vLLM to test") || tn.opened != 0 {
		t.Errorf("out = %s, opened %d", out, tn.opened)
	}
	if err := e.test(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "vllm-nope does not exist") {
		t.Errorf("err = %v", err)
	}
	if tn.opened != 0 {
		t.Error("a tunnel was opened for a model that does not exist")
	}
}

func TestModelsGrammar(t *testing.T) {
	var cli CLI
	p, err := kong.New(&cli, kong.Name("darkplum"), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"models", "add", "qwen", "--hf", "Qwen/Q", "--revision", "abc", "--served", "q", "--max-len", "4096"},
		{"models", "add", "qwen", "--model", "Qwen/Q"},
		{"models", "remove", "qwen"},
		{"models", "list"},
		{"models", "test"},
		{"models", "test", "qwen"},
	} {
		cli = CLI{}
		if _, err := p.Parse(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"models", "add", "qwen"}, {"models", "add"}, {"models", "test", "--context", "kind-x"}} {
		// a fresh parser: kong remembers which flags an earlier Parse saw
		var c2 CLI
		p2, _ := kong.New(&c2, kong.Name("darkplum"), kong.Exit(func(int) {}))
		if _, err := p2.Parse(args); err == nil {
			t.Errorf("%v parsed", args)
		}
	}
}
