package commands

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// tfvarsInfo is the subset of terraform.tfvars that doctor needs. The file is
// written by init (one `key = value` per line); hand-written files in the same
// shape work too.
type tfvarsInfo struct {
	Profile, Region, GPUType string
	AccountIDs, AZs          []string
	GPUDesired               int
	// LibraryBucket is library_bucket_enabled: nil when the file does not say
	// (Terraform's default is then false, and deploy refuses to let that
	// silently drop a bucket an install already has).
	LibraryBucket *bool
	// LibraryBucketRaw is the value when it is not a boolean Terraform would
	// read as one; deploy refuses it by name rather than treating it as unset.
	LibraryBucketRaw string
	Raw              map[string]string
}

var (
	tfLineRe = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s*=\s*(.*?)\s*$`)
	tfStrRe  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
)

func tfUnquote(s string) string {
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `$${`, "${", `%%{`, "%{")
	return r.Replace(s)
}

func parseTFVars(content string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := sc.Text()
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if m := tfLineRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

func tfStrings(raw string) []string {
	var out []string
	for _, m := range tfStrRe.FindAllStringSubmatch(raw, -1) {
		out = append(out, tfUnquote(m[1]))
	}
	return out
}

func readTFVars(path string) (tfvarsInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return tfvarsInfo{}, fmt.Errorf("cannot read %s: %w", path, err)
	}
	raw := parseTFVars(string(b))
	info := tfvarsInfo{Raw: raw, GPUDesired: 1}
	first := func(k string) string {
		if s := tfStrings(raw[k]); len(s) > 0 {
			return s[0]
		}
		return ""
	}
	info.Profile, info.Region = first("aws_profile"), first("aws_region")
	info.GPUType = first("gpu_instance_types")
	info.AccountIDs = tfStrings(raw["allowed_account_ids"])
	info.AZs = tfStrings(raw["azs"])
	if v, ok := raw["gpu_desired_size"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			info.GPUDesired = n
		}
	}
	if v, ok := raw["library_bucket_enabled"]; ok {
		// A trailing comment, and the quoted form Terraform also accepts.
		if i := strings.Index(v, "#"); i >= 0 {
			v = v[:i]
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if v == "true" || v == "false" {
			b := v == "true"
			info.LibraryBucket = &b
		} else {
			info.LibraryBucketRaw = v
		}
	}
	return info, nil
}
