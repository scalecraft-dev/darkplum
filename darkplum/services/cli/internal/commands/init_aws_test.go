package commands

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptRunner answers aws invocations by substring match on the joined
// arguments; anything unmatched fails, and every call is recorded so tests can
// assert nothing mutating ran.
type scriptRunner struct {
	rules []rule
	calls []string
}

type rule struct {
	match string
	out   string
	err   error
}

func (s *scriptRunner) run(name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, line)
	for _, r := range s.rules {
		if strings.Contains(line, r.match) {
			return r.out, r.err
		}
	}
	return "", errors.New("unscripted: " + line)
}

func (s *scriptRunner) mutating() []string {
	var m []string
	for _, c := range s.calls {
		for _, verb := range []string{" create-", " put-", " enable-", " delete-"} {
			if strings.Contains(c, verb) {
				m = append(m, c)
			}
		}
	}
	return m
}

func TestAWSIdentityAndProfileArgs(t *testing.T) {
	sr := &scriptRunner{rules: []rule{{"sts get-caller-identity", `{"UserId":"X","Account":"123456789012","Arn":"arn:aws:iam::123456789012:root"}`, nil}}}
	a := newAWSCloud("scalecraft", sr.run)
	id, err := a.Identity()
	if err != nil || id.Account != "123456789012" || !id.isRoot() {
		t.Fatalf("%+v %v", id, err)
	}
	if !strings.Contains(sr.calls[0], "--profile scalecraft") {
		t.Errorf("profile not passed: %s", sr.calls[0])
	}
}

func TestOfferedZonesIntersects(t *testing.T) {
	sr := &scriptRunner{rules: []rule{
		{"Values=g5.xlarge", `["us-east-2a","us-east-2b"]`, nil},
		{"Values=m5.large", `["us-east-2c","us-east-2b","us-east-2a"]`, nil},
	}}
	zs, err := newAWSCloud("", sr.run).OfferedZones("us-east-2", "m5.large", "g5.xlarge")
	if err != nil || strings.Join(zs, ",") != "us-east-2a,us-east-2b" {
		t.Fatalf("%v %v", zs, err)
	}
}

func TestNameCollisions(t *testing.T) {
	notFound := func(code string) error { return errors.New("exit 254: An error occurred (" + code + ")") }
	free := &scriptRunner{rules: []rule{
		{"eks describe-cluster", "", notFound("ResourceNotFoundException")},
		{"ec2 describe-vpcs", "0", nil},
		{"rds describe-db-instances", "", notFound("DBInstanceNotFound")},
		{"logs describe-log-groups", "0", nil},
	}}
	for _, r := range newAWSCloud("", free.run).NameCollisions("us-east-2", "darkplum") {
		if r.status != checkPass {
			t.Errorf("free account flagged: %+v", r)
		}
	}
	busy := &scriptRunner{rules: []rule{
		{"eks describe-cluster", "ACTIVE", nil},
		{"ec2 describe-vpcs", "1", nil},
		{"rds describe-db-instances", "1", nil},
		{"logs describe-log-groups", "1", nil},
	}}
	st := map[string]checkStatus{}
	for _, r := range newAWSCloud("", busy.run).NameCollisions("us-east-2", "darkplum") {
		st[r.name] = r.status
	}
	if st["cluster name free"] != checkFail || st["vpc name free"] != checkFail || st["database name free"] != checkFail {
		t.Errorf("collisions not failed: %v", st)
	}
	if st["log group"] != checkWarn {
		t.Errorf("leftover log group must warn, not fail: %v", st)
	}
	denied := &scriptRunner{rules: []rule{{"", "", errors.New("AccessDeniedException")}}}
	for _, r := range newAWSCloud("", denied.run).NameCollisions("us-east-2", "darkplum") {
		if r.status != checkWarn {
			t.Errorf("unreadable must warn, got %+v", r)
		}
	}
	if m := busy.mutating(); len(m) != 0 {
		t.Errorf("collision check mutated: %v", m)
	}
}

func TestBucketCommandsHardenBucket(t *testing.T) {
	arn := "arn:aws:kms:us-east-2:123456789012:key/k"
	joined := ""
	for _, c := range bucketCommands("acme", testAccount, "b", "us-east-2", arn, false) {
		line := formatAWS(c)
		joined += line + "\n"
		if c[3] != "create-bucket" && !strings.Contains(line, "--expected-bucket-owner "+testAccount) {
			t.Errorf("missing expected-bucket-owner: %s", line)
		}
	}
	for _, want := range []string{"--profile acme", "LocationConstraint=us-east-2", "BlockPublicAcls=true", "RestrictPublicBuckets=true",
		"Status=Enabled", "aws:kms", arn, "BucketKeyEnabled", "aws:SecureTransport", "DenyWrongKmsKey", "s3:x-amz-server-side-encryption-aws-kms-key-id"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(formatAWS(bucketCommands("", testAccount, "b", "us-east-1", arn, false)[0]), "LocationConstraint") {
		t.Error("us-east-1 must not pass a LocationConstraint")
	}
	// An existing bucket is neither re-created nor re-keyed.
	for _, c := range bucketCommands("", testAccount, "b", "us-east-2", arn, true) {
		if c[1] == "create-bucket" || c[1] == "put-bucket-encryption" {
			t.Errorf("existing bucket must not run %s", c[1])
		}
	}
}

const kmsARN = "arn:aws:kms:us-east-2:123456789012:key/k"

func TestInspectFreshAndReadOnly(t *testing.T) {
	sr := &scriptRunner{rules: []rule{
		{"sts get-caller-identity", `{"Account":"123456789012","Arn":"arn:aws:iam::123456789012:role/x"}`, nil},
		{"kms describe-key", "", errors.New("NotFoundException")},
		{"s3api head-bucket", "", errors.New("404 Not Found")},
	}}
	a := newAWSCloud("p", sr.run)
	a.Identity()
	st, err := a.Inspect("bkt", "us-east-2")
	if err != nil || st.KeyARN != "" || st.BucketExists || len(st.Checks) != 2 {
		t.Fatalf("%+v %v", st, err)
	}
	if !strings.Contains(sr.calls[len(sr.calls)-1], "--expected-bucket-owner 123456789012") {
		t.Errorf("head-bucket lacks owner assertion: %v", sr.calls)
	}
	if m := sr.mutating(); len(m) != 0 {
		t.Errorf("Inspect mutated: %v", m)
	}
}

func TestInspectExistingBucketKeyRules(t *testing.T) {
	base := func(aliasARN, bucketKey string) *scriptRunner {
		rules := []rule{{"sts get-caller-identity", `{"Account":"123456789012","Arn":"x"}`, nil},
			{"s3api head-bucket", "", nil},
			{"get-bucket-encryption", `{"SSEAlgorithm":"aws:kms","KMSMasterKeyID":"` + bucketKey + `"}`, nil}}
		if aliasARN == "" {
			rules = append(rules, rule{"--key-id alias/darkplum-tfstate", "", errors.New("NotFoundException")})
		} else {
			rules = append(rules, rule{"--key-id alias/darkplum-tfstate", aliasARN, nil})
		}
		return &scriptRunner{rules: append(rules, rule{"kms describe-key", bucketKey, nil})}
	}
	// alias absent: adopt the bucket's key, never create another.
	sr := base("", kmsARN)
	a := newAWSCloud("", sr.run)
	a.Identity()
	if st, err := a.Inspect("b", "us-east-2"); err != nil || st.KeyARN != kmsARN || !st.BucketExists {
		t.Fatalf("%+v %v", st, err)
	}
	// alias and bucket disagree: refuse with an explanation.
	sr = base("arn:aws:kms:us-east-2:123456789012:key/other", kmsARN)
	a = newAWSCloud("", sr.run)
	a.Identity()
	if _, err := a.Inspect("b", "us-east-2"); err == nil || !strings.Contains(err.Error(), "never replaces") {
		t.Fatalf("mismatch must fail, got %v", err)
	}
	// bucket in another account: head-bucket 403 is an error, not "exists".
	sr = &scriptRunner{rules: []rule{{"sts get-caller-identity", `{"Account":"123456789012","Arn":"x"}`, nil}, {"--key-id alias", kmsARN, nil}, {"s3api head-bucket", "", errors.New("403 Forbidden")}}}
	a = newAWSCloud("", sr.run)
	a.Identity()
	if _, err := a.Inspect("b", "us-east-2"); err == nil {
		t.Error("foreign bucket accepted")
	}
}

func TestCreateBackendSequence(t *testing.T) {
	sr := &scriptRunner{rules: []rule{
		{"sts get-caller-identity", `{"Account":"123456789012","Arn":"x"}`, nil},
		{"kms create-key", kmsARN, nil}, {"kms create-alias", "", nil}, {"kms enable-key-rotation", "", nil}, {"s3api", "", nil}}}
	a := newAWSCloud("p", sr.run)
	a.Identity()
	got, err := a.CreateBackend("bkt", "us-east-2", backendState{})
	if err != nil || got != kmsARN {
		t.Fatalf("%q %v", got, err)
	}
	order := strings.Join(sr.calls, "\n")
	for _, want := range []string{"create-key", "create-alias", "enable-key-rotation", "create-bucket", "put-public-access-block", "put-bucket-versioning", "put-bucket-encryption", "put-bucket-policy"} {
		if !strings.Contains(order, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Reuse: an existing key and bucket create nothing.
	sr = &scriptRunner{rules: []rule{{"sts get-caller-identity", `{"Account":"123456789012","Arn":"x"}`, nil}, {"s3api", "", nil}}}
	a = newAWSCloud("p", sr.run)
	a.Identity()
	if _, err := a.CreateBackend("bkt", "us-east-2", backendState{KeyARN: kmsARN, BucketExists: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range sr.calls {
		if strings.Contains(c, "create-key") || strings.Contains(c, "create-bucket") || strings.Contains(c, "put-bucket-encryption") {
			t.Errorf("reuse path ran: %s", c)
		}
	}
}

func TestCreateBackendRollsBackKeyWhenAliasFails(t *testing.T) {
	sr := &scriptRunner{rules: []rule{
		{"sts get-caller-identity", `{"Account":"123456789012","Arn":"x"}`, nil},
		{"kms create-key", kmsARN, nil},
		{"kms create-alias", "", errors.New("LimitExceeded")},
		{"kms schedule-key-deletion", "", nil}}}
	a := newAWSCloud("", sr.run)
	a.Identity()
	if _, err := a.CreateBackend("bkt", "us-east-2", backendState{}); err == nil {
		t.Fatal("want error")
	}
	last := sr.calls[len(sr.calls)-1]
	if !strings.Contains(last, "schedule-key-deletion") || !strings.Contains(last, kmsARN) {
		t.Errorf("new key not rolled back: %v", sr.calls)
	}
	for _, c := range sr.calls {
		if strings.Contains(c, "s3api") {
			t.Errorf("bucket touched after key failure: %s", c)
		}
	}
}

func TestRegionNotEnabledDetection(t *testing.T) {
	if !regionNotEnabled(errors.New("An error occurred (AuthFailure) when calling the DescribeAvailabilityZones")) {
		t.Error("AuthFailure")
	}
	if regionNotEnabled(errors.New("timeout")) || regionNotEnabled(nil) {
		t.Error("false positive")
	}
}

func TestSessionLifetime(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(m int) func(string) (time.Time, error) {
		return func(string) (time.Time, error) { return now.Add(time.Duration(m) * time.Minute), nil }
	}
	if r := sessionResult(at(30), "", 90, now); r.status != checkFail || !strings.Contains(r.hint, "aws sso login") {
		t.Errorf("30 min left must fail: %+v", r)
	}
	if r := sessionResult(at(120), "", 90, now); r.status != checkPass {
		t.Errorf("120 min left must pass: %+v", r)
	}
	if r := sessionResult(at(5), "", 0, now); r.status != checkPass {
		t.Errorf("report-only mode must not fail: %+v", r)
	}
	if r := sessionResult(func(string) (time.Time, error) { return time.Time{}, nil }, "", 90, now); r.status != checkPass {
		t.Errorf("static keys must pass: %+v", r)
	}
	if r := sessionResult(func(string) (time.Time, error) { return time.Time{}, errors.New("x") }, "", 90, now); r.status != checkWarn {
		t.Errorf("unreadable must warn: %+v", r)
	}
}

func TestAccountMatch(t *testing.T) {
	if r := accountMatchResult("111111111111", []string{"222222222222"}); r.status != checkFail {
		t.Errorf("wrong account must fail: %+v", r)
	}
	if r := accountMatchResult("111111111111", []string{"222222222222", "111111111111"}); r.status != checkPass {
		t.Errorf("%+v", r)
	}
	if r := accountMatchResult("111111111111", nil); r.status != checkWarn {
		t.Errorf("unpinned must warn: %+v", r)
	}
}

func TestOfferingsCheck(t *testing.T) {
	sr := &scriptRunner{rules: []rule{{"describe-instance-type-offerings", `["us-east-2a"]`, nil}}}
	r := checkOfferings(newAWSCloud("", sr.run), "us-east-2", []string{"us-east-2a", "us-east-2b"}, []string{"g5.xlarge"})
	if r.status != checkFail || !strings.Contains(r.detail, "us-east-2b") {
		t.Errorf("%+v", r)
	}
}

func TestGPUQuotaParameterisedByInstance(t *testing.T) {
	if r := gpuQuotaResult("4", nil, "us-east-2", "g5.xlarge"); r.status != checkPass {
		t.Errorf("%+v", r)
	}
	if r := gpuQuotaResult("4", nil, "us-east-2", "g5.2xlarge"); r.status != checkFail || !strings.Contains(r.detail, "8") {
		t.Errorf("g5.2xlarge needs 8 vCPUs: %+v", r)
	}
	if r := gpuQuotaResult("", errors.New("x"), "us-east-2", "g5.xlarge"); r.status != checkWarn {
		t.Errorf("%+v", r)
	}
}

func TestTFVarsRoundTripThroughDoctorReader(t *testing.T) {
	s := &settings{profile: "p", account: testAccount, region: "eu-west-1", cluster: "c", azs: []string{"eu-west-1a", "eu-west-1b"}, geo: "eu",
		gpu: true, gpuCount: 2, gpuInstance: "g5.2xlarge", model: modelCatalog[0]}
	p := filepath.Join(t.TempDir(), "terraform.tfvars")
	if err := os.WriteFile(p, []byte(renderTFVars(s.tfvars(), fixedNow)), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := readTFVars(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Profile != "p" || info.Region != "eu-west-1" || info.GPUType != "g5.2xlarge" || info.GPUDesired != 2 ||
		strings.Join(info.AccountIDs, ",") != testAccount || strings.Join(info.AZs, ",") != "eu-west-1a,eu-west-1b" {
		t.Errorf("%+v", info)
	}
	if _, err := readTFVars(p + ".missing"); err == nil {
		t.Error("missing tfvars must error")
	}
}

// The CI guard script (.github/scripts/check-tfvars-docs.py) must catch every
// phrasing, including wrapped lines, and honour only the explicit marker block.
func TestTFVarsDocsGuard(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	script := filepath.Join("..", "..", "..", "..", "..", ".github", "scripts", "check-tfvars-docs.py")
	run := func(content string) error {
		p := filepath.Join(t.TempDir(), "doc.md")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return exec.Command("python3", "-I", script, p).Run()
	}
	bad := []string{
		"cp terraform.tfvars.example terraform.tfvars   # edit",
		"Then edit `terraform/aws/terraform.tfvars` and set the region.",
		"Open the file terraform.tfvars in your editor.",
		"Run vim terraform/aws/terraform.tfvars first.",
		"nano terraform.tfvars",
		"Copy the example over to\nterraform.tfvars before you deploy.",
		"Set the region in your terraform.tfvars and edit it by hand.",
	}
	for _, b := range bad {
		if run(b+"\n") == nil {
			t.Errorf("guard missed: %q", b)
		}
	}
	good := "Configuration lives in terraform.tfvars, written by darkplum init. Re-run init to change it.\n"
	if err := run(good); err != nil {
		t.Errorf("guard rejected an init-based sentence: %v", err)
	}
	marked := "<!-- tfvars-manual:begin -->\ncp terraform.tfvars.example terraform.tfvars\n<!-- tfvars-manual:end -->\n"
	if err := run(marked); err != nil {
		t.Errorf("marker block not honoured: %v", err)
	}
	if run("cp terraform.tfvars.example terraform.tfvars\n<!-- tfvars-manual:begin -->\n<!-- tfvars-manual:end -->\n") == nil {
		t.Error("text outside the marker block must still fail")
	}
	// The repository itself is clean.
	if out, err := exec.Command("python3", "-I", script).CombinedOutput(); err != nil {
		t.Errorf("repository docs fail the guard:\n%s", out)
	}
}
