package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/bundle"
	"github.com/scalecraft-dev/darkplum/cli/internal/install"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// tfRunner is Terraform as deploy and destroy use it: always against the
// install's verified bundle, backend and variables. The real one is realTF; tests
// supply a recording fake.
type tfRunner interface {
	// Init initialises the backend (lock file read-only).
	Init() error
	// Capture runs a short command and returns its stdout. A failure is a
	// *tfFailure carrying Terraform's stderr.
	Capture(args ...string) ([]byte, error)
	// Stream runs a long command, writing its output to w as it is produced.
	// Cancelling ctx interrupts it.
	Stream(ctx context.Context, w io.Writer, args ...string) error
	// K8s is the bundle's Kubernetes tree, re-verified against the hash shipped in
	// this binary.
	K8s() (fs.FS, error)
	// VarFile is the install's variables file (absolute).
	VarFile() string
}

// tfFailure is a failed Terraform command: the error says what, Stderr is
// everything Terraform printed.
type tfFailure struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *tfFailure) Error() string {
	if s := strings.TrimSpace(e.Stderr); s != "" {
		return "terraform " + e.Cmd + ": " + firstLine(s)
	}
	return "terraform " + e.Cmd + ": " + e.Err.Error()
}
func (e *tfFailure) Unwrap() error { return e.Err }

type realTF struct {
	v    bundle.Verified
	dirs runner.TerraformDirs
	env  []string
	prof string
}

// openTF extracts (or re-verifies) the bundle for this binary in the install's
// workspace. note is non-empty when a tampered extraction was moved aside.
func openTF(in install.Install, rec install.Record) (tfRunner, string, error) {
	b, err := bundle.Embedded()
	if err != nil {
		return nil, "", err
	}
	m, err := release.Current()
	if err != nil {
		return nil, "", err
	}
	v, note, err := in.UseBundle(b, m.Version, time.Now)
	if err != nil {
		return nil, "", err
	}
	key := bundle.Key(m.Version, b.Hash())
	t := &realTF{v: v, prof: rec.Profile,
		dirs: runner.TerraformDirs{Workspace: in.Dir(), Data: in.TFData(key), PluginCache: in.PluginCache(), BackendHCL: in.BackendHCL(), TFVars: in.TFVars()}}
	if rec.Profile != "" {
		t.env = append(t.env, "AWS_PROFILE="+rec.Profile)
	}
	return t, note, nil
}

func (t *realTF) VarFile() string { return t.dirs.TFVars }

func (t *realTF) Init() error {
	stderr, err := runner.TerraformInit(t.v, t.dirs, t.prof, t.env)
	if err != nil {
		return &tfFailure{Cmd: "init", Stderr: stderr, Err: err}
	}
	return nil
}

func (t *realTF) Capture(args ...string) ([]byte, error) {
	out, stderr, err := runner.Terraform(t.v, t.dirs, t.env, args...)
	if err != nil {
		return out, &tfFailure{Cmd: args[0], Stderr: stderr, Err: err}
	}
	return out, nil
}

func (t *realTF) Stream(ctx context.Context, w io.Writer, args ...string) error {
	if err := runner.TerraformStream(ctx, t.v, t.dirs, t.env, w, args...); err != nil {
		return &tfFailure{Cmd: args[0], Err: err}
	}
	return nil
}

func (t *realTF) K8s() (fs.FS, error) {
	if _, err := bundle.Verify(t.v.Dir(), t.v.Hash()); err != nil {
		return nil, fmt.Errorf("the bundle changed since it was verified: %w", err)
	}
	return os.DirFS(t.v.K8sDir()), nil
}

// stateList returns the resource addresses in the install's state. A backend with
// no state yet makes Terraform exit 1 with "No state file was found": that is an
// empty state (a fresh or fully destroyed install), not a failure. ANY other
// failure (expired credentials, no access to the bucket, a lock) is returned as
// an error: it must never be read as "nothing is in the state", which is the
// mistake that once skipped the database's deletion-protection flip.
func stateList(tf tfRunner) (map[string]bool, error) {
	out, err := tf.Capture("state", "list")
	if err != nil {
		var f *tfFailure
		if errors.As(err, &f) && strings.Contains(f.Stderr, "No state file was found") {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	set := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			set[l] = true
		}
	}
	return set, nil
}

// ---- plan inspection -------------------------------------------------------

// planChange is one resource a plan deletes or replaces.
type planChange struct {
	Address string
	Replace bool // delete and create, rather than delete
}

// destructiveChanges reads `terraform show -json <plan>` and lists every resource
// the plan deletes or replaces, sorted. Anything else (create, update in place,
// read, no-op) is not in the list.
func destructiveChanges(planJSON []byte) ([]planChange, error) {
	var p struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &p); err != nil {
		return nil, errors.New("terraform printed a plan this darkplum cannot read")
	}
	var out []planChange
	for _, rc := range p.ResourceChanges {
		del, create := false, false
		for _, a := range rc.Change.Actions {
			del = del || a == "delete"
			create = create || a == "create"
		}
		if del {
			out = append(out, planChange{Address: rc.Address, Replace: create})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// ---- outputs ---------------------------------------------------------------

// deployOutputs are the Terraform outputs deploy needs, typed. They are read once,
// after apply: on a fresh install they do not exist before it, and an absent one
// is an error, never a default (a guessed gpu_desired_size once decided whether a
// GPU model was deployed).
type deployOutputs struct {
	ClusterName, Region, VPCCIDR, ServiceCIDR string
	DatabaseURL                               string
	InferenceRegion, BedrockModelFilter       string
	GPUDesired                                int
	ModelName, ModelRevision, ServedModel     string
	Repos                                     map[string]string // service -> ECR repository URL
	GatewayRoleARN                            string
	InvokeARNs                                []string
}

func parseOutputs(raw []byte) (deployOutputs, error) {
	var all map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return deployOutputs{}, errors.New("unexpected terraform output")
	}
	var errs []error
	get := func(key string, into any) {
		o, ok := all[key]
		if !ok || len(o.Value) == 0 || string(o.Value) == "null" {
			errs = append(errs, fmt.Errorf("terraform output %q is missing or null", key))
			return
		}
		if err := json.Unmarshal(o.Value, into); err != nil {
			errs = append(errs, fmt.Errorf("terraform output %q has an unexpected type", key))
		}
	}
	var o deployOutputs
	get("cluster_name", &o.ClusterName)
	get("provider_region", &o.Region)
	get("vpc_cidr", &o.VPCCIDR)
	get("cluster_service_cidr", &o.ServiceCIDR)
	get("database_url", &o.DatabaseURL)
	get("inference_region", &o.InferenceRegion)
	get("bedrock_model_filter", &o.BedrockModelFilter) // the key must exist; "" is a valid value
	get("gpu_desired_size", &o.GPUDesired)
	get("image_repositories", &o.Repos)
	get("gateway_role_arn", &o.GatewayRoleARN)
	get("bedrock_invoke_arns", &o.InvokeARNs)
	if o.GPUDesired > 0 {
		get("model_name", &o.ModelName)
		get("model_revision", &o.ModelRevision)
		get("served_model_name", &o.ServedModel)
	}
	for _, svc := range release.Services {
		if len(errs) == 0 && o.Repos[svc] == "" {
			errs = append(errs, fmt.Errorf("terraform output \"image_repositories\" has no repository for %s", svc))
		}
	}
	if len(errs) > 0 {
		return deployOutputs{}, errors.Join(errs...)
	}
	return o, nil
}
