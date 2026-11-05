package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/cli/internal/iamcheck"
)

const libBucket = "dp-trial-library-123456789012"
const libURL = "s3://" + libBucket + "/library/v1/"

// ---- terraform outputs ----------------------------------------------------------

func TestParseOutputsReadsTheLibraryOutputsAndTheirAbsenceIsNotAnError(t *testing.T) {
	o, err := parseOutputs([]byte(outputsJSON(0, func(v map[string]any) {
		v["library_store_url"] = libURL
		v["api_role_arn"] = "arn:aws:iam::123456789012:role/dp-trial-api"
	})))
	if err != nil || o.LibraryStoreURL != libURL || o.APIRoleARN != "arn:aws:iam::123456789012:role/dp-trial-api" {
		t.Fatalf("%+v %v", o, err)
	}
	// State applied by an older darkplum: neither output exists.
	o, err = parseOutputs([]byte(outputsJSON(0, nil)))
	if err != nil || o.LibraryStoreURL != "" || o.APIRoleARN != "" {
		t.Fatalf("absent outputs: %+v %v", o, err)
	}
	o, err = parseOutputs([]byte(outputsJSON(0, func(v map[string]any) { v["library_store_url"] = nil })))
	if err != nil || o.LibraryStoreURL != "" {
		t.Fatalf("null output: %+v %v", o, err)
	}
	if o.LibraryBucketEnabled != nil {
		t.Fatalf("library_bucket_enabled absent: want nil, got %v", *o.LibraryBucketEnabled)
	}
	o, err = parseOutputs([]byte(outputsJSON(0, func(v map[string]any) { v["library_bucket_enabled"] = false })))
	if err != nil || o.LibraryBucketEnabled == nil || *o.LibraryBucketEnabled {
		t.Fatalf("library_bucket_enabled=false: %+v %v", o, err)
	}
	if _, err = parseOutputs([]byte(outputsJSON(0, func(v map[string]any) { v["library_bucket_enabled"] = "yes" }))); err == nil {
		t.Fatal("a non-boolean library_bucket_enabled was accepted")
	}
	if _, err = parseOutputs([]byte(outputsJSON(0, func(v map[string]any) { v["library_store_url"] = []string{"x"} }))); err == nil {
		t.Error("a library_store_url of the wrong type was accepted")
	}
}

// ---- deploy -----------------------------------------------------------------------

// apiLibraryEnv reads LIBRARY_STORE_URL off the api Deployment that deploy applied.
func apiLibraryEnv(t *testing.T, h *deployHarness) (string, bool) {
	return apiEnv(t, h, "LIBRARY_STORE_URL")
}

func apiEnv(t *testing.T, h *deployHarness, name string) (string, bool) {
	t.Helper()
	for _, c := range h.k.matching("apply -f -") {
		var l struct {
			Items []struct {
				Kind     string                `json:"kind"`
				Metadata struct{ Name string } `json:"metadata"`
				Spec     struct {
					Template struct {
						Spec struct {
							Containers []struct {
								Env []struct{ Name, Value string } `json:"env"`
							} `json:"containers"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			} `json:"items"`
		}
		if json.Unmarshal(c.stdin, &l) != nil {
			continue
		}
		for _, it := range l.Items {
			if it.Kind != "Deployment" {
				continue
			}
			for _, ct := range it.Spec.Template.Spec.Containers {
				for _, e := range ct.Env {
					if e.Name == name {
						if it.Metadata.Name != "darkplum-api" {
							t.Errorf("%s is on %s", name, it.Metadata.Name)
						}
						return e.Value, true
					}
				}
			}
		}
	}
	return "", false
}

// bucketOn is terraform.tfvars saying library_bucket_enabled = true (the
// harness reads the file at construction; this is the parsed result).
func bucketOn(h *deployHarness) { on := true; h.env.tfv.LibraryBucket = &on }

func bucketOff(h *deployHarness) { off := false; h.env.tfv.LibraryBucket = &off }

func TestDeployGivesTheAPIItsLibraryStore(t *testing.T) {
	h := newDeployHarness(t, 0)
	bucketOn(h)
	h.tf.outputs = outputsJSON(0, func(v map[string]any) {
		v["library_store_url"] = libURL
		v["api_role_arn"] = "arn:aws:iam::123456789012:role/dp-trial-api"
	})
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if v, ok := apiLibraryEnv(t, h); !ok || v != libURL {
		t.Errorf("api LIBRARY_STORE_URL = %q (set=%v), want %q", v, ok, libURL)
	}
	if v, ok := apiEnv(t, h, "LIBRARY_S3_REGION"); !ok || v != "us-east-2" {
		t.Errorf("api LIBRARY_S3_REGION = %q (set=%v), want the install's region us-east-2", v, ok)
	}
	h.mustContain("library store", libURL)
}

func TestDeployWithoutTheLibraryOutputWarnsAndLeavesTheStoreEmpty(t *testing.T) {
	h := newDeployHarness(t, 0)
	if err := h.run(); err != nil {
		t.Fatalf("a missing library output must not fail the deploy: %v\n%s", err, h.out.String())
	}
	if v, ok := apiLibraryEnv(t, h); !ok || v != "" {
		t.Errorf("api LIBRARY_STORE_URL = %q (set=%v), want set and empty", v, ok)
	}
	h.mustContain("no library_store_url output", "Postgres only")
}

// The default install: Terraform says there is no bucket, and that is a pass,
// not a warning. The api's LIBRARY_STORE_URL stays empty.
func TestDeployWithoutABucketIsTheDefaultAndNotAWarning(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.outputs = outputsJSON(0, func(v map[string]any) {
		v["library_bucket_enabled"] = false
		v["library_store_url"] = ""
		v["api_role_arn"] = "arn:aws:iam::123456789012:role/dp-trial-api"
	})
	var got iamcheck.Outputs
	h.env.iam = func(_ context.Context, o iamcheck.Outputs) (iamcheck.Report, error) {
		got = o
		return h.iamRep, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if v, ok := apiLibraryEnv(t, h); !ok || v != "" {
		t.Errorf("api LIBRARY_STORE_URL = %q (set=%v), want set and empty", v, ok)
	}
	h.mustContain("Postgres (no bucket")
	if strings.Contains(h.out.String(), "no library_store_url output") {
		t.Errorf("an install without a bucket was warned about:\n%s", h.out.String())
	}
	if got.LibraryBucketEnabled == nil || *got.LibraryBucketEnabled {
		t.Errorf("iamcheck was not told there is no bucket: %+v", got)
	}
	// With terraform.tfvars silent (what init writes), a bucket a previous
	// install kept is looked for once, so the operator hears about it; here
	// there is none, and nothing is adopted.
	if h.tl.count("s3api head-bucket") != 1 || strings.Contains(h.out.String(), "exists from a previous install") {
		t.Errorf("kept-bucket check: %d lookups\n%s", h.tl.count("s3api head-bucket"), h.out.String())
	}
}

// A bucket is never dropped by a default. An install that has one in its state
// and a terraform.tfvars that does not say is stopped before the plan; saying
// false takes the bucket out of the state (kept, with its files) before the plan,
// the way destroy keeps it.
func TestDeployNeverLetsTheDefaultDeleteAnExistingBucket(t *testing.T) {
	for name, addr := range map[string]string{"counted module": libraryBucketAddr, "pre-optional state": libraryBucketAddrLegacy} {
		t.Run(name+": tfvars silent: refused before anything is planned", func(t *testing.T) {
			h := newDeployHarness(t, 0)
			h.tf.stateOut = "module.vpc.aws_vpc.this\n" + addr + "\n"
			if !IsReported(h.run()) {
				t.Fatalf("deployed over a bucket the tfvars would drop:\n%s", h.out.String())
			}
			if h.tf.ran("plan") || h.tf.ran("apply") || h.writes() != 0 {
				t.Errorf("something was planned or written:\n%s", h.tl)
			}
			for _, c := range rmCommandsOf(h.tf) {
				t.Errorf("the state was changed: %s", c)
			}
			h.mustContain("library_bucket_enabled = true", "library_bucket_enabled = false", "Nothing was applied", h.env.in.TFVars())
		})
		t.Run(name+": tfvars false: the bucket leaves the state and is kept", func(t *testing.T) {
			h := newDeployHarness(t, 0)
			bucketOff(h)
			h.tf.stateOut = "module.vpc.aws_vpc.this\n" + addr + "\n" + strings.Replace(addr, "aws_s3_bucket.this", "aws_s3_bucket_policy.this", 1) + "\n"
			h.tf.outputs = outputsJSON(0, func(v map[string]any) { v["library_bucket_enabled"] = false; v["library_store_url"] = "" })
			if err := h.run(); err != nil {
				t.Fatalf("%v\n%s", err, h.out.String())
			}
			rm := rmCommandsOf(h.tf)
			if len(rm) != 1 || !strings.Contains(rm[0], addr) || !strings.Contains(rm[0], "aws_s3_bucket_policy.this") {
				t.Fatalf("state rm = %v, want the bucket and its policy in one command", rm)
			}
			if i := h.tl.first("terraform state rm"); i < 0 || i > h.tl.first("terraform plan") {
				t.Errorf("the bucket left the state after the plan:\n%s", h.tl)
			}
			if h.tl.count("s3api head-bucket") != 0 || h.tl.count("delete-bucket")+h.tl.count("delete-objects") != 0 {
				t.Errorf("the bucket was looked for or touched:\n%s", h.tl)
			}
			if v, ok := apiLibraryEnv(t, h); !ok || v != "" {
				t.Errorf("api LIBRARY_STORE_URL = %q (set=%v), want set and empty", v, ok)
			}
			h.mustContain("kept with its files")
		})
	}
	t.Run("tfvars silent and no bucket in the state: nothing to decide", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		if len(rmCommandsOf(h.tf)) != 0 {
			t.Error("the state was changed")
		}
		h.mustContain("the Library lives in Postgres")
	})
}

func TestTFVarsReadsLibraryBucketEnabled(t *testing.T) {
	for raw, want := range map[string]string{
		"library_bucket_enabled = true\n":            "true",
		"library_bucket_enabled = false\n":           "false",
		"library_bucket_enabled = true  # keep it\n": "true",
		"library_bucket_enabled = \"false\"\n":       "false",
		"library_bucket_enabled = yes\n":             "raw:yes",
		"library_bucket_enabled = 1\n":               "raw:1",
		"library_bucket_enabled = True\n":            "raw:True",
		"":                                           "unset",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "terraform.tfvars")
		if err := os.WriteFile(p, []byte(pinnedTFVars+raw), 0o600); err != nil {
			t.Fatal(err)
		}
		tf, err := readTFVars(p)
		if err != nil {
			t.Fatal(err)
		}
		got := "unset"
		switch {
		case tf.LibraryBucketRaw != "":
			got = "raw:" + tf.LibraryBucketRaw
		case tf.LibraryBucket != nil:
			got = strconv.FormatBool(*tf.LibraryBucket)
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", raw, got, want)
		}
	}
}

func rmCommandsOf(tf *fakeTF) []string {
	var out []string
	for _, c := range tf.cmds {
		if len(c) > 1 && c[0] == "state" && c[1] == "rm" {
			out = append(out, strings.Join(c, " "))
		}
	}
	return out
}

func TestDeployRefusesAMalformedLibraryStoreBeforeAnythingReachesKubernetes(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.outputs = outputsJSON(0, func(v map[string]any) { v["library_store_url"] = "s3://Not_A_Bucket/library/v1/" })
	if !IsReported(h.run()) {
		t.Fatal("a malformed library store URL was applied")
	}
	if h.writes() != 0 {
		t.Errorf("kubernetes was written to:\n%s", h.tl)
	}
	h.mustContain("library store URL")
}

func TestDeployLayer1ProvesTheLibraryBoundaryWithTheOutputs(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.outputs = outputsJSON(0, func(v map[string]any) {
		v["library_store_url"] = libURL
		v["api_role_arn"] = "arn:aws:iam::123456789012:role/dp-trial-api"
	})
	var got iamcheck.Outputs
	h.env.iam = func(_ context.Context, o iamcheck.Outputs) (iamcheck.Report, error) {
		got = o
		return h.iamRep, nil
	}
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got.LibraryStoreURL != libURL || got.APIRoleARN != "arn:aws:iam::123456789012:role/dp-trial-api" {
		t.Errorf("iamcheck was given %+v", got)
	}
}

func TestDeployLibraryBucketAdoption(t *testing.T) {
	adopts := func(h *deployHarness) bool {
		for _, c := range h.tf.cmds {
			if c[0] == "plan" && strings.Contains(strings.Join(c, " "), "-var adopt_library_bucket=true") {
				return true
			}
		}
		return false
	}
	t.Run("already in the state: not looked for", func(t *testing.T) {
		for _, addr := range []string{libraryBucketAddr, libraryBucketAddrLegacy} {
			h := newDeployHarness(t, 0)
			bucketOn(h)
			h.tf.stateOut = "module.vpc.aws_vpc.this\n" + addr + "\n"
			if err := h.run(); err != nil {
				t.Fatal(err)
			}
			if adopts(h) || h.tl.count("s3api head-bucket") != 0 || len(rmCommandsOf(h.tf)) != 0 {
				t.Errorf("%s: looked for, imported or removed a bucket that is already managed", addr)
			}
		}
	})
	t.Run("tfvars silent: a kept bucket is noticed, never adopted", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.awsf.bucketErr = nil // it exists in the account
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if adopts(h) || h.tl.count("s3api head-bucket") != 1 {
			t.Error("adopted, or did not look for, a bucket a previous install kept")
		}
		h.mustContain(libBucket+" exists from a previous install", "library_bucket_enabled = true", "terraform.tfvars")
	})
	t.Run("tfvars false: a kept bucket is not looked for", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		bucketOff(h)
		h.awsf.bucketErr = nil
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if adopts(h) || h.tl.count("s3api head-bucket") != 0 {
			t.Error("looked for or adopted a bucket the install turned off")
		}
		h.mustContain("the Library lives in Postgres")
	})
	t.Run("tfvars unreadable: refused by name before anything is planned", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		h.env.tfv.LibraryBucketRaw = "yes"
		if !IsReported(h.run()) || h.tf.ran("plan") {
			t.Fatal("deployed with an unreadable library_bucket_enabled")
		}
		h.mustContain(`"yes"`, "not true or false")
	})
	t.Run("kept by an earlier destroy: the plan imports it, owner-checked", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		bucketOn(h)
		h.awsf.bucketErr = nil
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		if !adopts(h) {
			t.Errorf("the plan does not adopt the bucket: %v", h.tf.cmds)
		}
		if h.tf.ran("import") {
			t.Error("ran `terraform import`, which fails on an empty state")
		}
		if h.tl.first("s3api head-bucket --bucket "+libBucket+" --expected-bucket-owner 123456789012") < 0 {
			t.Errorf("the bucket was not looked up by its derived name with the owner pinned:\n%s", h.tl)
		}
		h.mustContain("the plan adopts " + libBucket)
	})
	t.Run("not there: nothing to adopt", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		bucketOn(h)
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if adopts(h) {
			t.Error("asked the plan to import a bucket that does not exist")
		}
	})
	t.Run("forbidden (another account's name, or no access): abort, nothing applied", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		bucketOn(h)
		h.awsf.bucketErr = errors.New("exit status 254: An error occurred (403) when calling the HeadBucket operation: Forbidden")
		if !IsReported(h.run()) || h.tf.ran("plan") || h.tf.ran("apply") {
			t.Fatal("a 403 was read as 'not there'")
		}
		h.mustContain("403")
	})
	t.Run("an expired session while looking: abort", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		bucketOn(h)
		h.awsf.bucketErr = errors.New("ExpiredToken")
		if !IsReported(h.run()) || h.tf.ran("plan") {
			t.Fatal("a failed lookup was read as 'not there'")
		}
	})
	t.Run("the plan would replace the kept bucket: refused even with --confirm-plan", func(t *testing.T) {
		for _, addr := range []string{libraryBucketAddr, libraryBucketAddrLegacy} {
			h := newDeployHarness(t, 0)
			bucketOn(h)
			h.awsf.bucketErr = nil
			h.env.flags.ConfirmPlan = true
			h.tf.planJSON = `{"resource_changes":[{"address":"` + addr + `","change":{"actions":["delete","create"],"importing":{"id":"` + libBucket + `"}}}]}`
			if !IsReported(h.run()) || h.tf.ran("apply") {
				t.Fatalf("%s: applied a plan that deletes the library:\n%s", addr, h.out.String())
			}
			h.mustContain("the Library in it")
		}
	})
	t.Run("the import is the only change: it is applied", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		bucketOn(h)
		h.awsf.bucketErr = nil
		h.tf.planJSON = `{"resource_changes":[{"address":"` + libraryBucketAddr + `","change":{"actions":["no-op"],"importing":{"id":"` + libBucket + `"}}}]}`
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if !h.tf.ran("apply") {
			t.Fatal("an import with nothing to change never reached the state")
		}
	})
}

// The Go side and the shipped Terraform must name the same things: the state check,
// the plan gate and destroy's state rm read these addresses; the import block and
// the bucket name are written in Terraform. `terraform test` cannot see this.
func TestTerraformAndTheCLIAgreeOnTheLibraryBucket(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(terraformDir, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	var block string
	for _, m := range regexp.MustCompile(`(?ms)^import \{\n(.*?)^\}`).FindAllSubmatch(b, -1) {
		if strings.Contains(string(m[1]), "adopt_library_bucket") {
			block = string(m[1])
		}
	}
	for _, want := range []string{
		`(?m)^\s*for_each\s*=\s*var\.adopt_library_bucket\s*&&\s*var\.library_bucket_enabled\b`,
		`(?m)^\s*to\s*=\s*` + regexp.QuoteMeta(libraryBucketAddr) + `\s*$`,
		`(?m)^\s*id\s*=\s*local\.library_bucket_name\s*$`,
	} {
		if !regexp.MustCompile(want).MatchString(block) {
			t.Errorf("the library import block does not match %s:\n%s", want, block)
		}
	}
	// The bucket is opt-in (a counted module), and a state from before that is
	// moved to the counted address rather than replaced: the CLI's legacy address
	// is what such a state holds until then.
	for _, want := range []string{
		`(?ms)^module "library_bucket" \{.*?^\s*count\s*=\s*var\.library_bucket_enabled \? 1 : 0\s*$`,
		`(?ms)^moved \{\s*from\s*=\s*module\.library_bucket\s*to\s*=\s*module\.library_bucket\[0\]\s*\}`,
		`(?m)^variable "library_bucket_enabled" \{`,
	} {
		if !regexp.MustCompile(want).Match(append(append([]byte{}, b...), mustRead(t, filepath.Join(terraformDir, "variables.tf"))...)) {
			t.Errorf("the shipped Terraform does not match %s", want)
		}
	}
	if !strings.HasPrefix(libraryBucketAddr, "module.library_bucket[0].") || !strings.HasPrefix(libraryBucketAddrLegacy, "module.library_bucket.") {
		t.Errorf("addresses: %s %s", libraryBucketAddr, libraryBucketAddrLegacy)
	}
	if !strings.Contains(string(b), `library_bucket_name = "${var.cluster_name}-library-${data.aws_caller_identity.current.account_id}"`) {
		t.Error("main.tf no longer names the bucket <cluster_name>-library-<account_id>, which libraryBucketName derives")
	}
	if got := libraryBucketName("dp-trial", "123456789012"); got != libBucket {
		t.Errorf("libraryBucketName = %q", got)
	}
	// Every address destroy removes from the state is a resource the module declares,
	// and every config resource the module declares for the bucket is in the list.
	mod, err := os.ReadFile(filepath.Join(terraformDir, "modules", "library-bucket", "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^resource "(aws_s3_bucket[a-z_]*)" "(\w+)"`).FindAllStringSubmatch(string(mod), -1) {
		declared["module.library_bucket[0]."+m[1]+"."+m[2]] = true
		declared["module.library_bucket."+m[1]+"."+m[2]] = true
	}
	listed := map[string]bool{}
	for _, a := range libraryAddrs {
		listed[a] = true
		if !declared[a] {
			t.Errorf("destroy removes %s from the state but the module declares no such resource", a)
		}
	}
	for a := range declared {
		if !listed[a] {
			t.Errorf("the module declares %s but destroy would not keep it with the bucket", a)
		}
	}
}

// ---- destroy ----------------------------------------------------------------------

type libVersion struct{ Key, VersionId string }

// fakeLibraryBucket is a versioned bucket as the aws CLI sees it.
type fakeLibraryBucket struct {
	t        *testing.T
	exists   bool
	versions []libVersion
	// refuse makes delete-objects answer this error code for the first batch.
	refuse string
	// deleteBucketErr fails delete-bucket.
	deleteBucketErr error
	calls           []string
}

func (b *fakeLibraryBucket) run(j string) (string, error) {
	b.calls = append(b.calls, j)
	if !strings.Contains(j, "--bucket "+libBucket) {
		b.t.Errorf("an s3api call was not for the library bucket: %s", j)
		return "", fmt.Errorf("wrong bucket")
	}
	switch {
	case strings.Contains(j, "s3api head-bucket"):
		if !strings.Contains(j, "--expected-bucket-owner 123456789012") {
			b.t.Errorf("head-bucket without the owner pinned: %s", j)
		}
		if !b.exists {
			return "", errNoBucket
		}
		return "", nil
	case strings.Contains(j, "s3api list-object-versions"):
		n := min(len(b.versions), 100)
		if n == 0 {
			return "null", nil
		}
		out, _ := json.Marshal(b.versions[:n])
		return string(out), nil
	case strings.Contains(j, "s3api delete-objects"):
		if b.refuse != "" {
			return `"` + b.refuse + `"`, nil
		}
		i := strings.Index(j, "--delete ")
		var body struct{ Objects []libVersion }
		raw := j[i+len("--delete "):]
		raw = raw[:strings.Index(raw, " --output")]
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			b.t.Fatalf("delete body: %v\n%s", err, raw)
		}
		if len(body.Objects) > 100 {
			b.t.Errorf("batch of %d", len(body.Objects))
		}
		b.versions = b.versions[len(body.Objects):]
		return "null", nil
	case strings.Contains(j, "s3api delete-bucket"):
		if len(b.versions) != 0 {
			b.t.Error("delete-bucket on a bucket that still has versions")
		}
		if b.deleteBucketErr != nil {
			return "", b.deleteBucketErr
		}
		b.exists = false
		return "", nil
	}
	return "", fmt.Errorf("unexpected aws %s", j)
}

func (b *fakeLibraryBucket) did(sub string) int {
	n := 0
	for _, c := range b.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func libraryDestroy(t *testing.T, b *fakeLibraryBucket) *destroyHarness {
	h := newDestroyHarness(t)
	b.t = t
	h.aws.s3 = b.run
	h.aws.extra["logs describe-log-groups"] = "0"
	h.aws.extra["rds describe-db-snapshots"] = "[]"
	return h
}

// libraryState is a state holding the bucket under the counted module;
// legacyLibraryState one applied before the bucket was optional.
var libraryState, legacyLibraryState = func() (string, string) {
	s, l := destroyState, destroyState
	for _, a := range libraryAddrs {
		if strings.HasPrefix(a, "module.library_bucket[0].") {
			s += a + "\n"
		} else {
			l += a + "\n"
		}
	}
	return s, l
}()

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rmCommands(h *destroyHarness) []string {
	var out []string
	for _, c := range h.tf.cmds {
		if len(c) > 1 && c[0] == "state" && c[1] == "rm" {
			out = append(out, strings.Join(c, " "))
		}
	}
	return out
}

func TestDestroyKeepsTheLibraryBucketByDefault(t *testing.T) {
	b := &fakeLibraryBucket{exists: true, versions: []libVersion{{"library/v1/org/a.skill.md", "v1"}, {"library/v1/org/a.skill.md", "v2"}}}
	h := libraryDestroy(t, b)
	h.tf.stateOut = libraryState
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	// The bucket and ALL its configuration resources leave the state in ONE command,
	// before the Kubernetes phase and the destroy: a bucket kept while its policy or
	// versioning was destroyed would be left unprotected.
	rm := rmCommands(h)
	var lib string
	for _, c := range rm {
		if strings.Contains(c, libraryBucketAddr) {
			lib = c
		}
	}
	if lib == "" {
		t.Fatalf("the bucket was not removed from the state: %v", rm)
	}
	for _, a := range strings.Fields(strings.TrimPrefix(libraryState, destroyState)) {
		if !strings.Contains(lib, a) {
			t.Errorf("%s was not removed from the state with the bucket: %s", a, lib)
		}
	}
	for _, a := range libraryAddrs {
		if strings.HasPrefix(a, "module.library_bucket.") && strings.Contains(lib, a) {
			t.Errorf("state rm names %s, which the state does not hold", a)
		}
	}
	if i := h.tl.first("terraform state rm " + libraryBucketAddr); i < 0 || i > h.tl.first("terraform destroy") {
		t.Errorf("the bucket left the state after the destroy began:\n%s", h.tl)
	}
	// Nothing in the bucket was touched; the report lists it with the opt-in command.
	if b.did("list-object-versions")+b.did("delete-objects")+b.did("delete-bucket") != 0 {
		t.Errorf("the default destroy deleted from the library bucket: %v", b.calls)
	}
	if len(b.versions) != 2 {
		t.Errorf("versions left = %d, want 2", len(b.versions))
	}
	h.contains("KEPT", "Library bucket s3://"+libBucket, "darkplum destroy --delete-library-data=dp-trial", "removed from the state so the destroy keeps it")
}

// A state applied before the bucket was optional holds it at the unindexed
// address; destroy keeps that bucket too.
func TestDestroyKeepsALegacyLibraryBucket(t *testing.T) {
	b := &fakeLibraryBucket{exists: true, versions: []libVersion{{"library/v1/org/a.skill.md", "v1"}}}
	h := libraryDestroy(t, b)
	h.tf.stateOut = legacyLibraryState
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	var lib string
	for _, c := range rmCommands(h) {
		if strings.Contains(c, "library_bucket") {
			lib = c
		}
	}
	if lib == "" || !strings.Contains(lib, libraryBucketAddrLegacy) || strings.Contains(lib, "[0]") || !strings.Contains(lib, "aws_s3_bucket_policy.this") {
		t.Fatalf("state rm = %q", lib)
	}
	if b.did("delete-objects")+b.did("delete-bucket") != 0 {
		t.Errorf("the bucket was touched: %v", b.calls)
	}
	h.contains("KEPT", "removed from the state so the destroy keeps it")
}

func TestDestroyEmptyBucketIsStillKept(t *testing.T) {
	b := &fakeLibraryBucket{exists: true}
	h := libraryDestroy(t, b)
	h.tf.stateOut = libraryState
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if len(rmCommands(h)) == 0 || b.did("delete-bucket") != 0 {
		t.Errorf("an empty bucket was deleted without the opt-in: %v", b.calls)
	}
	h.contains("Library bucket s3://" + libBucket)
}

func TestDestroyBucketAlreadyRemovedFromTheState(t *testing.T) {
	// A second run after the bucket was kept: nothing of it is in the state.
	b := &fakeLibraryBucket{exists: true}
	h := libraryDestroy(t, b)
	h.tf.stateOut = destroyState
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	for _, c := range rmCommands(h) {
		if strings.Contains(c, "library_bucket") {
			t.Errorf("state rm of what the state does not hold: %s", c)
		}
	}
	h.contains("not in the state: nothing to keep", "Library bucket s3://"+libBucket)
	if strings.Contains(h.out.String(), "is KEPT") {
		t.Errorf("the confirmation promised to keep a bucket the state does not hold:\n%s", h.out.String())
	}

	// And with no bucket at all there is nothing to report.
	b2 := &fakeLibraryBucket{exists: false}
	h2 := libraryDestroy(t, b2)
	h2.tf.stateOut = destroyState
	if err := h2.run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h2.out.String(), "Library bucket s3://") {
		t.Errorf("reported a bucket that does not exist:\n%s", h2.out.String())
	}
}

func TestDestroyOnlyRemovesTheLibraryResourcesTheStateHolds(t *testing.T) {
	b := &fakeLibraryBucket{exists: true}
	h := libraryDestroy(t, b)
	h.tf.stateOut = destroyState + libraryBucketAddr + "\nmodule.library_bucket[0].aws_s3_bucket_policy.this\n"
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	var lib string
	for _, c := range rmCommands(h) {
		if strings.Contains(c, libraryBucketAddr) {
			lib = c
		}
	}
	if !strings.Contains(lib, "aws_s3_bucket_policy.this") || strings.Contains(lib, "versioning") {
		t.Errorf("state rm = %q", lib)
	}
}

func TestDestroyDeleteLibraryDataPurgesEveryVersionThenTheBucket(t *testing.T) {
	var vs []libVersion
	for i := 0; i < 250; i++ { // three pages of 100
		vs = append(vs, libVersion{fmt.Sprintf("library/v1/people/a/%d.skill.md", i%40), fmt.Sprintf("v%d", i)})
	}
	b := &fakeLibraryBucket{exists: true, versions: vs}
	h := libraryDestroy(t, b)
	h.tf.stateOut = libraryState
	h.env.flags.DeleteLibraryData = "dp-trial"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	if len(b.versions) != 0 || b.exists {
		t.Fatalf("versions left %d, exists %v", len(b.versions), b.exists)
	}
	if b.did("delete-objects") != 3 {
		t.Errorf("delete-objects calls = %d, want 3", b.did("delete-objects"))
	}
	// The purge comes after the infrastructure is destroyed and the bucket is out of
	// the state, and the list covers delete markers too.
	if h.tl.first("s3api delete-bucket") < h.tl.first("terraform destroy") || h.tl.first("s3api list-object-versions") < h.tl.first("terraform destroy") {
		t.Errorf("the purge ran before the destroy finished:\n%s", h.tl)
	}
	if !strings.Contains(strings.Join(b.calls, "\n"), "DeleteMarkers") {
		t.Error("delete markers are not purged")
	}
	h.contains("bucket "+libBucket+" deleted with 250 version(s)", "will be deleted too")
	if strings.Contains(h.out.String(), "Library bucket s3://") {
		t.Errorf("the deleted bucket is still reported as left behind:\n%s", h.out.String())
	}
}

func TestDestroyDeleteLibraryDataNeedsTheClusterName(t *testing.T) {
	b := &fakeLibraryBucket{exists: true, versions: []libVersion{{"k", "v"}}}
	h := libraryDestroy(t, b)
	h.tf.stateOut = libraryState
	h.env.flags.DeleteLibraryData = "someone-else"
	if !IsReported(h.run()) {
		t.Fatal("destroyed with the wrong --delete-library-data")
	}
	h.assertNothingDeleted()
	if len(b.calls) != 0 {
		t.Errorf("the bucket was touched: %v", b.calls)
	}
	h.contains("not this install's cluster")
}

func TestDestroyDeleteLibraryDataWhenTheBucketIsAlreadyGone(t *testing.T) {
	b := &fakeLibraryBucket{exists: false}
	h := libraryDestroy(t, b)
	h.tf.stateOut = destroyState
	h.env.flags.DeleteLibraryData = "dp-trial"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	h.contains("is already gone")
	if b.did("list-object-versions")+b.did("delete-bucket") != 0 {
		t.Errorf("deleted from a bucket that is gone: %v", b.calls)
	}
}

func TestDestroyDeleteLibraryDataAfterTheInstallIsGone(t *testing.T) {
	// The state is empty (a second run after a default destroy): the opt-in still works.
	b := &fakeLibraryBucket{exists: true, versions: []libVersion{{"library/v1/org/a.skill.md", "v1"}}}
	h := libraryDestroy(t, b)
	h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "No state file was found!", Err: errors.New("x")}
	h.env.flags.DeleteLibraryData = "dp-trial"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if b.exists || len(b.versions) != 0 {
		t.Error("the bucket was not purged")
	}
	h.assertNothingDeleted()
}

func TestDestroyDeleteLibraryDataFailuresAreLoudAndRepeatable(t *testing.T) {
	t.Run("S3 refuses some versions", func(t *testing.T) {
		b := &fakeLibraryBucket{exists: true, versions: []libVersion{{"k", "v"}}, refuse: "AccessDenied"}
		h := libraryDestroy(t, b)
		h.tf.stateOut = destroyState
		h.env.flags.DeleteLibraryData = "dp-trial"
		if !IsReported(h.run()) {
			t.Fatal("a refused delete passed")
		}
		if b.did("delete-bucket") != 0 {
			t.Error("deleted the bucket after a refused delete")
		}
		h.contains("AccessDenied", "Library bucket s3://"+libBucket) // still reported as left
	})
	t.Run("the bucket cannot be deleted", func(t *testing.T) {
		b := &fakeLibraryBucket{exists: true, deleteBucketErr: errors.New("BucketNotEmpty")}
		h := libraryDestroy(t, b)
		h.tf.stateOut = destroyState
		h.env.flags.DeleteLibraryData = "dp-trial"
		if !IsReported(h.run()) {
			t.Fatal("a failed bucket delete passed")
		}
		h.contains("BucketNotEmpty")
	})
	t.Run("the lookup fails: nothing is deleted", func(t *testing.T) {
		b := &fakeLibraryBucket{exists: true, versions: []libVersion{{"k", "v"}}}
		h := libraryDestroy(t, b)
		h.tf.stateOut = destroyState
		h.aws.s3 = func(j string) (string, error) {
			if strings.Contains(j, "head-bucket") {
				return "", errors.New("ExpiredToken")
			}
			return b.run(j)
		}
		h.env.flags.DeleteLibraryData = "dp-trial"
		if !IsReported(h.run()) || b.did("delete-objects") != 0 {
			t.Fatal("deleted after a failed lookup")
		}
	})
}

func TestDestroyReportSaysWhenTheLibraryBucketCouldNotBeChecked(t *testing.T) {
	b := &fakeLibraryBucket{exists: true}
	h := libraryDestroy(t, b)
	h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "No state file was found!", Err: errors.New("x")}
	h.aws.s3 = func(string) (string, error) { return "", errors.New("403 Forbidden") }
	_ = h.run()
	h.contains("Library bucket " + libBucket + " could not be checked")
}

// ---- doctor -----------------------------------------------------------------------

func TestDoctorLibraryBucket(t *testing.T) {
	fake := func(err error, calls *[]string) cmdRunner {
		return func(name string, args ...string) (string, error) {
			*calls = append(*calls, strings.Join(args, " "))
			return "", err
		}
	}
	var calls []string
	t.Run("a kept bucket is a warning that says a reinstall adopts it", func(t *testing.T) {
		rs := checkLibraryBucket(fake(nil, &calls), "acme", "dp-trial", "123456789012", "us-east-2")
		if len(rs) != 1 || rs[0].status != checkWarn || !strings.Contains(rs[0].detail, libBucket+" exists") ||
			!strings.Contains(rs[0].hint, "adopts") || !strings.Contains(rs[0].hint, "--delete-library-data=dp-trial") {
			t.Fatalf("%+v", rs)
		}
		if !strings.Contains(calls[0], "--profile acme s3api head-bucket --bucket "+libBucket+" --expected-bucket-owner 123456789012 --region us-east-2") {
			t.Errorf("call = %s", calls[0])
		}
	})
	t.Run("no bucket is a pass", func(t *testing.T) {
		rs := checkLibraryBucket(fake(errNoBucket, &calls), "", "dp-trial", "123456789012", "us-east-2")
		if len(rs) != 1 || rs[0].status != checkPass {
			t.Fatalf("%+v", rs)
		}
	})
	t.Run("a 403 or any other error is a warning, not a pass", func(t *testing.T) {
		rs := checkLibraryBucket(fake(errors.New("403 Forbidden"), &calls), "", "dp-trial", "123456789012", "us-east-2")
		if len(rs) != 1 || rs[0].status != checkWarn || !strings.Contains(rs[0].detail, "could not check") {
			t.Fatalf("%+v", rs)
		}
	})
	t.Run("a name over 63 characters warns and does not call AWS", func(t *testing.T) {
		calls = nil
		long := strings.Repeat("a", 43) // 43 + 9 + 12 = 64
		rs := checkLibraryBucket(fake(nil, &calls), "", long, "123456789012", "us-east-2")
		if len(rs) != 1 || rs[0].status != checkWarn || !strings.Contains(rs[0].detail, "64 characters") || len(calls) != 0 {
			t.Fatalf("%+v calls=%v", rs, calls)
		}
		if _, bad := libraryNameCheck(strings.Repeat("a", 42), "123456789012"); bad {
			t.Error("63 characters is within S3's limit")
		}
	})
}

func TestDeployPreflightWarnsAboutALibraryBucketNameOverTheLimit(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.env.rec.ClusterName = strings.Repeat("a", 43)
	h.env.checks = h.env.realChecks
	found := false
	for _, r := range h.env.checks(30) {
		if r.name == "library bucket name" && r.status == checkWarn {
			found = true
		}
	}
	if !found {
		t.Error("no warning for a 64-character bucket name")
	}
}

// ---- status -----------------------------------------------------------------------

type urlForwarder struct {
	url    string
	closed int
	err    error
	done   chan struct{}
}

func (f *urlForwarder) Open(context.Context) (string, error) { return f.url, f.err }
func (f *urlForwarder) Done() <-chan struct{}                { return f.done }
func (f *urlForwarder) Close()                               { f.closed++ }

// libraryRow runs only the Library section of status against an api that answers
// with the given status and body.
func libraryRow(t *testing.T, code int, body string) (string, *urlForwarder, string) {
	t.Helper()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	f := &urlForwarder{url: srv.URL}
	var out strings.Builder
	e := &statusEnv{out: &out, fwd: func() Forwarder { return f }, httpc: srv.Client()}
	e.library()
	return out.String(), f, path
}

func TestStatusLibraryRow(t *testing.T) {
	t.Run("reads the admin status route and shows it", func(t *testing.T) {
		out, f, path := libraryRow(t, 200, `{"reachable":true,"bucket":"`+libBucket+`","queue_depth":4,"failing":0,"oldest_queued_seconds":90,"last_reconcile_at":"2026-01-01T00:00:00Z","foreign_objects":2}`)
		if path != "GET /api/v1/admin/library/status" {
			t.Errorf("request = %s", path)
		}
		for _, want := range []string{libBucket, "reachable", "queue 4 (oldest 1m30s)", "failing 0", "foreign objects 2", "2026-01-01T00:00:00Z"} {
			if !strings.Contains(out, want) {
				t.Errorf("row lacks %q:\n%s", want, out)
			}
		}
		if f.closed != 1 {
			t.Errorf("tunnel closed %d times", f.closed)
		}
	})
	t.Run("no bucket: Postgres, and no reachability to report", func(t *testing.T) {
		out, _, _ := libraryRow(t, 200, `{"enabled":false,"reachable":false,"bucket":"","queue_depth":3,"failing":0,"oldest_queued_seconds":null,"last_reconcile_at":null,"foreign_objects":0}`)
		if !strings.Contains(out, "Postgres") || !strings.Contains(out, "no bucket") || strings.Contains(out, "NOT reachable") || strings.Contains(out, "queue") {
			t.Errorf("%s", out)
		}
	})
	t.Run("nulls and an unreachable bucket", func(t *testing.T) {
		out, _, _ := libraryRow(t, 200, `{"reachable":false,"bucket":"b","queue_depth":0,"failing":3,"oldest_queued_seconds":null,"last_reconcile_at":null,"foreign_objects":0}`)
		for _, want := range []string{"NOT reachable", "oldest -", "last reconcile never", "3 item(s) keep failing"} {
			if !strings.Contains(out, want) {
				t.Errorf("row lacks %q:\n%s", want, out)
			}
		}
	})
	t.Run("404: not available on this api", func(t *testing.T) {
		out, _, _ := libraryRow(t, 404, "")
		if !strings.Contains(out, "library: not available on this api") {
			t.Errorf("%s", out)
		}
	})
	t.Run("401: says it needs an admin session instead of a failure", func(t *testing.T) {
		out, _, _ := libraryRow(t, 401, `{"error":"unauthorized"}`)
		if !strings.Contains(out, "needs an admin session") {
			t.Errorf("%s", out)
		}
	})
	t.Run("500 and garbage are warnings", func(t *testing.T) {
		if out, _, _ := libraryRow(t, 500, ""); !strings.Contains(out, "answered 500") {
			t.Errorf("%s", out)
		}
		if out, _, _ := libraryRow(t, 200, "<html>"); !strings.Contains(out, "unexpected answer") {
			t.Errorf("%s", out)
		}
	})
	t.Run("no tunnel or a tunnel that will not open", func(t *testing.T) {
		var out strings.Builder
		(&statusEnv{out: &out}).library()
		if !strings.Contains(out.String(), "not read") {
			t.Errorf("%s", out.String())
		}
		out.Reset()
		f := &urlForwarder{err: errors.New("port-forward: refused")}
		(&statusEnv{out: &out, fwd: func() Forwarder { return f }, httpc: http.DefaultClient}).library()
		if !strings.Contains(out.String(), "could not be read: port-forward: refused") || f.closed != 1 {
			t.Errorf("%s closed=%d", out.String(), f.closed)
		}
	})
}

func TestStatusRunShowsTheLibrarySectionAndNeverFailsOnIt(t *testing.T) {
	s := newStatusCluster()
	var out bytes.Buffer
	f := &urlForwarder{err: errors.New("no tunnel")}
	e := &statusEnv{out: &out, kc: s.client(), ns: testNS, fwd: func() Forwarder { return f }, httpc: http.DefaultClient}
	if err := e.run(); err != nil {
		t.Fatalf("status failed on an unreadable library: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "▸ Library") {
		t.Errorf("no Library section:\n%s", out.String())
	}
}
