// Package models renders and reasons about additional self-hosted vLLM models:
// the validated spec, the objects rendered from k8s/templates/vllm-model.yaml,
// the gateway's VLLM_UPSTREAMS list, and whether a free GPU exists.
//
// Rendering is structural. The template is parsed, its string values are
// replaced in one pass (so a value can never be mistaken for a placeholder), and
// the result is held to the invariants that matter: nothing placeholder-shaped
// survives, and the Service is headless. No sed, no shell.
package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/manifests"
)

// Namespace is where self-hosted model backends run.
const Namespace = "darkplum-inference"

// TemplatePath is the template inside the bundle's k8s/ tree.
const TemplatePath = "templates/vllm-model.yaml"

// DefaultMaxLen is vLLM's --max-model-len when none is given.
const DefaultMaxLen = 8192

var placeholderRe = regexp.MustCompile(`PLACEHOLDER_[A-Z_]+`)

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,29}[a-z0-9])?$`)
	hfRe       = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	revisionRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	servedRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// Spec is one model instance.
type Spec struct {
	Name     string // instance name: vllm-<Name>
	HF       string // Hugging Face repository, org/name
	Revision string // branch, tag or commit SHA
	Served   string // --served-model-name; defaults to Name
	MaxLen   int
}

// ValidateName checks an instance name (for add and remove).
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("NAME must be lowercase letters, digits and dashes, starting and ending with a letter or digit (max 31 characters). Got %q.", name)
	}
	if name == "default" || name == "inference" {
		return fmt.Errorf("NAME %q is reserved for the default vLLM deployment.", name)
	}
	return nil
}

// Normalize fills the defaults and validates every value before it is placed in
// a manifest.
func (s Spec) Normalize() (Spec, error) {
	if err := ValidateName(s.Name); err != nil {
		return s, err
	}
	if s.HF == "" {
		return s, errors.New("--hf is required (Hugging Face model ID, e.g. Qwen/Qwen2.5-7B-Instruct).")
	}
	if !hfRe.MatchString(s.HF) {
		return s, fmt.Errorf("--hf must look like org/name, with letters, digits, dots, dashes and underscores. Got %q.", s.HF)
	}
	if s.Revision == "" {
		s.Revision = "main"
	}
	if !revisionRe.MatchString(s.Revision) {
		return s, fmt.Errorf("--revision must be a branch, tag or commit SHA. Got %q.", s.Revision)
	}
	if s.Served == "" {
		s.Served = s.Name
	}
	if !servedRe.MatchString(s.Served) {
		return s, fmt.Errorf("--served must contain only letters, digits and . _ / -. Got %q.", s.Served)
	}
	if s.MaxLen == 0 {
		s.MaxLen = DefaultMaxLen
	}
	if s.MaxLen < 1 {
		return s, fmt.Errorf("--max-len must be a positive number. Got %d.", s.MaxLen)
	}
	return s, nil
}

// ServiceName is the Service (and Deployment) name of an instance.
func ServiceName(name string) string { return "vllm-" + name }

// UpstreamURL is where the gateway reaches an instance.
func UpstreamURL(name string) string {
	return "http://" + ServiceName(name) + "." + Namespace + ".svc.cluster.local:8000"
}

// Render returns the Deployment and the headless Service of the instance.
func Render(k8s fs.FS, s Spec) ([]manifests.Object, error) {
	s, err := s.Normalize()
	if err != nil {
		return nil, err
	}
	objs, err := manifests.File(k8s, TemplatePath)
	if err != nil {
		return nil, fmt.Errorf("the model template: %w", err)
	}
	tokens := []string{
		"PLACEHOLDER_SERVED_MODEL_NAME", s.Served,
		"PLACEHOLDER_MODEL_REVISION", s.Revision,
		"PLACEHOLDER_MODEL_NAME", s.HF,
		"PLACEHOLDER_MAX_MODEL_LEN", strconv.Itoa(s.MaxLen),
		"MODEL_INSTANCE", s.Name,
	}
	// One pass over each string: a substituted value is never scanned again.
	repl := strings.NewReplacer(tokens...)
	used := map[string]bool{}
	for i := 0; i < len(tokens); i += 2 {
		used[tokens[i]] = false
	}
	var unknown []string
	for _, o := range objs {
		replaceStrings(o, repl, used, tokens, &unknown)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("the model template has placeholders this CLI does not fill: %s", strings.Join(unknown, ", "))
	}
	for tok, ok := range used {
		if !ok {
			return nil, fmt.Errorf("the model template has no %s to fill in: it was renamed?", tok)
		}
	}
	var kinds []string
	for _, o := range objs {
		kinds = append(kinds, o.Kind())
		switch o.Kind() {
		case "Deployment", "Service":
			if o.Name() != ServiceName(s.Name) {
				return nil, fmt.Errorf("%s is not named %s", o.Key(), ServiceName(s.Name))
			}
		case "":
		}
		if o.Kind() == "Service" {
			spec, _ := o["spec"].(map[string]any)
			if spec["clusterIP"] != "None" {
				return nil, fmt.Errorf("%s is not headless: the gateway dials pod IPs directly", o.Key())
			}
		}
	}
	if strings.Join(kinds, ",") != "Deployment,Service" {
		return nil, fmt.Errorf("the model template holds %v, expected a Deployment and a Service", kinds)
	}
	return objs, nil
}

func replaceStrings(v any, r *strings.Replacer, used map[string]bool, tokens []string, unknown *[]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = replaceStrings(e, r, used, tokens, unknown)
		}
		return x
	case manifests.Object:
		for k, e := range x {
			x[k] = replaceStrings(e, r, used, tokens, unknown)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = replaceStrings(e, r, used, tokens, unknown)
		}
		return x
	case string:
		for i := 0; i < len(tokens); i += 2 {
			if strings.Contains(x, tokens[i]) {
				used[tokens[i]] = true
			}
		}
		// A placeholder-shaped word in the TEMPLATE that is not one we fill would
		// ship as literal text; refuse it.
		for _, m := range placeholderRe.FindAllString(x, -1) {
			if _, known := used[m]; !known {
				*unknown = append(*unknown, m)
			}
		}
		return r.Replace(x)
	}
	return v
}

// ---- the gateway's VLLM_UPSTREAMS ---------------------------------------------

// Entries splits the list on commas and drops empties.
func Entries(current string) []string {
	var out []string
	for _, e := range strings.Split(current, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func without(current, name string) []string {
	var out []string
	for _, e := range Entries(current) {
		// Anchored with "=": removing "qwen" must leave "qwen2".
		if !strings.HasPrefix(e, name+"=") {
			out = append(out, e)
		}
	}
	return out
}

// AddUpstream returns the list with name registered (replacing any earlier entry
// for the same name, appended last).
func AddUpstream(current, name string) string {
	return strings.Join(append(without(current, name), name+"="+UpstreamURL(name)), ",")
}

// RemoveUpstream returns the list without name.
func RemoveUpstream(current, name string) string {
	return strings.Join(without(current, name), ",")
}

// ---- GPU headroom ---------------------------------------------------------------

// GPUs sums the GPUs the GPU node group offers and the GPUs that pods which are
// running or pending in the inference namespace request.
func GPUs(nodesJSON, podsJSON []byte) (total, used int, err error) {
	var nodes struct {
		Items []struct {
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return 0, 0, fmt.Errorf("nodes: %w", err)
	}
	for _, n := range nodes.Items {
		total += count(n.Status.Allocatable["nvidia.com/gpu"])
	}
	var pods struct {
		Items []struct {
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
			Spec struct {
				Containers []struct {
					Resources struct {
						Requests map[string]string `json:"requests"`
					} `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(podsJSON, &pods); err != nil {
		return 0, 0, fmt.Errorf("pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase != "Running" && p.Status.Phase != "Pending" {
			continue
		}
		for _, c := range p.Spec.Containers {
			used += count(c.Resources.Requests["nvidia.com/gpu"])
		}
	}
	return total, used, nil
}

func count(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ServedName finds --served-model-name in a container's args.
func ServedName(args []string) string {
	for i, a := range args {
		if a == "--served-model-name" && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--served-model-name="); ok {
			return v
		}
	}
	return ""
}
