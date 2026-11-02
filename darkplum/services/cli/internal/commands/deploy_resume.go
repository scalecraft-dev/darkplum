package commands

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// Resuming a first install that was interrupted while EKS was being created.
//
// Two things go wrong, and both end in a deploy that can never finish:
//
//   - The cluster exists but the apply stopped before it made our access entry, so
//     every Kubernetes read the preflight makes is answered 401. Stopping there
//     means Terraform, which would make the access entry, is never reached.
//   - The cluster is tainted in the state (its create was interrupted). Terraform
//     plans the replacement of a tainted object, and for the cluster it plans it
//     create-before-destroy: it asks AWS for a second cluster of the same name while
//     the first exists, which is a 409, forever.
//
// Why the cluster is create-first (read from `terraform graph` of the shipped root,
// not guessed): create_before_destroy on an object makes every object it depends on
// create-first too. The cluster has more than one such dependent:
//
//   - the node-group submodules of the EKS module. Their launch template and node
//     group set create_before_destroy and take the cluster name from
//     time_sleep.this, which depends on the cluster. The submodule is in the graph
//     even with no node groups (an empty for_each still has its resources), and
//     Terraform propagates through it, so this alone makes the cluster create-first;
//   - our own database security group (modules/rds/main.tf), whose ingress takes
//     module.eks.cluster_primary_security_group_id, an attribute of the cluster.
//
// The cluster's own security groups are upstream of the cluster and are not the cause.
// Removing the lifecycle from our security group would therefore change nothing
// while the EKS module's submodules stay, which is why the fix is the untaint below
// and not a change to the database's rules.
//
// Both traps are acted on only when Terraform's own state says this is an
// interrupted FIRST install (interruptedFirstInstall). A finished install is never
// touched: there a 401 is a real access problem and a tainted cluster is a decision
// for a person.

const (
	// clusterAddr is the EKS cluster in the state.
	clusterAddr = "module.eks.module.eks.aws_eks_cluster.this[0]"
	// creatorAccessEntryAddr is the access entry the apply makes for the identity
	// that runs it (enable_cluster_creator_admin_permissions). It is created after
	// the cluster; a state that has it has an install whose Kubernetes access works.
	creatorAccessEntryAddr = `module.eks.module.eks.aws_eks_access_entry.this["cluster_creator"]`
	// eksModuleVersion is the terraform-aws-modules/eks release those two addresses,
	// and the key "cluster_creator", were read from. A bump must re-check them
	// (TestResumeAddressesMatchThePinnedEKSModule fails until this is changed with it).
	eksModuleVersion = "20.37.2"

	// clusterWaitLimit bounds the wait for a cluster that is still being created.
	clusterWaitLimit = 40 * time.Minute
	// kubeAccessWait bounds the wait for the cluster to honour the access entry the
	// apply has just made (EKS takes a little while to propagate it).
	kubeAccessWait = 3 * time.Minute
)

// createFirstDoomed says why replacing c create-first can never work, or "" when it
// might. The objects named here are all upstream of the cluster, so they are
// create-first whenever the cluster is (see above).
//
//   - The cluster and its log group have fixed names: the second one is refused
//     whatever made the plan replace it.
//   - A tainted subnet, security-group rule or CIDR reservation is replaced by an
//     identical one (same CIDR, same rule) while the old one exists: AWS refuses the
//     duplicate. A replacement forced by a changed CIDR is another matter and is not
//     refused here.
func createFirstDoomed(c planChange) string {
	if !c.Replace || !c.CreateFirst {
		return ""
	}
	addr := c.Address
	if i := strings.LastIndex(addr, "["); i >= 0 && strings.HasSuffix(addr, "]") {
		addr = addr[:i]
	}
	switch addr {
	case "module.eks.module.eks.aws_eks_cluster.this", "module.eks.module.eks.aws_cloudwatch_log_group.this":
		return "its name is fixed"
	}
	if c.Tainted {
		switch addr {
		case "module.vpc.module.vpc.aws_subnet.private", "module.vpc.aws_ec2_subnet_cidr_reservation.lb",
			"module.eks.module.eks.aws_security_group_rule.cluster", "module.eks.module.eks.aws_security_group_rule.node":
			return "the replacement is identical to the one it replaces, which still exists"
		}
	}
	return ""
}

// holdsData is true for an object whose replacement deletes data: a plan that
// replaces one is never described as a harmless leftover.
func holdsData(addr string) bool {
	if i := strings.LastIndex(addr, "["); i >= 0 && strings.HasSuffix(addr, "]") {
		addr = addr[:i]
	}
	parts := strings.Split(addr, ".")
	if len(parts) < 2 {
		return false
	}
	switch parts[len(parts)-2] {
	case "aws_db_instance", "aws_rds_cluster", "aws_ecr_repository", "aws_cloudwatch_log_group", "aws_kms_key",
		"aws_s3_bucket", "aws_route53_zone", "aws_ebs_volume", "aws_efs_file_system":
		return true
	}
	return false
}

// errKubeDeferred says the Kubernetes checks were postponed (see kubeRejected).
var errKubeDeferred = errors.New("the Kubernetes checks are postponed until the apply has made this identity's access")

// deferredKube is the refusal that would have been made, kept until Terraform's
// state can say whether the 401 is the expected one.
type deferredKube struct {
	row checkResult
	msg string
}

// firstInstallInProgress is the local half of the evidence: this workspace has a
// deploy that began and has not finished. (A re-deploy of a finished install is also
// "in progress" from its start, so this alone proves nothing; it only keeps the
// common case, a finished install, from ever being treated as anything else.)
func firstInstallInProgress(e *deployEnv) bool {
	c, ok := loadDeployClock(e.in)
	return ok && c.Finished.IsZero()
}

// interruptedFirstInstall is true when a deploy is unfinished AND Terraform's state
// holds the cluster but not the access entry the apply makes right after it: the
// apply stopped in that window. A finished install always has the entry, so for it
// this is false. Where the state is in any doubt it is false.
func (e *deployEnv) interruptedFirstInstall(state map[string]bool) bool {
	return firstInstallInProgress(e) && state[clusterAddr] && !state[creatorAccessEntryAddr]
}

// kubeRejected handles a cluster that answers 401 to a read. At the preflight, on an
// unfinished deploy, it postpones the refusal: the access entry that would make the
// read succeed is made by the apply, which the refusal would keep from ever running.
// Step 3 confirms from Terraform's state that this is the expected 401 and stops
// with the original refusal if it is not; nothing is applied before then. Everywhere
// else, and for a finished install, it is the hard stop it always was.
func (e *deployEnv) kubeRejected(refusal checkResult, msg string) error {
	if !e.deferKube || !firstInstallInProgress(e) {
		return e.stop(refusal, msg)
	}
	e.deferredKube = &deferredKube{row: refusal, msg: msg}
	e.row(checkResult{name: refusal.name, status: checkWarn,
		detail: "the cluster does not accept this identity's credentials yet",
		hint:   "an earlier first deploy stopped before Terraform made this identity's access to the cluster; continuing. Terraform's state is checked before anything is applied, and these checks run again before anything reaches Kubernetes"})
	return errKubeDeferred
}

// unchanged says, for a refusal's hint, what is true of the install: nothing at all
// before the apply, only the infrastructure after it. The refusals of the Kubernetes
// checks are made at both times, and "Nothing was changed" after an apply is false.
func (e *deployEnv) unchanged() string {
	if e.mutated {
		return "Terraform has already applied the infrastructure, but nothing was written to Kubernetes"
	}
	return "Nothing was changed"
}

// unauthorizedRow is the refusal for a cluster that rejects this identity.
func (e *deployEnv) unauthorizedRow(name string, err error) checkResult {
	return checkResult{name: name, status: checkFail,
		detail: "the cluster rejected this identity's credentials: " + firstLine(err.Error()),
		hint: "this AWS identity has no Kubernetes access to the cluster: it is not the one that installed it, or its access entry was removed " +
			"(`aws eks list-access-entries --cluster-name <cluster>` shows who has one). " + e.unchanged()}
}

// settleInterruptedInstall runs after Terraform's state has been read and before
// anything is planned. It confirms a postponed refusal, and makes an interrupted
// first install's cluster usable again.
func (e *deployEnv) settleInterruptedInstall(ctx context.Context, tf tfRunner, state map[string]bool) error {
	interrupted := e.interruptedFirstInstall(state)
	e.firstInstallInterrupted = interrupted
	if d := e.deferredKube; d != nil {
		if !interrupted {
			why := "Terraform's state does not show a cluster created without this identity's access entry (no cluster in the state, or no unfinished deploy), so the rejection is not explained by an interrupted first install"
			if state[creatorAccessEntryAddr] {
				why = "Terraform's state already holds this install's access entry, so this is not the case of an access entry that was never made"
			}
			e.printf("      %s\n", sDim("("+why+")"))
			return e.stop(d.row, d.msg)
		}
		e.row(checkResult{name: "kubernetes access", status: checkPass,
			detail: "not made yet: the earlier apply stopped after the cluster; this apply makes it"})
	}
	if !interrupted {
		return nil
	}
	return e.untaintInterruptedCluster(ctx, tf)
}

// eksStatus is the cluster's status as AWS reports it; found is false when AWS has
// no such cluster.
func (e *deployEnv) eksStatus() (status string, found bool, err error) {
	out, err := e.aws("aws", runner.ProfileArgs(e.rec.Profile, "eks", "describe-cluster", "--name", e.rec.ClusterName,
		"--region", e.rec.Region, "--query", "cluster.status", "--output", "text")...)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

// untaintInterruptedCluster clears the taint an interrupted create left on the EKS
// cluster, once AWS says the cluster is ACTIVE. Only for an interrupted first
// install (the caller's check). Every doubt is a stop that says what to do; nothing
// is untainted while the cluster is anything but ACTIVE.
func (e *deployEnv) untaintInterruptedCluster(ctx context.Context, tf tfRunner) error {
	raw, err := tf.Capture("state", "pull")
	if err != nil {
		printTFDiagnostic(e.out, err)
		return e.stop(checkResult{name: "cluster state", status: checkFail, detail: firstLine(err.Error()),
			hint: e.loginHint(tfErrorText(err)) + ". Could not tell whether the cluster is tainted; nothing was applied"}, "cannot read the state")
	}
	tainted, err := taintedInstances(raw)
	if err != nil {
		return e.stop(checkResult{name: "cluster state", status: checkFail, detail: err.Error(),
			hint: "nothing was applied"}, "cannot read the state")
	}
	if !tainted[clusterAddr] {
		return nil
	}
	name := e.rec.ClusterName
	start := e.now()
	lastMinute := -1
	for {
		status, found, err := e.eksStatus()
		if err != nil {
			return e.stop(checkResult{name: "cluster", status: checkFail, detail: firstLine(err.Error()),
				hint: e.loginHint(err.Error()) + ". The cluster is tainted in the state and its status could not be read; nothing was applied"}, "cannot read the cluster's status")
		}
		if !found {
			// Nothing in AWS for the taint to protect: Terraform plans a plain create.
			e.row(checkResult{name: "cluster", status: checkWarn, detail: name + " is tainted in the state and does not exist in AWS",
				hint: "Terraform creates it again"})
			return nil
		}
		switch status {
		case "ACTIVE":
			e.mutated = true
			if _, err := tf.Capture("untaint", clusterAddr); err != nil {
				printTFDiagnostic(e.out, err)
				return e.stop(checkResult{name: "cluster", status: checkFail, detail: firstLine(err.Error()),
					hint: "could not clear the taint on " + clusterAddr + "; without that the plan replaces the cluster and fails. Nothing was applied"}, "cannot untaint the cluster")
			}
			e.row(checkResult{name: "cluster", status: checkPass,
				detail: "untainted " + name + ": an earlier deploy was interrupted while AWS created it, and AWS reports it ACTIVE"})
			return nil
		case "CREATING", "UPDATING":
			el := e.now().Sub(start)
			if el > clusterWaitLimit {
				return e.stop(checkResult{name: "cluster", status: checkFail,
					detail: fmt.Sprintf("%s is still %s after %d minutes", name, status, int(clusterWaitLimit.Minutes())),
					hint:   "AWS is taking unusually long. Look at the cluster in the console; when it is ACTIVE or FAILED, re-run `darkplum deploy`. Nothing was applied"}, "the cluster is not ready")
			}
			if m := int(el.Minutes()); m != lastMinute {
				lastMinute = m
				e.printf("%s\n", sDim(fmt.Sprintf("  waiting for cluster %s: %s (%dm%02ds); Terraform's earlier apply was interrupted while it was being created",
					name, status, m, int(el.Seconds())%60)))
			}
			if err := e.sleep(ctx, e.poll); err != nil {
				return e.checkInterrupt(ctx)
			}
			if err := e.checkInterrupt(ctx); err != nil {
				return err
			}
		case "FAILED":
			del := "aws eks delete-cluster --name " + name + " --region " + e.rec.Region
			if e.rec.Profile != "" {
				del += " --profile " + e.rec.Profile
			}
			return e.stop(checkResult{name: "cluster", status: checkFail, detail: name + " is FAILED: AWS could not create it",
				hint: "it cannot be used or untainted. Delete the failed cluster (`" + del + "`), wait until it is gone, then re-run `darkplum deploy`: Terraform creates it again. Nothing was applied"}, "the cluster failed")
		default:
			return e.stop(checkResult{name: "cluster", status: checkFail, detail: name + " is " + status + ", tainted in the state",
				hint: "not touched: only an ACTIVE cluster is untainted, and only after CREATING ends. Wait for it to settle (or finish deleting), then re-run `darkplum deploy`. Nothing was applied"}, "the cluster is not in a state to continue")
		}
	}
}

// waitForKubeAccess is the short wait, after the apply has made this identity's
// access entry, for the cluster to honour it: EKS propagates an access entry in
// seconds, not instantly. Only a 401 is waited out, for at most kubeAccessWait, and
// then the checks run and refuse for real (a rejection that outlasts the window is
// not propagation). Anything else, including success, ends the wait at once.
func (e *deployEnv) waitForKubeAccess(ctx context.Context, probe func() error) {
	start := e.now()
	first := true
	for {
		err := probe()
		if !kubeUnauthorized(err) {
			return
		}
		if e.now().Sub(start) >= kubeAccessWait || ctx.Err() != nil {
			return
		}
		if first {
			first = false
			e.printf("%s\n", sDim("  the cluster does not accept this identity yet; an access entry takes a short while to propagate. Waiting up to "+
				fmt.Sprintf("%d minutes", int(kubeAccessWait.Minutes()))+" for EKS to honour it"))
		}
		if e.sleep(ctx, e.poll) != nil {
			return
		}
	}
}

// confirmDestructive is the plan gate: what the plan deletes or replaces is listed,
// and nothing goes ahead without --confirm-plan or a typed yes.
//
// A replacement that is only the leftover of an interrupted apply says so. A resource
// is tainted when an apply stopped while creating it; Terraform will not trust it and
// replaces it. It is still not approved on its own account, and it is called a
// harmless leftover only when this is an interrupted first install AND nothing on the
// list holds data: the list can hold a database as easily as a NAT gateway.
func (e *deployEnv) confirmDestructive(tf tfRunner, dangerous []planChange) error {
	if len(dangerous) == 0 {
		return nil
	}
	for _, d := range dangerous {
		if why := createFirstDoomed(d); why != "" {
			return e.refuseDoomedReplacement(tf, d, why)
		}
	}
	tainted, data := 0, 0
	for _, d := range dangerous {
		if d.Tainted {
			tainted++
		}
		if holdsData(d.Address) {
			data++
		}
	}
	// (A list that holds data is answered by its own case below, ahead of this one.)
	harmless := e.firstInstallInterrupted && tainted == len(dangerous)
	e.printf("\n%s\n", sWarn(fmt.Sprintf("! This plan deletes or replaces %d resource(s):", len(dangerous))))
	for _, d := range dangerous {
		what := "delete"
		if d.Replace {
			what = "replace"
		}
		note := ""
		if d.Tainted {
			note = "  " + sDim("(tainted: left half-built by an interrupted or failed apply)")
		}
		if holdsData(d.Address) {
			note += "  " + sWarn("(holds data: replacing it deletes what is in it)")
		}
		e.printf("    %-8s %s%s\n", what, d.Address, note)
	}
	if tainted > 0 {
		verb := "are"
		if tainted == 1 {
			verb = "is"
		}
		e.printf("%s\n", sDim(fmt.Sprintf("  %d of them %s tainted: an apply was interrupted or failed while creating them, so Terraform replaces them rather than trust them.", tainted, verb)))
	}
	switch {
	case e.flags.ConfirmPlan:
		e.printf("%s\n", sDim("approved by --confirm-plan"))
	case e.interactive && e.confirm("\nType 'yes' to apply this plan, anything else cancels: "):
	default:
		hint := "review the list above; re-run with --confirm-plan (or answer 'yes' at the prompt) to approve it. Nothing was applied"
		detail := "deletes or replaces resources and was not approved"
		switch {
		case data > 0:
			hint = "the list holds data (marked above): replacing it deletes what is in it, whatever made the plan do so. Read it, and re-run with --confirm-plan (or answer 'yes' at the prompt) only if that is meant. Nothing was applied"
		case harmless:
			detail = fmt.Sprintf("replaces %d half-built resource(s) left by an interrupted first install, and was not approved", tainted)
			hint = "this install's first deploy was interrupted, every item above is tainted by it, and none holds data: replacing them is how it finishes. Re-run with --confirm-plan (or answer 'yes' at the prompt). Nothing was applied"
		case tainted == len(dangerous):
			hint = "every item above is tainted (an apply was interrupted or failed while creating it), but the state does not show a first install interrupted before its access was made, so darkplum cannot tell what else the replacement affects. Review the list; re-run with --confirm-plan (or answer 'yes') only if replacing them is meant. Nothing was applied"
		}
		return e.stop(checkResult{name: "plan", status: checkFail, detail: detail, hint: hint}, "plan not approved")
	}
	return nil
}

// refuseDoomedReplacement stops a plan that replaces an object create-first where
// AWS will refuse the second one. --confirm-plan does not apply: nothing is gained
// by trying, and the apply would fail partway.
func (e *deployEnv) refuseDoomedReplacement(tf tfRunner, d planChange, why string) error {
	e.printf("\n%s\n", sWarn("! This plan replaces "+d.Address+" by creating the new one first."))
	untaint := tf.Command("untaint", d.Address)
	var hint string
	switch {
	case d.Tainted && d.Address == clusterAddr:
		hint = "The cluster is tainted: an apply was interrupted while creating it. darkplum clears that taint itself only for an interrupted FIRST install whose cluster AWS reports ACTIVE; " +
			"this one does not qualify, and replacing a cluster that matters is a person's decision. If the cluster is healthy in AWS, clear the taint and re-run: " + untaint + " . " +
			"If it must be rebuilt, `darkplum destroy` removes the whole install, the database included (a final snapshot is kept only if one is configured), and `darkplum deploy` builds it again"
	case d.Tainted:
		hint = "It is tainted: an apply was interrupted while creating it. If it is healthy in AWS, clear the taint and re-run: " + untaint
	default:
		hint = "Something in the configuration forces a new one: see the plan above for what changed, undo that change, and re-run"
	}
	return e.stop(checkResult{name: "plan", status: checkFail,
		detail: "replacing " + d.Address + " cannot work: " + why + ", and Terraform creates the new one before it destroys the old one, so AWS refuses the second",
		hint:   "--confirm-plan does not apply to this. " + hint + ". Nothing was applied"}, "the plan replaces an object that cannot be replaced create-first")
}

// interruptedHint says what an interrupt left, truthfully for either kind: a first
// Ctrl-C asks Terraform to stop and waits for it; a second kills it.
func (e *deployEnv) interruptedHint() string {
	select {
	case <-e.force:
		return "interrupted twice: Terraform was killed, not stopped. Its state lock may still be held and a resource may be half-built. " +
			"Re-run `darkplum deploy` to continue; if it reports the lock, the message names the command that releases it"
	default:
		return "interrupted: Terraform was asked to stop and has exited, with the work it had under way left in the state (a resource it was creating is tainted). " +
			"Re-run `darkplum deploy` to continue"
	}
}

// lockIDRe finds the id Terraform prints under "Lock Info:" when the state is locked.
var lockIDRe = regexp.MustCompile(`(?m)^[\s│]*ID:\s+([0-9A-Fa-f-]{36})\s*$`)

// lockHint is the remedy for a state lock held by another run: the exact command,
// with the lock's id when Terraform printed it. force-unlock asks for confirmation.
func lockHint(tf tfRunner, output string) string {
	id := "LOCK_ID"
	suffix := " (replace LOCK_ID with the ID printed above)"
	// Terraform colours its output (deploy does not pass -no-color), and the box it
	// draws around the diagnostic puts escape codes between the bar and the text.
	if m := lockIDRe.FindStringSubmatch(ansiRe.ReplaceAllString(output, "")); m != nil {
		id, suffix = m[1], ""
	}
	return "another Terraform run holds the state lock (its details are printed above); a run that was killed by a second Ctrl-C leaves one behind. " +
		"If you are sure nothing else is running, release it: " + tf.Command("force-unlock", id) + suffix
}
