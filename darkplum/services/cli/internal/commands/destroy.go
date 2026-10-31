package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
	"github.com/scalecraft-dev/darkplum/cli/internal/release"
	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// DestroyCmd is `darkplum destroy`: it removes the install's cloud resources.
//
// The order is what makes it safe, and each step answers a way an earlier
// teardown went wrong:
//
//  1. Credentials first, before anything is read or deleted. An expired session
//     once let the Kubernetes side be deleted and then failed on the
//     infrastructure, leaving a cluster-less install that was still billing.
//  2. Everything read-only that can fail: the state is listed (a failure is an
//     abort, never "no database"), the cluster is validated against the install's
//     account (gone: skip the Kubernetes phase, loudly; anything else: abort), and
//     the cluster is surveyed for what would block the teardown.
//  3. The cluster name is typed.
//  4. Terraform-side preparation, all before any Kubernetes deletion: the
//     database's deletion protection, the retained audit log group, and a
//     `terraform plan -destroy` sanity check. If any of it fails, Kubernetes has
//     not been touched.
//  5. Only then the Kubernetes phase (load balancers and volumes in the Darkplum
//     namespaces, through the install's own validated cluster, never the current
//     kubectl context), then the credentials are checked again, then
//     `terraform destroy`.
//  6. A report of what is deliberately left behind, from what exists, and a
//     non-zero exit if anything tagged for the cluster (load balancer, network
//     interface, security group) is still in the account.
//
// Load balancers and volume claims in namespaces other than the Darkplum ones are
// never deleted: they stop the run before anything is changed.
type DestroyCmd struct {
	Confirm           string `name:"confirm" help:"The cluster name, for scripts. Interactively you are asked to type it."`
	ConfirmNoSnapshot string `name:"confirm-no-snapshot" help:"The cluster name again, to delete the database WITHOUT a final snapshot when the install is configured to take none. --confirm does not cover this."`
	AllowDowngrade    bool   `name:"allow-downgrade" help:"Destroy even though this darkplum is older than (or cannot be ordered against) the release that last applied the install."`
	MinSession        int    `name:"min-session" help:"Stop unless the AWS session outlasts this many minutes." default:"30"`
}

// Run executes `darkplum destroy`.
func (c *DestroyCmd) Run(g *Globals) error {
	in, rec, err := openInstall(g.Install)
	if err != nil {
		printCheckTo(os.Stdout, noInstallRow(err))
		return reportedError{"no install to destroy"}
	}
	logf, logPath, err := openCommandLog(in, "destroy", time.Now())
	if err != nil {
		return err
	}
	defer logf.Close()
	tfv, _ := readTFVars(in.TFVars())
	m, err := release.Current()
	if err != nil {
		return err
	}
	e := &destroyEnv{version: m.Version,
		out: newLockedWriter(io.MultiWriter(os.Stdout, logf)), logPath: logPath, in: in, rec: rec, tfv: tfv, flags: *c,
		now: time.Now, sleep: sleepCtx, heartbeat: 30 * time.Second, poll: 10 * time.Second,
		aws:         runWithStderr,
		interactive: term.IsTerminal(int(os.Stdin.Fd())),
		prompt:      promptLine,
	}
	e.sessionOK = func(min int) checkResult { return strictSession(rec.Profile, min) }
	e.openTF = func() (tfRunner, string, error) { return openTF(in, rec) }
	e.cluster = func() (clusterTarget, checkResult, error) { return installCluster(in, e.aws) }
	e.kube = func(t clusterTarget) *kube.Client { return t.kube(platformNS) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	force, stop := watchInterrupts(e.out, cancel)
	defer stop()
	e.force = force
	return e.run(ctx)
}

type destroyEnv struct {
	out     io.Writer
	logPath string
	in      install.Install
	rec     install.Record
	tfv     tfvarsInfo
	flags   DestroyCmd
	// version is this binary's release version, for the downgrade guard.
	version string

	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
	heartbeat time.Duration
	poll      time.Duration

	interactive bool
	prompt      func(label string) string

	aws       cmdRunner
	sessionOK func(minMinutes int) checkResult
	openTF    func() (tfRunner, string, error)
	cluster   func() (clusterTarget, checkResult, error)
	kube      func(t clusterTarget) *kube.Client

	// protectionOff is set once the database's deletion protection has been turned
	// off, so a later failure can say so.
	protectionOff bool
	force         <-chan struct{}
}

var darkplumNamespaces = map[string]bool{platformNS: true, inferenceNS: true}

func (e *destroyEnv) printf(format string, a ...any) { fmt.Fprintf(e.out, format, a...) }
func (e *destroyEnv) row(r checkResult)              { printCheckTo(e.out, r) }
func (e *destroyEnv) stop(r checkResult, msg string) error {
	e.row(r)
	return reportedError{msg}
}
func (e *destroyEnv) phase(title string) { e.printf("\n%s\n", sHeader(title)) }

func (e *destroyEnv) run(ctx context.Context) (err error) {
	rec := e.rec
	e.printf("%s\n", sHeader("▸ destroy · "+rec.ClusterName+" · "+rec.Region+" · account "+strings.Join(rec.AccountIDs, ",")))
	e.printf("%s\n", sDim("log: "+e.logPath))
	mutated := false
	defer func() {
		if err == nil {
			return
		}
		switch {
		case !mutated:
			e.printf("\n%s\n", sDim("Nothing was deleted or changed."))
		case e.protectionOff:
			e.printf("\n%s\n", sWarn("! The database's deletion protection is now OFF. Run `darkplum destroy` again to finish, or `darkplum deploy` to turn it back on."))
		default:
			e.printf("\n%s\n", sDim("Run `darkplum destroy` again: it works out from the state what is left."))
		}
	}()

	// ---- 1. credentials, before anything is read or deleted ---------------------
	e.phase("[1/5] Credentials (nothing is changed)")
	login := "aws sso login"
	if rec.Profile != "" {
		login += " --profile " + rec.Profile
	}
	caller, cerr := e.aws("aws", runner.ProfileArgs(rec.Profile, "sts", "get-caller-identity", "--region", rec.Region, "--query", "Account", "--output", "text")...)
	caller = strings.TrimSpace(caller)
	if cerr != nil || !accountRe12(caller) {
		return e.stop(checkResult{name: "aws credentials", status: checkFail, detail: "cannot read who the AWS session belongs to",
			hint: "run `" + login + "` and re-run. Nothing was deleted"}, "no valid AWS credentials")
	}
	pinned := false
	for _, a := range rec.AccountIDs {
		pinned = pinned || a == caller
	}
	if !pinned {
		return e.stop(checkResult{name: "aws account", status: checkFail,
			detail: fmt.Sprintf("credentials are for account %s, the install is in %s", caller, strings.Join(rec.AccountIDs, ",")),
			hint:   "use the profile for the install's account. Nothing was deleted"}, "wrong AWS account")
	}
	e.row(checkResult{name: "aws account", status: checkPass, detail: "account " + caller})
	if r := e.sessionOK(e.flags.MinSession); r.status == checkFail {
		return e.stop(r, "the AWS session is too short for a teardown")
	}

	if err := versionGuard(rec.Bundle, e.version, e.flags.AllowDowngrade); err != nil {
		return e.stop(checkResult{name: "release", status: checkFail, detail: firstLine(err.Error())}, "refusing to act with an older darkplum")
	}

	// ---- 2. read-only: state, cluster, what would block the teardown ------------
	e.phase("[2/5] What exists (nothing is changed)")
	tf, note, err := e.openTF()
	if err != nil {
		return e.stop(checkResult{name: "terraform", status: checkFail, detail: firstLine(err.Error())}, "cannot prepare terraform")
	}
	if note != "" {
		e.printf("%s\n", sWarn("! "+note))
	}
	if err := tf.Init(); err != nil {
		return e.stop(checkResult{name: "terraform init", status: checkFail, detail: firstLine(err.Error()), hint: "run `" + login + "` if the session lapsed"}, "terraform init failed")
	}
	state, err := stateList(tf)
	if err != nil {
		return e.stop(checkResult{name: "terraform state", status: checkFail, detail: firstLine(err.Error()),
			hint: "the state could not be read, so what exists is unknown (an expired session looks like this). If it is the session: `" + login + "`. Nothing was deleted"}, "cannot read the state")
	}
	managed := managedCount(state)
	if managed == 0 {
		e.row(checkResult{name: "terraform state", status: checkPass, detail: "empty: nothing to destroy"})
		return e.report(tf)
	}
	if _, err := checkStateOwnership(tf, rec, state, true); err != nil {
		return e.stop(checkResult{name: "terraform state", status: checkFail, detail: firstLine(err.Error()),
			hint: "check backend.hcl: destroy will not delete resources it cannot show belong to " + rec.ClusterName + ". Nothing was deleted"}, "the state is not this install's")
	}
	e.row(checkResult{name: "terraform state", status: checkPass, detail: fmt.Sprintf("%d resources, belonging to %s", managed, rec.ClusterName)})

	var kc *kube.Client
	var survey kubeSurvey
	t, row, cerr2 := e.cluster()
	switch {
	case cerr2 == nil:
		e.row(row)
		kc = e.kube(t)
		if survey, err = surveyCluster(kc); err != nil {
			return e.stop(checkResult{name: "cluster survey", status: checkFail, detail: firstLine(err.Error()),
				hint: "cannot list the cluster's load balancers and volumes. Nothing was deleted"}, "cannot survey the cluster")
		}
		if len(survey.Foreign) > 0 {
			return e.foreignStop(survey)
		}
		e.row(checkResult{name: "load balancers and volumes", status: checkPass,
			detail: fmt.Sprintf("%d load balancer service(s) and %d volume claim(s) in the Darkplum namespaces", len(survey.Services), len(survey.Claims))})
	case errors.Is(cerr2, errClusterNotFound):
		e.row(checkResult{name: "cluster", status: checkWarn, detail: "gone: " + rec.ClusterName + " does not exist in " + rec.Region,
			hint: "SKIPPING the Kubernetes phase. Load balancers and volumes its controllers created cannot be cleaned up and may block the VPC's deletion; check the console if the destroy stalls"})
	default:
		return e.stop(row, "cannot validate the cluster; nothing was deleted")
	}

	// ---- 3. typed confirmation --------------------------------------------------
	e.phase("[3/5] Confirm")
	e.printf("This permanently deletes cluster %s and everything Terraform built for it in %s (account %s):\n  the EKS cluster and nodes, the VPC, the database, the registries' images and the web address.\n",
		rec.ClusterName, rec.Region, caller)
	if err := e.confirmName(); err != nil {
		return err
	}

	// ---- 4. terraform-side preparation, before any Kubernetes deletion ----------
	e.phase("[4/5] Prepare the infrastructure for deletion")
	vf := "-var-file=" + tf.VarFile()
	// First, while nothing has been changed: can the teardown be planned, and does it
	// delete only what the state holds? If not, the database is still protected.
	if err := e.sanityPlan(ctx, tf, vf, state); err != nil {
		return err
	}
	const rdsAddr = "module.rds.aws_db_instance.this"
	if state[rdsAddr] {
		snap, err := e.finalSnapshot(tf)
		if err != nil {
			return e.stop(checkResult{name: "database snapshot", status: checkFail, detail: firstLine(err.Error()),
				hint: "cannot tell whether a final snapshot is configured. Nothing was deleted"}, "cannot read the snapshot setting")
		}
		if snap == "" {
			if err := e.confirmNoSnapshot(); err != nil {
				return err
			}
		} else {
			e.row(checkResult{name: "database snapshot", status: checkPass, detail: "a final snapshot named " + snap + " will be kept"})
		}
		mutated = true
		e.printf("%s\n", sInfo("→ turning off the database's deletion protection (a targeted apply)"))
		if _, _, err := streamTerraform(ctx, tf, e.out, e.now, e.heartbeat, "terraform apply (protection)", e.force,
			"apply", "-auto-approve", "-input=false", vf, "-var", "db_deletion_protection=false", "-target="+rdsAddr); err != nil {
			return e.stop(checkResult{name: "deletion protection", status: checkFail, detail: firstLine(err.Error()),
				hint: "the database is unchanged and Kubernetes was not touched. If the session lapsed: `" + login + "`"}, "cannot turn off deletion protection")
		}
		e.protectionOff = true
		e.row(checkResult{name: "deletion protection", status: checkPass, detail: "off"})
	} else {
		e.row(checkResult{name: "database", status: checkPass, detail: "not in the state: no deletion protection to turn off"})
	}
	if state[logGroupAddr] {
		mutated = true
		if _, err := tf.Capture("state", "rm", logGroupAddr); err != nil {
			return e.stop(checkResult{name: "audit log group", status: checkFail, detail: firstLine(err.Error()),
				hint: "it must leave the state or the destroy would delete the Kubernetes audit logs. Kubernetes was not touched"}, "cannot keep the audit log group")
		}
		e.row(checkResult{name: "audit log group", status: checkPass, detail: "removed from the state so the destroy keeps it"})
		delete(state, logGroupAddr)
	}

	// ---- 5. kubernetes, credentials again, terraform destroy ---------------------
	e.phase("[5/5] Delete")
	if kc != nil {
		mutated = true
		if err := e.kubePhase(ctx, kc); err != nil {
			return err
		}
	}
	// The destroy takes 10-15 minutes and cannot be resumed mid-resource: the same margin
	// as at the start.
	if r := e.sessionOK(30); r.status == checkFail {
		return e.stop(r, "the AWS session is too short to start the destroy")
	}
	if c, cerr := e.aws("aws", runner.ProfileArgs(rec.Profile, "sts", "get-caller-identity", "--region", rec.Region, "--query", "Account", "--output", "text")...); cerr != nil || strings.TrimSpace(c) != caller {
		return e.stop(checkResult{name: "aws credentials", status: checkFail, detail: "the session no longer belongs to account " + caller,
			hint: "run `" + login + "` and re-run `darkplum destroy`"}, "credentials changed")
	}
	mutated = true
	e.printf("%s\n", sInfo("→ terraform destroy (10-15 minutes)"))
	tl, _, derr := streamTerraform(ctx, tf, e.out, e.now, e.heartbeat, "terraform destroy", e.force, "destroy", "-auto-approve", "-input=false", vf)
	if derr != nil {
		hint := "see the output above; the full log is " + e.logPath
		if h := (&deployEnv{rec: rec}).loginHint(tl.String()); !strings.HasPrefix(h, "fix the above") {
			hint = h
		}
		return e.stop(checkResult{name: "terraform destroy", status: checkFail, detail: firstLine(derr.Error()), hint: hint}, "terraform destroy failed")
	}
	e.protectionOff = false
	e.row(checkResult{name: "infrastructure", status: checkPass, detail: "destroyed"})
	return e.report(tf)
}

func accountRe12(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// confirmName requires the cluster's name, typed or passed as --confirm. Anything
// else, including no way to ask, stops the run.
func (e *destroyEnv) confirmName() error {
	want := e.rec.ClusterName
	if e.flags.Confirm != "" {
		if e.flags.Confirm != want {
			return e.stop(checkResult{name: "confirmation", status: checkFail, detail: fmt.Sprintf("--confirm %q is not this install's cluster (%s)", e.flags.Confirm, want),
				hint: "nothing was deleted"}, "wrong --confirm")
		}
		e.row(checkResult{name: "confirmation", status: checkPass, detail: "--confirm " + want})
		return nil
	}
	if !e.interactive {
		return e.stop(checkResult{name: "confirmation", status: checkFail, detail: "nothing to type the cluster name into",
			hint: "pass --confirm " + want + " to destroy from a script. Nothing was deleted"}, "not confirmed")
	}
	if got := strings.TrimSpace(e.prompt("\nType the cluster name (" + want + ") to destroy it")); got != want {
		return e.stop(checkResult{name: "confirmation", status: checkFail, detail: "the name typed does not match", hint: "nothing was deleted"}, "not confirmed")
	}
	return nil
}

// confirmNoSnapshot is the second, separate question when the install takes no
// final snapshot: --confirm does not answer it.
func (e *destroyEnv) confirmNoSnapshot() error {
	want := e.rec.ClusterName
	e.row(checkResult{name: "database snapshot", status: checkWarn, detail: "none configured: the database and every conversation in it will be deleted with NO copy"})
	if e.flags.ConfirmNoSnapshot != "" {
		if e.flags.ConfirmNoSnapshot != want {
			return e.stop(checkResult{name: "confirmation", status: checkFail, detail: "--confirm-no-snapshot does not match the cluster name", hint: "nothing was deleted"}, "wrong --confirm-no-snapshot")
		}
		return nil
	}
	if !e.interactive {
		return e.stop(checkResult{name: "confirmation", status: checkFail, detail: "deleting the database without a snapshot needs its own confirmation",
			hint: "pass --confirm-no-snapshot " + want + " as well. Nothing was deleted"}, "no-snapshot not confirmed")
	}
	if got := strings.TrimSpace(e.prompt("\nType 'no snapshot' to delete the database with no copy")); got != "no snapshot" {
		return e.stop(checkResult{name: "confirmation", status: checkFail, detail: "not confirmed", hint: "nothing was deleted"}, "no-snapshot not confirmed")
	}
	return nil
}

// finalSnapshot is the name of the snapshot a destroy will leave, "" when none is
// configured or the output cannot say (treated as none: the stricter question).
func (e *destroyEnv) finalSnapshot(tf tfRunner) (string, error) {
	raw, err := tf.Capture("output", "-json")
	if err != nil {
		return "", err
	}
	var all map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return "", errors.New("unexpected terraform output")
	}
	var s *string
	if v, ok := all["rds_final_snapshot_identifier"]; ok {
		_ = json.Unmarshal(v.Value, &s)
	}
	if s == nil {
		return "", nil
	}
	return *s, nil
}

// sanityPlan runs `terraform plan -destroy` and refuses to go on unless it only
// deletes things the state already holds. A create or an update in a destroy plan,
// or a deletion of something the state does not list, means this is not the
// teardown that was confirmed.
func (e *destroyEnv) sanityPlan(ctx context.Context, tf tfRunner, vf string, state map[string]bool) error {
	planPath := filepath.Join(e.in.Dir(), "destroy-"+e.now().UTC().Format("20060102T150405Z")+".tfplan")
	defer os.Remove(planPath)
	if err := tf.Stream(ctx, io.Discard, runner.StreamOptions{Force: e.force}, "plan", "-destroy", "-input=false", "-out="+planPath, vf); err != nil {
		return e.stop(checkResult{name: "plan -destroy", status: checkFail, detail: firstLine(err.Error()),
			hint: "Terraform cannot plan the teardown; fix that first. Kubernetes was not touched"}, "the destroy does not plan")
	}
	js, err := tf.Capture("show", "-json", planPath)
	if err != nil {
		return e.stop(checkResult{name: "plan -destroy", status: checkFail, detail: firstLine(err.Error())}, "cannot read the destroy plan")
	}
	var p struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if json.Unmarshal(js, &p) != nil {
		return e.stop(checkResult{name: "plan -destroy", status: checkFail, detail: "unreadable plan"}, "cannot read the destroy plan")
	}
	deletes := 0
	var odd []string
	for _, rc := range p.ResourceChanges {
		a := rc.Change.Actions
		switch {
		case len(a) == 1 && (a[0] == "no-op" || a[0] == "read"):
		case len(a) == 1 && a[0] == "delete":
			deletes++
			if !state[rc.Address] {
				odd = append(odd, "delete "+rc.Address+" (not in the state)")
			}
		default:
			odd = append(odd, strings.Join(a, "+")+" "+rc.Address)
		}
	}
	if len(odd) > 0 {
		sort.Strings(odd)
		return e.stop(checkResult{name: "plan -destroy", status: checkFail, detail: "the plan does more than delete what the state holds: " + strings.Join(odd, "; "),
			hint: "Kubernetes was not touched"}, "unexpected destroy plan")
	}
	e.row(checkResult{name: "plan -destroy", status: checkPass, detail: fmt.Sprintf("deletes %d resources, all in the state", deletes)})
	return nil
}

// ---- kubernetes ---------------------------------------------------------------

type objRef struct{ NS, Name string }

func (o objRef) String() string { return o.NS + "/" + o.Name }

// kubeSurvey is what the cluster holds that outlives it: load balancer Services
// and volume claims. Services and Claims are in the Darkplum namespaces and are
// deleted; Foreign are anywhere else and stop the run.
type kubeSurvey struct {
	Services, Claims, Foreign []objRef
}

func surveyCluster(kc *kube.Client) (kubeSurvey, error) {
	var s kubeSurvey
	list := func(kind string) ([]struct {
		Metadata struct{ Name, Namespace string } `json:"metadata"`
		Spec     struct{ Type string }            `json:"spec"`
	}, error) {
		out, err := kc.Run(nil, "get", kind, "-A", "-o", "json")
		if err != nil {
			return nil, err
		}
		var l struct {
			Items []struct {
				Metadata struct{ Name, Namespace string } `json:"metadata"`
				Spec     struct{ Type string }            `json:"spec"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			return nil, fmt.Errorf("unexpected answer listing %s", kind)
		}
		return l.Items, nil
	}
	svcs, err := list("svc")
	if err != nil {
		return s, err
	}
	for _, it := range svcs {
		if it.Spec.Type != "LoadBalancer" {
			continue
		}
		r := objRef{it.Metadata.Namespace, it.Metadata.Name}
		if darkplumNamespaces[r.NS] {
			s.Services = append(s.Services, r)
		} else {
			s.Foreign = append(s.Foreign, r)
		}
	}
	pvcs, err := list("pvc")
	if err != nil {
		return s, err
	}
	for _, it := range pvcs {
		r := objRef{it.Metadata.Namespace, it.Metadata.Name}
		if darkplumNamespaces[r.NS] {
			s.Claims = append(s.Claims, r)
		} else {
			s.Foreign = append(s.Foreign, r)
		}
	}
	return s, nil
}

func (e *destroyEnv) foreignStop(s kubeSurvey) error {
	var names []string
	for _, f := range s.Foreign {
		names = append(names, f.String())
	}
	return e.stop(checkResult{name: "load balancers and volumes", status: checkFail,
		detail: "outside the Darkplum namespaces: " + strings.Join(names, ", "),
		hint: "Darkplum does not delete what it did not create, and these would keep the VPC from being deleted (load balancer network interfaces, attached volumes). " +
			"Remove them yourself, then re-run. Nothing was deleted"}, "the cluster has load balancers or volumes that are not Darkplum's")
}

// kubePhase deletes the load balancer Services and volume claims of the Darkplum
// namespaces and waits until they are really gone (a Service leaves only after
// its cloud load balancer has). The cluster's own deletion by Terraform takes
// everything else with it. Nothing is forced: a stuck object is reported.
func (e *destroyEnv) kubePhase(ctx context.Context, kc *kube.Client) error {
	// The survey is repeated now: the first one was read before the confirmation.
	s, err := surveyCluster(kc)
	if err != nil {
		return e.stop(checkResult{name: "cluster survey", status: checkFail, detail: firstLine(err.Error())}, "cannot survey the cluster")
	}
	if len(s.Foreign) > 0 {
		return e.foreignStop(s)
	}
	for _, o := range s.Services {
		if _, err := kc.Run(nil, "-n", o.NS, "delete", "svc", o.Name, "--wait=false", "--ignore-not-found"); err != nil {
			return e.stop(checkResult{name: "service " + o.String(), status: checkFail, detail: firstLine(err.Error())}, "cannot delete a load balancer service")
		}
	}
	if len(s.Claims) > 0 {
		// A claim stays Terminating for as long as a pod uses it. Stop the Darkplum
		// workloads (their namespaces only; the load balancers are already going) so
		// the claims, and the volumes behind them, can actually be released.
		for ns := range darkplumNamespaces {
			if _, err := kc.Run(nil, "-n", ns, "delete", "deployments,statefulsets,daemonsets,jobs", "--all", "--wait=false", "--ignore-not-found"); err != nil {
				return e.stop(checkResult{name: "workloads in " + ns, status: checkFail, detail: firstLine(err.Error())}, "cannot stop the workloads that use volumes")
			}
		}
	}
	for _, o := range s.Claims {
		if _, err := kc.Run(nil, "-n", o.NS, "delete", "pvc", o.Name, "--wait=false", "--ignore-not-found"); err != nil {
			return e.stop(checkResult{name: "volume claim " + o.String(), status: checkFail, detail: firstLine(err.Error())}, "cannot delete a volume claim")
		}
	}
	deadline := e.now().Add(5 * time.Minute)
	for {
		left, err := surveyCluster(kc)
		if err != nil {
			return e.stop(checkResult{name: "cluster survey", status: checkFail, detail: firstLine(err.Error())}, "cannot confirm the deletions")
		}
		if len(left.Services)+len(left.Claims) == 0 {
			e.row(checkResult{name: "kubernetes", status: checkPass, detail: fmt.Sprintf("%d load balancer service(s) and %d volume claim(s) are gone", len(s.Services), len(s.Claims))})
			if e.rec.SemanticRouter {
				// Nothing to wait for: the router has no load balancer and no volume, and
				// its cluster-scoped objects go with the cluster. A router installed by helm
				// goes with it too; destroy never needs helm.
				e.row(checkResult{name: "semantic router", status: checkPass, detail: "goes with the cluster (no load balancer or volume to wait for)"})
			}
			return nil
		}
		if e.now().After(deadline) {
			var names []string
			for _, o := range append(left.Services, left.Claims...) {
				names = append(names, o.String())
			}
			return e.stop(checkResult{name: "kubernetes", status: checkFail, detail: "still present after 5 minutes: " + strings.Join(names, ", "),
				hint: "something holds them (a pod using a volume, a finalizer). Nothing is forced. Inspect with `eval \"$(darkplum kubeconfig)\"` and kubectl, then re-run. The infrastructure is intact"}, "kubernetes resources did not go away")
		}
		e.printf("%s\n", sDim(fmt.Sprintf("  waiting for %d object(s) to be deleted ...", len(left.Services)+len(left.Claims))))
		if err := e.sleep(ctx, e.poll); err != nil {
			return err
		}
	}
}

// ---- what is left behind --------------------------------------------------------

// report lists, from what exists now, what a destroy deliberately leaves, with
// the command that deletes each. It deletes nothing.
func (e *destroyEnv) report(tf tfRunner) error {
	rec := e.rec
	e.phase("Left behind on purpose (Darkplum deletes none of these)")
	any := false
	item := func(what, detail string, cmds ...string) {
		any = true
		e.printf("  • %s %s\n", what, sDim(detail))
		for _, c := range cmds {
			e.printf("      %s\n", sDim("$ "+c))
		}
	}
	prof := func(args ...string) string {
		return "aws " + strings.Join(runner.ProfileArgs(rec.Profile, args...), " ")
	}
	check := func(what string, err error) {
		e.printf("  ? %s %s\n", what, sDim("could not be checked: "+firstLine(err.Error())))
	}

	// Final database snapshot(s).
	out, err := e.aws("aws", runner.ProfileArgs(rec.Profile, "rds", "describe-db-snapshots", "--region", rec.Region, "--snapshot-type", "manual",
		"--query", "DBSnapshots[?starts_with(DBSnapshotIdentifier, '"+rec.ClusterName+"-db-final-')].DBSnapshotIdentifier", "--output", "json")...)
	if err != nil {
		check("final database snapshot", err)
	} else {
		var ids []string
		_ = json.Unmarshal([]byte(out), &ids)
		for _, id := range ids {
			item("Final database snapshot "+id, "(costs storage until deleted)",
				prof("rds", "delete-db-snapshot", "--region", rec.Region, "--db-snapshot-identifier", id))
		}
	}
	// EKS audit log group.
	lg := "/aws/eks/" + rec.ClusterName + "/cluster"
	out, err = e.aws("aws", runner.ProfileArgs(rec.Profile, "logs", "describe-log-groups", "--region", rec.Region, "--log-group-name-prefix", lg,
		"--query", "length(logGroups[?logGroupName=='"+lg+"'])", "--output", "text")...)
	switch {
	case err != nil:
		check("EKS audit log group", err)
	case strings.TrimSpace(out) == "1":
		item("EKS audit log group "+lg, "(Kubernetes API audit logs; a reinstall adopts it)",
			prof("logs", "delete-log-group", "--region", rec.Region, "--log-group-name", lg))
	}
	// State bucket and key.
	b := rec.Backend
	item("Terraform state bucket s3://"+b.Bucket, "(holds the state history of this and any other install; shared, so check first)",
		"empty and delete it only when no install uses it, with the S3 console or `aws s3 rb s3://"+b.Bucket+" --force` (versioned buckets need their versions removed)")
	if b.KMSKeyID != "" {
		item("State encryption key "+b.KMSKeyID, "(deleting it makes every state file in the bucket unrecoverable)",
			prof("kms", "schedule-key-deletion", "--region", b.Region, "--key-id", b.KMSKeyID, "--pending-window-in-days", "30"))
	}
	// Hosted zone from the install's settings.
	// Only a zone `darkplum init` created is Darkplum's to mention; one the customer
	// supplied (web_zone_id in the settings) is theirs and is not listed.
	if z := rec.CreatedZoneID; z != "" {
		item("Route 53 hosted zone "+z, "(created by `darkplum init` for your web address; it is not in Terraform)",
			prof("route53", "delete-hosted-zone", "--id", z))
	}
	// ECR: the per-cluster repositories must be gone with the stack (looked up, not
	// assumed); the unprefixed ones of the Makefile-era install are left alone.
	var names []string
	for _, svc := range []string{"gateway", "api", "web"} {
		names = append(names, rec.ClusterName+"/darkplum-"+svc)
	}
	for _, svc := range []string{"gateway", "api", "web"} {
		names = append(names, "darkplum-"+svc)
	}
	for _, name := range names {
		_, err := e.aws("aws", runner.ProfileArgs(rec.Profile, "ecr", "describe-repositories", "--region", rec.Region, "--repository-names", name)...)
		switch {
		case err == nil && strings.Contains(name, "/"):
			item("ECR repository "+name, "(this install's own repository should have been deleted with the stack, but exists)",
				prof("ecr", "delete-repository", "--region", rec.Region, "--repository-name", name, "--force"))
		case err == nil:
			item("ECR repository "+name, "(from the Makefile-era install; never deleted by Darkplum)",
				prof("ecr", "delete-repository", "--region", rec.Region, "--repository-name", name, "--force"))
		case strings.Contains(err.Error(), "RepositoryNotFoundException"):
		default:
			check("ECR repository "+name, err)
		}
	}
	if !any {
		e.printf("  nothing found\n")
	}

	// Things that must NOT be left: anything the cluster's controllers made in the
	// account, found by the cluster tag (a load balancer, a network interface or a
	// security group keeps a VPC, and the bill, alive).
	if orphans := e.orphans(); len(orphans) > 0 {
		e.phase("Still exists and should not")
		for _, o := range orphans {
			e.printf("  %s %s\n", sFail("✗"), o)
		}
		e.printf("\n  %s\n", sDim("These carry this cluster's tag. Delete them (or find what holds them) before the VPC can go; re-run `darkplum destroy` after."))
		return reportedError{fmt.Sprintf("%d resource(s) tagged for the cluster still exist", len(orphans))}
	}
	e.printf("\n%s\n", sSuccess("✓ Done. Nothing tagged for the cluster is left in the account."))
	return nil
}

// orphans finds load balancers, network interfaces and security groups tagged for
// this cluster. A lookup that fails is printed and is not an orphan.
func (e *destroyEnv) orphans() []string {
	rec := e.rec
	c := rec.ClusterName
	prof := func(args ...string) []string {
		return runner.ProfileArgs(rec.Profile, append(args, "--region", rec.Region)...)
	}
	var found []string
	list := func(kind string, out string, err error) {
		if err != nil {
			e.printf("  ? %s %s\n", kind, sDim("could not be checked: "+firstLine(err.Error())))
			return
		}
		var ids []string
		_ = json.Unmarshal([]byte(out), &ids)
		for _, id := range ids {
			found = append(found, kind+" "+id)
		}
	}
	for _, f := range []string{"Name=tag-key,Values=kubernetes.io/cluster/" + c, "Name=tag:cluster.k8s.amazonaws.com/name,Values=" + c, "Name=tag:aws:eks:cluster-name,Values=" + c} {
		out, err := e.aws("aws", prof("ec2", "describe-network-interfaces", "--filters", f, "--query", "NetworkInterfaces[].NetworkInterfaceId", "--output", "json")...)
		list("network interface", out, err)
		out, err = e.aws("aws", prof("ec2", "describe-security-groups", "--filters", f, "--query", "SecurityGroups[].GroupId", "--output", "json")...)
		list("security group", out, err)
	}
	// Load balancers carry the tag on the balancer, so list them and ask for tags.
	out, err := e.aws("aws", prof("elbv2", "describe-load-balancers", "--query", "LoadBalancers[].LoadBalancerArn", "--output", "json")...)
	if err != nil {
		e.printf("  ? load balancer %s\n", sDim("could not be checked: "+firstLine(err.Error())))
	} else {
		var arns []string
		_ = json.Unmarshal([]byte(out), &arns)
		for i := 0; i < len(arns); i += 20 {
			end := min(i+20, len(arns))
			args := []string{"elbv2", "describe-tags", "--resource-arns"}
			args = append(args, arns[i:end]...)
			args = append(args, "--query", "TagDescriptions[?Tags[?(Key=='kubernetes.io/cluster/"+c+"' || (Key=='elbv2.k8s.aws/cluster' && Value=='"+c+"'))]].ResourceArn", "--output", "json")
			tags, err := e.aws("aws", prof(args...)...)
			list("load balancer", tags, err)
		}
	}
	sort.Strings(found)
	var uniq []string
	for i, f := range found {
		if i == 0 || f != found[i-1] {
			uniq = append(uniq, f)
		}
	}
	return uniq
}
