package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	darkplum "github.com/scalecraft-dev/darkplum"
	"github.com/scalecraft-dev/darkplum/cli/internal/iamcheck"
	"github.com/scalecraft-dev/darkplum/cli/internal/install"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/mirror"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/verify"
)

// fakeTF is Terraform: it records every command on the shared timeline, answers
// from a script, and flips the world (the cluster now exists, outputs now exist)
// when an apply runs.
type fakeTF struct {
	t  *testing.T
	tl *timeline

	mu          sync.Mutex
	stateOut    string
	stateErr    error
	planJSON    string
	showErr     error
	outputs     string
	outputsErr  error
	initErr     error
	importErr   error
	rmErr       error
	streamErr   map[string]error // by first arg
	streamText  map[string]string
	streamDelay time.Duration
	applied     bool
	onApply     func()
	cmds        [][]string
}

func (f *fakeTF) record(args ...string) {
	f.mu.Lock()
	f.cmds = append(f.cmds, args)
	f.mu.Unlock()
	f.tl.add("terraform " + strings.Join(args, " "))
}

func (f *fakeTF) VarFile() string { return "/ws/terraform.tfvars" }

func (f *fakeTF) Init() error { f.record("init"); return f.initErr }

func (f *fakeTF) Capture(args ...string) ([]byte, error) {
	f.record(args...)
	switch args[0] {
	case "state":
		if len(args) > 1 && args[1] == "rm" {
			return nil, f.rmErr
		}
		if f.stateErr != nil {
			return nil, f.stateErr
		}
		return []byte(f.stateOut), nil
	case "show":
		if f.showErr != nil {
			return nil, f.showErr
		}
		if f.planJSON == "" {
			return []byte(`{"resource_changes":[{"address":"module.vpc.aws_vpc.this","change":{"actions":["create"]}}]}`), nil
		}
		return []byte(f.planJSON), nil
	case "import":
		return nil, f.importErr
	case "output":
		f.mu.Lock()
		applied := f.applied
		f.mu.Unlock()
		if !applied && f.outputs != "" && !strings.Contains(f.outputs, "ALWAYS") {
			// On a fresh install the outputs do not exist until the apply has run.
			return []byte("{}"), nil
		}
		if f.outputsErr != nil {
			return nil, f.outputsErr
		}
		return []byte(f.outputs), nil
	}
	return nil, fmt.Errorf("unexpected terraform %v", args)
}

func (f *fakeTF) Stream(ctx context.Context, w io.Writer, args ...string) error {
	f.record(args...)
	if txt := f.streamText[args[0]]; txt != "" {
		fmt.Fprintln(w, txt)
	}
	if f.streamDelay > 0 {
		select {
		case <-time.After(f.streamDelay):
		case <-ctx.Done():
			return &tfFailure{Cmd: args[0], Err: ctx.Err()}
		}
	}
	if err := f.streamErr[args[0]]; err != nil {
		return &tfFailure{Cmd: args[0], Err: err}
	}
	if args[0] == "apply" {
		f.mu.Lock()
		f.applied = true
		f.mu.Unlock()
		if f.onApply != nil {
			f.onApply()
		}
	}
	return nil
}

func (f *fakeTF) K8s() (fs.FS, error) { return fs.Sub(darkplum.Tree(), "k8s") }

func (f *fakeTF) ran(first string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if c[0] == first {
			return true
		}
	}
	return false
}

// deployAWS answers the aws calls deploy makes beyond describe-cluster and
// get-caller-identity (which fakeAWS answers).
type deployAWS struct {
	base      *fakeAWS
	tl        *timeline
	logGroups string
	logErr    error
}

func (a *deployAWS) run(name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "eks describe-cluster") || strings.Contains(joined, "sts get-caller-identity") {
		a.tl.add("aws " + joined)
		return a.base.run(name, args...)
	}
	a.tl.add("aws " + joined)
	if strings.Contains(joined, "logs describe-log-groups") {
		return a.logGroups, a.logErr
	}
	return "", fmt.Errorf("unexpected aws %s", joined)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

const (
	dpAccount  = "123456789012"
	dpRegistry = "123456789012.dkr.ecr.us-east-2.amazonaws.com"
)

func digestFor(i int) string { return "sha256:" + strings.Repeat(fmt.Sprintf("%x", i+10), 64)[:64] }

func outputsJSON(gpu int, mut func(map[string]any)) string {
	v := map[string]any{
		"cluster_name": "dp-trial", "provider_region": "us-east-2", "vpc_cidr": "10.0.0.0/16", "cluster_service_cidr": "172.20.0.0/16",
		"database_url": "postgres://darkplum:s3cr3t-db-pw@db.example:5432/darkplum", "inference_region": "us-east-2",
		"bedrock_model_filter": "us.anthropic.claude-*", "gpu_desired_size": gpu,
		"image_repositories": map[string]string{
			"gateway": dpRegistry + "/dp-trial/darkplum-gateway", "api": dpRegistry + "/dp-trial/darkplum-api", "web": dpRegistry + "/dp-trial/darkplum-web"},
		"gateway_role_arn":    "arn:aws:iam::123456789012:role/dp-trial-gateway",
		"bedrock_invoke_arns": []string{"arn:aws:bedrock:us-east-2:123456789012:inference-profile/us.anthropic.claude-*"},
	}
	if gpu > 0 {
		v["model_name"], v["model_revision"], v["served_model_name"] = "Qwen/Qwen2.5-7B-Instruct", "main", "qwen-2.5-7b"
	}
	if mut != nil {
		mut(v)
	}
	out := map[string]any{}
	for k, val := range v {
		out[k] = map[string]any{"value": val, "sensitive": k == "database_url"}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

type deployHarness struct {
	t     *testing.T
	env   *deployEnv
	out   *bytes.Buffer
	tl    *timeline
	tf    *fakeTF
	k     *scriptKube
	awsf  *deployAWS
	clock *fakeClock
	in    install.Install

	manifest release.Manifest
	images   map[string]string // service -> mirrored ref
	// imageID reported by the container runtime per deployment; defaults to the mirrored digest.
	imageID    map[string]string
	nodeArch   string
	iamRep     iamcheck.Report
	iamErr     error
	checks     []checkResult
	session    checkResult
	mirrorErr  error
	platforms  []string
	resolveErr error
	unsigned   bool
	gpuNode    func() bool
}

func releaseManifest() release.Manifest {
	m := release.Manifest{Schema: 1, Channel: release.Release, Version: "1.2.3", Commit: strings.Repeat("a", 40),
		Repository: "scalecraft-dev/darkplum", Ref: "refs/tags/v1.2.3", Workflow: ".github/workflows/release.yml", RunID: "42", Images: map[string]release.Image{}}
	for i, svc := range release.Services {
		m.Images[svc] = release.Image{Repository: "ghcr.io/scalecraft-dev/darkplum-" + svc, Digest: digestFor(i), RunAttempt: "1"}
	}
	return m
}

func newDeployHarness(t *testing.T, gpu int) *deployHarness {
	t.Helper()
	in := testInstall(t, nil)
	if err := in.WriteFile("terraform.tfvars", []byte(pinnedTFVars+fmt.Sprintf("gpu_desired_size = %d\n", gpu))); err != nil {
		t.Fatal(err)
	}
	rec, err := in.Load()
	if err != nil {
		t.Fatal(err)
	}
	tfv, err := readTFVars(in.TFVars())
	if err != nil {
		t.Fatal(err)
	}
	h := &deployHarness{t: t, in: in, out: &bytes.Buffer{}, tl: &timeline{}, manifest: releaseManifest(),
		images: map[string]string{}, imageID: map[string]string{}, nodeArch: "amd64",
		iamRep:    iamcheck.Report{Checks: []iamcheck.Check{{Resource: "r", Expect: "implicitDeny", Got: "implicitDeny", Pass: true}}},
		session:   checkResult{name: "session lifetime", status: checkPass},
		platforms: []string{"linux/amd64", "linux/arm64"},
		clock:     &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	for i, svc := range release.Services {
		h.images[svc] = dpRegistry + "/dp-trial/darkplum-" + svc + "@" + digestFor(i)
	}
	h.tf = &fakeTF{t: t, tl: h.tl, outputs: outputsJSON(gpu, nil), streamErr: map[string]error{}, streamText: map[string]string{}}
	base := &fakeAWS{arn: testARN, caller: dpAccount, describeErr: errors.New("ResourceNotFoundException: no such cluster")}
	h.tf.onApply = func() { base.describeErr = nil }
	h.awsf = &deployAWS{base: base, tl: h.tl, logGroups: "0"}
	h.k = newScriptKube(t)
	h.k.tl = h.tl
	h.gpuNode = func() bool { return gpu > 0 }
	h.script(gpu)

	h.env = &deployEnv{
		out: newLockedWriter(h.out), logPath: filepath.Join(in.LogsDir(), "test.log"),
		in: in, rec: rec, tfv: tfv, manifest: h.manifest,
		now: h.clock.now, sleep: h.clock.sleep, heartbeat: time.Hour, poll: time.Second,
		interactive: false, confirm: func(string) bool { return false },
		aws:       h.awsf.run,
		checks:    func(int) []checkResult { return h.checks },
		sessionOK: func(int) checkResult { return h.session },
		openTF:    func() (tfRunner, string, error) { return h.tf, "", nil },
		cluster:   func() (clusterTarget, checkResult, error) { return installCluster(in, h.awsf.run) },
		kube:      func(clusterTarget) *kube.Client { return h.k.client(platformNS) },
		resolve: func(context.Context) (verify.Verified, error) {
			h.tl.add("verify images")
			if h.resolveErr != nil {
				return verify.Verified{}, h.resolveErr
			}
			if h.unsigned {
				m := h.manifest
				m.Channel = release.Dev
				v, _ := verify.Unverified(m)
				return v, nil
			}
			return verify.ForTest(h.manifest), nil
		},
		platforms: func(context.Context, release.Image) ([]string, error) { return h.platforms, nil },
		mirror: func(_ context.Context, v verify.Verified, repos map[string]string, region string) ([]mirror.Mirrored, error) {
			h.tl.add("mirror images " + region)
			if h.mirrorErr != nil {
				return nil, h.mirrorErr
			}
			var out []mirror.Mirrored
			for _, svc := range release.Services {
				if repos[svc] == "" {
					t.Errorf("mirror given no repository for %s", svc)
				}
				out = append(out, mirror.Mirrored{Service: svc, Ref: h.images[svc], Copied: true})
			}
			return out, nil
		},
		iam: func(context.Context, iamcheck.Outputs) (iamcheck.Report, error) {
			h.tl.add("iam simulate")
			return h.iamRep, h.iamErr
		},
	}
	h.env.flags.GPUWait = 5 * time.Minute
	return h
}

// script gives the cluster the answers of a healthy, empty one.
func (h *deployHarness) script(gpu int) {
	k := h.k
	k.onFn(`node-type=gpu-inference -o jsonpath`, func([]string, []byte) ([]byte, error) {
		if h.gpuNode() {
			return []byte("1\n"), nil
		}
		return []byte("\n"), nil
	})
	k.onFn("-o json -l darkplum.ai/node-type=gpu-inference", func([]string, []byte) ([]byte, error) {
		if h.gpuNode() {
			return []byte(h.nodesJSON("amd64", true)), nil
		}
		return []byte(`{"items":[]}`), nil
	})
	k.onFn("get nodes -o json", func([]string, []byte) ([]byte, error) { return []byte(h.nodesJSON(h.nodeArch, true)), nil })
	k.on("get deploy darkplum-gateway", "", errors.New(`kubectl: Error from server (NotFound): deployments.apps "darkplum-gateway" not found`))
	k.on("get svc darkplum-web", "", errors.New(`kubectl: Error from server (NotFound): services "darkplum-web" not found`))
	k.on("owner=helm", "", nil)
	k.on("get secret darkplum-auth", "", secretNotFound("darkplum-auth"))
	k.on("get secret darkplum-user-hmac", "", secretNotFound("darkplum-user-hmac"))
	k.on("create -f -", "", nil)
	k.on("rollout status", "deployment/ successfully rolled out", nil)
	k.onFn("get pods -l app.kubernetes.io/name=", func(args []string, _ []byte) ([]byte, error) {
		for _, a := range args {
			if strings.HasPrefix(a, "app.kubernetes.io/name=darkplum-") {
				svc := strings.TrimPrefix(a, "app.kubernetes.io/name=darkplum-")
				return []byte(podsJSON(h.images[svc], h.imageIDFor(svc))), nil
			}
		}
		return nil, errors.New("no selector")
	})
	k.on("get svc vllm-inference", "None", nil)
	k.on("patch secret postgres-credentials", "", nil)
	k.on("annotate", "", nil)
	k.on("apply", "", nil)
}

func (h *deployHarness) imageIDFor(svc string) string {
	if id, ok := h.imageID["darkplum-"+svc]; ok {
		return id
	}
	ref := h.images[svc]
	return ref
}

func (h *deployHarness) nodesJSON(arch string, ready bool) string {
	st := "True"
	if !ready {
		st = "False"
	}
	b, _ := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"name": "ip-10-0-1-5"},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": st}},
			"nodeInfo": map[string]any{"operatingSystem": "linux", "architecture": arch}},
	}}})
	return string(b)
}

func podsJSON(image, imageID string) string {
	b, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"metadata": map[string]any{"name": "p-new"},
			"spec":   map[string]any{"containers": []any{map[string]any{"name": "c", "image": image}}},
			"status": map[string]any{"containerStatuses": []any{map[string]any{"name": "c", "imageID": imageID}}}},
		// an old pod on its way out must not be judged
		map[string]any{"metadata": map[string]any{"name": "p-old", "deletionTimestamp": "2026-01-01T00:00:00Z"},
			"spec":   map[string]any{"containers": []any{map[string]any{"name": "c", "image": image}}},
			"status": map[string]any{"containerStatuses": []any{map[string]any{"name": "c", "imageID": "old@sha256:0000"}}}},
	}})
	return string(b)
}

func (h *deployHarness) passChecks() {
	h.checks = []checkResult{{name: "aws credentials", status: checkPass}}
}

func (h *deployHarness) run() error {
	h.t.Helper()
	if h.checks == nil {
		h.passChecks()
	}
	return h.env.run(context.Background())
}

// appliedLists returns the kinds of each `apply -f -` List, in order.
func (h *deployHarness) appliedLists() [][]string {
	var out [][]string
	for _, c := range h.k.matching("apply -f -") {
		var l struct {
			Kind  string `json:"kind"`
			Items []struct {
				Kind     string                `json:"kind"`
				Metadata struct{ Name string } `json:"metadata"`
			} `json:"items"`
		}
		if json.Unmarshal(c.stdin, &l) != nil {
			continue
		}
		var kinds []string
		if l.Kind == "List" {
			for _, it := range l.Items {
				kinds = append(kinds, it.Kind+"/"+it.Metadata.Name)
			}
		} else {
			kinds = append(kinds, l.Kind)
		}
		out = append(out, kinds)
	}
	return out
}

// writes are the kubectl calls that change the cluster.
func (h *deployHarness) writes() int {
	n := 0
	for _, w := range []string{"apply", "create", "patch", "delete", "rollout restart"} {
		n += len(h.k.matching(w))
	}
	return n
}

func (h *deployHarness) mustContain(want ...string) {
	h.t.Helper()
	for _, w := range want {
		if !strings.Contains(h.out.String(), w) {
			h.t.Errorf("output lacks %q:\n%s", w, h.out.String())
		}
	}
}

func writeLogFile(t *testing.T, in install.Install) (*os.File, string) {
	t.Helper()
	f, p, err := openDeployLog(in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, p
}
