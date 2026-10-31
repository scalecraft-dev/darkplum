package commands

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// shippedRouterValues is the values file this build ships, parsed the way helm
// would hand it back as a release's config.
func shippedRouterValues(t *testing.T) map[string]any {
	t.Helper()
	k8s, err := embeddedK8s()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(k8s, "semantic-router/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// helmListJSON is what `kubectl get secret|configmap -l owner=helm,...` returns
// for one revision of a helm-installed router.
func helmListJSON(t *testing.T, name string, config map[string]any, secret bool) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": "semantic-router", "config": config})
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zw.Write(raw)
	zw.Close()
	stored := base64.StdEncoding.EncodeToString(zb.Bytes())
	if secret {
		stored = base64.StdEncoding.EncodeToString([]byte(stored))
	}
	b, _ := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"name": name, "labels": map[string]string{"owner": "helm", "name": "semantic-router", "version": "1", "status": "deployed"}},
		"data":     map[string]string{"release": stored}}}})
	return string(b)
}

const helmSecretName = "sh.helm.release.v1.semantic-router.v1"

// withHelmRouter makes the cluster hold a helm-installed router with config.
func (h *deployHarness) withHelmRouter(config map[string]any) {
	h.k.handlers = append([]skHandler{
		{"get secret -l owner=helm", func([]string, []byte) ([]byte, error) {
			return []byte(helmListJSON(h.t, helmSecretName, config, true)), nil
		}},
	}, h.k.handlers...)
}

// routerReady lets the gateway be found (Apply needs it) and the router roll out.
func (h *deployHarness) routerCluster() {
	h.k.handlers = append([]skHandler{
		{"get deploy darkplum-gateway -o name", func([]string, []byte) ([]byte, error) { return []byte("deployment.apps/darkplum-gateway\n"), nil }},
	}, h.k.handlers...)
	h.k.on("set env", "", nil)
	h.k.on("delete", "", nil)
}

func routerOn(h *deployHarness) {
	h.env.rec.SemanticRouter = true
	h.routerCluster()
}

func TestDeployRouterOffAndNotInstalledTouchesNothing(t *testing.T) {
	h := newDeployHarness(t, 0)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	h.mustContain("[13/14] Semantic router", "semantic router", "off")
	for _, bad := range []string{"semantic-router", "set env", "SEMANTIC_ROUTER_URL"} {
		for _, c := range h.k.matching(bad) {
			if !strings.Contains(c.joined(), "get") { // reads only
				t.Errorf("a router-off install ran %v", c.args)
			}
		}
	}
	h.k.assertClean()
}

func TestDeployRouterOnAppliesWaitsThenPointsTheGateway(t *testing.T) {
	h := newDeployHarness(t, 0)
	routerOn(h)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	tl := h.tl
	// the platform and its record first, then the router, objects before the wait,
	// the wait before the gateway is pointed at it
	// the router's apply is the first apply that carries it
	applyAt := -1
	for i, e := range strings.Split(tl.String(), "\n") {
		if strings.Contains(e, "apply -f -") && i > tl.last("rollout status deployment/darkplum-web") {
			applyAt = i
			break
		}
	}
	ready := tl.first("rollout status deploy/semantic-router")
	point := tl.first("set env deploy/darkplum-gateway SEMANTIC_ROUTER_URL=http://semantic-router.darkplum-system.svc.cluster.local:8080")
	if applyAt < 0 || ready < applyAt || point < ready {
		t.Fatalf("router steps out of order (apply %d, ready %d, point %d):\n%s", applyAt, ready, point, tl)
	}
	// the router's objects were applied, labelled for pruning, in the platform namespace
	var applied string
	for _, c := range h.k.matching("apply -f -") {
		if strings.Contains(string(c.stdin), "semantic-router") {
			applied = string(c.stdin)
		}
	}
	for _, want := range []string{`"darkplum.ai/component":"semantic-router"`, `"kind":"Deployment"`, `"kind":"NetworkPolicy"`, "ghcr.io/vllm-project/semantic-router/extproc:v0.3.0@sha256:"} {
		if !strings.Contains(applied, want) {
			t.Errorf("applied router objects lack %s", want)
		}
	}
	// the router comes before the GPU/Layer 1 step, and nothing was deleted
	if tl.first("iam simulate") < tl.last("SEMANTIC_ROUTER_URL=") {
		t.Error("the router step ran after the Layer 1 self-test")
	}
	if n := len(h.k.matching("delete")); n != 0 {
		t.Errorf("a fresh router install deleted %d things", n)
	}
	h.mustContain("[13/14] Semantic router", "on · http://semantic-router.darkplum-system.svc.cluster.local:8080")
	h.k.assertClean()
}

// A helm release with the shipped values is taken over: its records are removed
// only after the router the CLI applied is ready.
func TestDeployRouterTakesOverADefaultHelmRelease(t *testing.T) {
	h := newDeployHarness(t, 0)
	routerOn(h)
	h.withHelmRouter(shippedRouterValues(t))
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	del := h.tl.first("delete secret " + helmSecretName)
	if del < 0 {
		t.Fatalf("helm's release record was not deleted:\n%s", h.tl)
	}
	if ready := h.tl.first("rollout status deploy/semantic-router"); ready < 0 || del < ready {
		t.Errorf("helm's records went before the router was ready (ready %d, delete %d):\n%s", ready, del, h.tl)
	}
	if h.tl.first("apply") > h.tl.first("delete secret "+helmSecretName) {
		t.Error("deleted before applying")
	}
	// only helm's release records; the workload is left running
	for _, c := range h.k.matching("delete") {
		if !strings.Contains(c.joined(), helmSecretName) {
			t.Errorf("unexpected delete %v", c.args)
		}
	}
	h.mustContain("taking over the helm release")
	h.k.assertClean()
}

// A customised helm release stops the run before Terraform, naming the keys; the
// flag is the explicit consent to lose them.
func TestDeployRouterRefusesACustomisedHelmReleaseUnlessConverted(t *testing.T) {
	custom := shippedRouterValues(t)
	custom["resources"] = map[string]any{"limits": map[string]any{"memory": "9Gi"}}

	h := newDeployHarness(t, 0)
	liveCluster(h)
	routerOn(h)
	h.withHelmRouter(custom)
	err := h.run()
	if !IsReported(err) {
		t.Fatalf("err = %v\n%s", err, h.out.String())
	}
	if h.tl.count("terraform") != 0 || h.writes() != 0 || len(h.k.matching("delete")) != 0 {
		t.Errorf("went on:\n%s", h.tl)
	}
	h.mustContain("resources.limits.memory", "--convert-router-defaults", "Nothing was changed")
	h.k.assertClean()

	h = newDeployHarness(t, 0)
	routerOn(h)
	h.env.flags.ConvertRouterDefaults = true
	h.withHelmRouter(custom)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if h.tl.first("delete secret "+helmSecretName) < 0 || h.tl.first("rollout status deploy/semantic-router") < 0 {
		t.Errorf("--convert-router-defaults did not take it over:\n%s", h.tl)
	}
}

// The customised refusal also holds when the cluster only became readable after
// Terraform: before the router is applied, the helm records are kept.
func TestDeployRouterNotReadyKeepsHelmsRecords(t *testing.T) {
	h := newDeployHarness(t, 0)
	routerOn(h)
	h.withHelmRouter(shippedRouterValues(t))
	h.k.handlers = append([]skHandler{{"rollout status deploy/semantic-router", func([]string, []byte) ([]byte, error) {
		return nil, errors.New("error: timed out waiting for the condition")
	}}}, h.k.handlers...)
	err := h.run()
	if !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	if h.tl.count("delete") != 0 {
		t.Errorf("helm's records were deleted although the router never became ready:\n%s", h.tl)
	}
	if h.tl.count("SEMANTIC_ROUTER_URL") != 0 {
		t.Error("the gateway was pointed at a router that is not ready")
	}
	h.mustContain("did not become ready", "re-run `darkplum deploy`")
}

func TestDeployRouterOffRemovesWhatDarkplumInstalled(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.routerCluster()
	h.k.handlers = append([]skHandler{{"darkplum.ai/component=semantic-router -o name", func([]string, []byte) ([]byte, error) {
		return []byte("deployment.apps/semantic-router\n"), nil
	}}}, h.k.handlers...)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if len(h.k.matching("set env deploy/darkplum-gateway SEMANTIC_ROUTER_URL-")) != 1 {
		t.Errorf("the gateway was not unpointed:\n%s", h.tl)
	}
	var del []kcall
	for _, c := range h.k.matching("delete") {
		del = append(del, c)
	}
	if len(del) != 1 || !strings.Contains(del[0].joined(), "-l darkplum.ai/component=semantic-router") || strings.Contains(del[0].joined(), "--all") {
		t.Errorf("removal is not selected by Darkplum's label alone: %v", del)
	}
	h.mustContain("removed")
	h.k.assertClean()
}

// A router someone installed with helm is theirs: with the setting off it is
// neither removed nor unpointed.
func TestDeployRouterOffLeavesAHelmReleaseAlone(t *testing.T) {
	h := newDeployHarness(t, 0)
	liveCluster(h)
	h.routerCluster()
	h.withHelmRouter(shippedRouterValues(t))
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if h.tl.count("set env") != 0 || h.tl.count("delete") != 0 {
		t.Errorf("deploy touched a helm-installed router with the setting off:\n%s", h.tl)
	}
	h.mustContain("installed by helm", "leaves it alone")
}
