package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
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
	Stream(ctx context.Context, w io.Writer, opts runner.StreamOptions, args ...string) error
	// K8s is the bundle's Kubernetes tree, re-verified against the hash shipped in
	// this binary.
	K8s() (fs.FS, error)
	// VarFile is the install's variables file (absolute).
	VarFile() string
	// Command is the command line that runs this Terraform command by hand, for a
	// hint a person can paste: the verified bundle is the root and its data directory
	// is under the workspace, so a bare `terraform ...` anywhere else does nothing.
	Command(args ...string) string
}

// tfFailure is a failed Terraform command: the error says what, Stderr is
// everything Terraform printed.
type tfFailure struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *tfFailure) Error() string {
	if d := e.diagnostic(); len(d) > 0 {
		return "terraform " + e.Cmd + ": " + d[tfSummaryLine(d)]
	}
	return "terraform " + e.Cmd + ": " + e.Err.Error()
}
func (e *tfFailure) Unwrap() error { return e.Err }

// tfBoxRe is the box Terraform draws around a diagnostic.
var tfBoxRe = regexp.MustCompile(`^\s*[╷│╵]\s?`)

// diagnostic is what Terraform printed without its colours and box, blank lines
// dropped, so a diagnostic never reads as its opening "╷".
func (e *tfFailure) diagnostic() []string {
	var lines []string
	for _, l := range strings.Split(e.Stderr, "\n") {
		l = tfBoxRe.ReplaceAllString(ansiRe.ReplaceAllString(l, ""), "")
		if l = strings.TrimRight(l, " \t\r"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// tfSummaryLine picks the line that says what failed: a warning printed ahead of
// the error (a deprecated backend setting, say) is not why the command failed.
func tfSummaryLine(d []string) int {
	for i, l := range d {
		if strings.HasPrefix(l, "Error:") {
			return i
		}
	}
	return 0
}

// tfErrorText is all of what a failed command said, for recognising a cause (an
// expired session) that need not be on the line the failure row shows.
func tfErrorText(err error) string {
	var f *tfFailure
	if errors.As(err, &f) && f.Stderr != "" {
		return f.Stderr
	}
	return err.Error()
}

// printTFDiagnostic shows the rest of what a failed Terraform command said, on the
// screen and in the run's log; the failure row that follows carries its summary line.
func printTFDiagnostic(w io.Writer, err error) {
	var f *tfFailure
	if !errors.As(err, &f) {
		return
	}
	d := f.diagnostic()
	if len(d) < 2 {
		return
	}
	fmt.Fprintf(w, "      %s\n", sDim("┌ terraform "+f.Cmd))
	summary := tfSummaryLine(d)
	for i, l := range d {
		if i != summary {
			fmt.Fprintf(w, "      %s\n", sDim("│ "+l))
		}
	}
}

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

func (t *realTF) Command(args ...string) string {
	line, err := runner.TerraformCommand(t.v, t.dirs, t.env, args...)
	if err != nil {
		return "terraform " + strings.Join(args, " ") + " (in this install's workspace; darkplum could not build the exact command: " + firstLine(err.Error()) + ")"
	}
	return line
}

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

func (t *realTF) Stream(ctx context.Context, w io.Writer, opts runner.StreamOptions, args ...string) error {
	if err := runner.TerraformStream(ctx, t.v, t.dirs, t.env, w, opts, args...); err != nil {
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
	// CreateFirst is a replacement whose new object is created BEFORE the old one is
	// destroyed. That is create_before_destroy, set on an object or inherited by every
	// object it depends on: the EKS module's node-group submodules and our database
	// security group make the cluster and everything upstream of it create-first (see
	// deploy_resume.go). Where the name is fixed, that order can never work.
	CreateFirst bool
	// Tainted is a replacement because an apply was interrupted or failed while
	// creating the object, so Terraform no longer trusts it.
	Tainted bool
}

// reasonTainted is the plan JSON's action_reason for a replacement of a tainted object.
const reasonTainted = "replace_because_tainted"

// plansToCreate says whether the plan creates addr outright (not a replacement: there
// is then no object of it in the state). An unreadable plan says no; the caller's
// next step reads it and reports that.
func plansToCreate(planJSON []byte, addr string) bool {
	var p struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if json.Unmarshal(planJSON, &p) != nil {
		return false
	}
	for _, rc := range p.ResourceChanges {
		if rc.Address == addr && len(rc.Change.Actions) == 1 && rc.Change.Actions[0] == "create" {
			return true
		}
	}
	return false
}

// destructiveChanges reads `terraform show -json <plan>` and lists every resource
// the plan deletes or replaces, sorted. Anything else (create, update in place,
// read, no-op) is not in the list.
func destructiveChanges(planJSON []byte) ([]planChange, error) {
	var p struct {
		ResourceChanges []struct {
			Address      string `json:"address"`
			ActionReason string `json:"action_reason"`
			Change       struct {
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
			out = append(out, planChange{Address: rc.Address, Replace: create,
				CreateFirst: create && len(rc.Change.Actions) > 0 && rc.Change.Actions[0] == "create",
				Tainted:     rc.ActionReason == reasonTainted})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// ---- tainted objects ---------------------------------------------------------

// taintedInstances lists the addresses of the instances the state marks tainted,
// from `terraform state pull`. That JSON is the one place every Terraform version
// writes the status (instances[].status); `terraform show -json` exposes it only
// on some. The state holds secrets (the database password): this reads the
// addresses and nothing else, and the bytes are never printed or kept.
func taintedInstances(statePull []byte) (map[string]bool, error) {
	var st struct {
		Resources []struct {
			Module    string `json:"module"`
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				IndexKey any     `json:"index_key"`
				Status   string  `json:"status"`
				Deposed  *string `json:"deposed"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(statePull, &st); err != nil {
		return nil, errors.New("terraform printed a state this darkplum cannot read")
	}
	out := map[string]bool{}
	for _, r := range st.Resources {
		if r.Mode != "managed" {
			continue
		}
		for _, i := range r.Instances {
			if i.Status != "tainted" || i.Deposed != nil {
				continue
			}
			addr := r.Type + "." + r.Name
			if r.Module != "" {
				addr = r.Module + "." + addr
			}
			switch k := i.IndexKey.(type) {
			case nil:
			case float64:
				addr += fmt.Sprintf("[%d]", int(k))
			case string:
				addr += fmt.Sprintf("[%q]", k)
			}
			out[addr] = true
		}
	}
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
	// Terraform sets this on every apply (bedrock_region, else aws_region), even
	// with no Bedrock model allowed, so empty means state from before the output
	// existed or a broken apply. The Makefile fell back to the base manifest's
	// us-east-1 here, which sends a cluster elsewhere to the wrong region; refuse
	// instead.
	if o.InferenceRegion == "" && len(errs) == 0 {
		errs = append(errs, errors.New(`terraform output "inference_region" is empty; run darkplum deploy again so the apply sets it`))
	}
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

// changeCount is the number of resources a plan would create, update, delete or
// replace; 0 means Terraform found nothing to do.
func changeCount(planJSON []byte) (int, error) {
	var p struct {
		ResourceChanges []struct {
			Change struct {
				Actions []string `json:"actions"`
				// An import with nothing to change is the action no-op, and it
				// still has to be applied to reach the state.
				Importing *struct{} `json:"importing"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &p); err != nil {
		return 0, errors.New("terraform printed a plan this darkplum cannot read")
	}
	n := 0
	for _, rc := range p.ResourceChanges {
		a := rc.Change.Actions
		if len(a) == 1 && (a[0] == "no-op" || a[0] == "read") && rc.Change.Importing == nil {
			continue
		}
		n++
	}
	return n, nil
}
