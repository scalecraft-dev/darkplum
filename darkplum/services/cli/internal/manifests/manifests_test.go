package manifests

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	darkplum "github.com/scalecraft-dev/darkplum"
)

const dg = "@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"

func k8s(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(darkplum.Tree(), "k8s")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func inputs() Inputs {
	return Inputs{
		Images: map[string]string{
			"gateway": "123456789012.dkr.ecr.us-east-2.amazonaws.com/acme/darkplum-gateway" + dg,
			"api":     "123456789012.dkr.ecr.us-east-2.amazonaws.com/acme/darkplum-api" + dg,
			"web":     "123456789012.dkr.ecr.us-east-2.amazonaws.com/acme/darkplum-web" + dg,
		},
		BedrockRegion: "us-east-1", BedrockModelFilter: "us.anthropic.claude-*", VLLMUpstreams: "qwen=http://vllm-qwen.darkplum-inference:8000",
		ServiceCIDR: "172.20.0.0/16", VPCCIDR: "10.0.0.0/16",
	}
}

func platform(t *testing.T) []Phase {
	t.Helper()
	ph, err := Platform(k8s(t), inputs())
	if err != nil {
		t.Fatal(err)
	}
	return ph
}

func all(ph []Phase) []Object {
	var out []Object
	for _, p := range ph {
		out = append(out, p.Objects...)
	}
	return out
}

func TestPlatformPhasesAreOrderedPoliciesBeforeWorkloads(t *testing.T) {
	ph := platform(t)
	var names []string
	for _, p := range ph {
		names = append(names, p.Name)
	}
	want := "namespaces,network policies,identities and vendor-key objects,api,gateway,web"
	if strings.Join(names, ",") != want {
		t.Fatalf("phases = %v", names)
	}
	// no workload before every policy; no Deployment before its identities
	seenWorkload := false
	for _, p := range ph {
		for _, o := range p.Objects {
			if o.Kind() == "Deployment" {
				seenWorkload = true
			}
			if o.Kind() == "NetworkPolicy" && seenWorkload {
				t.Errorf("%s is applied after a workload has started", o.Key())
			}
		}
	}
	rollouts := map[string][]string{}
	for _, p := range ph {
		rollouts[p.Name] = p.Rollouts
	}
	if rollouts["api"][0] != "darkplum-api" || rollouts["gateway"][0] != "darkplum-gateway" || rollouts["web"][0] != "darkplum-web" {
		t.Errorf("rollouts = %v: api, gateway and web must each be waited for, web included", rollouts)
	}
}

func TestPlatformRendersExactlyWhatIsExpected(t *testing.T) {
	objs := all(platform(t))
	keys := strings.Join(Sorted(objs), "\n")
	for _, want := range []string{
		"Namespace//darkplum-system", "Namespace//darkplum-inference",
		"Deployment/darkplum-system/darkplum-api", "Deployment/darkplum-system/darkplum-gateway", "Deployment/darkplum-system/darkplum-web",
		"Secret/darkplum-system/darkplum-provider-keys", "ConfigMap/darkplum-system/darkplum-provider-config",
		"NetworkPolicy/darkplum-system/gateway-egress-services", "NetworkPolicy/darkplum-system/gateway-egress-database",
	} {
		if !strings.Contains(keys, want) {
			t.Errorf("missing %s in:\n%s", want, keys)
		}
	}
}

func TestEveryDarkplumContainerRunsTheMirroredDigest(t *testing.T) {
	in := inputs()
	for _, o := range all(platform(t)) {
		if o.Kind() != "Deployment" {
			continue
		}
		svc := strings.TrimPrefix(o.Name(), "darkplum-")
		found := false
		for _, c := range containers(o) {
			if c["image"] == in.Images[svc] {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not run %s", o.Key(), in.Images[svc])
		}
	}
}

func TestPatchedValuesLandOnTheirFields(t *testing.T) {
	objs := all(platform(t))
	var gw, svcPol, dbPol Object
	for _, o := range objs {
		switch o.Key() {
		case "Deployment/darkplum-system/darkplum-gateway":
			gw = o
		case "NetworkPolicy/darkplum-system/gateway-egress-services":
			svcPol = o
		case "NetworkPolicy/darkplum-system/gateway-egress-database":
			dbPol = o
		}
	}
	for name, want := range map[string]string{"BEDROCK_REGION": "us-east-1", "BEDROCK_MODEL_FILTER": "us.anthropic.claude-*",
		"VLLM_UPSTREAMS": "qwen=http://vllm-qwen.darkplum-inference:8000"} {
		if got, _ := EnvValue(gw, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for pol, cidr := range map[*Object]string{&svcPol: "172.20.0.0/16", &dbPol: "10.0.0.0/16"} {
		b, _ := json.Marshal(*pol)
		if !strings.Contains(string(b), `"cidr":"`+cidr+`"`) {
			t.Errorf("%s lacks cidr %s: %s", (*pol).Key(), cidr, b)
		}
	}
	// an empty carried-over upstream list is a valid value (no models added)
	in := inputs()
	in.VLLMUpstreams = ""
	if _, err := Platform(k8s(t), in); err != nil {
		t.Errorf("empty VLLM_UPSTREAMS: %v", err)
	}
}

func TestInputsAreValidated(t *testing.T) {
	for name, mut := range map[string]func(*Inputs){
		"tag instead of digest":     func(i *Inputs) { i.Images["api"] = "ecr/x/darkplum-api:latest" },
		"missing image":             func(i *Inputs) { delete(i.Images, "web") },
		"service cidr not a cidr":   func(i *Inputs) { i.ServiceCIDR = "172.20.0.0" },
		"vpc cidr empty":            func(i *Inputs) { i.VPCCIDR = "" },
		"vpc cidr is a placeholder": func(i *Inputs) { i.VPCCIDR = "PLACEHOLDER_VPC_CIDR" },
		"no bedrock region":         func(i *Inputs) { i.BedrockRegion = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := inputs()
			mut(&in)
			if _, err := Platform(k8s(t), in); err == nil {
				t.Error("rendered")
			}
		})
	}
}

// The assertions hold the render to the invariants; each is shown to fire.
func TestCheckCatchesWhatTheMakefileOnlyHoped(t *testing.T) {
	images := inputs().Images
	dep := func(img string) Object {
		return Object{"kind": "Deployment", "metadata": map[string]any{"name": "x", "namespace": "n"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "c", "image": img}}}}}}
	}
	for name, tc := range map[string]struct {
		obj Object
		bad bool
	}{
		"expected digest":            {dep(images["api"]), false},
		"pinned third party":         {dep("vllm/vllm-openai:v0.6.6" + dg), false},
		"mutable first party":        {dep("darkplum-api:latest"), true},
		"another registry's latest":  {dep("123.dkr.ecr/x/darkplum-api:latest"), true},
		"unpinned third party":       {dep("postgres:16-alpine"), true},
		"a placeholder survived":     {Object{"kind": "NetworkPolicy", "metadata": map[string]any{"name": "p"}, "spec": map[string]any{"cidr": "PLACEHOLDER_VPC_CIDR"}}, true},
		"provider keys with data":    {Object{"kind": "Secret", "metadata": map[string]any{"name": "darkplum-provider-keys"}, "data": map[string]any{}}, true},
		"billing keys with data":     {Object{"kind": "Secret", "metadata": map[string]any{"name": "darkplum-billing-keys"}, "stringData": map[string]any{"a": "b"}}, true},
		"provider config with data":  {Object{"kind": "ConfigMap", "metadata": map[string]any{"name": "darkplum-provider-config"}, "data": map[string]any{"a": "b"}}, true},
		"provider keys without data": {Object{"kind": "Secret", "metadata": map[string]any{"name": "darkplum-provider-keys"}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := Check([]Object{tc.obj}, images)
			if (err != nil) != tc.bad {
				t.Errorf("Check = %v, want failure=%v", err, tc.bad)
			}
		})
	}
}

// A rule that matches nothing, or twice, is an error: a renamed variable or image
// must not turn a setting into a silent no-op.
func TestPatchesMustMatchExactlyOnce(t *testing.T) {
	d := Object{"kind": "Deployment", "metadata": map[string]any{"name": "d"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{
			map[string]any{"name": "a", "image": "x:1", "env": []any{map[string]any{"name": "E", "value": "old"}, map[string]any{"name": "F", "valueFrom": map[string]any{}}}},
			map[string]any{"name": "b", "image": "x:1", "env": []any{map[string]any{"name": "E", "value": "old2"}}},
		},
		"initContainers": []any{map[string]any{"name": "i", "image": "y:1"}},
	}}}}
	if err := SetContainerImage(d, "x:1", "z"); err == nil {
		t.Error("two containers with the image: want an error")
	}
	if err := SetContainerImage(d, "nope", "z"); err == nil {
		t.Error("no container with the image: want an error")
	}
	if err := SetContainerImage(d, "y:1", "z@sha256:"); err != nil {
		t.Errorf("init container: %v", err)
	}
	if err := SetEnv(d, "E", "v"); err == nil {
		t.Error("env declared twice: want an error")
	}
	if err := SetEnv(d, "MISSING", "v"); err == nil {
		t.Error("env not declared: want an error")
	}
	if err := SetEnv(d, "F", "v"); err == nil {
		t.Error("env that is a reference: want an error")
	}
	np := Object{"kind": "NetworkPolicy", "metadata": map[string]any{"name": "p"}, "spec": map[string]any{"egress": []any{
		map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": "PLACEHOLDER_X"}}, map[string]any{"ipBlock": map[string]any{"cidr": "PLACEHOLDER_X"}}}}}}}
	if err := SetCIDR(np, "PLACEHOLDER_X", "10.0.0.0/8"); err == nil {
		t.Error("placeholder twice: want an error")
	}
	if err := SetCIDR(np, "PLACEHOLDER_Y", "10.0.0.0/8"); err == nil {
		t.Error("placeholder absent: want an error")
	}
}

// The vendor-key objects shipped in the bundle carry no data (the invariant the
// renderer enforces holds for the real files, not just the test fixtures).
func TestShippedVendorKeyObjectsHaveNoData(t *testing.T) {
	objs, err := Kustomize(k8s(t), "base/gateway")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range objs {
		if vendorKeyObjects[o.Name()] {
			n++
			if _, has := o["data"]; has {
				t.Errorf("%s has data", o.Key())
			}
		}
	}
	api, _ := Kustomize(k8s(t), "base/api")
	for _, o := range api {
		if vendorKeyObjects[o.Name()] {
			n++
		}
	}
	if n < 3 {
		t.Errorf("found %d vendor-key objects, want the provider keys, billing keys and provider config", n)
	}
}

func TestJSONListIsWhatKubectlApplyTakes(t *testing.T) {
	b, err := JSONList(all(platform(t)))
	if err != nil {
		t.Fatal(err)
	}
	var l struct {
		Kind  string `json:"kind"`
		Items []map[string]any
	}
	if err := json.Unmarshal(b, &l); err != nil || l.Kind != "List" || len(l.Items) < 10 {
		t.Fatalf("list = %s %v", l.Kind, err)
	}
}

func TestGPUPlanPatchesTheModelAndPinsEverything(t *testing.T) {
	plan, err := GPU(k8s(t), GPUInputs{ModelName: "Qwen/Qwen2.5-7B-Instruct", ModelRevision: "a09a35458c702b33eeacc393d103063234e8bc28", ServedModelName: "qwen-2.5-7b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DevicePlugin) != 1 || plan.DevicePlugin[0].Kind() != "DaemonSet" {
		t.Errorf("device plugin = %v", Sorted(plan.DevicePlugin))
	}
	var kinds []string
	for _, o := range plan.Workload {
		kinds = append(kinds, o.Kind())
	}
	if strings.Join(kinds, ",") != "ServiceAccount,Deployment,Service" {
		t.Errorf("workload kinds = %v", kinds)
	}
	b, _ := json.Marshal(plan)
	for _, want := range []string{"Qwen/Qwen2.5-7B-Instruct", "a09a35458c702b33eeacc393d103063234e8bc28", "qwen-2.5-7b"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the render lacks %q", want)
		}
	}
	if strings.Contains(string(b), "PLACEHOLDER_") {
		t.Error("an unreplaced placeholder")
	}
	d, _ := find(plan.Workload, "Deployment", "vllm-inference")
	if v, _ := EnvValue(d, "MODEL_NAME"); v != "Qwen/Qwen2.5-7B-Instruct" {
		t.Errorf("MODEL_NAME = %q", v)
	}
}

func TestGPUPlanRefusesBadInputs(t *testing.T) {
	good := GPUInputs{ModelName: "Qwen/Qwen2.5-7B-Instruct", ModelRevision: "main", ServedModelName: "qwen"}
	for name, mut := range map[string]func(*GPUInputs){
		"empty model":        func(i *GPUInputs) { i.ModelName = "" },
		"model with a space": func(i *GPUInputs) { i.ModelName = "a b" },
		"yaml in revision":   func(i *GPUInputs) { i.ModelRevision = "main\n- --trust-remote-code" },
		"served with caps":   func(i *GPUInputs) { i.ServedModelName = "Qwen" },
		"flag as served":     func(i *GPUInputs) { i.ServedModelName = "--trust-remote-code" },
	} {
		in := good
		mut(&in)
		if _, err := GPU(k8s(t), in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := GPU(k8s(t), good); err != nil {
		t.Fatal(err)
	}
}
