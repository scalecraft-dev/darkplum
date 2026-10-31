package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// defaultTFVars is where `darkplum init` writes the install settings, relative
// to the darkplum/ directory the CLI is run from.
var defaultTFVars = filepath.Join("terraform", "aws", "terraform.tfvars")

// installContext is the only context in the install's private kubeconfig.
const installContext = runner.Context

// clusterTarget is the install's EKS cluster and how kubectl reaches it.
//
// Every command that touches Kubernetes establishes it itself, at its start:
// it never relies on kubeconfig state an earlier command (or the operator)
// left behind, because nobody knows what happened in between. The operator's
// own ~/.kube/config and current context are never read or written.
type clusterTarget struct {
	Name, Region, Profile, Account string
	Endpoint                       string
	// Kubeconfig is the absolute path of the install's private kubeconfig
	// (one cluster, one exec user, context "darkplum"); "" only for the
	// explicit --context escape, which uses the operator's own kubeconfig.
	Kubeconfig string
	Context    string
	// h is the proof that this cluster was validated; kubectl is only
	// reachable through it. The zero value (tests, or a target that failed
	// validation) runs nothing.
	h runner.ValidatedCluster
}

// env is what kubectl needs: the install's AWS profile for the exec credential
// (`aws eks get-token`) and, via kube.NewPinned, KUBECONFIG.
func (t clusterTarget) env() []string {
	if t.Profile == "" {
		return nil
	}
	return []string{"AWS_PROFILE=" + t.Profile}
}

func (t clusterTarget) kube(namespace string) *kube.Client {
	return kube.ForCluster(namespace, t.h)
}

var (
	// errNoSettings means init has not been run here: there is no install to pin to.
	errNoSettings = errors.New("no install settings")
	// errClusterNotFound means the settings name a cluster that does not exist
	// (never created, or already destroyed).
	errClusterNotFound = runner.ErrClusterNotFound
)

// kubeconfigPath is the install's private kubeconfig, next to its settings
// (terraform/aws/.darkplum/kubeconfig; gitignored).
func kubeconfigPath(tfvarsPath string) (string, error) {
	return filepath.Abs(filepath.Join(filepath.Dir(tfvarsPath), ".darkplum", "kubeconfig"))
}

// installCluster establishes and validates the install's cluster from its
// settings, and (re)writes the private kubeconfig. It refuses unless the
// cluster exists, its ARN names the settings' region and name, and both the
// cluster's account and the caller's account are the account the settings
// pin. On any refusal the private kubeconfig is removed, so nothing can keep
// using a cluster that no longer validates.
//
// The returned row is a pass or a fail with a hint. The error wraps
// errNoSettings or errClusterNotFound where those apply.
func installCluster(path string, aws cmdRunner) (clusterTarget, checkResult, error) {
	if _, err := os.Stat(path); err != nil {
		return clusterTarget{}, checkResult{name: "cluster", status: checkFail, detail: "no install found in this directory",
			hint: "run from the darkplum/ directory of an install (after ./bin/darkplum init and the deploy)"}, fmt.Errorf("%w at %s", errNoSettings, path)
	}
	kp, err := kubeconfigPath(path)
	if err != nil {
		return clusterTarget{}, checkResult{name: "cluster", status: checkFail, detail: err.Error()}, err
	}
	fail := func(detail, hint string, cause error) (clusterTarget, checkResult, error) {
		_ = os.Remove(kp)
		if cause == nil {
			cause = errors.New(detail)
		}
		return clusterTarget{}, checkResult{name: "cluster", status: checkFail, detail: detail, hint: hint}, cause
	}

	tf, err := readTFVars(path)
	if err != nil {
		return fail("install settings unreadable", "re-run ./bin/darkplum init", nil)
	}
	t := clusterTarget{Name: "darkplum", Region: tf.Region, Profile: tf.Profile, Kubeconfig: kp, Context: installContext}
	if s := tfStrings(tf.Raw["cluster_name"]); len(s) > 0 && s[0] != "" {
		t.Name = s[0]
	}
	if t.Region == "" {
		return fail("install settings name no region", "re-run ./bin/darkplum init", nil)
	}
	if len(tf.AccountIDs) == 0 {
		return fail("install settings pin no AWS account", "re-run ./bin/darkplum init so the install is pinned to one account", nil)
	}
	v, err := runner.Validate(runner.Claim{Name: t.Name, Region: t.Region, Profile: t.Profile, AccountIDs: tf.AccountIDs},
		func(args ...string) (string, error) { return aws("aws", args...) })
	if err != nil {
		var rf *runner.Refusal
		if errors.As(err, &rf) {
			return fail(rf.Detail, rf.Hint, rf.Cause)
		}
		return fail(err.Error(), "", nil)
	}
	t.Endpoint, t.Account = v.Endpoint(), v.Account()
	t.h = v.WithKubeconfig(kp)
	dcCA := v.CA()

	if err := writePrivateKubeconfig(kp, renderKubeconfig(t, dcCA)); err != nil {
		return fail("cannot write the install's kubeconfig: "+firstLine(err.Error()), "check the permissions of "+filepath.Dir(kp), nil)
	}
	return t, checkResult{name: "cluster", status: checkPass, detail: fmt.Sprintf("%s (%s) · account %s", t.Name, t.Region, t.Account)}, nil
}

// renderKubeconfig is the private kubeconfig: endpoint, CA and an exec stanza
// (no token is ever stored). JSON is valid YAML, and escapes every value.
func renderKubeconfig(t clusterTarget, caData string) []byte {
	args := awsArgs(t.Profile, "--region", t.Region, "eks", "get-token", "--cluster-name", t.Name, "--output", "json")
	var env []map[string]string
	if t.Profile != "" {
		env = []map[string]string{{"name": "AWS_PROFILE", "value": t.Profile}}
	}
	cfg := map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"current-context": installContext,
		"clusters": []any{map[string]any{"name": installContext, "cluster": map[string]any{
			"server": t.Endpoint, "certificate-authority-data": caData}}},
		"users": []any{map[string]any{"name": installContext, "user": map[string]any{"exec": map[string]any{
			"apiVersion": "client.authentication.k8s.io/v1beta1", "command": "aws", "args": args,
			"env": env, "interactiveMode": "Never"}}}},
		"contexts": []any{map[string]any{"name": installContext, "context": map[string]any{
			"cluster": installContext, "user": installContext}}},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return append(b, '\n')
}

// writePrivateKubeconfig writes b to path (0600, directory 0700) by renaming a
// temporary file over it, so a concurrent reader never sees half a file.
func writePrivateKubeconfig(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".kubeconfig-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after the rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// newLocalKind admits only a kind-* context whose API server is on this machine;
// tests swap it.
var newLocalKind = runner.NewLocalKind

// resolveInstall establishes the install in the current directory; tests swap it.
var resolveInstall = func() (clusterTarget, checkResult, error) {
	return installCluster(defaultTFVars, runWithStderr)
}

// installKube is the kube client for every command that touches the cluster
// (status, admin create, sso configure). It fails closed: an explicit
// --context is used as given (the operator's own kubeconfig, for clusters
// darkplum did not install); otherwise the install must establish and
// validate. It never falls back to the kubeconfig's current context.
func installKube(w io.Writer, namespace, contextFlag string) (*kube.Client, error) {
	if contextFlag != "" {
		k, err := newLocalKind(contextFlag)
		if err != nil {
			printCheckTo(w, checkResult{name: "cluster", status: checkFail, detail: firstLine(err.Error()),
				hint: "--context is only for a local kind cluster (kind-*); run from the darkplum/ directory of an install for any other"})
			return nil, reportedError{"--context refused"}
		}
		fmt.Fprintln(w, sDim("context "+contextFlag+" (from --context, your own kubeconfig; a local kind cluster)"))
		return kube.ForLocalKind(namespace, k), nil
	}
	t, row, err := resolveInstall()
	if errors.Is(err, errNoSettings) {
		printCheckTo(w, checkResult{name: "cluster", status: checkFail, detail: "no install found in this directory",
			hint: "run from the darkplum/ directory of the install, or pass --context <kind context>"})
		return nil, reportedError{"no install settings and no --context"}
	}
	if err != nil {
		printCheckTo(w, row)
		return nil, reportedError{"cannot reach the install's cluster"}
	}
	fmt.Fprintln(w, sDim("cluster "+row.detail))
	return t.kube(namespace), nil
}

// kubectlHint is a kubectl command an operator can paste: it first has the
// CLI validate the install and point that shell at its private kubeconfig.
func kubectlHint(args string) string {
	return `eval "$(./bin/darkplum kubeconfig)" && kubectl ` + args
}

// KubeconfigCmd is `darkplum kubeconfig`.
type KubeconfigCmd struct {
	Write  bool   `help:"Validate and write the install's kubeconfig without printing anything (for scripts)."`
	Tfvars string `help:"Install settings written by darkplum init (default: terraform/aws/terraform.tfvars)." default:""`
}

// Run validates the install's cluster, rewrites its private kubeconfig and
// prints `export KUBECONFIG=<path>` for `eval "$(./bin/darkplum kubeconfig)"`.
// It never touches ~/.kube/config or the current context. Rows go to stderr,
// so only the export line reaches an eval.
func (c *KubeconfigCmd) Run(_ *Globals) error {
	path := c.Tfvars
	if path == "" {
		path = defaultTFVars
	}
	t, row, err := installCluster(path, runWithStderr)
	if err != nil {
		printCheckTo(os.Stderr, row)
		return reportedError{"cannot establish the install's cluster"}
	}
	if c.Write {
		return nil
	}
	fmt.Fprintf(os.Stderr, "# cluster %s\n", row.detail)
	fmt.Printf("export KUBECONFIG=%s\n", shellQuote(t.Kubeconfig))
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
