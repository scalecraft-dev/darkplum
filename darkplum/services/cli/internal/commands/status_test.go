package commands

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/release"
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

// dep is a Deployment as status reads it: annotations and the image of its container.
type dep struct {
	ann   map[string]string
	image string
}

func deployList(deps map[string]dep) string {
	var items []string
	for name, d := range deps {
		a, _ := json.Marshal(d.ann)
		items = append(items, fmt.Sprintf(`{"metadata":{"name":%q,"annotations":%s},"spec":{"template":{"spec":{"containers":[{"image":%q}]}}}}`, name, a, d.image))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

const (
	ecr     = "123456789012.dkr.ecr.us-east-2.amazonaws.com"
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func img(svc, digest string) string { return ecr + "/acme/darkplum-" + svc + "@" + digest }

// releaseProof is the proof a build of v1.4.0 has for an install named acme.
func releaseProof() imageProof {
	want := map[string]string{}
	for _, svc := range []string{"gateway", "api", "web"} {
		want[svc] = img(svc, digestA)
	}
	return imageProof{want: want, version: "v1.4.0"}
}

func (s *statusCluster) runWith(proof imageProof, recorded string) (string, error) {
	var out bytes.Buffer
	e := &statusEnv{out: &out, kc: s.client(), ns: testNS, proof: proof, recorded: recorded}
	err := e.run()
	return out.String(), err
}

func TestStatusUnsignedImagesAreAFailure(t *testing.T) {
	s := newStatusCluster()
	s.deploys = deployList(map[string]dep{
		"darkplum-gateway": {ann: map[string]string{"darkplum.ai/image-trust": "unsigned"}, image: img("gateway", digestB)},
		"darkplum-api":     {ann: map[string]string{"darkplum.ai/image-trust": "verified"}, image: img("api", digestA)},
		"postgres":         {},
	})
	out, err := s.run(nil)
	if err == nil || !IsReported(err) {
		t.Fatalf("pods are ready but unsigned images must fail the status: %v", err)
	}
	if !strings.Contains(out, "image-trust: unsigned") || !strings.Contains(out, "darkplum-gateway") ||
		strings.Contains(out, "all pods ready") || strings.Contains(out, "✓ verified") {
		t.Errorf("out:\n%s", out)
	}
}

// Verified means the running digest is the release's digest. The annotation, which
// anyone who can patch the Deployment can write, proves nothing.
func TestStatusVerifiedNeedsADigestMatch(t *testing.T) {
	good := map[string]dep{}
	for _, svc := range []string{"gateway", "api", "web"} {
		good["darkplum-"+svc] = dep{ann: map[string]string{"darkplum.ai/image-trust": "verified"}, image: img(svc, digestA)}
	}
	s := newStatusCluster()
	s.release = `{"data":{"version":"v1.4.0","channel":"release","commit":"0123456789abcdef0123","time":"2026-10-31T12:00:00Z"}}`
	s.deploys = deployList(good)
	out, err := s.runWith(releaseProof(), "")
	if err != nil || !strings.Contains(out, "✓ verified: running the images v1.4.0 signed (digests match)") {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"v1.4.0", "channel release", "commit 0123456789ab", "deployed 2026-10-31T12:00:00Z", "all pods ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}

	// the same annotations over a different image: not verified
	forged := map[string]dep{}
	for k, v := range good {
		forged[k] = v
	}
	forged["darkplum-gateway"] = dep{ann: map[string]string{"darkplum.ai/image-trust": "verified"}, image: img("gateway", digestB)}
	s.deploys = deployList(forged)
	out, err = s.runWith(releaseProof(), "v1.4.0")
	if err == nil || !IsReported(err) || !strings.Contains(out, "not this release's images") || strings.Contains(out, "✓ verified") {
		t.Errorf("an install that claims v1.4.0 but runs other bytes must fail:\n%v\n%s", err, out)
	}
	// the right digest in another registry or repository is not it either
	other := map[string]dep{}
	for k, v := range good {
		other[k] = v
	}
	other["darkplum-api"] = dep{image: "ghcr.io/evil/darkplum-api@" + digestA}
	s.deploys = deployList(other)
	if out, _ := s.runWith(releaseProof(), "v1.4.0"); strings.Contains(out, "✓ verified") || !strings.Contains(out, "darkplum-api") {
		t.Errorf("a foreign registry passed:\n%s", out)
	}
	// an older release is running: not verified by this build, but not a tamper alarm
	s.deploys = deployList(forged)
	s.release = `{"data":{"version":"v1.3.0"}}`
	out, err = s.runWith(releaseProof(), "")
	if err != nil || !strings.Contains(out, "did not verify") || strings.Contains(out, "✓ verified") {
		t.Errorf("%v\n%s", err, out)
	}
	// rc
	good["darkplum-gateway"] = dep{ann: map[string]string{"darkplum.ai/image-trust": "verified", "darkplum.ai/release-channel": "rc"}, image: img("gateway", digestA)}
	s.deploys = deployList(good)
	if out, err := s.runWith(releaseProof(), ""); err != nil || !strings.Contains(out, "channel rc: a prerelease build") {
		t.Errorf("rc must warn without failing: %v\n%s", err, out)
	}
}

// "verified" is about every first-party image of the release: two of three
// running is a warning, never the green line.
func TestStatusVerifiedNeedsEveryImage(t *testing.T) {
	part := map[string]dep{}
	for _, svc := range []string{"gateway", "api"} {
		part["darkplum-"+svc] = dep{image: img(svc, digestA)}
	}
	s := newStatusCluster()
	s.deploys = deployList(part)
	out, err := s.runWith(releaseProof(), "v1.4.0")
	if err != nil || strings.Contains(out, "✓ verified") || !strings.Contains(out, "only 2 of 3 first-party images are confirmed") {
		t.Errorf("%v\n%s", err, out)
	}
}

// A development build verified nothing, so it confirms nothing.
func TestStatusDevBuildConfirmsNothing(t *testing.T) {
	s := newStatusCluster()
	s.deploys = deployList(map[string]dep{"darkplum-api": {ann: map[string]string{"darkplum.ai/image-trust": "verified"}, image: img("api", digestA)}})
	out, err := s.run(nil)
	if err != nil || !strings.Contains(out, "development build: it cannot confirm which images run") || strings.Contains(out, "✓ verified") {
		t.Errorf("%v\n%s", err, out)
	}
}

func TestStatusOldInstallWithoutRecords(t *testing.T) {
	s := newStatusCluster() // no ConfigMap, no annotations
	s.deploys = deployList(map[string]dep{"darkplum-api": {image: img("api", digestA)}})
	out, err := s.run(nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "release  ") || strings.Contains(out, "✓ verified") {
		t.Errorf("out:\n%s", out)
	}
	// a ConfigMap that cannot be read is shown, not hidden
	s.releaseEr = errors.New("Unauthorized")
	if out, _ := s.run(nil); !strings.Contains(out, "unavailable: Unauthorized") {
		t.Errorf("out:\n%s", out)
	}
}

func TestWantFromRecord(t *testing.T) {
	m := release.Manifest{Images: map[string]release.Image{"api": {Digest: digestA}}}
	rec := map[string]string{"image-api": img("api", digestB)} // the record says where, the build says which
	if got := wantFromRecord(m, rec)["api"]; got != img("api", digestA) {
		t.Errorf("got %s", got)
	}
	for name, bad := range map[string]map[string]string{
		"no record":         nil,
		"no digest in ref":  {"image-api": ecr + "/acme/darkplum-api"},
		"another service's": {"image-api": img("web", digestB)},
	} {
		if wantFromRecord(m, bad) != nil {
			t.Errorf("%s confirmed something", name)
		}
	}
	if wantFromRecord(release.Manifest{}, rec) != nil {
		t.Error("a build with no images wants some")
	}
}

// The registry comes from the install's record, so an install with several
// accounts (adopted) or a registry the CLI would not guess is still confirmed.
func TestStatusReadsTheRegistryFromTheRecord(t *testing.T) {
	m := release.Manifest{Version: "v1.4.0", Images: map[string]release.Image{}}
	good := map[string]dep{}
	data := map[string]string{"version": "v1.4.0"}
	for _, svc := range []string{"gateway", "api", "web"} {
		m.Images[svc] = release.Image{Digest: digestA}
		data["image-"+svc] = "999999999999.dkr.ecr.eu-west-1.amazonaws.com/acme/darkplum-" + svc + "@" + digestA
		good["darkplum-"+svc] = dep{image: data["image-"+svc]}
	}
	rec, _ := json.Marshal(map[string]any{"data": data})
	s := newStatusCluster()
	s.release, s.deploys = string(rec), deployList(good)
	var out bytes.Buffer
	e := &statusEnv{out: &out, kc: s.client(), ns: testNS, build: &m, proof: imageProof{version: "v1.4.0"}}
	if err := e.run(); err != nil || !strings.Contains(out.String(), "✓ verified") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// no record of where the images went: nothing is confirmed, and it says why
	s.release = ""
	out.Reset()
	e = &statusEnv{out: &out, kc: s.client(), ns: testNS, build: &m, proof: imageProof{version: "v1.4.0"}}
	if err := e.run(); err != nil || strings.Contains(out.String(), "✓ verified") || !strings.Contains(out.String(), "no record of where its images") {
		t.Errorf("%v\n%s", err, out.String())
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
