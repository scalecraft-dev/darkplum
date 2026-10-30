package commands

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

const testAccount = "123456789012"

var fixedNow = time.Time{} // zero: golden output carries no timestamp

// fakeCloud is the scripted AWS side of init. Nothing here touches a network.
type fakeCloud struct {
	id         identity
	idErr      error
	zones      []string
	zonesErr   error
	quota      checkResult
	collisions []checkResult
	region     string

	regionErr  error
	state      backendState
	inspectErr error

	createCalls int
	createErr   error
	createdWith backendState

	// Existing state buckets and objects ("bucket/key" -> resources; absent =
	// no object). objectErr makes a read fail.
	buckets     []stateBucket
	bucketsErr  error
	prefixOnly  []string
	prefixErr   error
	objects     map[string]int
	objectsFull map[string]stateContents // wins over objects (outputs, too)
	objectErr   map[string]error
	reads       []string

	// Route53 / ACM (init_web.go).
	hzones      map[string]hostedZone // by name: public hosted zones in the account
	records     map[string][]string   // "zoneID name" -> record types
	cert        certInfo
	certErr     error
	zoneCreates int
}

func (f *fakeCloud) PublicZone(name string) (hostedZone, bool, error) {
	z, ok := f.hzones[name]
	return z, ok, nil
}
func (f *fakeCloud) HostedZone(id string) (hostedZone, error) {
	for _, z := range f.hzones {
		if z.ID == id {
			return z, nil
		}
	}
	return hostedZone{}, errors.New("NoSuchHostedZone")
}
func (f *fakeCloud) RecordTypes(zoneID, name string) ([]string, error) {
	return f.records[zoneID+" "+name], nil
}
func (f *fakeCloud) Certificate(string) (certInfo, error) { return f.cert, f.certErr }
func (f *fakeCloud) CreateWebZone(host string) (hostedZone, error) {
	f.zoneCreates++
	if z, ok := f.hzones[host]; ok {
		return z, nil
	}
	z := hostedZone{ID: "ZNEWZONE123", Name: host, NameServers: []string{"ns-1.awsdns-01.org", "ns-2.awsdns-02.co.uk", "ns-3.awsdns-03.com", "ns-4.awsdns-04.net"}}
	if f.hzones == nil {
		f.hzones = map[string]hostedZone{}
	}
	f.hzones[host] = z
	return z, nil
}

func (f *fakeCloud) Profiles() []string    { return []string{"dev", "scalecraft"} }
func (f *fakeCloud) ProfileRegion() string { return f.region }
func (f *fakeCloud) Identity() (identity, error) {
	return f.id, f.idErr
}
func (f *fakeCloud) OfferedZones(string, ...string) ([]string, error) { return f.zones, f.zonesErr }
func (f *fakeCloud) GPUQuota(string, string) checkResult {
	if f.quota.name == "" {
		return checkResult{name: "GPU vCPU quota", status: checkPass, detail: "8 vCPUs"}
	}
	return f.quota
}
func (f *fakeCloud) NameCollisions(_, _ string) []checkResult { return f.collisions }
func (f *fakeCloud) CheckRegion(string) error                 { return f.regionErr }
func (f *fakeCloud) Inspect(string, string) (backendState, error) {
	return f.state, f.inspectErr
}
func (f *fakeCloud) StateBuckets(string) ([]stateBucket, error) { return f.buckets, f.bucketsErr }

// PrefixBuckets: every prefixed bucket. Defaults to the reusable ones plus
// prefixOnly (buckets that fail the reuse checks but exist).
func (f *fakeCloud) PrefixBuckets(string) ([]string, error) {
	if f.prefixErr != nil {
		return nil, f.prefixErr
	}
	var names []string
	for _, b := range f.buckets {
		names = append(names, b.Name)
	}
	return append(names, f.prefixOnly...), nil
}
func (f *fakeCloud) ReadState(bucket, key, _ string) (stateContents, bool, error) {
	k := bucket + "/" + key
	f.reads = append(f.reads, k)
	if err := f.objectErr[k]; err != nil {
		return stateContents{}, false, err
	}
	if sc, ok := f.objectsFull[k]; ok {
		return sc, true, nil
	}
	n, ok := f.objects[k]
	return stateContents{Resources: n}, ok, nil
}
func (f *fakeCloud) CreateBackend(_, _ string, st backendState) (string, error) {
	f.createCalls++
	f.createdWith = st
	return "arn:aws:kms:us-east-2:" + testAccount + ":key/abc", f.createErr
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{
		id:     identity{Account: testAccount, Arn: "arn:aws:sts::" + testAccount + ":assumed-role/Admin/me"},
		zones:  []string{"us-east-2a", "us-east-2b", "us-east-2c"},
		region: "us-east-2",
		hzones: map[string]hostedZone{"acme.com": {ID: "ZPARENT123", Name: "acme.com"}},
	}
}

type fakeHF struct {
	m   hfModel
	err error
	n   int
}

func (f *fakeHF) Lookup(context.Context, string) (hfModel, error) { f.n++; return f.m, f.err }
func (f *fakeHF) endpoint() string                                { return "https://hf.test" }

func testEnv(t *testing.T, cloud initCloud, hf modelLookup, answers string) (*initEnv, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	if hf == nil {
		hf = &fakeHF{}
	}
	return &initEnv{
		out:         &out,
		prompt:      newIOPrompter(strings.NewReader(answers), &out),
		interactive: answers != "-",
		newCloud:    func(string) initCloud { return cloud },
		hf:          hf,
		now:         func() time.Time { return fixedNow },
		suffix:      func() string { return "a1b2c3" },

		lookupNS:          func(string) ([]string, error) { return nil, errors.New("NXDOMAIN") },
		sleep:             func(time.Duration) {},
		delegationTimeout: time.Minute,
	}, &out
}

func yesCmd(dir string, extra func(*InitCmd)) *InitCmd {
	c := &InitCmd{Cloud: "aws", Yes: true, Dir: dir, ClusterName: "darkplum", GPUCount: 1, Backend: "local", Domain: "acme.com"}
	if extra != nil {
		extra(c)
	}
	return c
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden:\n--- got\n%s\n--- want\n%s", name, got, string(want))
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInitYesBedrockGolden(t *testing.T) {
	dir := t.TempDir()
	env, _ := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Profile = "acme" }).execute(env); err != nil {
		t.Fatal(err)
	}
	golden(t, "bedrock.tfvars.golden", readFile(t, filepath.Join(dir, "terraform.tfvars")))
	st, _ := os.Stat(filepath.Join(dir, "terraform.tfvars"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "backend.tf")); err == nil {
		t.Error("--backend local must not write backend.tf")
	}
}

func TestInitYesGPUGolden(t *testing.T) {
	dir := t.TempDir()
	env, _ := testEnv(t, newFakeCloud(), nil, "-")
	err := yesCmd(dir, func(c *InitCmd) { c.Mode = "gpu"; c.Throwaway = true }).execute(env)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "gpu.tfvars.golden", readFile(t, filepath.Join(dir, "terraform.tfvars")))
}

func TestInitS3BackendGolden(t *testing.T) {
	dir := t.TempDir()
	cloud := newFakeCloud()
	env, _ := testEnv(t, cloud, nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Backend = "s3"; c.Profile = "acme" }).execute(env); err != nil {
		t.Fatal(err)
	}
	if cloud.createCalls != 1 {
		t.Fatalf("CreateBackend calls = %d, want 1", cloud.createCalls)
	}
	golden(t, "backend.tf.golden", readFile(t, filepath.Join(dir, "backend.tf")))
}

func TestInitS3BackendFailureWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cloud := newFakeCloud()
	cloud.createErr = errors.New("AccessDenied")
	env, _ := testEnv(t, cloud, nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Backend = "s3" }).execute(env); err == nil {
		t.Fatal("want error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("files written despite backend failure: %v", entries)
	}
}

func TestInitDryRunWritesNothingAndPrintsCommands(t *testing.T) {
	dir := t.TempDir()
	cloud := newFakeCloud()
	env, out := testEnv(t, cloud, nil, "-")
	err := yesCmd(dir, func(c *InitCmd) { c.Backend = "s3"; c.DryRun = true }).execute(env)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dry run wrote %v", entries)
	}
	if cloud.createCalls != 0 {
		t.Error("dry run created a backend")
	}
	for _, want := range []string{"kms create-key", "s3api create-bucket", "put-bucket-versioning", "put-public-access-block", "use_lockfile = true"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output lacks %q", want)
		}
	}
}

func TestInitRefusesRoot(t *testing.T) {
	cloud := newFakeCloud()
	cloud.id.Arn = "arn:aws:iam::" + testAccount + ":root"
	env, _ := testEnv(t, cloud, nil, "-")
	err := yesCmd(t.TempDir(), nil).execute(env)
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("want root refusal, got %v", err)
	}
}

func TestInitGCPNotSupported(t *testing.T) {
	env, _ := testEnv(t, newFakeCloud(), nil, "-")
	err := (&InitCmd{Cloud: "gcp", Yes: true}).execute(env)
	if err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("got %v", err)
	}
}

func TestInitNonTTYWithoutYesListsFlags(t *testing.T) {
	err := (&InitCmd{Cloud: "aws"}).missingFlagsError()
	for _, want := range []string{"--region", "--mode", "--backend", "--yes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %s", err, want)
		}
	}
}

func TestInitFewerThanTwoZonesFails(t *testing.T) {
	cloud := newFakeCloud()
	cloud.zones = []string{"us-east-2a"}
	dir := t.TempDir()
	env, _ := testEnv(t, cloud, nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Mode = "gpu" }).execute(env); err == nil {
		t.Fatal("want error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestInitQuotaFailure(t *testing.T) {
	cloud := newFakeCloud()
	cloud.quota = checkResult{name: "GPU vCPU quota", status: checkFail, detail: "0 vCPUs"}
	env, _ := testEnv(t, cloud, nil, "-")
	if err := yesCmd(t.TempDir(), func(c *InitCmd) { c.Mode = "gpu" }).execute(env); err == nil {
		t.Fatal("--yes must stop on a quota failure")
	}
	// Bedrock-only never consults the GPU quota.
	env, _ = testEnv(t, cloud, nil, "-")
	if err := yesCmd(t.TempDir(), nil).execute(env); err != nil {
		t.Fatalf("bedrock mode: %v", err)
	}
}

func TestInitNameCollision(t *testing.T) {
	cloud := newFakeCloud()
	cloud.collisions = []checkResult{{name: "cluster name free", status: checkFail, detail: "EKS cluster darkplum exists"}}
	env, _ := testEnv(t, cloud, nil, "-")
	if err := yesCmd(t.TempDir(), nil).execute(env); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("got %v", err)
	}
	// Interactive: re-asks, and the second name is used. The fake flips to free.
	dir := t.TempDir()
	env, _ = testEnv(t, &collideOnce{fakeCloud: newFakeCloud()}, nil, "\ndarkplum-b\n\n")
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Backend: "local", Profile: "acme", Domain: "acme.com"}
	if err := c.execute(env); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "terraform.tfvars")); !strings.Contains(got, `cluster_name = "darkplum-b"`) {
		t.Errorf("renamed cluster not written:\n%s", got)
	}
}

type collideOnce struct{ *fakeCloud }

func (c *collideOnce) NameCollisions(_, name string) []checkResult {
	if name == "darkplum" {
		return []checkResult{{name: "cluster name free", status: checkFail}}
	}
	return nil
}

func TestInitOverwriteGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terraform.tfvars")
	if err := os.WriteFile(path, []byte("aws_region = \"eu-west-1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _ := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, nil).execute(env); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("--yes without --force must refuse, got %v", err)
	}
	if readFile(t, path) != "aws_region = \"eu-west-1\"\n" {
		t.Fatal("file was modified")
	}
	env, _ = testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, func(c *InitCmd) { c.Force = true }).execute(env); err != nil {
		t.Fatal(err)
	}
	if readFile(t, path+".bak") != "aws_region = \"eu-west-1\"\n" {
		t.Error(".bak does not hold the previous file")
	}
}

func TestInitConfirmOverwriteDefaultsToNo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terraform.tfvars")
	_ = os.WriteFile(path, []byte("old\n"), 0o600)
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Backend: "local", Profile: "acme", Domain: "acme.com"}
	// Start over (not just add the address), default networks, then Enter at the overwrite question.
	env, _ := testEnv(t, newFakeCloud(), nil, "2\n\n\n")
	if err := c.execute(env); !errors.Is(err, errAborted) {
		t.Fatalf("got %v, want errAborted", err)
	}
	if readFile(t, path) != "old\n" {
		t.Fatal("overwritten despite default No")
	}
}

func TestInitAbortOnEOFWritesNothing(t *testing.T) {
	dir := t.TempDir()
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Profile: "acme"}
	env, _ := testEnv(t, newFakeCloud(), nil, "us-east-2\n") // input ends at the mode question
	if err := c.execute(env); !errors.Is(err, errAborted) {
		t.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestInitInteractiveGPUFlow(t *testing.T) {
	dir := t.TempDir()
	// profile, region, mode 2 (gpu), model 2 (mistral), backend 2 (local), final confirm
	env, out := testEnv(t, newFakeCloud(), nil, "acme\n\n2\n2\nacme.com\n\n2\n\n")
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1}
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := readFile(t, filepath.Join(dir, "terraform.tfvars"))
	for _, want := range []string{`model_name = "mistralai/Mistral-7B-Instruct-v0.3"`, `served_model_name = "mistral-7b"`, `aws_profile = "acme"`, `gpu_desired_size = 1`} {
		if !strings.Contains(got, want) {
			t.Errorf("tfvars lacks %s:\n%s", want, got)
		}
	}
	for _, want := range []string{"Installing into account " + testAccount, "make deploy", "Settings saved"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// File names are an implementation detail the operator should not need.
	for _, unwanted := range []string{"backend.tf", "terraform.tfvars"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("output mentions %q", unwanted)
		}
	}
}

func TestInitCustomModel(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	cases := []struct {
		name    string
		hf      *fakeHF
		cmd     func(*InitCmd)
		wantErr string
		wantRev string
	}{
		{"pins current sha", &fakeHF{m: hfModel{SHA: sha, Safetensors: true}}, nil, "", sha},
		{"explicit revision wins", &fakeHF{m: hfModel{SHA: sha, Safetensors: true}}, func(c *InitCmd) { c.ModelRevision = strings.Repeat("cd", 20) }, "", strings.Repeat("cd", 20)},
		{"missing", &fakeHF{err: errHFNotFound}, nil, "not found", ""},
		{"gated refused under --yes", &fakeHF{err: errHFGated}, nil, "gated", ""},
		{"no safetensors", &fakeHF{m: hfModel{SHA: sha}}, nil, "safetensors", ""},
		{"too big for 24GB", &fakeHF{m: hfModel{SHA: sha, Safetensors: true, WeightBytes: 30 << 30}}, nil, "does not fit", ""},
		{"network error", &fakeHF{err: errors.New("dial tcp: timeout")}, nil, "could not reach", ""},
		{"offline needs revision", &fakeHF{}, func(c *InitCmd) { c.Offline = true }, "--model-revision", ""},
		{"offline with revision", &fakeHF{}, func(c *InitCmd) { c.Offline = true; c.ModelRevision = sha }, "", sha},
		{"bad revision", &fakeHF{}, func(c *InitCmd) { c.ModelRevision = "main" }, "invalid --model-revision", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env, _ := testEnv(t, newFakeCloud(), tc.hf, "-")
			c := yesCmd(dir, func(c *InitCmd) {
				c.Model = "acme/Custom-Model_1"
				if tc.cmd != nil {
					tc.cmd(c)
				}
			})
			err := c.execute(env)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if entries, _ := os.ReadDir(dir); len(entries) != 0 {
					t.Errorf("wrote %v", entries)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := readFile(t, filepath.Join(dir, "terraform.tfvars"))
			if !strings.Contains(got, `model_revision = "`+tc.wantRev+`"`) || !strings.Contains(got, `served_model_name = "custom-model_1"`) {
				t.Errorf("tfvars:\n%s", got)
			}
			if tc.cmd != nil && tc.hf.n != 0 && strings.Contains(tc.name, "offline") {
				t.Error("--offline must not call Hugging Face")
			}
		})
	}
}

func TestInitGatedConfirmed(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	dir := t.TempDir()
	// profile, region, mode(gpu), model: custom(3), id, gated confirm y, sha, networks, backend local, final confirm
	env, _ := testEnv(t, newFakeCloud(), &fakeHF{err: errHFGated}, "acme\n\n2\n3\nmeta/Gated-1\ny\n"+sha+"\n\n2\n\n")
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Domain: "acme.com"}
	if err := c.execute(env); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "terraform.tfvars")); !strings.Contains(got, sha) {
		t.Errorf("sha not written:\n%s", got)
	}
}

func TestCatalogEntriesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range modelCatalog {
		if validateSHA(m.Revision) != nil || validateModelID(m.ID) != nil || !servedRe.MatchString(m.Served) {
			t.Errorf("bad catalog entry %+v", m)
		}
		if _, ok := gpuInstances[m.GPUType]; !ok {
			t.Errorf("%s: unknown GPU type %s", m.ID, m.GPUType)
		}
		if seen[m.ID] {
			t.Errorf("duplicate %s", m.ID)
		}
		seen[m.ID] = true
	}
	if modelCatalog[0].ID != "Qwen/Qwen2.5-7B-Instruct" || modelCatalog[0].Revision != "a09a35458c702b33eeacc393d103063234e8bc28" {
		t.Error("first catalog entry is the default model and must stay the pinned Qwen")
	}
}

func TestHFClient(t *testing.T) {
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		switch r.URL.Path {
		case "/api/models/acme/ok":
			w.Write([]byte(`{"sha":"` + strings.Repeat("a", 40) + `","gated":false,"siblings":[{"rfilename":"model.safetensors"}],"safetensors":{"parameters":{"BF16":1000}}}`))
		case "/api/models/acme/gatedflag":
			w.Write([]byte(`{"sha":"` + strings.Repeat("a", 40) + `","gated":"auto","siblings":[]}`))
		case "/api/models/acme/private":
			w.WriteHeader(401)
		case "/api/models/acme/forbidden":
			w.WriteHeader(403)
		case "/api/models/acme/badsha":
			w.Write([]byte(`{"sha":"main","siblings":[]}`))
		case "/api/models/acme/boom":
			w.WriteHeader(500)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	t.Setenv("HF_TOKEN", "hf_secret")
	c := newHFClient()
	ctx := context.Background()

	m, err := c.Lookup(ctx, "acme/ok")
	if err != nil || m.SHA != strings.Repeat("a", 40) || !m.Safetensors || m.WeightBytes != 2000 {
		t.Fatalf("ok: %+v %v", m, err)
	}
	if gotAuth != "" {
		t.Errorf("client sent Authorization %q; it must never send a token", gotAuth)
	}
	for id, want := range map[string]error{"acme/private": errHFGated, "acme/forbidden": errHFGated, "acme/gatedflag": errHFGated, "acme/missing": errHFNotFound} {
		if _, err := c.Lookup(ctx, id); !errors.Is(err, want) {
			t.Errorf("%s: got %v want %v", id, err, want)
		}
	}
	for _, id := range []string{"acme/badsha", "acme/boom"} {
		if _, err := c.Lookup(ctx, id); err == nil {
			t.Errorf("%s: want error", id)
		}
	}
	if _, err := c.Lookup(ctx, "../etc/passwd"); err == nil {
		t.Error("invalid id accepted")
	}
	_ = gotPath
	srv.Close()
	if _, err := c.Lookup(ctx, "acme/ok"); err == nil || !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("network error must name the endpoint, got %v", err)
	}
}

func TestPrompter(t *testing.T) {
	var out bytes.Buffer
	p := newIOPrompter(strings.NewReader("bad\nok\n\ny\nmaybe\nN\n\n9\n2\n"), &out)
	v := func(s string) error {
		if s == "bad" {
			return errors.New("nope")
		}
		return nil
	}
	if got, _ := p.Ask("Label", "def", v); got != "ok" {
		t.Errorf("Ask = %q (validation must re-ask)", got)
	}
	if got, _ := p.Ask("Label", "def", v); got != "def" {
		t.Errorf("Ask empty = %q, want default", got)
	}
	if got, _ := p.Confirm("sure", false); !got {
		t.Error("y")
	}
	if got, _ := p.Confirm("sure", true); got {
		t.Error("maybe then N should be false")
	}
	if got, _ := p.Select("pick", []Option{{Label: "a"}, {Label: "b"}}, 0); got != 0 {
		t.Errorf("Select default = %d", got)
	}
	if got, _ := p.Select("pick", []Option{{Label: "a"}, {Label: "b"}}, 0); got != 1 {
		t.Errorf("Select 9 then 2 = %d", got)
	}
	if _, err := p.Ask("eof", "", nil); !errors.Is(err, errAborted) {
		t.Errorf("EOF = %v", err)
	}
	if !strings.Contains(out.String(), "nope") || !strings.Contains(out.String(), "1 to 2") {
		t.Errorf("validation hints missing:\n%s", out.String())
	}
}
