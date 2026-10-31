package commands

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestOIDCConfigured(t *testing.T) {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := map[string]bool{
		"":                                 false,
		enc("unconfigured"):                false,
		enc("https://accounts.google.com"): true,
		"not base64!":                      false,
	}
	for in, want := range cases {
		if got := oidcConfigured(in); got != want {
			t.Errorf("oidcConfigured(%q) = %v, want %v", in, got, want)
		}
	}
}

// ---- the sections status gained from the Makefile ------------------------------

type statusCluster struct {
	*fakeCluster
	release   string // darkplum-install ConfigMap JSON, "" = absent
	releaseEr error
	deploys   string
	pods      map[string]string
	nodes     string
	gpuNodes  string
	vllmLog   string // "" = the vllm container has no log yet
	dlLog     string
	hasVLLM   bool
}

func newStatusCluster() *statusCluster {
	s := &statusCluster{fakeCluster: newFakeCluster(), deploys: `{"items":[]}`,
		pods: map[string]string{"darkplum-system": "darkplum-api-1 1/1 Running 0 1m\n", "darkplum-inference": ""}}
	s.extra = s.handle
	return s
}

func (s *statusCluster) handle(c call) (string, bool, error) {
	line := c.line()
	switch {
	case strings.Contains(line, "get configmap darkplum-install"):
		if s.releaseEr != nil {
			return "", true, s.releaseEr
		}
		if s.release == "" {
			return "", true, notFound("configmaps", "darkplum-install")
		}
		return s.release, true, nil
	case strings.Contains(line, "get secret darkplum-auth"):
		return "", true, nil
	case strings.HasSuffix(line, "get deploy -o json"):
		return s.deploys, true, nil
	case strings.Contains(line, "get pods --no-headers"):
		for ns, out := range s.pods {
			if strings.Contains(line, "-n "+ns+" ") {
				return out, true, nil
			}
		}
	case strings.Contains(line, "get deploy vllm-inference -o name"):
		if s.hasVLLM {
			return "deployment.apps/vllm-inference", true, nil
		}
		return "", true, notFound("deployments", "vllm-inference")
	case strings.HasSuffix(line, "get nodes -L darkplum.ai/node-type"):
		return s.nodes, true, nil
	case strings.Contains(line, "get nodes -l darkplum.ai/node-type=gpu-inference"):
		return s.gpuNodes, true, nil
	case strings.Contains(line, "logs deploy/vllm-inference -c vllm"):
		if s.vllmLog == "" {
			return "", true, errors.New("container vllm is waiting to start")
		}
		return s.vllmLog, true, nil
	case strings.Contains(line, "logs deploy/vllm-inference -c model-download"):
		if s.dlLog == "" {
			return "", true, errors.New("no logs")
		}
		return s.dlLog, true, nil
	}
	return "", false, nil
}

func (s *statusCluster) run(tf *tfvarsInfo) (string, error) {
	var out bytes.Buffer
	e := &statusEnv{out: &out, kc: s.client(), ns: testNS, tf: tf}
	err := e.run()
	return out.String(), err
}

func deployList(annotations map[string]map[string]string) string {
	var items []string
	for name, ann := range annotations {
		a, _ := json.Marshal(ann)
		items = append(items, fmt.Sprintf(`{"metadata":{"name":%q,"annotations":%s}}`, name, a))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func TestStatusUnsignedImagesAreAFailure(t *testing.T) {
	s := newStatusCluster()
	s.deploys = deployList(map[string]map[string]string{
		"darkplum-gateway": {"darkplum.ai/image-trust": "unsigned"},
		"darkplum-api":     {"darkplum.ai/image-trust": "verified"},
		"postgres":         {},
	})
	out, err := s.run(nil)
	if err == nil || !IsReported(err) {
		t.Fatalf("pods are ready but unsigned images must fail the status: %v", err)
	}
	if !strings.Contains(out, "image-trust: unsigned") || !strings.Contains(out, "darkplum-gateway") ||
		strings.Contains(out, "all pods ready") || strings.Contains(out, "verified release images") {
		t.Errorf("out:\n%s", out)
	}
}

func TestStatusVerifiedReleaseAndRC(t *testing.T) {
	s := newStatusCluster()
	s.release = `{"data":{"version":"v1.4.0","channel":"release","commit":"0123456789abcdef0123","time":"2026-10-31T12:00:00Z"}}`
	s.deploys = deployList(map[string]map[string]string{
		"darkplum-gateway": {"darkplum.ai/image-trust": "verified"},
		"darkplum-api":     {"darkplum.ai/image-trust": "verified"},
	})
	out, err := s.run(nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"v1.4.0", "channel release", "commit 0123456789ab", "deployed 2026-10-31T12:00:00Z", "verified release images", "all pods ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0123456789abcdef0123") {
		t.Error("the commit is not shortened")
	}
	s.deploys = deployList(map[string]map[string]string{
		"darkplum-gateway": {"darkplum.ai/image-trust": "verified", "darkplum.ai/release-channel": "rc"}})
	out, err = s.run(nil)
	if err != nil || !strings.Contains(out, "channel rc: a prerelease build") {
		t.Errorf("rc must warn without failing: %v\n%s", err, out)
	}
}

func TestStatusOldInstallWithoutRecords(t *testing.T) {
	s := newStatusCluster() // no ConfigMap, no annotations
	s.deploys = deployList(map[string]map[string]string{"darkplum-api": {}})
	out, err := s.run(nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "release  ") || !strings.Contains(out, "image trust not recorded") {
		t.Errorf("out:\n%s", out)
	}
	// a ConfigMap that cannot be read is shown, not hidden
	s.releaseEr = errors.New("Unauthorized")
	if out, _ := s.run(nil); !strings.Contains(out, "unavailable: Unauthorized") {
		t.Errorf("out:\n%s", out)
	}
}

func TestStatusWebUIAndBedrockOnly(t *testing.T) {
	s := newStatusCluster()
	tf := &tfvarsInfo{GPUDesired: 0, Raw: parseTFVars("web_domain = \"acme.example\"\nweb_hostname = \"ai.acme.example\"\n")}
	out, err := s.run(tf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "https://ai.acme.example") || !strings.Contains(out, "none (Bedrock-only install)") {
		t.Errorf("out:\n%s", out)
	}
	for _, l := range s.lines() {
		if strings.Contains(l, "get nodes") || strings.Contains(l, "logs deploy/vllm") {
			t.Errorf("a Bedrock-only status read GPU state: %s", l)
		}
	}
	out, _ = s.run(&tfvarsInfo{GPUDesired: 0, Raw: map[string]string{}})
	if !strings.Contains(out, "cluster-internal; `darkplum configure` opens a tunnel") {
		t.Errorf("out:\n%s", out)
	}
}

func TestStatusGPUInstall(t *testing.T) {
	s := newStatusCluster()
	s.nodes = "NAME  STATUS  NODE-TYPE\nip-1  Ready   gpu-inference\n"
	s.gpuNodes = "NAME   GPU   STATUS\nip-1   1     Ready\n"
	s.dlLog = "downloading shard 2/4"
	out, err := s.run(&tfvarsInfo{GPUDesired: 1, Raw: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ip-1  Ready   gpu-inference", "ip-1   1     Ready", "downloading shard 2/4"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	// the vllm container's log wins when it has one
	s.vllmLog = "Uvicorn running"
	if out, _ := s.run(&tfvarsInfo{GPUDesired: 1, Raw: map[string]string{}}); !strings.Contains(out, "Uvicorn running") || strings.Contains(out, "downloading shard") {
		t.Errorf("out:\n%s", out)
	}
	// nothing yet
	s.vllmLog, s.dlLog, s.gpuNodes = "", "", ""
	out, _ = s.run(&tfvarsInfo{GPUDesired: 1, Raw: map[string]string{}})
	if !strings.Contains(out, "No GPU nodes found.") || !strings.Contains(out, "No vLLM pods running yet.") {
		t.Errorf("out:\n%s", out)
	}
}

// Without an install record (--context) the GPU section follows the live cluster.
func TestStatusWithoutSettingsReadsTheLiveCluster(t *testing.T) {
	s := newStatusCluster()
	out, _ := s.run(nil)
	if !strings.Contains(out, "none (Bedrock-only install)") {
		t.Errorf("out:\n%s", out)
	}
	s.hasVLLM = true
	s.dlLog = "x"
	if out, _ := s.run(nil); strings.Contains(out, "Bedrock-only") || !strings.Contains(out, "▸ Nodes") {
		t.Errorf("out:\n%s", out)
	}
}

func TestStatusNotReadyPodStillFails(t *testing.T) {
	s := newStatusCluster()
	s.pods["darkplum-system"] = "darkplum-api-1 0/1 CrashLoopBackOff 3 1m\n"
	if _, err := s.run(nil); err == nil || IsReported(err) {
		t.Errorf("err = %v", err)
	}
}
