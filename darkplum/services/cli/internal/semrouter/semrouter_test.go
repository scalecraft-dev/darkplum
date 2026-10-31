package semrouter

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	darkplum "github.com/scalecraft-dev/darkplum"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"sigs.k8s.io/yaml"
)

var pin = []string{"--kubeconfig", "/ws/kubeconfig", "--context", "darkplum"}

type fakeKube struct {
	calls      []string
	stdin      []string
	fail       map[string]error // substring of the call -> error
	secret     string           // JSON answer to `get secret -l owner=helm`
	configmaps string           // ... and to `get configmap -l owner=helm`
}

func (f *fakeKube) Run(stdin []byte, _ string, args ...string) ([]byte, error) {
	if strings.Join(args[:4], " ") != strings.Join(pin, " ") {
		return nil, fmt.Errorf("unpinned kubectl call: %v", args)
	}
	line := strings.Join(args[4:], " ")
	f.calls = append(f.calls, line)
	f.stdin = append(f.stdin, string(stdin))
	for sub, err := range f.fail {
		if strings.Contains(line, sub) {
			return nil, err
		}
	}
	if strings.Contains(line, "get secret -l owner=helm") {
		return []byte(f.secret), nil
	}
	if strings.Contains(line, "get configmap -l owner=helm") {
		if f.configmaps == "" {
			return []byte(`{"items":[]}`), nil
		}
		return []byte(f.configmaps), nil
	}
	return nil, nil
}

func (f *fakeKube) client() *kube.Client { return kube.ForTest(f, "darkplum-system", pin...) }

var checked, _ = CheckTakeover(HelmRelease{}, []byte(defaults), false)

func k8sFS(rendered bool) fs.FS {
	m := fstest.MapFS{
		policiesPath: {Data: []byte("apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: semantic-router-ingress\n  namespace: darkplum-system\nspec: {}\n")},
		valuesPath:   {Data: []byte("image:\n  tag: v1\nconfig:\n  routing:\n    decisions:\n      - name: law\n")},
	}
	if rendered {
		m[renderedPath] = &fstest.MapFile{Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: semantic-router\nspec: {}\n---\napiVersion: v1\nkind: Service\nmetadata:\n  name: semantic-router\n  namespace: other\n  labels: {a: b}\nspec: {}\n")}
	}
	return m
}

func TestObjectsAreLabelledAndPlaced(t *testing.T) {
	objs, err := Objects(k8sFS(true), "darkplum-system")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("objs = %d", len(objs))
	}
	for _, o := range objs {
		md := o["metadata"].(map[string]any)
		if md["labels"].(map[string]any)[ComponentLabel] != Component {
			t.Errorf("%s is not labelled", o.Key())
		}
		if o.Namespace() == "" {
			t.Errorf("%s has no namespace", o.Key())
		}
	}
	if objs[0].Namespace() != "darkplum-system" || objs[1].Namespace() != "other" || objs[1]["metadata"].(map[string]any)["labels"].(map[string]any)["a"] != "b" {
		t.Error("existing namespace or labels were overwritten")
	}
	if _, err := Objects(k8sFS(false), "darkplum-system"); !errors.Is(err, ErrNotRendered) {
		t.Errorf("err = %v", err)
	}
}

func TestApplyOrderAndCalls(t *testing.T) {
	f := &fakeKube{}
	objs, _ := Objects(k8sFS(true), "darkplum-system")
	var out bytes.Buffer
	if err := Apply(f.client(), checked, objs, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-n darkplum-system get deploy darkplum-gateway -o name",
		"apply -f -",
		"-n darkplum-system rollout status deploy/semantic-router --timeout=1800s",
		"-n darkplum-system set env deploy/darkplum-gateway SEMANTIC_ROUTER_URL=http://semantic-router.darkplum-system.svc.cluster.local:8080",
		"-n darkplum-system rollout status deploy/darkplum-gateway --timeout=180s",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(f.stdin[1], `"darkplum.ai/component":"semantic-router"`) {
		t.Error("applied objects are not labelled")
	}
}

func TestApplyStopsBeforePointingTheGatewayAtAnUnreadyRouter(t *testing.T) {
	f := &fakeKube{fail: map[string]error{"rollout status deploy/semantic-router": errors.New("timed out")}}
	objs, _ := Objects(k8sFS(true), "darkplum-system")
	err := Apply(f.client(), checked, objs, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), "re-run, it is idempotent") {
		t.Errorf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "set env") {
			t.Error("the gateway was pointed at a router that is not ready")
		}
	}
	f = &fakeKube{fail: map[string]error{"get deploy darkplum-gateway": errors.New("NotFound")}}
	if err := Apply(f.client(), checked, objs, &bytes.Buffer{}); err == nil || len(f.calls) != 1 {
		t.Errorf("applied without a gateway: %v %v", err, f.calls)
	}
}

func TestRemove(t *testing.T) {
	f := &fakeKube{fail: map[string]error{"set env": errors.New(`Error from server (NotFound): deployments "darkplum-gateway" not found`)}}
	if err := Remove(f.client(), nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("NotFound on the gateway is fine: %v", err)
	}
	if len(f.calls) != 2 || !strings.HasPrefix(f.calls[0], "-n darkplum-system set env deploy/darkplum-gateway SEMANTIC_ROUTER_URL-") ||
		!strings.Contains(f.calls[1], "delete ") || !strings.Contains(f.calls[1], "-l darkplum.ai/component=semantic-router --ignore-not-found") {
		t.Errorf("calls = %v", f.calls)
	}
	f = &fakeKube{fail: map[string]error{"set env": errors.New("Unauthorized")}}
	if err := Remove(f.client(), nil, &bytes.Buffer{}); err == nil || len(f.calls) != 1 {
		t.Errorf("a real error was swallowed: %v %v", err, f.calls)
	}
}

// helmSecret builds a release Secret the way helm 3 stores it.
func helmSecret(t *testing.T, name string, version int, config map[string]any) map[string]any {
	return helmObject(t, name, version, "deployed", config, true)
}

func helmObject(t *testing.T, name string, version int, status string, config map[string]any, secret bool) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": ReleaseName, "config": config, "chart": map[string]any{"values": map[string]any{"huge": "defaults"}}})
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zw.Write(raw)
	zw.Close()
	stored := base64.StdEncoding.EncodeToString(zb.Bytes())
	if secret {
		stored = base64.StdEncoding.EncodeToString([]byte(stored)) // Kubernetes' own layer
	}
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]string{"owner": "helm", "name": ReleaseName, "version": fmt.Sprint(version), "status": status}},
		"data": map[string]string{"release": stored}}
}

func list(items ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

const defaults = "image:\n  tag: v1\nconfig:\n  routing:\n    decisions:\n      - name: law\n"

var defaultCfg = map[string]any{"image": map[string]any{"tag": "v1"}, "config": map[string]any{"routing": map[string]any{"decisions": []any{map[string]any{"name": "law"}}}}}

func (f *fakeKube) withConfigMaps(js string) *fakeKube { f.configmaps = js; return f }

func TestDetectHelmPrefersTheDeployedRevision(t *testing.T) {
	custom := map[string]any{"image": "wrong"}
	// v3 failed and never ran; v2 is what is deployed.
	f := &fakeKube{secret: list(
		helmObject(t, "sh.helm.release.v1.semantic-router.v1", 1, "superseded", custom, true),
		helmObject(t, "sh.helm.release.v1.semantic-router.v2", 2, "deployed", defaultCfg, true),
		helmObject(t, "sh.helm.release.v1.semantic-router.v3", 3, "failed", custom, true))}
	rel, err := DetectHelm(f.client())
	if err != nil || !rel.Found || len(rel.Secrets) != 3 || rel.Revision != 2 || rel.Status != "deployed" {
		t.Fatalf("%+v %v", rel, err)
	}
	if _, err := CheckTakeover(rel, []byte(defaults), false); err != nil {
		t.Errorf("the deployed (default) revision was not what decided: %v", err)
	}
	// no deployed revision: the highest decides, and says so
	f = &fakeKube{secret: list(helmObject(t, "a.v1", 1, "failed", custom, true), helmObject(t, "a.v2", 2, "failed", defaultCfg, true))}
	if rel, _ := DetectHelm(f.client()); rel.Revision != 2 || rel.Status != "failed" {
		t.Errorf("%+v", rel)
	}
}

func TestDetectHelmConfigMapDriver(t *testing.T) {
	f := (&fakeKube{secret: `{"items":[]}`}).withConfigMaps(list(helmObject(t, "sh.helm.release.v1.semantic-router.v1", 1, "deployed", defaultCfg, false)))
	rel, err := DetectHelm(f.client())
	if err != nil || !rel.Found || len(rel.Secrets) != 0 || len(rel.ConfigMaps) != 1 {
		t.Fatalf("%+v %v", rel, err)
	}
	if d, _ := Differences(rel, []byte(defaults)); len(d) != 0 {
		t.Errorf("configmap values misread: %v", d)
	}
	tok, err := CheckTakeover(rel, []byte(defaults), false)
	if err != nil {
		t.Fatal(err)
	}
	f2 := &fakeKube{}
	if err := FinishTakeover(f2.client(), tok); err != nil || len(f2.calls) != 1 || !strings.Contains(f2.calls[0], "delete configmap sh.helm.release.v1.semantic-router.v1") {
		t.Errorf("%v %v", err, f2.calls)
	}
}

func TestCheckTakeoverCustomisedAndTheToken(t *testing.T) {
	custom := map[string]any{"image": map[string]any{"tag": "v1"}, "config": map[string]any{"routing": map[string]any{"decisions": []any{map[string]any{"name": "law"}, map[string]any{"name": "math"}}}}}
	f := &fakeKube{secret: list(helmSecret(t, "sh.helm.release.v1.semantic-router.v1", 1, custom))}
	rel, _ := DetectHelm(f.client())
	_, err := CheckTakeover(rel, []byte(defaults), false)
	var ce *ErrCustomised
	if !errors.As(err, &ce) || !reflect.DeepEqual(ce.Keys, []string{"config.routing.decisions"}) {
		t.Fatalf("err = %v", err)
	}
	if _, err := CheckTakeover(rel, []byte(defaults), true); err != nil {
		t.Errorf("explicit conversion refused: %v", err)
	}
	// The zero token, the only thing a caller who skipped the check can hold, does nothing.
	var none Takeover
	f2 := &fakeKube{}
	if err := Apply(f2.client(), none, nil, &bytes.Buffer{}); err == nil || len(f2.calls) != 0 {
		t.Errorf("Apply ran without a checked takeover: %v %v", err, f2.calls)
	}
	if err := FinishTakeover(f2.client(), none); err == nil || len(f2.calls) != 0 {
		t.Errorf("FinishTakeover ran without a checked takeover: %v %v", err, f2.calls)
	}
	if tok, _ := CheckTakeover(rel, []byte(defaults), true); !tok.Found() {
		t.Error("token lost the release")
	}
}

// A router installed from any values.yaml this project ever shipped is the default one.
func TestEveryShippedValuesFileIsRecognisedAsTheDefault(t *testing.T) {
	current, err := os.ReadFile("../../../../k8s/semantic-router/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("testdata/values-*.yaml")
	if len(files) != len(shippedValues) {
		t.Fatalf("%d historical files, %d recorded hashes", len(files), len(shippedValues))
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var m map[string]any
		if err := yaml.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		// as helm stores it: through JSON
		raw, _ := json.Marshal(m)
		var viaJSON map[string]any
		_ = json.Unmarshal(raw, &viaJSON)
		d, err := Differences(HelmRelease{Found: true, Values: viaJSON}, current)
		if err != nil || len(d) != 0 {
			t.Errorf("%s flagged as customised: %v %v", f, d, err)
		}
	}
	// and an edited one still is
	b, _ := os.ReadFile(files[0])
	var m map[string]any
	_ = yaml.Unmarshal(b, &m)
	m["extra"] = "edited"
	if d, _ := Differences(HelmRelease{Found: true, Values: m}, current); len(d) == 0 {
		t.Error("an edited router passed as default")
	}
}

func TestDetectHelmNothingAndErrors(t *testing.T) {
	f := &fakeKube{secret: `{"items":[]}`}
	if rel, err := DetectHelm(f.client()); err != nil || rel.Found {
		t.Errorf("%+v %v", rel, err)
	}
	f = &fakeKube{fail: map[string]error{"get secret": errors.New("Unauthorized")}}
	if _, err := DetectHelm(f.client()); err == nil {
		t.Error("a failed read looked like no release")
	}
	f = &fakeKube{fail: map[string]error{"get configmap": errors.New("Unauthorized")}, secret: `{"items":[]}`}
	if _, err := DetectHelm(f.client()); err == nil {
		t.Error("a failed configmap read looked like no release")
	}
	bad := helmSecret(t, "x", 1, nil)
	bad["data"] = map[string]string{"release": "!!"}
	f = &fakeKube{secret: list(bad)}
	if _, err := DetectHelm(f.client()); err == nil {
		t.Error("garbage release accepted")
	}
}

func TestFinishTakeoverDeletesOnlyReleaseObjects(t *testing.T) {
	f := &fakeKube{}
	tok := Takeover{checked: true, rel: HelmRelease{Found: true, Secrets: []string{"sh.helm.release.v1.semantic-router.v1", "sh.helm.release.v1.semantic-router.v2"}}}
	if err := FinishTakeover(f.client(), tok); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	for _, c := range f.calls {
		if !strings.Contains(c, "delete secret sh.helm.release.v1.semantic-router.v") {
			t.Errorf("unexpected call %s", c)
		}
	}
}

func TestDecisionsFromTheRealValues(t *testing.T) {
	k8s, _ := fs.Sub(darkplum.Tree(), "k8s")
	v, err := DefaultValues(k8s)
	if err != nil {
		t.Fatal(err)
	}
	if len(Decisions(v)) == 0 {
		t.Error("no decisions parsed from k8s/semantic-router/values.yaml")
	}
}
