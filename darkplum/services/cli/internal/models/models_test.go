package models

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	darkplum "github.com/scalecraft-dev/darkplum"
	"io/fs"
)

func realK8s(t *testing.T) fs.FS {
	t.Helper()
	k, err := fs.Sub(darkplum.Tree(), "k8s")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestValidate(t *testing.T) {
	good := Spec{Name: "qwen", HF: "Qwen/Qwen2.5-7B-Instruct"}
	if s, err := good.Normalize(); err != nil || s.Revision != "main" || s.Served != "qwen" || s.MaxLen != 8192 {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	bad := map[string]Spec{
		"reserved default":   {Name: "default", HF: "a/b"},
		"reserved inference": {Name: "inference", HF: "a/b"},
		"upper case":         {Name: "Qwen", HF: "a/b"},
		"too long":           {Name: strings.Repeat("a", 32), HF: "a/b"},
		"leading dash":       {Name: "-a", HF: "a/b"},
		"trailing dash":      {Name: "a-", HF: "a/b"},
		"no model":           {Name: "q"},
		"model no slash":     {Name: "q", HF: "justname"},
		"model with sed":     {Name: "q", HF: "a/b|c"},
		"model with space":   {Name: "q", HF: "a/b c"},
		"revision injection": {Name: "q", HF: "a/b", Revision: "main; rm"},
		"served injection":   {Name: "q", HF: "a/b", Served: "x\ny: z"},
		"negative len":       {Name: "q", HF: "a/b", MaxLen: -5},
	}
	for name, s := range bad {
		if _, err := s.Normalize(); err == nil {
			t.Errorf("%s accepted: %+v", name, s)
		}
	}
	if _, err := (Spec{Name: strings.Repeat("a", 31), HF: "a/b", Served: "org/model_1.5", Revision: "0123abc"}).Normalize(); err != nil {
		t.Error(err)
	}
}

func TestRenderRealTemplate(t *testing.T) {
	objs, err := Render(realK8s(t), Spec{Name: "qwen", HF: "Qwen/Qwen2.5-7B-Instruct", Revision: "abc123", Served: "qwen-2.5-7b", MaxLen: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Name() != "vllm-qwen" || objs[1].Name() != "vllm-qwen" {
		t.Fatalf("objs = %v", objs)
	}
	b, _ := json.Marshal(objs)
	s := string(b)
	for _, want := range []string{`"Qwen/Qwen2.5-7B-Instruct"`, `"abc123"`, `"qwen-2.5-7b"`, `"4096"`, `"darkplum.ai/model-instance":"qwen"`} {
		if !strings.Contains(s, want) {
			t.Errorf("render lacks %s", want)
		}
	}
	for _, bad := range []string{"PLACEHOLDER_", "MODEL_INSTANCE"} {
		if strings.Contains(s, bad) {
			t.Errorf("render still contains %s", bad)
		}
	}
	// invariants carried by the template that must survive rendering
	svc := objs[1]["spec"].(map[string]any)
	if svc["clusterIP"] != "None" {
		t.Error("service is not headless")
	}
	dep := objs[0]["spec"].(map[string]any)
	if dep["strategy"].(map[string]any)["type"] != "Recreate" {
		t.Error("deployment lost its Recreate strategy")
	}
}

// A value that looks like another placeholder is data, not a token.
func TestRenderSubstitutesOncePerString(t *testing.T) {
	k8s := fstest.MapFS{TemplatePath: {Data: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-MODEL_INSTANCE
spec:
  a: PLACEHOLDER_MODEL_NAME
  b: PLACEHOLDER_MODEL_REVISION
  c: PLACEHOLDER_SERVED_MODEL_NAME
  d: PLACEHOLDER_MAX_MODEL_LEN
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-MODEL_INSTANCE
spec:
  clusterIP: None
`)}}
	objs, err := Render(k8s, Spec{Name: "x", HF: "a/PLACEHOLDER_MAX_MODEL_LEN", Revision: "MODEL_INSTANCE"})
	if err != nil {
		t.Fatal(err)
	}
	sp := objs[0]["spec"].(map[string]any)
	if sp["a"] != "a/PLACEHOLDER_MAX_MODEL_LEN" || sp["b"] != "MODEL_INSTANCE" || sp["d"] != "8192" {
		t.Errorf("spec = %v", sp)
	}
}

func TestRenderRefusesATemplateThatChanged(t *testing.T) {
	for name, tpl := range map[string]string{
		"missing token": "apiVersion: v1\nkind: Service\nmetadata:\n  name: vllm-MODEL_INSTANCE\nspec:\n  clusterIP: None\n",
		"not headless": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-MODEL_INSTANCE
spec: {a: PLACEHOLDER_MODEL_NAME, b: PLACEHOLDER_MODEL_REVISION, c: PLACEHOLDER_SERVED_MODEL_NAME, d: PLACEHOLDER_MAX_MODEL_LEN}
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-MODEL_INSTANCE
spec: {clusterIP: 10.0.0.1}
`,
	} {
		if _, err := Render(fstest.MapFS{TemplatePath: {Data: []byte(tpl)}}, Spec{Name: "x", HF: "a/b"}); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
	if _, err := Render(fstest.MapFS{}, Spec{Name: "x", HF: "a/b"}); err == nil {
		t.Error("missing template rendered")
	}
}

func TestUpstreams(t *testing.T) {
	u := func(n string) string { return n + "=" + UpstreamURL(n) }
	cases := []struct{ name, cur, add, remove string }{
		{"empty", "", u("qwen"), ""},
		{"trailing comma", u("a") + ",", u("a") + "," + u("qwen"), u("a")},
		{"duplicate replaced and moved last", u("qwen") + "," + u("a"), u("a") + "," + u("qwen"), u("a")},
		{"prefix collision", u("qwen2"), u("qwen2") + "," + u("qwen"), u("qwen2")},
		{"empties dropped", ",," + u("a") + ",,", u("a") + "," + u("qwen"), u("a")},
	}
	for _, c := range cases {
		if got := AddUpstream(c.cur, "qwen"); got != c.add {
			t.Errorf("%s: Add = %q, want %q", c.name, got, c.add)
		}
		if got := RemoveUpstream(c.cur, "qwen"); got != c.remove {
			t.Errorf("%s: Remove = %q, want %q", c.name, got, c.remove)
		}
	}
	if AddUpstream(AddUpstream("", "q"), "q") != AddUpstream("", "q") {
		t.Error("Add is not idempotent")
	}
	if UpstreamURL("qwen") != "http://vllm-qwen.darkplum-inference.svc.cluster.local:8000" {
		t.Error(UpstreamURL("qwen"))
	}
}

func TestGPUs(t *testing.T) {
	nodes := `{"items":[{"status":{"allocatable":{"nvidia.com/gpu":"1"}}},{"status":{"allocatable":{"cpu":"4"}}},{"status":{"allocatable":{"nvidia.com/gpu":"2"}}}]}`
	pods := `{"items":[
	 {"status":{"phase":"Running"},"spec":{"containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}},{"resources":{}}]}},
	 {"status":{"phase":"Pending"},"spec":{"containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]}},
	 {"status":{"phase":"Succeeded"},"spec":{"containers":[{"resources":{"requests":{"nvidia.com/gpu":"1"}}}]}}]}`
	total, used, err := GPUs([]byte(nodes), []byte(pods))
	if err != nil || total != 3 || used != 2 {
		t.Errorf("GPUs = %d %d %v", total, used, err)
	}
	if _, _, err := GPUs([]byte(`x`), []byte(pods)); err == nil {
		t.Error("bad nodes accepted")
	}
	if total, used, err := GPUs([]byte(`{"items":[]}`), []byte(`{"items":[]}`)); err != nil || total != 0 || used != 0 {
		t.Error("empty")
	}
}

func TestServedName(t *testing.T) {
	if ServedName([]string{"--model", "/m", "--served-model-name", "x", "--port", "8000"}) != "x" ||
		ServedName([]string{"--served-model-name=y"}) != "y" || ServedName([]string{"--model"}) != "" {
		t.Error("ServedName")
	}
}

func TestRenderRefusesAnUnfilledPlaceholder(t *testing.T) {
	tpl := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: vllm-MODEL_INSTANCE\nspec: {a: PLACEHOLDER_MODEL_NAME, b: PLACEHOLDER_MODEL_REVISION, c: PLACEHOLDER_SERVED_MODEL_NAME, d: PLACEHOLDER_MAX_MODEL_LEN, e: PLACEHOLDER_GPU_COUNT}\n---\napiVersion: v1\nkind: Service\nmetadata:\n  name: vllm-MODEL_INSTANCE\nspec: {clusterIP: None}\n"
	if _, err := Render(fstest.MapFS{TemplatePath: {Data: []byte(tpl)}}, Spec{Name: "x", HF: "a/b"}); err == nil || !strings.Contains(err.Error(), "PLACEHOLDER_GPU_COUNT") {
		t.Errorf("err = %v", err)
	}
}
