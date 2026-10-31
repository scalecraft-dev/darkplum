package commands

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const terraformDir = "../../../../terraform/aws"

// hclUnquoteStrict decodes an HCL quoted string as Terraform would, failing on
// any raw ${ or %{ (which Terraform would treat as interpolation) or raw control
// character. It is deliberately a separate implementation from hclString.
func hclUnquoteStrict(q string) (string, error) {
	if len(q) < 2 || q[0] != '"' || q[len(q)-1] != '"' {
		return "", fmt.Errorf("not quoted: %q", q)
	}
	body := q[1 : len(q)-1]
	var b strings.Builder
	for i := 0; i < len(body); {
		switch {
		case strings.HasPrefix(body[i:], "$${"):
			b.WriteString("${")
			i += 3
		case strings.HasPrefix(body[i:], "%%{"):
			b.WriteString("%{")
			i += 3
		case strings.HasPrefix(body[i:], "${"), strings.HasPrefix(body[i:], "%{"):
			return "", fmt.Errorf("unescaped template sequence at %d in %q", i, q)
		case body[i] == '\\':
			if i+1 >= len(body) {
				return "", fmt.Errorf("dangling backslash")
			}
			switch body[i+1] {
			case '\\', '"':
				b.WriteByte(body[i+1])
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if i+6 > len(body) {
					return "", fmt.Errorf("short \\u")
				}
				n, err := strconv.ParseUint(body[i+2:i+6], 16, 32)
				if err != nil {
					return "", err
				}
				b.WriteRune(rune(n))
				i += 4
			default:
				return "", fmt.Errorf("bad escape \\%c", body[i+1])
			}
			i += 2
		case body[i] == '"':
			return "", fmt.Errorf("raw quote at %d in %q", i, q)
		case body[i] < 0x20 || body[i] == 0x7f:
			return "", fmt.Errorf("raw control char at %d", i)
		default:
			b.WriteByte(body[i])
			i++
		}
	}
	return b.String(), nil
}

func checkHCLRoundTrip(t *testing.T, s string) {
	t.Helper()
	q := hclString(s)
	if strings.ContainsAny(q, "\n\r") {
		t.Fatalf("newline leaked into %q", q)
	}
	got, err := hclUnquoteStrict(q)
	if err != nil {
		t.Fatalf("hclString(%q) = %q: %v", s, q, err)
	}
	if got != s {
		t.Fatalf("round trip of %q gave %q (encoded %q)", s, got, q)
	}
}

func TestHCLStringEscaping(t *testing.T) {
	for _, s := range []string{"", "plain", `a"b`, `a\b`, "a\nb", "a\r\nb", "tab\t", "${x}", "$${x}", "$$${x}", "%{if}", "%%{x}", "$", "${", "$ {", "{", "end$", "\x00\x1f\x7f", "日本語", `\"`, `\${`} {
		checkHCLRoundTrip(t, s)
	}
}

func TestHCLStringFuzzSeeded(t *testing.T) {
	alphabet := []rune("ab$%{}\\\"\n\r\t \x00é日")
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		n := rng.Intn(12)
		rs := make([]rune, n)
		for j := range rs {
			rs[j] = alphabet[rng.Intn(len(alphabet))]
		}
		checkHCLRoundTrip(t, string(rs))
	}
}

func FuzzHCLString(f *testing.F) {
	for _, s := range []string{"${", "%{", `"\`, "a\nb", "$${"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			t.Skip()
		}
		checkHCLRoundTrip(t, s)
	})
}

func TestRenderedTFVarsKeepOneKeyPerLine(t *testing.T) {
	out := renderTFVars([]tfvarsKV{{"aws_profile", "we\"ird\nname"}, {"azs", []string{"a", "b"}}, {"n", 3}, {"b", false}}, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(out, "on 2026-01-02") {
		t.Error("clock not honoured")
	}
	// The Makefile's sed must read aws_profile back from the rendered file.
	re := regexp.MustCompile(`(?m)^\s*aws_profile\s*=\s*"([^"]+)".*$`)
	if m := re.FindStringSubmatch(renderTFVars([]tfvarsKV{{"aws_profile", "scalecraft"}, {"aws_region", "us-east-2"}}, fixedNow)); m == nil || m[1] != "scalecraft" {
		t.Errorf("makefile sed shape broken: %v", m)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !tfLineRe.MatchString(line) {
			t.Errorf("not a key = value line: %q", line)
		}
	}
}

func TestMakefileSedReadsAWSProfile(t *testing.T) {
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("no sed")
	}
	p := filepath.Join(t.TempDir(), "terraform.tfvars")
	if err := os.WriteFile(p, []byte(renderTFVars([]tfvarsKV{{"aws_profile", "scalecraft"}, {"cluster_name", "x"}}, fixedNow)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same expression as the Makefile's TFVARS_AWS_PROFILE (BSD and GNU sed -E).
	out, err := exec.Command("sed", "-nE", `s/^[[:space:]]*aws_profile[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p`, p).Output()
	if err != nil || strings.TrimSpace(string(out)) != "scalecraft" {
		t.Fatalf("sed gave %q, %v", out, err)
	}
}

func TestRenderedKeysExistInVariablesTF(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(terraformDir, "variables.tf"))
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^variable "([a-z_0-9]+)"`).FindAllStringSubmatch(string(b), -1) {
		declared[m[1]] = true
	}
	// Every setting init can render, in every mode.
	all := []*settings{
		{profile: "p", account: testAccount, region: "us-east-2", cluster: "c", azs: []string{"us-east-2a", "us-east-2b"}, geo: "us", bedrockRegion: "us-east-1", throwaway: true, gpu: true, gpuCount: 1, gpuInstance: "g5.xlarge", model: modelCatalog[0]},
		{account: testAccount, region: "us-east-2", cluster: "c", azs: []string{"us-east-2a", "us-east-2b"}, geo: "us"},
	}
	for _, s := range all {
		for _, kv := range s.tfvars() {
			if !declared[kv.Key] {
				t.Errorf("init renders %q, which variables.tf does not declare", kv.Key)
			}
		}
	}
}

type variableCase struct {
	Var    string            `json:"var"`
	Region string            `json:"region"`
	Extra  map[string]string `json:"extra"`
	Value  json.RawMessage   `json:"value"`
	Valid  bool              `json:"valid"`
}

// The same fixtures drive `terraform test` (tests/gen_validation_tests.sh): the
// CLI and variables.tf must agree on what is acceptable.
func TestVariableCasesMatchGoValidators(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(terraformDir, "tests", "variable_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []variableCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		var verr error
		switch c.Var {
		case "model_revision":
			var v string
			mustJSON(t, c.Value, &v)
			verr = validateTFModelRevision(c.Extra["model_name"], v)
		case "cluster_name":
			var v string
			mustJSON(t, c.Value, &v)
			verr = validateClusterName(v)
		case "allowed_account_ids":
			var v []string
			mustJSON(t, c.Value, &v)
			for _, a := range v {
				if e := validateAccountID(a); e != nil {
					verr = e
				}
			}
		case "azs":
			var v []string
			mustJSON(t, c.Value, &v)
			verr = validateAZs(c.Region, v)
		case "web_domain":
			var v string
			mustJSON(t, c.Value, &v)
			verr = validateTFWebDomain(v)
		case "web_hostname":
			var v string
			mustJSON(t, c.Value, &v)
			verr = validateTFWebHostname(c.Extra["web_domain"], v)
		case "web_zone_id":
			var v string
			mustJSON(t, c.Value, &v)
			verr = validateTFWebZoneID(c.Extra["web_domain"], v)
		case "web_certificate_arn":
			var v string
			mustJSON(t, c.Value, &v)
			verr = validateTFWebCertARN(c.Extra["web_domain"], c.Region, v)
		case "web_allowed_cidrs":
			var v []string
			mustJSON(t, c.Value, &v)
			verr = validateTFWebCIDRs(c.Extra["web_domain"], v)
		default:
			t.Fatalf("case %d: no Go validator for %q", i, c.Var)
		}
		if (verr == nil) != c.Valid {
			t.Errorf("case %d %s=%s: Go valid=%v, fixture valid=%v (%v)", i, c.Var, c.Value, verr == nil, c.Valid, verr)
		}
	}
}

func mustJSON(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// TestWriteRenderedSamples is a CI hook, not a unit test: with
// DARKPLUM_RENDER_SAMPLES=<dir> it writes the rendered tfvars (with a hostile
// profile name) and backend.tf so the workflow can run terraform against them.
func TestWriteRenderedSamples(t *testing.T) {
	dir := os.Getenv("DARKPLUM_RENDER_SAMPLES")
	if dir == "" {
		t.Skip("set DARKPLUM_RENDER_SAMPLES to write samples")
	}
	s := &settings{profile: `we"ird\${x}%{y}`, account: testAccount, region: "us-east-2", cluster: "darkplum",
		azs: []string{"us-east-2a", "us-east-2b", "us-east-2c"}, geo: "us", gpu: true, gpuCount: 1,
		gpuInstance: "g5.xlarge", model: modelCatalog[1], throwaway: true,
		web: webSettings{domain: "acme.com", hostname: "chat.acme.com", zoneID: "Z0123456789ABCDEFGHIJ",
			certARN: "arn:aws:acm:us-east-2:" + testAccount + ":certificate/0b8f2a3c-1d2e-4f5a-8b9c-0d1e2f3a4b5c",
			allowed: []string{"10.0.0.0/16", "10.8.0.0/16"}}}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sample.tfvars", renderTFVars(s.tfvars(), fixedNow))
	write(backendFileName, renderBackend(backendSpec{Bucket: stateBucketName(testAccount, "us-east-2", "a1b2c3"), Key: stateKeyFor(s.cluster), Region: "us-east-2",
		KMSKeyARN: "arn:aws:kms:us-east-2:" + testAccount + ":key/abc"}))
}
