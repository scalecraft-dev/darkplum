package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBackendTF(t *testing.T, dir, bucket, region string) string {
	t.Helper()
	p := filepath.Join(dir, "backend.hcl")
	body := renderBackend(backendSpec{Bucket: bucket, Key: legacyStateKey, Region: region, KMSKeyARN: kmsARN})
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInitS3ReRunReusesBucketFromBackendTF(t *testing.T) {
	dir := t.TempDir()
	existing := "darkplum-tfstate-" + testAccount + "-us-east-2-ffffff"
	writeBackendTF(t, dir, existing, "us-east-2")
	cloud := newFakeCloud()
	cloud.state = backendState{KeyARN: kmsARN, BucketExists: true}
	env, out := testEnv(t, cloud, nil, "-")
	if err := yesCmd(dir, nil).execute(env); err != nil {
		t.Fatalf("a reused backend.hcl is not an overwrite, --yes must succeed: %v\n%s", err, out)
	}
	got := readFile(t, filepath.Join(dir, "backend.hcl"))
	if !strings.Contains(got, existing) || strings.Contains(got, "a1b2c3") {
		t.Errorf("did not reuse the bucket:\n%s", got)
	}
	if !cloud.createdWith.BucketExists {
		t.Error("CreateBackend must be told the bucket exists")
	}
	if !strings.Contains(out.String(), "S3 "+existing+" (reused; same bucket, key and region)") || !strings.Contains(out.String(), "unrecoverable") {
		t.Errorf("output lacks reuse note or KMS warning:\n%s", out)
	}
}

func TestInitS3IgnoresBackendTFFromAnotherAccount(t *testing.T) {
	dir := t.TempDir()
	writeBackendTF(t, dir, "darkplum-tfstate-999999999999-us-east-2-ffffff", "us-east-2")
	env, _ := testEnv(t, newFakeCloud(), nil, "-")
	err := yesCmd(dir, nil).execute(env)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("a foreign backend.hcl is an overwrite and needs --force, got %v", err)
	}
}

func TestInitLocalStateBlocksBackendUnderYes(t *testing.T) {
	dir := t.TempDir()
	state := `{"version":4,"resources":[{"type":"aws_vpc"},{"type":"aws_eks_cluster"}]}`
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	cloud := newFakeCloud()
	env, out := testEnv(t, cloud, nil, "-")
	err := yesCmd(dir, func(c *InitCmd) { c.Force = true }).execute(env)
	if err == nil || !strings.Contains(err.Error(), "local state") {
		t.Fatalf("--yes must refuse over live local state, got %v", err)
	}
	if !strings.Contains(out.String(), "terraform init -migrate-state") {
		t.Errorf("exact command not printed:\n%s", out)
	}
	if cloud.createCalls != 0 {
		t.Error("bucket created before the local-state acknowledgement")
	}
	if _, err := os.Stat(filepath.Join(dir, "backend.hcl")); err == nil {
		t.Error("backend.hcl written")
	}
	// Interactive and confirmed: proceeds. Answers: networks (default), confirm local state (y), final confirm.
	env, _ = testEnv(t, newFakeCloud(), nil, "\ny\n\n")
	c := &InitCmd{Cloud: "aws", Dir: dir, ClusterName: "darkplum", GPUCount: 1, Region: "us-east-2", Mode: "bedrock", Profile: "acme", Domain: "acme.com"}
	if err := c.execute(env); err != nil {
		t.Fatal(err)
	}
	// An empty state (no resources) or invalid JSON is not "live".
	for _, body := range []string{`{"resources":[]}`, `not json`} {
		d := t.TempDir()
		_ = os.WriteFile(filepath.Join(d, "terraform.tfstate"), []byte(body), 0o600)
		if n := localStateResources(d); n != 0 {
			t.Errorf("%s counted %d", body, n)
		}
	}
}

func TestBakFilesAreAlways0600(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "terraform.tfvars")
	_ = os.WriteFile(p, []byte("old\n"), 0o600)
	_ = os.WriteFile(p+".bak", []byte("older\n"), 0o644)
	if err := writeFileAtomic(p, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p + ".bak")
	if st.Mode().Perm() != 0o600 {
		t.Errorf("pre-existing .bak kept mode %v", st.Mode().Perm())
	}
	if readFile(t, p+".bak") != "old\n" {
		t.Error(".bak not refreshed")
	}
}

func TestInitRegionRows(t *testing.T) {
	// Opt-in region that is not enabled: the exact copy, no raw AWS text, nothing written.
	cloud := newFakeCloud()
	cloud.regionErr = errors.New("exit status 254: An error occurred (AuthFailure) when calling the DescribeAvailabilityZones operation")
	dir := t.TempDir()
	env, out := testEnv(t, cloud, nil, "-")
	err := yesCmd(dir, func(c *InitCmd) { c.Region = "af-south-1" }).execute(env)
	if !IsReported(err) {
		t.Fatalf("want a reported error, got %v", err)
	}
	for _, want := range []string{"✗ region", "af-south-1 is not enabled for account " + testAccount, "→ enable it under Account → AWS Regions, or choose another region"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "AuthFailure") {
		t.Error("raw AWS error text leaked")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
	// Interactive: re-asks the region and carries on.
	cloud2 := &regionFlaky{fakeCloud: newFakeCloud(), bad: "af-south-1"}
	env, out = testEnv(t, cloud2, nil, "af-south-1\nus-east-2\n1\n\n\n")
	c := &InitCmd{Cloud: "aws", Dir: t.TempDir(), ClusterName: "darkplum", GPUCount: 1, Profile: "acme", Domain: "acme.com"}
	if err := c.execute(env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "af-south-1 is not enabled") {
		t.Errorf("no row for the bad region:\n%s", out)
	}
}

type regionFlaky struct {
	*fakeCloud
	bad string
}

func (r *regionFlaky) CheckRegion(region string) error {
	if region == r.bad {
		return errors.New("AuthFailure")
	}
	return nil
}

func TestInitRegionNoteOnlyWhenDefaultAccepted(t *testing.T) {
	env, out := testEnv(t, newFakeCloud(), nil, "\n1\n\n\n") // Enter accepts the profile region
	c := &InitCmd{Cloud: "aws", Dir: t.TempDir(), ClusterName: "darkplum", GPUCount: 1, Profile: "acme", Domain: "acme.com"}
	if err := c.execute(env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "us-east-2 (from profile)") {
		t.Errorf("default accepted: want the note:\n%s", out)
	}
	cloud := newFakeCloud()
	env, out = testEnv(t, cloud, nil, "eu-west-1\n1\n\n\n")
	c = &InitCmd{Cloud: "aws", Dir: t.TempDir(), ClusterName: "darkplum", GPUCount: 1, Profile: "acme", Domain: "acme.com"}
	cloud.zones = []string{"eu-west-1a", "eu-west-1b"}
	if err := c.execute(env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "eu-west-1 (from profile)") {
		t.Errorf("typed region must not claim to come from the profile:\n%s", out)
	}
}

func TestInitLocalFlagsValidatedBeforeAnyNetworkCall(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*InitCmd)
		want string
	}{
		{"cluster name", func(c *InitCmd) { c.ClusterName = "Bad_Name" }, "invalid --cluster-name"},
		{"gpu count", func(c *InitCmd) { c.GPUCount = 99 }, "invalid --gpu-count"},
		{"region", func(c *InitCmd) { c.Region = "mars" }, "invalid --region"},
		{"model id", func(c *InitCmd) { c.Model = "no-slash" }, "invalid --model"},
		{"revision", func(c *InitCmd) { c.ModelRevision = "v1" }, "invalid --model-revision"},
		{"gpu instance", func(c *InitCmd) { c.GPUInstance = "p5.48xlarge" }, "unknown --gpu-instance"},
		{"offline custom", func(c *InitCmd) { c.Offline = true; c.Model = "acme/x" }, "--offline needs --model-revision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			called := &countingCloud{fakeCloud: newFakeCloud()}
			env, out := testEnv(t, called, nil, "-")
			err := yesCmd(dir, tc.mod).execute(env)
			if !IsReported(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want reported %q", err, tc.want)
			}
			if called.n != 0 {
				t.Errorf("%d cloud calls before local validation failed", called.n)
			}
			if !strings.Contains(out.String(), "✗") || !strings.Contains(out.String(), "→") {
				t.Errorf("want a ✗ row with a → hint:\n%s", out)
			}
		})
	}
}

type countingCloud struct {
	*fakeCloud
	n int
}

func (c *countingCloud) Identity() (identity, error) { c.n++; return c.fakeCloud.Identity() }

func TestInitDryRunNextLineAndPreChecks(t *testing.T) {
	cloud := newFakeCloud()
	cloud.state = backendState{Checks: []string{"aws kms describe-key --key-id alias/darkplum-tfstate", "aws s3api head-bucket --bucket b"}}
	env, out := testEnv(t, cloud, nil, "-")
	if err := yesCmd(t.TempDir(), func(c *InitCmd) { c.DryRun = true }).execute(env); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Next: re-run without --dry-run to write these files.", "read-only pre-checks that ran", "describe-key", "head-bucket", "unrecoverable"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out.String(), "Next: darkplum deploy") {
		t.Error("a dry run must not point at darkplum deploy")
	}
}

func TestInitModeChoicesAreAlignedWithCosts(t *testing.T) {
	env, out := testEnv(t, newFakeCloud(), nil, "1\n\n\n")
	c := &InitCmd{Cloud: "aws", Dir: t.TempDir(), ClusterName: "darkplum", GPUCount: 1, Profile: "acme", Region: "us-east-2", Domain: "acme.com"}
	if err := c.execute(env); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1) Bedrock only     no GPU · ~US$10/day base · ~20 min · recommended for a trial",
		"2) Self-hosted GPU  base + ~US$1/h per g5.xlarge · ~35-50 min",
		"Switch later by re-running darkplum init.",
		"✓ 10 passed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestInitAbortIsReportedOnce(t *testing.T) {
	if !IsReported(errAborted) {
		t.Error("errAborted must be silent in main")
	}
	env, out := testEnv(t, newFakeCloud(), nil, "")
	c := &InitCmd{Cloud: "aws", Dir: t.TempDir(), ClusterName: "darkplum", GPUCount: 1, Profile: "acme"}
	_ = c.execute(env)
	if strings.Count(out.String(), "nothing was written") != 0 {
		t.Error("execute must leave the abort line to Run")
	}
}

func TestInitModelFieldsAlwaysExplicit(t *testing.T) {
	dir := t.TempDir()
	env, _ := testEnv(t, newFakeCloud(), nil, "-")
	if err := yesCmd(dir, nil).execute(env); err != nil { // bedrock mode
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(dir, "terraform.tfvars"))
	for _, k := range []string{"model_name", "served_model_name", "model_revision"} {
		if !strings.Contains(got, k+" = ") {
			t.Errorf("%s not written in bedrock mode:\n%s", k, got)
		}
	}
}

func TestInitErrorsAreReportedRows(t *testing.T) {
	cloud := newFakeCloud()
	cloud.id.Arn = "arn:aws:iam::" + testAccount + ":root"
	env, out := testEnv(t, cloud, nil, "-")
	err := yesCmd(t.TempDir(), nil).execute(env)
	if !IsReported(err) || !strings.Contains(out.String(), "✗ aws identity") {
		t.Errorf("root refusal must be a row: %v\n%s", err, out)
	}
	env, out = testEnv(t, newFakeCloud(), nil, "-")
	err = (&InitCmd{Cloud: "gcp", Yes: true}).execute(env)
	if !IsReported(err) || !strings.Contains(out.String(), "✗ gcp terraform root") {
		t.Errorf("%v\n%s", err, out)
	}
}

// A backend.tf in the Terraform root is a Makefile-era install. init refuses to
// write beside it and says how to take it over; nothing is written or created.
func TestInitRefusesAMakefileEraRoot(t *testing.T) {
	dir := t.TempDir()
	legacy := legacyBackendTF(backendSpec{Bucket: "darkplum-tfstate-" + testAccount + "-us-east-2-ffffff", Key: legacyStateKey, Region: "us-east-2", KMSKeyARN: kmsARN})
	if err := os.WriteFile(filepath.Join(dir, "backend.tf"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cloud := newFakeCloud()
	env, out := testEnv(t, cloud, nil, "-")
	err := yesCmd(dir, nil).execute(env)
	if err == nil || !strings.Contains(out.String(), "darkplum init --adopt") {
		t.Fatalf("want a refusal pointing at --adopt, got %v\n%s", err, out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || cloud.createCalls != 0 {
		t.Errorf("a refusal wrote %v / created %d", entries, cloud.createCalls)
	}
}
