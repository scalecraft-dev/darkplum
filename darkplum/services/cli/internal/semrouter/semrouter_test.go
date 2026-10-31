package semrouter

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	darkplum "github.com/scalecraft-dev/darkplum"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
)

var pin = []string{"--kubeconfig", "/ws/kubeconfig", "--context", "darkplum"}

type fakeKube struct {
	calls  []string
	stdin  []string
	fail   map[string]error // substring of the call -> error
	secret string           // JSON answer to `get secret -l owner=helm`
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
	return nil, nil
}

func (f *fakeKube) client() *kube.Client { return kube.ForTest(f, "darkplum-system", pin...) }

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
	if err := Apply(f.client(), objs, &out); err != nil {
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
	err := Apply(f.client(), objs, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !strings.Contains(err.Error(), "re-run, it is idempotent") {
		t.Errorf("err = %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "set env") {
			t.Error("the gateway was pointed at a router that is not ready")
		}
	}
	f = &fakeKube{fail: map[string]error{"get deploy darkplum-gateway": errors.New("NotFound")}}
	if err := Apply(f.client(), objs, &bytes.Buffer{}); err == nil || len(f.calls) != 1 {
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
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": ReleaseName, "config": config, "chart": map[string]any{"values": map[string]any{"huge": "defaults"}}})
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zw.Write(raw)
	zw.Close()
	helmB64 := base64.StdEncoding.EncodeToString(zb.Bytes())
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]string{"owner": "helm", "name": ReleaseName, "version": fmt.Sprint(version)}},
		"data": map[string]string{"release": base64.StdEncoding.EncodeToString([]byte(helmB64))}}
}

func list(items ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b)
}

const defaults = "image:\n  tag: v1\nconfig:\n  routing:\n    decisions:\n      - name: law\n"

func TestDetectHelmDefaultsAndCustom(t *testing.T) {
	def := map[string]any{"image": map[string]any{"tag": "v1"}, "config": map[string]any{"routing": map[string]any{"decisions": []any{map[string]any{"name": "law"}}}}}
	f := &fakeKube{secret: list(helmSecret(t, "sh.helm.release.v1.semantic-router.v1", 1, map[string]any{"image": "old"}),
		helmSecret(t, "sh.helm.release.v1.semantic-router.v2", 2, def))}
	rel, err := DetectHelm(f.client())
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Found || len(rel.Secrets) != 2 {
		t.Fatalf("rel = %+v", rel)
	}
	if !strings.Contains(f.calls[0], "-l owner=helm,name=semantic-router") {
		t.Errorf("selector = %s", f.calls[0])
	}
	// the LATEST revision decides
	if d, err := Differences(rel, []byte(defaults)); err != nil || len(d) != 0 {
		t.Errorf("default values differ: %v %v", d, err)
	}
	if err := CheckTakeover(rel, []byte(defaults), false); err != nil {
		t.Errorf("default release refused: %v", err)
	}

	custom := map[string]any{"image": map[string]any{"tag": "v1"}, "config": map[string]any{"routing": map[string]any{"decisions": []any{map[string]any{"name": "law"}, map[string]any{"name": "math"}}}}}
	f = &fakeKube{secret: list(helmSecret(t, "sh.helm.release.v1.semantic-router.v1", 1, custom))}
	rel, _ = DetectHelm(f.client())
	err = CheckTakeover(rel, []byte(defaults), false)
	var ce *ErrCustomised
	if !errors.As(err, &ce) || !reflect.DeepEqual(ce.Keys, []string{"config.routing.decisions"}) {
		t.Fatalf("err = %v", err)
	}
	if err := CheckTakeover(rel, []byte(defaults), true); err != nil {
		t.Errorf("explicit conversion refused: %v", err)
	}
}

func TestDetectHelmNothingAndErrors(t *testing.T) {
	f := &fakeKube{secret: `{"items":[]}`}
	if rel, err := DetectHelm(f.client()); err != nil || rel.Found {
		t.Errorf("%+v %v", rel, err)
	}
	if err := CheckTakeover(HelmRelease{}, []byte(defaults), false); err != nil {
		t.Error(err)
	}
	f = &fakeKube{fail: map[string]error{"get secret": errors.New("Unauthorized")}}
	if _, err := DetectHelm(f.client()); err == nil {
		t.Error("a failed read looked like no release")
	}
	bad := helmSecret(t, "x", 1, nil)
	bad["data"] = map[string]string{"release": "!!"}
	f = &fakeKube{secret: list(bad)}
	if _, err := DetectHelm(f.client()); err == nil {
		t.Error("garbage release accepted")
	}
}

func TestFinishTakeoverDeletesOnlyReleaseSecrets(t *testing.T) {
	f := &fakeKube{}
	err := FinishTakeover(f.client(), HelmRelease{Found: true, Secrets: []string{"sh.helm.release.v1.semantic-router.v1", "sh.helm.release.v1.semantic-router.v2"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if !strings.Contains(c, "delete secret sh.helm.release.v1.semantic-router.v") {
			t.Errorf("unexpected call %s", c)
		}
	}
	if len(f.calls) != 2 {
		t.Errorf("calls = %v", f.calls)
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
	if got := Decisions([]byte(defaults)); len(got) != 0 {
		// the shipped file indents the block differently from this fixture
		t.Logf("fixture decisions: %v", got)
	}
}
