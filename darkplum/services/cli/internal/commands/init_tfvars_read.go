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
	Raw                      map[string]string
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
	return info, nil
}
