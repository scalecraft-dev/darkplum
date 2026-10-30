package commands

import (
	"fmt"
	"regexp"
	"strings"
)

// catalogModel is a self-hosted model Darkplum has reviewed: non-gated, safetensors
// weights, and a pinned commit SHA so a re-download cannot change the weights.
// Add an entry only after verifying all three against the Hugging Face API; the
// scheduled CI job (.github/workflows/catalog-sha.yml) re-checks every SHA.
type catalogModel struct {
	ID       string
	Revision string // full 40-hex commit SHA
	Served   string // short name on the OpenAI-compatible API
	GPUMemGB int    // smallest GPU memory class it runs on
	GPUType  string // instance that provides it
	Note     string
}

var modelCatalog = []catalogModel{
	{
		ID:       "Qwen/Qwen2.5-7B-Instruct",
		Revision: "a09a35458c702b33eeacc393d103063234e8bc28",
		Served:   "qwen-2.5-7b",
		GPUMemGB: 24,
		GPUType:  "g5.xlarge",
		Note:     "Apache-2.0, general chat",
	},
	{
		ID:       "mistralai/Mistral-7B-Instruct-v0.3",
		Revision: "c170c708c41dac9275d15a8fff4eca08d52bab71",
		Served:   "mistral-7b",
		GPUMemGB: 24,
		GPUType:  "g5.xlarge",
		Note:     "Apache-2.0, general chat",
	},
}

// gpuInstance describes the GPU instance types init knows how to size.
type gpuInstance struct {
	VCPU  int
	MemGB int // GPU memory
}

var gpuInstances = map[string]gpuInstance{
	"g5.xlarge":  {VCPU: 4, MemGB: 24},
	"g5.2xlarge": {VCPU: 8, MemGB: 24},
	"g5.4xlarge": {VCPU: 16, MemGB: 24},
}

const (
	defaultCPUInstance = "m5.large"
	defaultGPUInstance = "g5.xlarge"
	defaultClusterName = "darkplum"
)

var (
	shaRe       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	clusterRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)
	accountRe   = regexp.MustCompile(`^[0-9]{12}$`)
	regionRe    = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	modelIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	servedRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	instanceRe  = regexp.MustCompile(`^[a-z0-9]+\.[a-z0-9]+$`)
	nonServedRe = regexp.MustCompile(`[^a-z0-9._-]+`)
)

func validateSHA(s string) error {
	if !shaRe.MatchString(s) {
		return fmt.Errorf("revision must be a full 40-character commit SHA (lowercase hex), not a branch or tag")
	}
	return nil
}

func validateClusterName(s string) error {
	if !clusterRe.MatchString(s) || strings.Contains(s, "--") {
		return fmt.Errorf("cluster name must be 2-40 characters of lowercase letters, digits and single hyphens, starting with a letter and not ending with a hyphen")
	}
	return nil
}

func validateRegion(s string) error {
	if !regionRe.MatchString(s) {
		return fmt.Errorf("%q is not an AWS region name (e.g. us-east-2)", s)
	}
	return nil
}

func validateModelID(s string) error {
	if !modelIDRe.MatchString(s) {
		return fmt.Errorf("%q is not a Hugging Face model id (expected org/name)", s)
	}
	return nil
}

func validateAccountID(s string) error {
	if !accountRe.MatchString(s) {
		return fmt.Errorf("account id must be 12 digits")
	}
	return nil
}

func validateAZs(region string, azs []string) error {
	if len(azs) < 2 {
		return fmt.Errorf("need at least 2 availability zones, have %d", len(azs))
	}
	for _, z := range azs {
		if !strings.HasPrefix(z, region) {
			return fmt.Errorf("availability zone %q is not in %s", z, region)
		}
	}
	return nil
}

// servedNameFor derives an API model name for a custom Hugging Face id.
func servedNameFor(id string) string {
	name := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		name = id[i+1:]
	}
	name = nonServedRe.ReplaceAllString(strings.ToLower(name), "-")
	name = strings.Trim(name, "-._")
	if name == "" || !servedRe.MatchString(name) {
		return "custom-model"
	}
	return name
}

func catalogFor(id string) (catalogModel, bool) {
	for _, m := range modelCatalog {
		if m.ID == id {
			return m, true
		}
	}
	return catalogModel{}, false
}

// geoForRegion maps a region to its Bedrock inference geography. ok is false for
// regions outside us/eu/apac, where the caller must pick a bedrock_region.
func geoForRegion(region string) (geo string, ok bool) {
	switch {
	case strings.HasPrefix(region, "us-"):
		return "us", true
	case strings.HasPrefix(region, "eu-"):
		return "eu", true
	case strings.HasPrefix(region, "ap-"):
		return "apac", true
	}
	return "us", false
}

// defaultTFModel is variables.tf's default model_name: the one model for which
// model_revision may stay the literal "main" (installs that predate pinning).
const defaultTFModel = "mistralai/Mistral-7B-Instruct-v0.3"

// validateTFModelRevision mirrors the two model_revision validation blocks in
// variables.tf; one fixture table drives both. model "" means the default.
func validateTFModelRevision(model, rev string) error {
	if model == "" {
		model = defaultTFModel
	}
	if rev != "main" && !shaRe.MatchString(rev) {
		return fmt.Errorf("revision must be a 40-hex SHA or main")
	}
	if rev == "main" && model != defaultTFModel {
		return fmt.Errorf("a non-default model needs a pinned SHA")
	}
	return nil
}
