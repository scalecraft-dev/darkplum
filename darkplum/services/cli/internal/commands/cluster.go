package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

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
	// (one cluster, one exec user, context "darkplum"), in the install's workspace.
	Kubeconfig string
	Context    string
	// h is the proof that this cluster was validated, and it is what wrote the
	// kubeconfig; kubectl is only reachable through it. The zero value (tests, or a
	// target that failed validation) runs nothing.
	h runner.ValidatedCluster
}

func (t clusterTarget) kube(namespace string) *kube.Client {
	return kube.ForCluster(namespace, t.h)
}

var (
	// errNoSettings means there is no install to act on.
	errNoSettings = errors.New("no install settings")
	// errClusterNotFound means the install names a cluster that does not exist
	// (never created, or already destroyed).
	errClusterNotFound = runner.ErrClusterNotFound
	// errClusterNotReady means the cluster exists but is still being created.
	errClusterNotReady = runner.ErrClusterNotReady
)

// legacyRootHere reports a Makefile-era Terraform root near the working
// directory (the darkplum/ directory, or the repository root above it): an
// install the CLI has not adopted yet.
func legacyRootHere() bool {
	for _, root := range []string{filepath.Join("terraform", "aws"), filepath.Join("darkplum", "terraform", "aws")} {
		for _, f := range []string{"backend.tf", "terraform.tfvars"} {
			if _, err := os.Stat(filepath.Join(root, f)); err == nil {
				return true
			}
		}
	}
	return false
}

// openInstall finds the install a command acts on: --install NAME, or the only
// one. It never looks at the environment or the working directory to choose.
func openInstall(name string) (install.Install, install.Record, error) {
	home, err := install.DefaultHome()
	if err != nil {
		return install.Install{}, install.Record{}, err
	}
	in, err := install.Resolve(home, name)
	if err != nil {
		return install.Install{}, install.Record{}, err
	}
	rec, err := in.Load()
	return in, rec, err
}

// noInstallRow is the refusal for "there is nothing to act on". An install from
// the Makefile flow in this directory gets the way to take it over.
func noInstallRow(err error) checkResult {
	if legacyRootHere() {
		return checkResult{name: "cluster", status: checkFail, detail: "this directory holds an install from the Makefile flow",
			hint: "run `darkplum init --adopt` to take it over (its state stays where it is), then re-run"}
	}
	hint := "run `darkplum init` to create one"
	if err != nil && !errors.Is(err, install.ErrNoInstall) {
		hint = firstLine(err.Error())
	}
	return checkResult{name: "cluster", status: checkFail, detail: "no install found", hint: hint}
}

// installCluster establishes and validates the install's cluster from its
// workspace record, and (re)writes the private kubeconfig. It refuses unless the
// credentials belong to an account the install is pinned to, and the cluster
// exists in that account with an ARN that names the install's region and name.
// On any refusal the private kubeconfig is removed, so nothing can keep using a
// cluster that no longer validates.
//
// The returned row is a pass or a fail with a hint. The error wraps
// errNoSettings or errClusterNotFound where those apply.
func installCluster(in install.Install, aws cmdRunner) (clusterTarget, checkResult, error) {
	rec, err := in.Load()
	if err != nil {
		if errors.Is(err, install.ErrNoInstall) {
			return clusterTarget{}, noInstallRow(err), fmt.Errorf("%w: %v", errNoSettings, err)
		}
		_ = os.Remove(in.Kubeconfig())
		return clusterTarget{}, checkResult{name: "cluster", status: checkFail, detail: "install record unreadable: " + firstLine(err.Error()),
			hint: "re-run `darkplum init --adopt` or `darkplum init`"}, err
	}
	kp := in.Kubeconfig()
	fail := func(detail, hint string, cause error) (clusterTarget, checkResult, error) {
		_ = os.Remove(kp)
		if cause == nil {
			cause = errors.New(detail)
		}
		return clusterTarget{}, checkResult{name: "cluster", status: checkFail, detail: detail, hint: hint}, cause
	}

	v, err := runner.Validate(runner.Claim{Name: rec.ClusterName, Region: rec.Region, Profile: rec.Profile, AccountIDs: rec.AccountIDs},
		func(args ...string) (string, error) { return aws("aws", args...) })
	if err != nil {
		var rf *runner.Refusal
		if errors.As(err, &rf) {
			return fail(rf.Detail, rf.Hint, rf.Cause)
		}
		return fail(err.Error(), "", nil)
	}
	h, err := v.WriteKubeconfig(kp)
	if err != nil {
		return fail("cannot write the install's kubeconfig: "+firstLine(err.Error()), "check the permissions of "+filepath.Dir(kp), nil)
	}
	t := clusterTarget{Name: rec.ClusterName, Region: rec.Region, Profile: rec.Profile, Account: v.Account(), Endpoint: v.Endpoint(),
		Kubeconfig: h.Kubeconfig(), Context: installContext, h: h}
	return t, checkResult{name: "cluster", status: checkPass, detail: fmt.Sprintf("%s (%s) · account %s", t.Name, t.Region, t.Account)}, nil
}

// newLocalKind admits only a kind-* context whose API server is on this machine;
// tests swap it.
var newLocalKind = runner.NewLocalKind

// resolveInstall establishes the named (or only) install; tests swap it.
var resolveInstall = func(name string) (clusterTarget, checkResult, error) {
	home, err := install.DefaultHome()
	if err != nil {
		return clusterTarget{}, checkResult{name: "cluster", status: checkFail, detail: firstLine(err.Error())}, err
	}
	in, err := install.Resolve(home, name)
	if err != nil {
		return clusterTarget{}, noInstallRow(err), fmt.Errorf("%w: %v", errNoSettings, err)
	}
	return installCluster(in, runWithStderr)
}

// installKube is the kube client for every command that touches the cluster
// (status, admin create, sso configure). It fails closed: an explicit
// --context is only for a local kind cluster; otherwise the install must
// establish and validate. It never falls back to the kubeconfig's current
// context.
func installKube(w io.Writer, namespace, contextFlag, installName string) (*kube.Client, error) {
	if contextFlag != "" {
		k, err := newLocalKind(contextFlag)
		if err != nil {
			printCheckTo(w, checkResult{name: "cluster", status: checkFail, detail: firstLine(err.Error()),
				hint: "--context is only for a local kind cluster (kind-*); an install is found with --install or as the only one"})
			return nil, reportedError{"--context refused"}
		}
		fmt.Fprintln(w, sDim("context "+contextFlag+" (from --context, your own kubeconfig; a local kind cluster)"))
		return kube.ForLocalKind(namespace, k), nil
	}
	t, row, err := resolveInstall(installName)
	if errors.Is(err, errNoSettings) {
		printCheckTo(w, row)
		return nil, reportedError{"no install and no --context"}
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
	Write bool `help:"Validate and write the install's kubeconfig without printing anything (for scripts)."`
}

// Run validates the install's cluster, rewrites its private kubeconfig and
// prints `export KUBECONFIG=<path>` for `eval "$(./bin/darkplum kubeconfig)"`.
// It never touches ~/.kube/config or the current context. Rows go to stderr,
// so only the export line reaches an eval.
func (c *KubeconfigCmd) Run(g *Globals) error {
	t, row, err := resolveInstall(g.Install)
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
