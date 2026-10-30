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
	// The new install's key was checked before it was taken.
	if strings.Join(cloud.reads, ",") != bucketN("ffffff")+"/darkplum/aws/darkplum/terraform.tfstate" {
		t.Errorf("key not checked: %v", cloud.reads)
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

func TestInitStateExistingBackendKeptByteForByte(t *testing.T) {
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
}

func TestInitStateKeyCollision(t *testing.T) {
	taken := func() *fakeCloud {
		c := newFakeCloud()
		c.buckets = []stateBucket{{Name: bucketN("ffffff")}}
		c.state = backendState{KeyARN: fakeKMS, BucketExists: true}
		c.objects = map[string]int{bucketN("ffffff") + "/darkplum/aws/darkplum/terraform.tfstate": 42}
		return c
	}
	dir := t.TempDir()
	cloud := taken()
	env, out := testEnv(t, cloud, nil, "-")
	err := s3Cmd(dir, nil).execute(env)
	if err == nil || !strings.Contains(err.Error(), "another install named darkplum already keeps state here") {
		t.Fatalf("want collision refusal, got %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 || cloud.createCalls != 0 {
		t.Errorf("wrote %v / created %d", entries, cloud.createCalls)
	}

	// An emptied state (destroyed install of the same name) is reused.
	cloud = taken()
	cloud.objects[bucketN("ffffff")+"/darkplum/aws/darkplum/terraform.tfstate"] = 0
	env, _ = testEnv(t, cloud, nil, "-")
	if err := s3Cmd(t.TempDir(), nil).execute(env); err != nil {
		t.Fatalf("empty state at the key must be reusable: %v", err)
	}

	// Interactive: another name is asked for and lands in both files.
	dir = t.TempDir()
	env, out = testEnv(t, taken(), nil, "\ndarkplum-2\n\n")
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
	sr := &scriptRunner{rules: []rule{
		{"sts get-caller-identity", `{"Account":"` + testAccount + `","Arn":"x"}`, nil},
		{"s3api list-buckets", `[{"Name":"` + good + `","Created":"2026-01-01T00:00:00+00:00"},{"Name":"` + newer + `","Created":"2026-06-01T00:00:00+00:00"},` +
			`{"Name":"` + unversioned + `","Created":"2026-07-01T00:00:00+00:00"},{"Name":"` + otherKey + `","Created":"2026-08-01T00:00:00+00:00"}]`, nil},
		{"--key-id alias/darkplum-tfstate", kmsARN, nil},
		{"get-bucket-versioning --bucket " + unversioned, "Suspended", nil},
		{"get-bucket-versioning", "Enabled", nil},
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
	if len(got) != 2 || got[0].Name != newer || got[1].Name != good {
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
		n         int
		found, ok bool
	}{
		{"counts", []rule{{"head-bucket", "", nil}, {"s3 cp", `{"resources":[{},{},{}]}`, nil}}, 3, true, true},
		{"empty", []rule{{"head-bucket", "", nil}, {"s3 cp", `{"version":4,"resources":[]}`, nil}}, 0, true, true},
		{"no object", []rule{{"head-bucket", "", nil}, {"s3 cp", "", errors.New("exit 1: fatal error: An error occurred (404) when calling the HeadObject operation: Key does not exist")}}, 0, false, true},
		{"no bucket", []rule{{"head-bucket", "", errors.New("An error occurred (404) when calling the HeadBucket operation: Not Found")}}, 0, false, true},
		{"foreign bucket", []rule{{"head-bucket", "", errors.New("An error occurred (403) Forbidden")}}, 0, false, false},
		{"denied", []rule{{"head-bucket", "", nil}, {"s3 cp", "", errors.New("AccessDenied")}}, 0, false, false},
		{"garbage", []rule{{"head-bucket", "", nil}, {"s3 cp", "<html>", nil}}, 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr := &scriptRunner{rules: append([]rule{id}, tc.rules...)}
			a := newAWSCloud("p", sr.run)
			a.Identity()
			n, found, err := a.StateResources("b", "darkplum/aws/terraform.tfstate", "us-east-2")
			if n != tc.n || found != tc.found || (err == nil) != tc.ok {
				t.Fatalf("got %d %v %v", n, found, err)
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
