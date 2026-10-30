package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeKMS = "arn:aws:kms:us-east-2:" + testAccount + ":key/abc" // what fakeCloud.CreateBackend returns

// The legacy key is shared by older installs: it stops init only for the same
// cluster name, or when the name cannot be read.
func TestInitStateLegacyKeyByClusterName(t *testing.T) {
	run := func(t *testing.T, sc stateContents) (error, string, *fakeCloud, string) {
		dir := t.TempDir()
		cloud := newFakeCloud()
		cloud.buckets = []stateBucket{{Name: bucketN("111111")}}
		cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true}
		cloud.objectsFull = map[string]stateContents{bucketN("111111") + "/" + legacyStateKey: sc}
		env, out := testEnv(t, cloud, nil, "-")
		err := s3Cmd(dir, nil).execute(env) // cluster "darkplum"
		return err, out.String(), cloud, dir
	}
	restore := `key          = "` + legacyStateKey + `"`

	t.Run("same name stops", func(t *testing.T) {
		err, out, cloud, _ := run(t, stateContents{Resources: 90, ClusterName: "darkplum"})
		if err == nil || !strings.Contains(out, restore) || cloud.createCalls != 0 {
			t.Fatalf("want stop with backend.tf to restore, got %v\n%s", err, out)
		}
	})
	t.Run("unreadable name stops", func(t *testing.T) {
		err, out, _, _ := run(t, stateContents{Resources: 90})
		if err == nil || !strings.Contains(out, restore) {
			t.Fatalf("want stop, got %v\n%s", err, out)
		}
	})
	t.Run("other name is left alone", func(t *testing.T) {
		err, out, cloud, dir := run(t, stateContents{Resources: 90, ClusterName: "prod"})
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(out, "another install (prod) keeps state at the earlier key in "+bucketN("111111")+"; left untouched") {
			t.Errorf("no note:\n%s", out)
		}
		if got := backendLine(t, dir, "key"); !strings.Contains(got, "darkplum/aws/darkplum/terraform.tfstate") || !cloud.createdWith.BucketExists {
			t.Errorf("want the shared bucket under the cluster key: %s", got)
		}
	})
}

// The live install's own resources collide by name; its state must answer
// first: restore message, never a rename prompt.
func TestInitStateLiveInstallBeatsNameCollision(t *testing.T) {
	cloud := newFakeCloud()
	cloud.collisions = []checkResult{{name: "cluster name free", status: checkFail, detail: "EKS cluster darkplum exists"}}
	cloud.buckets = []stateBucket{{Name: bucketN("bf3057")}}
	cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true}
	cloud.objectsFull = map[string]stateContents{bucketN("bf3057") + "/" + legacyStateKey: {Resources: 120, ClusterName: "darkplum"}}
	for _, answers := range []string{"-", "\n"} { // --yes, and interactive (networks default; a rename prompt would hit EOF)
		dir := t.TempDir()
		env, out := testEnv(t, cloud, nil, answers)
		c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Profile: "acme", Domain: "acme.com", Yes: answers == "-"}
		err := c.execute(env)
		if err == nil || errors.Is(err, errAborted) || !strings.Contains(err.Error(), "an install already keeps state at s3://"+bucketN("bf3057")+"/"+legacyStateKey) {
			t.Fatalf("%q: want restore stop, got %v\n%s", answers, err, out)
		}
		for _, want := range []string{"install darkplum already exists", "not a name clash", `bucket       = "` + bucketN("bf3057") + `"`, `key          = "` + legacyStateKey + `"`} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%q: lacks %q:\n%s", answers, want, out)
			}
		}
		if strings.Contains(out.String(), "darkplum-2") || strings.Contains(out.String(), "EKS cluster darkplum exists") {
			t.Errorf("%q: name-collision path ran:\n%s", answers, out)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("wrote %v", entries)
		}
	}
}

func TestParseStateClusterName(t *testing.T) {
	eks := `{"version":4,"resources":[{"module":"module.eks.module.eks","mode":"managed","type":"aws_eks_cluster","name":"this",
		"instances":[{"index_key":0,"attributes":{"name":"prod","arn":"x"}}]},{"type":"aws_vpc","instances":[{"attributes":{"id":"v"}}]}],
		"outputs":{"cluster_name":{"value":"ignored","type":"string"}}}`
	if sc, err := parseState([]byte(eks)); err != nil || sc.ClusterName != "prod" || sc.Resources != 2 || sc.Outputs != 1 {
		t.Errorf("resource name: %+v %v", sc, err)
	}
	out := `{"version":4,"resources":[{"type":"aws_vpc","instances":[{"attributes":{}}]}],"outputs":{"cluster_name":{"value":"staging","type":"string"}}}`
	if sc, _ := parseState([]byte(out)); sc.ClusterName != "staging" {
		t.Errorf("output fallback: %+v", sc)
	}
	if sc, _ := parseState([]byte(`{"resources":[{"type":"aws_vpc"}]}`)); sc.ClusterName != "" {
		t.Errorf("no name: %+v", sc)
	}
}

func bucketN(suffix string) string { return stateBucketName(testAccount, "us-east-2", suffix) }

func s3Cmd(dir string, extra func(*InitCmd)) *InitCmd {
	return yesCmd(dir, func(c *InitCmd) {
		c.Backend = "s3"
		c.Profile = "acme"
		if extra != nil {
			extra(c)
		}
	})
}

// writeCache writes Terraform's saved backend settings as `terraform init` does.
func writeCache(t *testing.T, dir, bucket, key string) string {
	t.Helper()
	p := filepath.Join(dir, ".terraform", "terraform.tfstate")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":3,"terraform_version":"1.10.0","backend":{"type":"s3","config":{"bucket":"` + bucket +
		`","key":"` + key + `","region":"us-east-2","use_lockfile":true},"hash":1}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func backendLine(t *testing.T, dir, name string) string {
	t.Helper()
	for _, l := range strings.Split(readFile(t, filepath.Join(dir, "backend.tf")), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), name+" ") {
			return strings.TrimSpace(l)
		}
	}
	t.Fatalf("backend.tf has no %s", name)
	return ""
}

func TestInitStateReusesSingleBucket(t *testing.T) {
	dir := t.TempDir()
	cloud := newFakeCloud()
	cloud.buckets = []stateBucket{{Name: bucketN("ffffff")}}
	cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true}
	env, out := testEnv(t, cloud, nil, "-")
	if err := s3Cmd(dir, nil).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := backendLine(t, dir, "bucket"); got != `bucket       = "`+bucketN("ffffff")+`"` {
		t.Errorf("bucket not reused: %s", got)
	}
	if !cloud.createdWith.BucketExists {
		t.Error("CreateBackend must be told the bucket exists (no create-bucket, no re-key)")
	}
	if !strings.Contains(out.String(), "S3 "+bucketN("ffffff")+" (reused)") {
		t.Errorf("no reuse row:\n%s", out)
	}
	// Fresh checkout: the legacy and this cluster's keys were looked at for a
	// live install, and the chosen key was checked before it was taken.
	want := []string{bucketN("ffffff") + "/" + legacyStateKey, bucketN("ffffff") + "/darkplum/aws/darkplum/terraform.tfstate",
		bucketN("ffffff") + "/darkplum/aws/darkplum/terraform.tfstate"}
	if strings.Join(cloud.reads, ",") != strings.Join(want, ",") {
		t.Errorf("reads = %v", cloud.reads)
	}
}

func TestInitStateUntaggedBucketSaysSo(t *testing.T) {
	cloud := newFakeCloud()
	cloud.buckets = []stateBucket{{Name: bucketN("ffffff"), Untagged: true}}
	cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true}
	env, out := testEnv(t, cloud, nil, "-")
	if err := s3Cmd(t.TempDir(), nil).execute(env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "S3 "+bucketN("ffffff")+" (reused; untagged, from an earlier release)") {
		t.Errorf("no provenance note:\n%s", out)
	}
}

func TestBucketCommandsTagNewBucketOnly(t *testing.T) {
	joined := func(exists bool) string {
		var s []string
		for _, c := range bucketCommands("", testAccount, "b", "us-east-2", kmsARN, exists) {
			s = append(s, formatAWS(c))
		}
		return strings.Join(s, "\n")
	}
	if got := joined(false); !strings.Contains(got, "put-bucket-tagging --bucket b --tagging '{\"TagSet\":[{\"Key\":\"darkplum:purpose\",\"Value\":\"terraform-state\"}]}' --expected-bucket-owner "+testAccount) {
		t.Errorf("new bucket not tagged:\n%s", got)
	}
	if strings.Contains(joined(true), "put-bucket-tagging") {
		t.Error("an existing bucket's tag set must not be replaced")
	}
}

func TestInitStateNoBucketCreatesOne(t *testing.T) {
	dir := t.TempDir()
	cloud := newFakeCloud()
	env, out := testEnv(t, cloud, nil, "-")
	if err := s3Cmd(dir, nil).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := backendLine(t, dir, "bucket"); !strings.Contains(got, bucketN("a1b2c3")) {
		t.Errorf("want a new bucket: %s", got)
	}
	if cloud.createCalls != 1 || cloud.createdWith.BucketExists {
		t.Errorf("want one create of a new bucket: %d %+v", cloud.createCalls, cloud.createdWith)
	}
	if strings.Contains(out.String(), "(reused)") {
		t.Errorf("new bucket reported as reused:\n%s", out)
	}
}

func TestInitStateSeveralBuckets(t *testing.T) {
	newest, older := stateBucket{Name: bucketN("222222"), Created: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
		stateBucket{Name: bucketN("111111"), Created: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	several := func() *fakeCloud {
		c := newFakeCloud()
		c.buckets = []stateBucket{newest, older} // StateBuckets returns newest first
		c.state = backendState{KeyARN: fakeKMS, BucketExists: true}
		return c
	}

	t.Run("yes takes the newest and says so", func(t *testing.T) {
		dir := t.TempDir()
		env, out := testEnv(t, several(), nil, "-")
		if err := s3Cmd(dir, nil).execute(env); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(backendLine(t, dir, "bucket"), newest.Name) {
			t.Error("not the newest")
		}
		if !strings.Contains(out.String(), "using the newest, "+newest.Name) {
			t.Errorf("choice not explained:\n%s", out)
		}
	})
	t.Run("the bucket the saved settings name wins", func(t *testing.T) {
		dir := t.TempDir()
		writeCache(t, dir, older.Name, "darkplum/aws/darkplum/terraform.tfstate")
		env, out := testEnv(t, several(), nil, "-")
		if err := s3Cmd(dir, nil).execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(backendLine(t, dir, "bucket"), older.Name) {
			t.Error("did not follow the saved backend settings")
		}
		if _, err := os.Stat(filepath.Join(dir, ".terraform", "terraform.tfstate")); err != nil {
			t.Error("matching saved settings must be left alone")
		}
	})
	t.Run("interactive asks", func(t *testing.T) {
		dir := t.TempDir()
		// networks (default), bucket 2, final confirm
		env, out := testEnv(t, several(), nil, "\n2\n\n")
		c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Backend: "s3", Profile: "acme", Domain: "acme.com"}
		if err := c.execute(env); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if !strings.Contains(backendLine(t, dir, "bucket"), older.Name) {
			t.Errorf("picked the wrong bucket:\n%s", out)
		}
		if !strings.Contains(out.String(), "Several Darkplum state buckets") {
			t.Errorf("no question:\n%s", out)
		}
	})
}

func TestInitStateNewInstallKeyCarriesClusterName(t *testing.T) {
	dir := t.TempDir()
	env, _ := testEnv(t, newFakeCloud(), nil, "-")
	if err := s3Cmd(dir, func(c *InitCmd) { c.ClusterName = "acme-prod" }).execute(env); err != nil {
		t.Fatal(err)
	}
	if got := backendLine(t, dir, "key"); got != `key          = "darkplum/aws/acme-prod/terraform.tfstate"` {
		t.Errorf("key = %s", got)
	}
}

// An existing backend.tf keeps the same bucket, key and region (the whole file
// comes out identical when the key ARN is unchanged).
func TestInitStateExistingBackendKeepsBucketKeyRegion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "backend.tf")
	before := renderBackend(backendSpec{Bucket: bucketN("ffffff"), Key: legacyStateKey, Region: "us-east-2", KMSKeyARN: fakeKMS})
	if err := os.WriteFile(p, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	cloud := newFakeCloud()
	// Another, newer bucket exists; the install's own must still win.
	cloud.buckets = []stateBucket{{Name: bucketN("999999"), Created: time.Now()}}
	cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true}
	env, out := testEnv(t, cloud, nil, "-")
	if err := s3Cmd(dir, func(c *InitCmd) { c.ClusterName = "renamed" }).execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := readFile(t, p); got != before {
		t.Errorf("backend.tf changed:\n--- got\n%s\n--- want\n%s", got, before)
	}
	if len(cloud.reads) != 0 {
		t.Errorf("an existing install's key is not re-checked: %v", cloud.reads)
	}
	if !strings.Contains(out.String(), "(reused; same bucket, key and region)") {
		t.Errorf("kept path not described:\n%s", out)
	}
}

func TestInitStateKeptBucketVersioningOffWarns(t *testing.T) {
	dir := t.TempDir()
	writeBackendTF(t, dir, bucketN("ffffff"), "us-east-2")
	cloud := newFakeCloud()
	cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true, Versioning: "Suspended"}
	env, out := testEnv(t, cloud, nil, "-")
	if err := s3Cmd(dir, nil).execute(env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "versioning is Suspended on "+bucketN("ffffff")) || !strings.Contains(out.String(), "turns it back on") {
		t.Errorf("no versioning warning:\n%s", out)
	}
}

// A fresh checkout (no backend.tf, no saved settings) must find a live install
// in any reusable bucket, under the legacy key or this cluster's key, before it
// picks a bucket, and print the backend.tf that manages it.
func TestInitStateFreshCheckoutFindsLiveInstall(t *testing.T) {
	cases := map[string]struct {
		bucket, key string
		sc          stateContents
	}{
		"cluster key, newest bucket skipped over": {bucketN("111111"), "darkplum/aws/darkplum/terraform.tfstate", stateContents{Resources: 42}},
		"legacy key":   {bucketN("111111"), legacyStateKey, stateContents{Resources: 3}},
		"outputs only": {bucketN("222222"), legacyStateKey, stateContents{Outputs: 2}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cloud := newFakeCloud()
			cloud.buckets = []stateBucket{{Name: bucketN("222222")}, {Name: bucketN("111111")}}
			cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true}
			cloud.objectsFull = map[string]stateContents{tc.bucket + "/" + tc.key: tc.sc}
			env, out := testEnv(t, cloud, nil, "-")
			err := s3Cmd(dir, nil).execute(env)
			if err == nil || !strings.Contains(err.Error(), "an install already keeps state at s3://"+tc.bucket+"/"+tc.key) {
				t.Fatalf("want refusal, got %v\n%s", err, out)
			}
			for _, want := range []string{`bucket       = "` + tc.bucket + `"`, `key          = "` + tc.key + `"`, `kms_key_id   = "` + fakeKMS + `"`, "save the following as backend.tf"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("restore instructions lack %q:\n%s", want, out)
				}
			}
			if strings.Contains(out.String(), "using the newest") {
				t.Error("picked a bucket before looking for the live install")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 || cloud.createCalls != 0 {
				t.Errorf("wrote %v / created %d", entries, cloud.createCalls)
			}
		})
	}
	// Destroyed installs (empty states) do not block a reinstall.
	cloud := newFakeCloud()
	cloud.buckets = []stateBucket{{Name: bucketN("111111")}}
	cloud.state = backendState{KeyARN: fakeKMS, BucketExists: true}
	cloud.objects = map[string]int{bucketN("111111") + "/" + legacyStateKey: 0}
	env, out := testEnv(t, cloud, nil, "-")
	if err := s3Cmd(t.TempDir(), nil).execute(env); err != nil {
		t.Fatalf("empty legacy state must not block: %v\n%s", err, out)
	}
}

func TestInitStateKeyCollision(t *testing.T) {
	ck := bucketN("ffffff") + "/darkplum/aws/darkplum/terraform.tfstate"
	taken := func(n int) *fakeCloud {
		c := newFakeCloud()
		c.buckets = []stateBucket{{Name: bucketN("ffffff")}}
		c.state = backendState{KeyARN: fakeKMS, BucketExists: true}
		c.objects = map[string]int{ck: n}
		return c
	}
	// Not a fresh checkout: saved settings name some other, gone, state.
	notFresh := func(t *testing.T) string {
		dir := t.TempDir()
		writeCache(t, dir, bucketN("000000"), legacyStateKey)
		return dir
	}
	for _, n := range []int{42, 0} { // an existing object, even an empty one, is another install's
		dir := notFresh(t)
		cloud := taken(n)
		env, out := testEnv(t, cloud, nil, "-")
		err := s3Cmd(dir, nil).execute(env)
		if err == nil || !strings.Contains(err.Error(), "another install named darkplum already keeps state here") {
			t.Fatalf("%d resources: want collision refusal, got %v\n%s", n, err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "backend.tf")); err == nil || cloud.createCalls != 0 {
			t.Errorf("%d resources: wrote backend.tf / created %d", n, cloud.createCalls)
		}
	}

	// This checkout's own saved settings may claim the key.
	dir := t.TempDir()
	writeCache(t, dir, bucketN("ffffff"), "darkplum/aws/darkplum/terraform.tfstate")
	env, out := testEnv(t, taken(0), nil, "-")
	if err := s3Cmd(dir, nil).execute(env); err != nil {
		t.Fatalf("own key refused: %v\n%s", err, out)
	}

	// Interactive: another name is asked for and lands in both files.
	dir = notFresh(t)
	env, out = testEnv(t, taken(42), nil, "\ndarkplum-2\n\n")
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Backend: "s3", Profile: "acme", Domain: "acme.com"}
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(backendLine(t, dir, "key"), "darkplum/aws/darkplum-2/terraform.tfstate") ||
		!strings.Contains(readFile(t, filepath.Join(dir, "terraform.tfvars")), `cluster_name = "darkplum-2"`) {
		t.Errorf("rename not applied:\n%s", out)
	}
}

func TestInitStateStaleCacheEmptySetAside(t *testing.T) {
	for name, objects := range map[string]map[string]int{
		"object missing": nil,
		"zero resources": {bucketN("000000") + "/" + legacyStateKey: 0},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cache := writeCache(t, dir, bucketN("000000"), legacyStateKey)
			cloud := newFakeCloud()
			cloud.objects = objects
			env, out := testEnv(t, cloud, nil, "-")
			if err := s3Cmd(dir, nil).execute(env); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if _, err := os.Stat(cache); err == nil {
				t.Error("stale backend settings still in place: terraform init would fail")
			}
			if _, err := os.Stat(cache + ".stale-00010101T000000Z"); err != nil {
				t.Errorf("not set aside inside .terraform: %v", err)
			}
			if !strings.Contains(out.String(), "set aside Terraform's saved settings for the earlier, empty state at s3://"+bucketN("000000")) {
				t.Errorf("no note:\n%s", out)
			}
			if strings.Contains(out.String(), ".terraform") || strings.Contains(out.String(), "stale-") {
				t.Errorf("output names a file:\n%s", out)
			}
		})
	}
}

func TestInitStateStaleCacheWithResourcesRefuses(t *testing.T) {
	dir := t.TempDir()
	cache := writeCache(t, dir, bucketN("000000"), legacyStateKey)
	cloud := newFakeCloud()
	cloud.objects = map[string]int{bucketN("000000") + "/" + legacyStateKey: 7}
	env, out := testEnv(t, cloud, nil, "-")
	err := s3Cmd(dir, nil).execute(env)
	if err == nil {
		t.Fatal("want refusal")
	}
	for _, want := range []string{"7 resources at s3://" + bucketN("000000") + "/" + legacyStateKey,
		"cd " + dir + " && terraform init -migrate-state -backend-config=profile=acme"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(cache); err != nil {
		t.Error("non-empty state's settings were moved")
	}
	if _, err := os.Stat(filepath.Join(dir, "backend.tf")); err == nil || cloud.createCalls != 0 {
		t.Error("wrote a backend away from live state")
	}
}

func TestInitStateStaleCacheUnreadableRefuses(t *testing.T) {
	dir := t.TempDir()
	cache := writeCache(t, dir, bucketN("000000"), legacyStateKey)
	cloud := newFakeCloud()
	cloud.objectErr = map[string]error{bucketN("000000") + "/" + legacyStateKey: errors.New("AccessDenied")}
	env, out := testEnv(t, cloud, nil, "-")
	if err := s3Cmd(dir, nil).execute(env); err == nil || !strings.Contains(err.Error(), "cannot read the earlier state") {
		t.Fatalf("want refusal, got %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "kms:Decrypt") {
		t.Errorf("no hint on what to check:\n%s", out)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Error("moved despite unreadable state")
	}
}

func TestInitStateFinalLineSaysConfigure(t *testing.T) {
	env, out := testEnv(t, newFakeCloud(), nil, "-")
	if err := s3Cmd(t.TempDir(), nil).execute(env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "then ./bin/darkplum configure") || strings.Contains(out.String(), "darkplum status") {
		t.Errorf("final line:\n%s", out)
	}
}

func TestAWSStateBucketsFiltersAndSorts(t *testing.T) {
	good, newer, unversioned, otherKey := bucketN("aaaaaa"), bucketN("bbbbbb"), bucketN("cccccc"), bucketN("dddddd")
	public, foreignTag := bucketN("eeeeee"), bucketN("ffffff")
	listed := ""
	for i, b := range []string{good, newer, unversioned, otherKey, public, foreignTag} {
		if i > 0 {
			listed += ","
		}
		listed += `{"Name":"` + b + `","Created":"2026-0` + string(rune('1'+i)) + `-01T00:00:00+00:00"}`
	}
	allBlocked := `{"BlockPublicAcls":true,"IgnorePublicAcls":true,"BlockPublicPolicy":true,"RestrictPublicBuckets":true}`
	sr := &scriptRunner{rules: []rule{
		{"sts get-caller-identity", `{"Account":"` + testAccount + `","Arn":"x"}`, nil},
		{"s3api list-buckets", "[" + listed + "]", nil},
		{"--key-id alias/darkplum-tfstate", kmsARN, nil},
		{"get-bucket-versioning --bucket " + unversioned, "Suspended", nil},
		{"get-bucket-versioning", "Enabled", nil},
		{"get-public-access-block --bucket " + public, `{"BlockPublicAcls":true,"IgnorePublicAcls":true,"BlockPublicPolicy":false,"RestrictPublicBuckets":true}`, nil},
		{"get-public-access-block", allBlocked, nil},
		{"get-bucket-tagging --bucket " + good, "", errors.New("An error occurred (NoSuchTagSet) when calling the GetBucketTagging operation")},
		{"get-bucket-tagging --bucket " + foreignTag, `[{"Key":"darkplum:purpose","Value":"logs"}]`, nil},
		{"get-bucket-tagging", `[{"Key":"darkplum:purpose","Value":"terraform-state"}]`, nil},
		{"get-bucket-encryption --bucket " + otherKey, `{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":"other"}`, nil},
		{"get-bucket-encryption --bucket " + newer, `{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":"k"}`, nil},
		{"get-bucket-encryption", `{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":"` + kmsARN + `"}`, nil},
		{"--key-id other", "arn:aws:kms:us-east-2:" + testAccount + ":key/other", nil},
		{"--key-id k ", kmsARN, nil},
	}}
	a := newAWSCloud("p", sr.run)
	a.Identity()
	got, err := a.StateBuckets("us-east-2")
	if err != nil {
		t.Fatal(err)
	}
	// unversioned, wrong key, public access not fully blocked, tagged for
	// something else: all left out. good is untagged (earlier release).
	if len(got) != 2 || got[0].Name != newer || got[1].Name != good || got[0].Untagged || !got[1].Untagged {
		t.Fatalf("got %+v", got)
	}
	for _, c := range sr.calls {
		if strings.Contains(c, "get-bucket-") && !strings.Contains(c, "--expected-bucket-owner "+testAccount) {
			t.Errorf("no owner assertion: %s", c)
		}
		if !strings.Contains(c, "--profile p") {
			t.Errorf("no profile: %s", c)
		}
	}
	if m := sr.mutating(); len(m) != 0 {
		t.Errorf("mutated: %v", m)
	}
}

func TestAWSStateResources(t *testing.T) {
	id := rule{"sts get-caller-identity", `{"Account":"` + testAccount + `","Arn":"x"}`, nil}
	cases := []struct {
		name      string
		rules     []rule
		want      stateContents
		found, ok bool
	}{
		{"counts", []rule{{"head-bucket", "", nil}, {"s3 cp", `{"resources":[{},{},{}]}`, nil}}, stateContents{Resources: 3}, true, true},
		{"empty", []rule{{"head-bucket", "", nil}, {"s3 cp", `{"version":4,"resources":[],"outputs":{}}`, nil}}, stateContents{}, true, true},
		{"outputs only", []rule{{"head-bucket", "", nil}, {"s3 cp", `{"version":4,"resources":[],"outputs":{"vpc_id":{"value":"vpc-1"}}}`, nil}}, stateContents{Outputs: 1}, true, true},
		{"no object", []rule{{"head-bucket", "", nil}, {"s3 cp", "", errors.New("exit 1: fatal error: An error occurred (404) when calling the HeadObject operation: Key does not exist")}}, stateContents{}, false, true},
		{"no bucket", []rule{{"head-bucket", "", errors.New("An error occurred (404) when calling the HeadBucket operation: Not Found")}}, stateContents{}, false, true},
		{"foreign bucket", []rule{{"head-bucket", "", errors.New("An error occurred (403) Forbidden")}}, stateContents{}, false, false},
		{"denied", []rule{{"head-bucket", "", nil}, {"s3 cp", "", errors.New("AccessDenied")}}, stateContents{}, false, false},
		{"garbage", []rule{{"head-bucket", "", nil}, {"s3 cp", "<html>", nil}}, stateContents{}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr := &scriptRunner{rules: append([]rule{id}, tc.rules...)}
			a := newAWSCloud("p", sr.run)
			a.Identity()
			sc, found, err := a.ReadState("b", "darkplum/aws/terraform.tfstate", "us-east-2")
			if sc != tc.want || found != tc.found || (err == nil) != tc.ok {
				t.Fatalf("got %+v %v %v", sc, found, err)
			}
			if tc.name == "outputs only" && sc.empty() {
				t.Error("a state with outputs is not empty")
			}
			for _, c := range sr.calls {
				if strings.Contains(c, "s3 cp") && !strings.Contains(c, "s3 cp s3://b/darkplum/aws/terraform.tfstate - ") {
					t.Errorf("not streamed to stdout: %s", c)
				}
				if strings.Contains(c, "head-bucket") && !strings.Contains(c, "--expected-bucket-owner "+testAccount) {
					t.Errorf("no owner assertion: %s", c)
				}
			}
		})
	}
}
