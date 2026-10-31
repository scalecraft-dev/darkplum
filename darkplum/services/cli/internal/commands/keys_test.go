package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/alecthomas/kong"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/vendor"
)

const testNS = "darkplum-system"

var pinned = []string{"--kubeconfig", "/ws/kubeconfig", "--context", "darkplum"}

// call is one kubectl invocation as the fake saw it.
type call struct {
	args  []string // after the pinned selectors
	stdin string
}

func (c call) line() string { return strings.Join(c.args, " ") }

// fakeCluster is a small stateful kubectl: ConfigMaps, Secrets (data is base64,
// as in the API) and Namespaces, with the argv of every call recorded. Anything
// it does not model is an error, so an unexpected kubectl call fails the test.
type fakeCluster struct {
	mu         sync.Mutex
	calls      []call
	configmaps map[string]map[string]string
	secrets    map[string]map[string]string // "ns/name" -> data (base64)
	namespaces map[string]bool
	failGet    map[string]error // "configmap/name" or "secret/name" -> error
	deploys    map[string]fakeDeploy
	extra      func(c call) (string, bool, error)
}

type fakeDeploy struct{ env map[string]string }

func newFakeCluster() *fakeCluster {
	return &fakeCluster{configmaps: map[string]map[string]string{}, secrets: map[string]map[string]string{},
		namespaces: map[string]bool{testNS: true}, failGet: map[string]error{}, deploys: map[string]fakeDeploy{}}
}

func notFound(kind, name string) error {
	return fmt.Errorf(`kubectl: Error from server (NotFound): %s "%s" not found`, kind, name)
}

func (f *fakeCluster) Run(stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) < 4 || strings.Join(args[:4], " ") != strings.Join(pinned, " ") {
		return nil, fmt.Errorf("kubectl call without the pinned selectors: %v", args)
	}
	c := call{args: args[4:], stdin: string(stdin)}
	f.calls = append(f.calls, c)
	if f.extra != nil {
		if out, ok, err := f.extra(c); ok {
			return []byte(out), err
		}
	}
	a := c.args
	ns := ""
	if len(a) >= 2 && a[0] == "-n" {
		ns, a = a[1], a[2:]
	}
	sec := func(name string) string { return ns + "/" + name }
	switch {
	case len(a) >= 3 && a[0] == "get" && a[1] == "configmap":
		if err := f.failGet["configmap/"+a[2]]; err != nil {
			return nil, err
		}
		d, ok := f.configmaps[a[2]]
		if !ok {
			return nil, notFound("configmaps", a[2])
		}
		b, _ := json.Marshal(map[string]any{"data": d})
		return b, nil
	case len(a) >= 3 && a[0] == "get" && a[1] == "secret":
		if err := f.failGet["secret/"+a[2]]; err != nil {
			return nil, err
		}
		d, ok := f.secrets[sec(a[2])]
		if !ok {
			return nil, notFound("secrets", a[2])
		}
		if len(a) > 3 && strings.HasPrefix(a[4], "go-template=") { // names only
			var out string
			for k := range d {
				out += k + "\n"
			}
			return []byte(out), nil
		}
		return nil, nil
	case len(a) >= 3 && a[0] == "get" && a[1] == "namespace":
		if !f.namespaces[a[2]] {
			return nil, notFound("namespaces", a[2])
		}
		return nil, nil
	case len(a) == 4 && a[0] == "create" && a[1] == "secret" && a[2] == "generic":
		f.secrets[sec(a[3])] = map[string]string{}
		return nil, nil
	case len(a) == 3 && a[0] == "create" && a[1] == "configmap":
		f.configmaps[a[2]] = map[string]string{}
		return nil, nil
	case len(a) >= 3 && a[0] == "patch" && (a[1] == "secret" || a[1] == "configmap"):
		typ := a[4]
		var target map[string]string
		if a[1] == "secret" {
			target = f.secrets[sec(a[2])]
		} else {
			target = f.configmaps[a[2]]
		}
		if target == nil {
			return nil, notFound(a[1], a[2])
		}
		if typ == "merge" {
			var p struct{ Data map[string]string }
			if err := json.Unmarshal(stdin, &p); err != nil {
				return nil, err
			}
			for k, v := range p.Data {
				target[k] = v
			}
		} else {
			var ops []map[string]string
			if err := json.Unmarshal(stdin, &ops); err != nil {
				return nil, err
			}
			for _, op := range ops {
				k := strings.TrimPrefix(op["path"], "/data/")
				if _, ok := target[k]; !ok {
					return nil, fmt.Errorf("the server rejected our request: remove of missing path %s", op["path"])
				}
				delete(target, k)
			}
		}
		return nil, nil
	case len(a) >= 1 && a[0] == "apply":
		var m map[string]any
		if err := json.Unmarshal(stdin, &m); err != nil {
			// a YAML manifest (the bundled namespace): record only.
			if strings.Contains(string(stdin), "kind: Namespace") {
				f.namespaces[inferenceNamespace] = true
				return nil, nil
			}
			return nil, err
		}
		if m["kind"] == "Secret" {
			md := m["metadata"].(map[string]any)
			d := map[string]string{}
			for k, v := range m["stringData"].(map[string]any) {
				d[k] = b64(v.(string))
			}
			f.secrets[md["namespace"].(string)+"/"+md["name"].(string)] = d
		}
		return nil, nil
	case len(a) >= 2 && a[0] == "rollout":
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected kubectl %s", c.line())
}

func (f *fakeCluster) client() *kube.Client { return kube.ForTest(f, testNS, pinned...) }

func (f *fakeCluster) lines() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.line())
	}
	return out
}

// writes are the calls that change the cluster.
func (f *fakeCluster) writes() []string {
	var out []string
	for _, c := range f.calls {
		switch c.args[0] {
		case "patch", "create", "apply", "delete":
			out = append(out, c.line())
		case "rollout":
			if c.args[1] == "restart" {
				out = append(out, c.line())
			}
		case "-n":
			if len(c.args) > 2 && (c.args[2] == "patch" || c.args[2] == "create" || c.args[2] == "apply" || c.args[2] == "delete") {
				out = append(out, c.line())
			}
		}
	}
	return out
}

// noSecretInArgv is the guard every keys test ends with: the secret appears in
// no argument of any kubectl call.
func (f *fakeCluster) noSecretInArgv(t *testing.T, secrets ...string) {
	t.Helper()
	for _, c := range f.calls {
		for _, s := range secrets {
			if strings.Contains(c.line(), s) {
				t.Errorf("secret %q is in argv: %s", s, c.line())
			}
		}
	}
}

func (f *fakeCluster) stdinHas(s string) bool {
	for _, c := range f.calls {
		if strings.Contains(c.stdin, s) {
			return true
		}
	}
	return false
}

// vendorServer answers the model list for any key that starts with "good".
func vendorServer(t *testing.T, ids ...string) (*vendor.Checker, *[]string) {
	t.Helper()
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" ws="+r.Header.Get("anthropic-workspace-id"))
		mu.Unlock()
		k := r.Header.Get("x-api-key") + r.Header.Get("Authorization")
		if !strings.Contains(k, "good") {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		type m struct {
			ID string `json:"id"`
		}
		var data []m
		for _, id := range ids {
			data = append(data, m{id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return &vendor.Checker{HTTP: srv.Client(), AnthropicURL: srv.URL, OpenAIURL: srv.URL, Now: time.Now}, &seen
}

type keysHarness struct {
	t   *testing.T
	f   *fakeCluster
	env *keysEnv
	out *bytes.Buffer
	now time.Time
	web *[]string
}

func newKeysHarness(t *testing.T, ids ...string) *keysHarness {
	t.Helper()
	f := newFakeCluster()
	chk, seen := vendorServer(t, ids...)
	h := &keysHarness{t: t, f: f, out: &bytes.Buffer{}, now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), web: seen}
	h.env = &keysEnv{out: h.out, kc: f.client(), getenv: func(string) string { return "" },
		readKey: func(bool, string) (string, error) { return "", errors.New("no key supplied by the test") },
		check:   chk, now: func() time.Time { return h.now },
		k8s: fstest.MapFS{"base/namespaces/darkplum-inference.yaml": {Data: []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: darkplum-inference\n")}}}
	return h
}

func (h *keysHarness) withKey(k string) {
	h.env.readKey = func(bool, string) (string, error) { return k, nil }
}

func (h *keysHarness) set(c KeysSetCmd) error {
	h.t.Helper()
	if err := c.validate(); err != nil {
		return err
	}
	return h.env.set(context.Background(), &c)
}

func TestKeysSetAnthropicFirstTime(t *testing.T) {
	h := newKeysHarness(t, "claude-a", "claude-b", "embed-x")
	h.withKey("good-key_1")
	if err := h.set(KeysSetCmd{Vendor: "anthropic", Models: "claude-*"}); err != nil {
		t.Fatal(err)
	}
	if got := h.f.secrets[testNS+"/darkplum-provider-keys"]["anthropic-api-key"]; got != b64("good-key_1") {
		t.Errorf("stored key = %q", got)
	}
	cm := h.f.configmaps["darkplum-provider-config"]
	if cm["ANTHROPIC_MODEL_FILTER"] != "claude-*" || cm["ANTHROPIC_MODEL_FILTER_SET_AT"] != "2026-10-31T12:00:00Z" {
		t.Errorf("configmap = %v", cm)
	}
	if _, ok := cm["ANTHROPIC_WORKSPACE_ID"]; ok {
		t.Errorf("a workspace id was written that nobody asked for: %v", cm)
	}
	if !strings.Contains(strings.Join(h.f.lines(), "\n"), "rollout restart deploy/darkplum-gateway") ||
		!strings.Contains(strings.Join(h.f.lines(), "\n"), "rollout status deploy/darkplum-gateway --timeout=180s") {
		t.Errorf("no gateway restart: %v", h.f.lines())
	}
	h.f.noSecretInArgv(t, "good-key_1", b64("good-key_1"))
	if !h.f.stdinHas(b64("good-key_1")) {
		t.Error("the key never reached kubectl on stdin")
	}
	if !strings.Contains(h.out.String(), "This key can reach 3 Anthropic models. The filter 'claude-*' admits 2 of them:") ||
		strings.Contains(h.out.String(), "good-key_1") {
		t.Errorf("output = %s", h.out)
	}
	// Order: the key is stored before the filter, and the verification came first.
	w := strings.Join(h.f.writes(), "\n")
	if strings.Index(w, "patch secret darkplum-provider-keys") > strings.Index(w, "patch configmap darkplum-provider-config") {
		t.Errorf("the filter was written before the key:\n%s", w)
	}
	if len(*h.web) != 1 || !strings.HasPrefix((*h.web)[0], "/v1/models") {
		t.Errorf("vendor calls = %v", *h.web)
	}
}

func TestKeysSetRefusesBareWildcardBeforeAnyCall(t *testing.T) {
	for _, models := range []string{"*", "claude-*,*", "a,,b", "?"} {
		h := newKeysHarness(t, "claude-a")
		h.withKey("good")
		err := h.set(KeysSetCmd{Vendor: "anthropic", Models: models})
		if err == nil || !strings.Contains(err.Error(), "admits every model") || !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%q: err = %v", models, err)
		}
		if len(h.f.writes()) != 0 || len(*h.web) != 0 {
			t.Errorf("%q: writes %v, vendor calls %v", models, h.f.writes(), *h.web)
		}
	}
	h := newKeysHarness(t, "claude-a")
	h.withKey("good")
	if err := h.set(KeysSetCmd{Vendor: "anthropic", Models: "*", AllowAll: true}); err != nil {
		t.Errorf("--allow-all: %v", err)
	}
}

// A reused stored filter goes through the same check.
func TestKeysSetChecksStoredFilterToo(t *testing.T) {
	h := newKeysHarness(t, "gpt-4o")
	h.f.configmaps["darkplum-provider-config"] = map[string]string{"OPENAI_MODEL_FILTER": "*"}
	h.withKey("good")
	if err := h.set(KeysSetCmd{Vendor: "openai"}); err == nil || !strings.Contains(err.Error(), "admits every model") {
		t.Errorf("err = %v", err)
	}
	if len(h.f.writes()) != 0 {
		t.Errorf("writes = %v", h.f.writes())
	}
}

func TestKeysSetNeedsFilterTheFirstTime(t *testing.T) {
	h := newKeysHarness(t)
	h.withKey("good")
	err := h.set(KeysSetCmd{Vendor: "openai"})
	if err == nil || !strings.Contains(err.Error(), "--models is required the first time") || !strings.Contains(err.Error(), "gpt-*") {
		t.Errorf("err = %v", err)
	}
}

func TestKeysSetVerificationFailuresChangeNothing(t *testing.T) {
	cases := map[string]struct {
		key, models, want string
		skip              bool
		ids               []string
	}{
		"rejected key":      {key: "bad", models: "claude-*", want: "rejected this key (HTTP 401). Nothing was changed.", ids: []string{"claude-a"}},
		"filter matches no": {key: "good", models: "claude-*", want: "the filter matches nothing this key can reach. Nothing was changed.", ids: []string{"gpt-4o"}},
		"bad charset":       {key: "good key", models: "claude-*", want: "contains characters no Anthropic key has. Nothing was changed.", ids: []string{"claude-a"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newKeysHarness(t, tc.ids...)
			h.withKey(tc.key)
			err := h.set(KeysSetCmd{Vendor: "anthropic", Models: tc.models})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if w := h.f.writes(); len(w) != 0 {
				t.Errorf("writes = %v", w)
			}
		})
	}
}

func TestKeysSetUnreachableVendorAndSkipVerify(t *testing.T) {
	h := newKeysHarness(t)
	h.withKey("good")
	h.env.check = &vendor.Checker{HTTP: &http.Client{Timeout: time.Second}, AnthropicURL: "http://127.0.0.1:1", Now: time.Now}
	err := h.set(KeysSetCmd{Vendor: "anthropic", Models: "claude-*"})
	if err == nil || !strings.Contains(err.Error(), "could not reach Anthropic. Nothing was changed. (--skip-verify skips this check.)") {
		t.Errorf("err = %v", err)
	}
	if len(h.f.writes()) != 0 {
		t.Errorf("writes = %v", h.f.writes())
	}
	if err := h.set(KeysSetCmd{Vendor: "anthropic", Models: "claude-*", SkipVerify: true}); err != nil {
		t.Errorf("--skip-verify: %v", err)
	}
}

func TestKeysSetOrganisationKeyNeedsWorkspace(t *testing.T) {
	f := newFakeCluster()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-workspace-id") == "" {
			http.Error(w, `{"error":"anthropic-workspace-id is required"}`, 400)
			return
		}
		w.Write([]byte(`{"data":[{"id":"claude-a"}]}`))
	}))
	defer srv.Close()
	env := &keysEnv{out: &bytes.Buffer{}, kc: f.client(), getenv: func(string) string { return "" },
		readKey: func(bool, string) (string, error) { return "good", nil },
		check:   &vendor.Checker{HTTP: srv.Client(), AnthropicURL: srv.URL, Now: time.Now}, now: time.Now}
	err := env.set(context.Background(), &KeysSetCmd{Vendor: "anthropic", Models: "claude-*"})
	if err == nil || !strings.Contains(err.Error(), "--workspace wrkspc_") {
		t.Errorf("err = %v", err)
	}
	if len(f.writes()) != 0 {
		t.Errorf("writes = %v", f.writes())
	}
	if err := env.set(context.Background(), &KeysSetCmd{Vendor: "anthropic", Models: "claude-*", Workspace: "wrkspc_01"}); err != nil {
		t.Fatal(err)
	}
	if f.configmaps["darkplum-provider-config"]["ANTHROPIC_WORKSPACE_ID"] != "wrkspc_01" {
		t.Errorf("workspace not stored: %v", f.configmaps)
	}
}

// Rotating the key with nothing else changed must not restart the gateway.
func TestKeysRotationInPlaceDoesNotRestart(t *testing.T) {
	h := newKeysHarness(t, "claude-a")
	h.f.secrets[testNS+"/darkplum-provider-keys"] = map[string]string{"anthropic-api-key": b64("old")}
	h.f.configmaps["darkplum-provider-config"] = map[string]string{"ANTHROPIC_MODEL_FILTER": "claude-*", "ANTHROPIC_MODEL_FILTER_SET_AT": "2026-01-01T00:00:00Z"}
	h.withKey("good-new")
	if err := h.set(KeysSetCmd{Vendor: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	for _, l := range h.f.lines() {
		if strings.Contains(l, "rollout") {
			t.Errorf("restarted: %s", l)
		}
	}
	if h.f.configmaps["darkplum-provider-config"]["ANTHROPIC_MODEL_FILTER_SET_AT"] != "2026-01-01T00:00:00Z" {
		t.Error("the stamp moved although the filter did not")
	}
	if !strings.Contains(h.out.String(), "Key rotated in place") {
		t.Errorf("out = %s", h.out)
	}
}

// Filter change: the stamp outranks the previous one even from a future-dated one.
func TestKeysFilterChangeStampSkewProof(t *testing.T) {
	h := newKeysHarness(t, "claude-a")
	h.f.secrets[testNS+"/darkplum-provider-keys"] = map[string]string{"anthropic-api-key": b64("old")}
	h.f.configmaps["darkplum-provider-config"] = map[string]string{"ANTHROPIC_MODEL_FILTER": "claude-x*", "ANTHROPIC_MODEL_FILTER_SET_AT": "2030-01-01T00:00:00Z"}
	h.withKey("good")
	if err := h.set(KeysSetCmd{Vendor: "anthropic", Models: "claude-*"}); err != nil {
		t.Fatal(err)
	}
	if got := h.f.configmaps["darkplum-provider-config"]["ANTHROPIC_MODEL_FILTER_SET_AT"]; got != "2030-01-01T00:00:01Z" {
		t.Errorf("stamp = %s", got)
	}
}

func TestKeysWorkspaceKeptByDefaultAndClearedExplicitly(t *testing.T) {
	seed := func(h *keysHarness) {
		h.f.secrets[testNS+"/darkplum-provider-keys"] = map[string]string{"anthropic-api-key": b64("old")}
		h.f.configmaps["darkplum-provider-config"] = map[string]string{"ANTHROPIC_MODEL_FILTER": "claude-*",
			"ANTHROPIC_MODEL_FILTER_SET_AT": "2026-01-01T00:00:00Z", "ANTHROPIC_WORKSPACE_ID": "wrkspc_keep"}
		h.withKey("good")
	}
	// omitted: kept, used for verification, no restart
	h := newKeysHarness(t, "claude-a")
	seed(h)
	if err := h.set(KeysSetCmd{Vendor: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if h.f.configmaps["darkplum-provider-config"]["ANTHROPIC_WORKSPACE_ID"] != "wrkspc_keep" {
		t.Error("the stored workspace was dropped by a rotation")
	}
	if len(*h.web) != 1 || !strings.Contains((*h.web)[0], "ws=wrkspc_keep") {
		t.Errorf("verification did not use the stored workspace: %v", *h.web)
	}
	for _, l := range h.f.lines() {
		if strings.Contains(l, "rollout") {
			t.Errorf("restart for an unchanged workspace: %s", l)
		}
	}
	// --no-workspace: cleared, stamp recomputed, restart
	h = newKeysHarness(t, "claude-a")
	seed(h)
	if err := h.set(KeysSetCmd{Vendor: "anthropic", NoWorkspace: true}); err != nil {
		t.Fatal(err)
	}
	cm := h.f.configmaps["darkplum-provider-config"]
	if v, ok := cm["ANTHROPIC_WORKSPACE_ID"]; !ok || v != "" {
		t.Errorf("workspace = %q, %v", v, ok)
	}
	if cm["ANTHROPIC_MODEL_FILTER_SET_AT"] != "2026-10-31T12:00:00Z" {
		t.Errorf("stamp = %s", cm["ANTHROPIC_MODEL_FILTER_SET_AT"])
	}
	if !strings.Contains(strings.Join(h.f.lines(), "\n"), "rollout restart") {
		t.Error("no restart after the workspace changed")
	}
	if (*h.web)[0] != "/v1/models ws=" {
		t.Errorf("verification used %v", *h.web)
	}
}

func TestKeysSetFlagValidation(t *testing.T) {
	cases := map[string]KeysSetCmd{
		"--workspace is an Anthropic setting":                       {Vendor: "openai", Workspace: "wrkspc_1"},
		"contradict each other":                                     {Vendor: "anthropic", Workspace: "wrkspc_1", NoWorkspace: true},
		"must be an Anthropic workspace id":                         {Vendor: "anthropic", Workspace: "bad id"},
		"--billing stores the admin key only":                       {Vendor: "openai", Billing: true, Models: "gpt-*"},
		"keys set huggingface takes only --key-stdin and --context": {Vendor: "huggingface", Models: "x"},
	}
	for want, c := range cases {
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want %q", c, err, want)
		}
	}
}

func TestKeysReadErrorIsNotTreatedAsAbsent(t *testing.T) {
	h := newKeysHarness(t, "claude-a")
	h.f.failGet["configmap/darkplum-provider-config"] = errors.New("kubectl: error: You must be logged in to the server (Unauthorized)")
	h.withKey("good")
	if err := h.set(KeysSetCmd{Vendor: "anthropic", Models: "claude-*"}); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("err = %v", err)
	}
	if len(h.f.writes()) != 0 {
		t.Errorf("wrote after a failed read: %v", h.f.writes())
	}
	h = newKeysHarness(t)
	h.f.failGet["secret/darkplum-provider-keys"] = errors.New("Unauthorized")
	if err := h.env.remove(&KeysRemoveCmd{Vendor: "openai"}); err == nil {
		t.Error("remove swallowed a read error")
	}
}

func TestKeySourceOrder(t *testing.T) {
	h := newKeysHarness(t)
	var calls []bool
	h.env.readKey = func(stdin bool, _ string) (string, error) { calls = append(calls, stdin); return "from-reader", nil }
	h.env.getenv = func(n string) string {
		if n == "ANTHROPIC_API_KEY" {
			return "from-env"
		}
		return ""
	}
	if k, _ := h.env.key(true, "ANTHROPIC_API_KEY", "x"); k != "from-reader" || len(calls) != 1 || !calls[0] {
		t.Errorf("--key-stdin lost to the environment: %q %v", k, calls)
	}
	if k, _ := h.env.key(false, "ANTHROPIC_API_KEY", "x"); k != "from-env" || len(calls) != 1 {
		t.Errorf("env key = %q %v", k, calls)
	}
	if k, _ := h.env.key(false, "OPENAI_API_KEY", "x"); k != "from-reader" || len(calls) != 2 || calls[1] {
		t.Errorf("prompt = %q %v", k, calls)
	}
	h.env.readKey = func(bool, string) (string, error) { return " \n", nil }
	if _, err := h.env.key(false, "OPENAI_API_KEY", "x"); err == nil || !strings.Contains(err.Error(), "no key given") {
		t.Errorf("empty key accepted: %v", err)
	}
}

func TestKeysRemovePatchesOnlyWhatIsThere(t *testing.T) {
	h := newKeysHarness(t)
	h.f.secrets[testNS+"/darkplum-provider-keys"] = map[string]string{"anthropic-api-key": "x", "openai-api-key": "y"}
	h.f.configmaps["darkplum-provider-config"] = map[string]string{"ANTHROPIC_MODEL_FILTER": "claude-*", "ANTHROPIC_MODEL_FILTER_SET_AT": "t",
		"OPENAI_MODEL_FILTER": "gpt-*"}
	if err := h.env.remove(&KeysRemoveCmd{Vendor: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.f.secrets[testNS+"/darkplum-provider-keys"]["anthropic-api-key"]; ok {
		t.Error("key not removed")
	}
	if h.f.secrets[testNS+"/darkplum-provider-keys"]["openai-api-key"] != "y" {
		t.Error("another vendor's key was touched")
	}
	cm := h.f.configmaps["darkplum-provider-config"]
	if len(cm) != 1 || cm["OPENAI_MODEL_FILTER"] != "gpt-*" {
		t.Errorf("configmap = %v", cm)
	}
	if !strings.Contains(h.out.String(), "Revoke the key at Anthropic as well") {
		t.Errorf("out = %s", h.out)
	}
	// a second removal is a no-op: no patch (the fake rejects removing a missing path), no restart
	n := len(h.f.calls)
	if err := h.env.remove(&KeysRemoveCmd{Vendor: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.f.calls[n:] {
		if c.args[0] == "patch" || c.args[0] == "rollout" {
			t.Errorf("unexpected %s", c.line())
		}
	}
}

func TestKeysBilling(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-api-key")
		if gotKey != "adm-good" {
			http.Error(w, "no", 403)
		}
	}))
	defer srv.Close()
	f := newFakeCluster()
	out := &bytes.Buffer{}
	env := &keysEnv{out: out, kc: f.client(), getenv: func(n string) string {
		if n == "ANTHROPIC_ADMIN_KEY" {
			return "adm-good"
		}
		return "adm-bad"
	}, check: &vendor.Checker{HTTP: srv.Client(), AnthropicURL: srv.URL, OpenAIURL: srv.URL, Now: time.Now}, now: time.Now}
	if err := env.set(context.Background(), &KeysSetCmd{Vendor: "anthropic", Billing: true}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/organizations/usage_report/messages" || f.secrets[testNS+"/darkplum-billing-keys"]["anthropic-billing-key"] != b64("adm-good") {
		t.Errorf("path %s, secrets %v", gotPath, f.secrets)
	}
	for _, l := range f.lines() {
		if strings.Contains(l, "rollout") {
			t.Errorf("billing keys do not restart the gateway: %s", l)
		}
	}
	f.noSecretInArgv(t, "adm-good", b64("adm-good"))
	// a key the vendor refuses is not stored
	f2 := newFakeCluster()
	env.kc = f2.client()
	err := env.set(context.Background(), &KeysSetCmd{Vendor: "openai", Billing: true})
	if err == nil || !strings.Contains(err.Error(), "OpenAI answered HTTP 403 to a usage query with this key. It needs an organisation admin key") {
		t.Errorf("err = %v", err)
	}
	if len(f2.writes()) != 0 {
		t.Errorf("writes = %v", f2.writes())
	}
	// removal
	if err := env.removeBilling(mustInfo(vendor.Anthropic)); err != nil {
		t.Fatal(err)
	}
}

func mustInfo(n vendor.Name) vendor.Info { i, _ := vendor.Get(n); return i }

func TestKeysHuggingFace(t *testing.T) {
	h := newKeysHarness(t)
	delete(h.f.namespaces, inferenceNamespace)
	h.withKey("hf_secretToken123")
	if err := h.set(KeysSetCmd{Vendor: "huggingface"}); err != nil {
		t.Fatal(err)
	}
	if !h.f.namespaces[inferenceNamespace] {
		t.Error("the inference namespace was not created")
	}
	if h.f.secrets[inferenceNamespace+"/huggingface-token"]["token"] != b64("hf_secretToken123") {
		t.Errorf("secrets = %v", h.f.secrets)
	}
	h.f.noSecretInArgv(t, "hf_secretToken123")
	var applied bool
	for _, c := range h.f.calls {
		if c.args[0] == "apply" && strings.Contains(c.stdin, "huggingface-token") {
			applied = strings.Contains(c.line(), "--server-side")
		}
	}
	if !applied {
		t.Error("the token Secret must be applied server-side so it is not copied into an annotation")
	}
	if !strings.Contains(h.out.String(), "rollout restart -n darkplum-inference deploy/<vllm-deployment>") {
		t.Errorf("out = %s", h.out)
	}
	if err := h.set(KeysSetCmd{Vendor: "huggingface", KeyStdin: true}); err != nil {
		t.Errorf("second run: %v", err)
	}
}

func TestKeysListNeverPrintsValues(t *testing.T) {
	h := newKeysHarness(t)
	h.f.secrets[testNS+"/darkplum-provider-keys"] = map[string]string{"anthropic-api-key": b64("sk-ant-SECRET")}
	h.f.secrets[inferenceNamespace+"/huggingface-token"] = map[string]string{"token": b64("hf_SECRET")}
	h.f.configmaps["darkplum-provider-config"] = map[string]string{"ANTHROPIC_MODEL_FILTER": "claude-*", "ANTHROPIC_MODEL_FILTER_SET_AT": "2026-01-01T00:00:00Z"}
	if err := h.env.list(); err != nil {
		t.Fatal(err)
	}
	o := h.out.String()
	for _, bad := range []string{"SECRET", b64("sk-ant-SECRET"), b64("hf_SECRET")} {
		if strings.Contains(o, bad) {
			t.Errorf("list printed %q:\n%s", bad, o)
		}
	}
	for _, want := range []string{"Anthropic", "claude-*", "2026-01-01T00:00:00Z", "installed", "Hugging Face"} {
		if !strings.Contains(o, want) {
			t.Errorf("list lacks %q:\n%s", want, o)
		}
	}
	// the values are never even fetched: only key names are asked for
	for _, c := range h.f.calls {
		if c.args[len(c.args)-2] == "-o" && c.args[1] != "configmap" && !strings.Contains(c.args[len(c.args)-1], "go-template") && strings.Contains(c.line(), "secret") {
			t.Errorf("a secret was read in full: %s", c.line())
		}
	}
}

// Every call goes through the pinned handle: the fake refuses any other argv.
func TestKeysCallsArePinned(t *testing.T) {
	h := newKeysHarness(t, "claude-a")
	h.withKey("good")
	if err := h.set(KeysSetCmd{Vendor: "anthropic", Models: "claude-*"}); err != nil {
		t.Fatal(err)
	}
	if len(h.f.calls) == 0 {
		t.Fatal("no calls")
	}
}

func TestKeysGrammar(t *testing.T) {
	var cli CLI
	p, err := kong.New(&cli, kong.Name("darkplum"), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"keys", "set", "anthropic", "--models", "claude-*", "--workspace", "wrkspc_1", "--allow-all", "--skip-verify", "--key-stdin"},
		{"keys", "set", "openai", "--billing", "--key-stdin"},
		{"keys", "set", "huggingface", "--key-stdin"},
		{"keys", "remove", "anthropic"},
		{"keys", "remove", "openai", "--billing"},
		{"keys", "list"},
	} {
		cli = CLI{}
		if _, err := p.Parse(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"keys", "set", "cohere"}, {"keys", "set"}, {"keys", "remove", "huggingface"}, {"keys", "set", "anthropic", "--key", "sk-x"}} {
		cli = CLI{}
		if _, err := p.Parse(args); err == nil {
			t.Errorf("%v parsed", args)
		}
	}
}
