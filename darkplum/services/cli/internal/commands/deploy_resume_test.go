package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The resume traps of a first install interrupted while EKS was being created
// (live e2e row 4a, rc.4). See deploy_resume.go.

const (
	// The state of a first apply that stopped after the cluster and before the access
	// entry that the apply makes for the identity running it.
	interruptedState = "module.vpc.module.vpc.aws_vpc.this[0]\n" + clusterAddr + "\n"
	// The same state after the access entry exists: a finished install has it.
	finishedState = interruptedState + creatorAccessEntryAddr + "\n"

	kube401 = "kubectl: E1102 05:22:42.410092   65597 memcache.go:265] couldn't get current server API group list: the server has asked for the client to provide credentials"
)

func (h *deployHarness) markUnfinished() {
	h.t.Helper()
	if err := saveDeployClock(h.in, deployClock{Started: h.clock.now()}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *deployHarness) markFinished() {
	h.t.Helper()
	if err := saveDeployClock(h.in, deployClock{Started: h.clock.now().Add(-time.Hour), Finished: h.clock.now()}); err != nil {
		h.t.Fatal(err)
	}
}

// activeCluster makes the preflight see an ACTIVE cluster.
func (h *deployHarness) activeCluster() { h.awsf.base.describeErr = nil }

// rejectUntilApplied answers every Kubernetes read with 401 until Terraform has
// applied, as a cluster does that has no access entry for this identity yet. After
// the apply the reads are answered as the harness scripts them, except where an
// override says otherwise (the first override whose pattern matches wins).
func (h *deployHarness) rejectUntilApplied(overrides ...skHandler) {
	h.rejectUntil(func() bool {
		h.tf.mu.Lock()
		defer h.tf.mu.Unlock()
		return h.tf.applied
	}, overrides...)
}

// rejectUntil is rejectUntilApplied with the moment the 401s stop in the test's hands.
func (h *deployHarness) rejectUntil(ok func() bool, overrides ...skHandler) {
	orig := append([]skHandler(nil), h.k.handlers...)
	h.k.handlers = []skHandler{{"", func(args []string, stdin []byte) ([]byte, error) {
		if !ok() {
			return nil, errors.New(kube401)
		}
		joined := strings.Join(args, " ")
		for _, o := range append(append([]skHandler(nil), overrides...), orig...) {
			if strings.Contains(joined, o.pattern) {
				return o.fn(args, stdin)
			}
		}
		return nil, errors.New("unexpected kubectl " + joined)
	}}}
}

func (h *deployHarness) ranBefore(a, b string) bool {
	h.t.Helper()
	ia, ib := h.tl.first(a), h.tl.first(b)
	return ia >= 0 && ib >= 0 && ia < ib
}

// ---- bug B: a 401 before the apply has made our access ------------------------------

func TestDeployResumesWhenTheClusterRejectsUsBeforeOurAccessEntryExists(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.rejectUntilApplied()
	if err := h.run(); err != nil {
		t.Fatalf("an install interrupted between the cluster and its access entry could not resume:\n%v\n%s", err, h.out.String())
	}
	if !h.tf.ran("apply") {
		t.Fatal("Terraform never made the access entry")
	}
	h.mustContain("does not accept this identity's credentials yet", "kubernetes access")
	// Nothing was written to the cluster before the apply, and the postponed checks ran
	// again once it was up (the web Service was read at the preflight and again at step 6).
	applyAt := h.tl.first("terraform apply")
	h.tl.mu.Lock()
	for i, ev := range h.tl.ev {
		if i < applyAt && strings.HasPrefix(ev, "kubectl") && (strings.Contains(ev, " apply") || strings.Contains(ev, " create") || strings.Contains(ev, " patch")) {
			t.Errorf("Kubernetes written before the apply: %s", ev)
		}
	}
	h.tl.mu.Unlock()
	if n := h.tl.count("get svc darkplum-web"); n < 2 {
		t.Errorf("the web exposure check ran %d time(s), want it repeated after the apply", n)
	}
}

func TestDeployStillRefusesA401OnAFinishedInstall(t *testing.T) {
	// The workspace says the last deploy finished: a 401 is a real access problem and
	// stops at the preflight, before Terraform, as it always did.
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markFinished()
	h.tf.stateOut = finishedState
	h.rejectUntilApplied()
	if !IsReported(h.run()) {
		t.Fatal("went on past a cluster that rejects the identity of a finished install")
	}
	if h.tl.count("terraform") != 0 {
		t.Errorf("terraform ran:\n%s", h.tl)
	}
	h.mustContain("rejected this identity's credentials", "Nothing was changed")
}

func TestDeployRefusesA401WhenTerraformsStateHoldsOurAccessEntry(t *testing.T) {
	// An unfinished deploy (an interrupted upgrade, say) of an install whose access
	// entry exists: the postponed refusal is confirmed against the state, and it is
	// real. Stops before the plan.
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = finishedState
	h.rejectUntilApplied()
	if !IsReported(h.run()) {
		t.Fatal("went on although the state holds the access entry, so the rejection is real")
	}
	if h.tf.ran("plan") || h.tf.ran("apply") {
		t.Errorf("planned or applied:\n%s", h.tl)
	}
	h.mustContain("rejected this identity's credentials", "not the case of an access entry that was never made", "Nothing was changed")
}

// What a second Ctrl-C during CREATING leaves: AWS has the cluster, the state does not.
func TestDeployNamesAClusterOutsideTheStateOnA401(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = "module.vpc.module.vpc.aws_vpc.this[0]\n"
	h.rejectUntilApplied()
	if !IsReported(h.run()) || h.tf.ran("plan") {
		t.Fatalf("went on:\n%s", h.out.String())
	}
	h.mustContain("exists in AWS", "state does not hold it", "describe-cluster --name", "delete-cluster --name", "-web", "Importing the cluster does not work", "someone else's", "Nothing was changed")
}

func TestDeployStopsAPlanThatCreatesAClusterAWSAlreadyHas(t *testing.T) {
	// CREATING: no 401 yet, so only the plan can show it. --confirm-plan does not help.
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.env.flags.ConfirmPlan = true
	h.tf.stateOut = "module.vpc.module.vpc.aws_vpc.this[0]\n"
	h.tf.planJSON = `{"resource_changes":[{"address":"` + clusterAddr + `","change":{"actions":["create"]}}]}`
	if !IsReported(h.run()) {
		t.Fatalf("went on:\n%s", h.out.String())
	}
	if h.tf.ran("apply") {
		t.Errorf("applied a plan that can only end in 409:\n%s", h.tl)
	}
	h.mustContain("exists in AWS", "state does not hold it")
}

func TestDeploySaysWaitForAClusterThatIsBeingDeleted(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.awsf.eksStatus = func() (string, error) { return "DELETING\n", nil }
	h.tf.stateOut = "module.vpc.module.vpc.aws_vpc.this[0]\n"
	h.tf.planJSON = `{"resource_changes":[{"address":"` + clusterAddr + `","change":{"actions":["create"]}}]}`
	if !IsReported(h.run()) || h.tf.ran("apply") {
		t.Fatalf("went on:\n%s", h.out.String())
	}
	h.mustContain("is being deleted", "wait for it to go")
	if strings.Contains(h.out.String(), "delete-cluster") {
		t.Errorf("told to delete a cluster already going:\n%s", h.out)
	}
}

func TestDeployStopsWhenItCannotReadTheClusterAPlanWouldCreate(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.awsf.eksStatus = func() (string, error) { return "", errors.New("AccessDeniedException: no") }
	h.tf.stateOut = "module.vpc.module.vpc.aws_vpc.this[0]\n"
	h.tf.planJSON = `{"resource_changes":[{"address":"` + clusterAddr + `","change":{"actions":["create"]}}]}`
	if !IsReported(h.run()) || h.tf.ran("apply") {
		t.Fatalf("went on:\n%s", h.out.String())
	}
	h.mustContain("could not read cluster", "AccessDeniedException", "eks:DescribeCluster")
}

func TestDoomedClusterOffersStateRmOnlyToAnInstallThatNeverFinished(t *testing.T) {
	const forced = `{"resource_changes":[{"address":"` + clusterAddr + `","action_reason":"replace_because_cannot_update","change":{"actions":["create","delete"]}}]}`
	run := func(mark func(*deployHarness)) string {
		h := newDeployHarness(t, 0)
		h.activeCluster()
		mark(h)
		h.tf.stateOut = finishedState
		h.tf.planJSON = forced
		if !IsReported(h.run()) || h.tf.ran("apply") {
			t.Fatalf("went on:\n%s", h.out.String())
		}
		return h.out.String()
	}
	if out := run((*deployHarness).markUnfinished); !strings.Contains(out, "state rm") {
		t.Errorf("no way out for a hand import on an install that never finished:\n%s", out)
	}
	// Re-deploy of a finished install: the marker is carried, and a live cluster is
	// never offered to be un-tracked.
	redeploy := func(h *deployHarness) {
		now := h.clock.now()
		if err := saveDeployClock(h.in, deployClock{Started: now, EverFinished: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if out := run(redeploy); strings.Contains(out, "state rm") {
		t.Errorf("offered to un-track a cluster on an install that has finished:\n%s", out)
	}
}

func TestReDeployCarriesThatADeployOnceFinished(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.markFinished()
	before, _ := loadDeployClock(h.in)
	h.env.markDeployStart()
	c, ok := loadDeployClock(h.in)
	if !ok || !c.Finished.IsZero() {
		t.Fatalf("a re-deploy starts a new clock: %+v", c)
	}
	if !c.EverFinished.Equal(before.Finished) {
		t.Errorf("EverFinished = %v, want %v", c.EverFinished, before.Finished)
	}
	if firstDeployNeverFinished(h.in) {
		t.Error("a re-deploy of a finished install counts as a first deploy")
	}
	// And it survives a further interrupted start.
	h.env.markDeployStart()
	if firstDeployNeverFinished(h.in) {
		t.Error("lost across a second start")
	}
}

func TestDeployCreatesTheClusterWhenAWSHasNone(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.planJSON = `{"resource_changes":[{"address":"` + clusterAddr + `","change":{"actions":["create"]}}]}`
	_ = h.run()
	if !h.tf.ran("apply") {
		t.Fatalf("a fresh install's plan creates the cluster and must apply:\n%s", h.out.String())
	}
}

func TestWebLoadBalancerNameMatchesTheModule(t *testing.T) {
	if got := webLoadBalancerName("darkplum"); got != "darkplum-web" {
		t.Error(got)
	}
	if got := webLoadBalancerName("a-cluster-name-of-thirty-chars"); got != "a-cluster-name-of-thirty-web" {
		t.Error(got)
	}
}

func TestPlansToCreateIsOnlyAnOutrightCreate(t *testing.T) {
	plan := func(actions string) []byte {
		return []byte(`{"resource_changes":[{"address":"` + clusterAddr + `","change":{"actions":[` + actions + `]}}]}`)
	}
	for actions, want := range map[string]bool{`"create"`: true, `"create","delete"`: false, `"delete","create"`: false, `"no-op"`: false, `"update"`: false} {
		if got := plansToCreate(plan(actions), clusterAddr); got != want {
			t.Errorf("%s: got %v", actions, got)
		}
	}
	if plansToCreate(plan(`"create"`), "module.other") || plansToCreate([]byte("nope"), clusterAddr) {
		t.Error("another address, or an unreadable plan, is not a create")
	}
}

func TestDeployWithNoDeployClockKeepsTheHardStopOn401(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.tf.stateOut = interruptedState
	h.rejectUntilApplied()
	if !IsReported(h.run()) || h.tl.count("terraform") != 0 {
		t.Fatal("a 401 with no record of an unfinished deploy must stop at the preflight")
	}
}

func TestDeployForbiddenIsNotPostponed(t *testing.T) {
	// 403 is "connected, and this identity may not": a different problem, never waited out.
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.k.handlers = append([]skHandler{{"get svc darkplum-web", func([]string, []byte) ([]byte, error) {
		return nil, errors.New(`kubectl: Error from server (Forbidden): services "darkplum-web" is forbidden: User "x" cannot get resource`)
	}}}, h.k.handlers...)
	if !IsReported(h.run()) || h.tl.count("terraform") != 0 {
		t.Fatal("a Forbidden answer was treated as the expected 401")
	}
}

func TestDeployPostponesEveryKubernetesReadThatGetsA401(t *testing.T) {
	// Each pre-apply read is tried on its own: web exposure, the release record, the
	// helm lookup. Each, rejected, is postponed on an interrupted first install and
	// refused on a finished one.
	for _, read := range []struct{ name, pattern, row string }{
		{"web exposure", "get svc darkplum-web", "web exposure"},
		{"release record", "get configmap darkplum-install", "release"},
		{"helm releases", "owner=helm", "semantic router"},
	} {
		t.Run(read.name, func(t *testing.T) {
			for _, finished := range []bool{false, true} {
				h := newDeployHarness(t, 0)
				h.activeCluster()
				h.tf.stateOut = interruptedState
				if finished {
					h.markFinished()
				} else {
					h.markUnfinished()
				}
				orig := append([]skHandler(nil), h.k.handlers...)
				h.k.handlers = []skHandler{{"", func(args []string, stdin []byte) ([]byte, error) {
					joined := strings.Join(args, " ")
					h.tf.mu.Lock()
					applied := h.tf.applied
					h.tf.mu.Unlock()
					if strings.Contains(joined, read.pattern) && !applied {
						return nil, errors.New(kube401)
					}
					for _, o := range orig {
						if strings.Contains(joined, o.pattern) {
							return o.fn(args, stdin)
						}
					}
					return nil, errors.New("unexpected kubectl " + joined)
				}}}
				err := h.run()
				if finished {
					if !IsReported(err) || h.tl.count("terraform") != 0 || !strings.Contains(h.out.String(), "rejected this identity's credentials") {
						t.Errorf("finished install, %s rejected: err=%v\n%s", read.name, err, h.out.String())
					}
					continue
				}
				if err != nil || !h.tf.ran("apply") {
					t.Errorf("interrupted first install, %s rejected: err=%v\n%s", read.name, err, h.out.String())
				}
			}
		})
	}
}

// ---- bug A: a tainted cluster -------------------------------------------------------

func TestDeployUntaintsAnInterruptedClusterOnceItIsActive(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.tf.tainted = []string{clusterAddr}
	h.rejectUntilApplied()
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	var untaint []string
	for _, c := range h.tf.cmds {
		if c[0] == "untaint" {
			untaint = c
		}
	}
	if len(untaint) != 2 || untaint[1] != clusterAddr {
		t.Fatalf("untaint = %v", untaint)
	}
	if !h.ranBefore("terraform untaint", "terraform plan") {
		t.Errorf("the taint must be cleared before the plan sees it:\n%s", h.tl)
	}
	if len(h.tf.tainted) != 0 {
		t.Errorf("still tainted: %v", h.tf.tainted)
	}
	h.mustContain("untainted dp-trial")
}

func TestDeployWaitsForACreatingClusterBeforeUntainting(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.awsf.base.status, h.awsf.base.noEndpoint = "CREATING", true
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.tf.tainted = []string{clusterAddr}
	polls := 0
	h.awsf.eksStatus = func() (string, error) {
		polls++
		if polls < 4 {
			return "CREATING\n", nil
		}
		return "ACTIVE\n", nil
	}
	h.tf.onApply = func() { h.awsf.base.status, h.awsf.base.noEndpoint = "ACTIVE", false }
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if polls != 4 {
		t.Errorf("status read %d times, want 4 (three CREATING, then ACTIVE)", polls)
	}
	if !h.ranBefore("terraform untaint", "terraform plan") {
		t.Errorf("untaint must wait for ACTIVE and come before the plan:\n%s", h.tl)
	}
	h.mustContain("waiting for cluster dp-trial: CREATING")
}

func TestDeployNeverUntaintsAClusterThatIsNotActive(t *testing.T) {
	for _, status := range []string{"FAILED", "DELETING", "PENDING"} {
		t.Run(status, func(t *testing.T) {
			h := newDeployHarness(t, 0)
			h.activeCluster()
			h.markUnfinished()
			h.tf.stateOut = interruptedState
			h.tf.tainted = []string{clusterAddr}
			h.awsf.eksStatus = func() (string, error) { return status + "\n", nil }
			if !IsReported(h.run()) {
				t.Fatal("went on with a cluster that is " + status)
			}
			if h.tf.ran("untaint") || h.tf.ran("plan") || h.tf.ran("apply") {
				t.Errorf("touched a %s cluster:\n%s", status, h.tl)
			}
			if status == "FAILED" {
				h.mustContain("aws eks delete-cluster --name dp-trial", "FAILED")
			}
		})
	}
}

func TestDeployWaitForTheClusterIsBounded(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.tf.tainted = []string{clusterAddr}
	h.env.poll = 5 * time.Minute // the 40 minute bound is then about 9 reads
	polls := 0
	h.awsf.eksStatus = func() (string, error) {
		if polls++; polls > 200 {
			t.Fatal("the wait for the cluster has no bound")
		}
		return "CREATING\n", nil
	}
	if !IsReported(h.run()) {
		t.Fatal("waited forever")
	}
	if h.tf.ran("untaint") || h.tf.ran("plan") {
		t.Errorf("went on:\n%s", h.tl)
	}
	h.mustContain("still CREATING after 40 minutes")
}

func TestDeployCtrlCDuringTheClusterWaitStopsCleanly(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.tf.tainted = []string{clusterAddr}
	h.awsf.eksStatus = func() (string, error) { return "CREATING\n", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	h.env.sleep = func(ctx context.Context, _ time.Duration) error {
		if sleeps++; sleeps > 5 {
			t.Fatal("kept waiting for the cluster after Ctrl-C")
		}
		cancel()
		return ctx.Err()
	}
	h.passChecks()
	err := h.env.run(ctx)
	if !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	if h.tf.ran("untaint") || h.tf.ran("plan") || h.tf.ran("apply") {
		t.Errorf("went on after Ctrl-C:\n%s", h.tl)
	}
	h.mustContain("stopped at your request")
}

func TestDeployLeavesTheTaintWhenAWSHasNoCluster(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.tf.tainted = []string{clusterAddr}
	h.awsf.eksStatus = func() (string, error) { return "", errors.New("ResourceNotFoundException: No cluster found") }
	_ = h.run()
	if h.tf.ran("untaint") {
		t.Error("untainted a cluster that AWS does not have")
	}
	h.mustContain("does not exist in AWS")
}

func TestDeployDoesNotUntaintOnAFinishedInstall(t *testing.T) {
	// A tainted cluster on an install whose state holds the access entry: someone's
	// replacement, or a manual taint. Replacing a cluster that matters is a person's
	// decision. Not untainted, and the create-first replacement is refused even with
	// --confirm-plan, because it cannot work.
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markFinished()
	h.tf.stateOut = finishedState
	h.tf.tainted = []string{clusterAddr}
	h.env.flags.ConfirmPlan = true
	h.tf.planJSON = `{"resource_changes":[{"address":"` + clusterAddr + `","action_reason":"replace_because_tainted","change":{"actions":["create","delete"]}}]}`
	if !IsReported(h.run()) {
		t.Fatal("went on to replace the cluster")
	}
	if h.tf.ran("untaint") {
		t.Error("untainted a cluster on a finished install")
	}
	if h.tf.ran("apply") {
		t.Error("applied a plan that replaces the cluster create-first")
	}
	h.mustContain("cannot work", "--confirm-plan does not apply", "TFCMD terraform untaint "+clusterAddr, "the database included")
}

func TestDeployRefusesACreateFirstClusterReplacementOnAnyInstall(t *testing.T) {
	// Not tainted: something in the configuration forces a new cluster. Same trap.
	h := newDeployHarness(t, 0)
	h.env.flags.ConfirmPlan = true
	h.tf.planJSON = `{"resource_changes":[{"address":"` + clusterAddr + `","action_reason":"replace_because_cannot_update","change":{"actions":["create","delete"]}}]}`
	if !IsReported(h.run()) || h.tf.ran("apply") {
		t.Fatal("applied a create-first cluster replacement")
	}
	h.mustContain("cannot work", "undo that change")
	if strings.Contains(h.out.String(), "darkplum destroy") {
		t.Error("a configuration-forced replacement was told to destroy the install")
	}
}

// ---- the plan gate's message ---------------------------------------------------------

const interruptedPlan = `{"resource_changes":[` +
	`{"address":"module.vpc.module.vpc.aws_nat_gateway.this[0]","action_reason":"replace_because_tainted","change":{"actions":["delete","create"]}},` +
	`{"address":"module.web_access[0].aws_lb.web","action_reason":"replace_because_tainted","change":{"actions":["delete","create"]}}]}`

func TestDeployPlanGateSaysTheReplacementsAreHalfBuilt(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.rejectUntilApplied()
	h.tf.planJSON = interruptedPlan
	if !IsReported(h.run()) || h.tf.ran("apply") {
		t.Fatal("a non-interactive run without --confirm-plan applied replacements")
	}
	h.mustContain("(tainted: left half-built by an interrupted or failed apply)",
		"2 of them are tainted", "this install's first deploy was interrupted, every item above is tainted by it, and none holds data", "--confirm-plan")
}

func TestDeployPlanGateWithConfirmPlanAppliesTheHalfBuiltReplacements(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.planJSON = interruptedPlan
	h.env.flags.ConfirmPlan = true
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if !h.tf.ran("apply") {
		t.Fatal("not applied")
	}
}

func TestDeployPlanGateDoesNotCallAnAuthoredReplacementHalfBuilt(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.planJSON = `{"resource_changes":[{"address":"module.rds.aws_db_instance.this","action_reason":"replace_because_cannot_update","change":{"actions":["delete","create"]}}]}`
	if !IsReported(h.run()) {
		t.Fatal("applied")
	}
	if strings.Contains(h.out.String(), "half-built") || strings.Contains(h.out.String(), "leftover") {
		t.Errorf("a configuration change was described as a leftover:\n%s", h.out.String())
	}
}

// ---- units ---------------------------------------------------------------------------

func TestTaintedInstancesReadsAddressesAndNothingElse(t *testing.T) {
	pull := statePullJSON([]string{clusterAddr, `module.eks.module.eks.aws_eks_access_entry.this["cluster_creator"]`, "terraform_data.plain"})
	got, err := taintedInstances(pull)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{clusterAddr, `module.eks.module.eks.aws_eks_access_entry.this["cluster_creator"]`, "terraform_data.plain"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %v", got)
	}
	if _, err := taintedInstances([]byte("not json")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestDestructiveChangesReadsOrderAndReason(t *testing.T) {
	got, err := destructiveChanges([]byte(`{"resource_changes":[
		{"address":"a","action_reason":"replace_because_tainted","change":{"actions":["create","delete"]}},
		{"address":"b","change":{"actions":["delete","create"]}},
		{"address":"c","change":{"actions":["delete"]}},
		{"address":"d","change":{"actions":["update"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []planChange{{Address: "a", Replace: true, CreateFirst: true, Tainted: true}, {Address: "b", Replace: true}, {Address: "c"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestKubeUnauthorized(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{kube401, true},
		{"kubectl: error: You must be logged in to the server (Unauthorized)", true},
		{`kubectl: Error from server (Forbidden): services is forbidden`, false},
		{"kubectl: Unable to connect to the server: dial tcp 10.0.0.1:443: i/o timeout", false},
		{"kubectl: Error from server (NotFound): namespaces \"darkplum-system\" not found", false},
	} {
		if got := kubeUnauthorized(errors.New(c.msg)); got != c.want {
			t.Errorf("kubeUnauthorized(%q) = %v", c.msg, got)
		}
	}
	if kubeUnauthorized(nil) {
		t.Error("nil")
	}
}

// The parser against the real terraform binary: a local-backend state with a
// tainted instance in a nested module with a count index, a for_each key, and a
// resource at the root, and the same `untaint` the deploy runs. Skipped where there
// is no terraform on the PATH (CI has one).
func TestTaintedInstancesAgainstRealTerraform(t *testing.T) {
	tfbin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("no terraform on the PATH")
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.tf", `module "eks" { source = "./inner" }
resource "terraform_data" "plain" { input = "x" }
`)
	write("inner/main.tf", `resource "terraform_data" "cluster" {
  count = 1
  input = "c"
}
resource "terraform_data" "keyed" {
  for_each = toset(["cluster_creator"])
  input    = "k"
}
`)
	// One data dir for all calls, so init's work is kept.
	dd := filepath.Join(t.TempDir(), "data")
	run2 := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(tfbin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_DATA_DIR="+dd, "TF_IN_AUTOMATION=1", "CHECKPOINT_DISABLE=1")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("terraform %v: %v\n%s", args, err, stderr.String())
		}
		return out
	}
	run2("init", "-input=false")
	run2("apply", "-auto-approve", "-input=false")
	run2("taint", "module.eks.terraform_data.cluster[0]")
	run2("taint", `module.eks.terraform_data.keyed["cluster_creator"]`)
	run2("taint", "terraform_data.plain")
	got, err := taintedInstances(run2("state", "pull"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"module.eks.terraform_data.cluster[0]", `module.eks.terraform_data.keyed["cluster_creator"]`, "terraform_data.plain"} {
		if !got[want] {
			t.Errorf("real state pull: %s not read as tainted; got %v", want, got)
		}
	}
	// These are the addresses `state list` prints, which the deploy matches the
	// cluster and the access entry against.
	list := string(run2("state", "list"))
	for a := range got {
		if !strings.Contains(list, a+"\n") {
			t.Errorf("%s is not spelled as `state list` spells it:\n%s", a, list)
		}
	}
	run2("untaint", "module.eks.terraform_data.cluster[0]")
	after, err := taintedInstances(run2("state", "pull"))
	if err != nil {
		t.Fatal(err)
	}
	if after["module.eks.terraform_data.cluster[0]"] || len(after) != 2 {
		t.Errorf("after untaint: %v", after)
	}
}

// ---- step 6, after a postponed apply: the checks must still be able to refuse --------

const afterApply = "Terraform has already applied the infrastructure, but nothing was written to Kubernetes"

func TestDeployStep6StillRefusesAnInternetFacingServiceAfterAPostponedApply(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.rejectUntilApplied(skHandler{"get svc darkplum-web", func([]string, []byte) ([]byte, error) {
		return []byte(webSvc("LoadBalancer", "internet-facing", "abc.elb.us-east-2.amazonaws.com")), nil
	}})
	if !IsReported(h.run()) {
		t.Fatalf("went on past an internet-facing web Service:\n%s", h.out.String())
	}
	if !h.tf.ran("apply") {
		t.Fatal("the apply was meant to have happened before the step-6 check")
	}
	if h.writes() != 0 {
		t.Errorf("Kubernetes was written to:\n%s", h.tl)
	}
	h.mustContain("elb.us-east-2.amazonaws.com", "private load balancer", afterApply)
	if strings.Contains(h.out.String(), "Nothing was changed.") {
		t.Errorf("a refusal after the apply says nothing was changed:\n%s", h.out.String())
	}
}

func TestDeployStep6StillRefusesA401ThatOutlastsThePropagationWindow(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	// Rejected before the apply and for ever after it.
	probes := 0
	h.rejectUntil(func() bool {
		if probes++; probes > 2000 {
			t.Fatal("the wait for access to propagate has no bound")
		}
		return false
	})
	if !IsReported(h.run()) {
		t.Fatalf("went on although the cluster never accepts this identity:\n%s", h.out.String())
	}
	if !h.tf.ran("apply") {
		t.Fatal("the apply was meant to have happened first")
	}
	if h.writes() != 0 {
		t.Errorf("Kubernetes was written to:\n%s", h.tl)
	}
	h.mustContain("rejected this identity's credentials", afterApply, "Waiting up to 3 minutes")
	if strings.Contains(h.out.String(), "Nothing was changed.") {
		t.Errorf("a refusal after the apply says nothing was changed:\n%s", h.out.String())
	}
	// The wait was bounded: the fake clock moved about kubeAccessWait, not for ever.
	if waited := h.clock.now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); waited < kubeAccessWait || waited > kubeAccessWait+10*time.Minute {
		t.Errorf("waited %v for access to propagate, want about %v", waited, kubeAccessWait)
	}
}

func TestDeployWaitsOutTheAccessEntrysPropagation(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	// 401 until the apply, and for 45 seconds of the (fake) clock after it.
	var appliedAt time.Time
	h.rejectUntil(func() bool {
		h.tf.mu.Lock()
		applied := h.tf.applied
		h.tf.mu.Unlock()
		if !applied {
			return false
		}
		if appliedAt.IsZero() {
			appliedAt = h.clock.now()
		}
		return h.clock.now().Sub(appliedAt) >= 45*time.Second
	})
	if err := h.run(); err != nil {
		t.Fatalf("a short propagation delay stopped the resume:\n%v\n%s", err, h.out.String())
	}
	h.mustContain("takes a short while to propagate", "Waiting up to 3 minutes")
}

func TestDeployDoesNotWaitOnAFinishedInstallsStep6(t *testing.T) {
	// inspected is true after the preflight of a healthy install: no wait, no extra read.
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markFinished()
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if strings.Contains(h.out.String(), "waiting up to") {
		t.Errorf("waited for access on a healthy install:\n%s", h.out.String())
	}
}

// ---- hints a person can follow ---------------------------------------------------------

func TestDeployRefusalsAfterTheApplyDoNotClaimNothingChanged(t *testing.T) {
	// The wording is chosen by whether Terraform has applied, not fixed.
	e := &deployEnv{}
	if got := e.unchanged(); got != "Nothing was changed" {
		t.Errorf("before the apply: %q", got)
	}
	e.mutated = true
	if got := e.unchanged(); !strings.Contains(got, "already applied") || strings.Contains(got, "Nothing was changed") {
		t.Errorf("after the apply: %q", got)
	}
	if r := e.unauthorizedRow("web exposure", errors.New(kube401)); !strings.Contains(r.hint, "already applied") {
		t.Errorf("the 401 refusal after the apply: %q", r.hint)
	}
}

func TestDoomedReplacementHintsAreRunnableAndHonest(t *testing.T) {
	plan := func(addr, reason string) string {
		return `{"resource_changes":[{"address":"` + addr + `","action_reason":"` + reason + `","change":{"actions":["create","delete"]}}]}`
	}
	logGroup := "module.eks.module.eks.aws_cloudwatch_log_group.this[0]"
	cases := []struct {
		name, addr, reason string
		want, never        []string
	}{
		{"tainted cluster", clusterAddr, "replace_because_tainted",
			[]string{"TFCMD terraform untaint " + clusterAddr, "the database included", "final snapshot"}, nil},
		{"tainted log group", logGroup, "replace_because_tainted",
			[]string{"TFCMD terraform untaint " + logGroup}, []string{"The cluster is tainted", "darkplum destroy"}},
		{"forced log group", logGroup, "replace_because_cannot_update",
			[]string{"undo that change"}, []string{"darkplum destroy", "untaint"}},
		{"forced cluster", clusterAddr, "replace_because_cannot_update",
			[]string{"undo that change"}, []string{"darkplum destroy", "untaint"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newDeployHarness(t, 0)
			h.env.flags.ConfirmPlan = true
			h.tf.planJSON = plan(c.addr, c.reason)
			if !IsReported(h.run()) || h.tf.ran("apply") {
				t.Fatal("applied a doomed replacement")
			}
			h.mustContain(append([]string{"cannot work", "Nothing was applied"}, c.want...)...)
			for _, n := range c.never {
				if strings.Contains(h.out.String(), n) {
					t.Errorf("hint contains %q:\n%s", n, h.out.String())
				}
			}
		})
	}
}

func TestLockHintNamesTheRunnableCommandWithTheLocksID(t *testing.T) {
	h := newDeployHarness(t, 0)
	// As Terraform prints it with colours on (deploy does not pass -no-color): the box
	// bar, a reset, spaces and the ID, with escape codes between them.
	h.tf.streamText["plan"] = "\x1b[31m╷\x1b[0m\x1b[0m\n\x1b[31m│\x1b[0m \x1b[0m\x1b[1m\x1b[31mError: \x1b[0m\x1b[0m\x1b[1mError acquiring the state lock\x1b[0m\n" +
		"\x1b[31m│\x1b[0m \x1b[0m\n\x1b[31m│\x1b[0m \x1b[0m\x1b[0mLock Info:\n\x1b[31m│\x1b[0m \x1b[0m  \x1b[0m\x1b[0m\n" +
		"\x1b[31m│\x1b[0m \x1b[0m  ID:        6f9f4b2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b\x1b[0m\n\x1b[31m│\x1b[0m \x1b[0m  Path:      bucket/key\n"
	h.tf.streamErr["plan"] = errors.New("exit status 1")
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	h.mustContain("TFCMD terraform force-unlock 6f9f4b2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b", "second Ctrl-C")
	if strings.Contains(h.out.String(), "<id>") {
		t.Errorf("a placeholder was left for the person to work out:\n%s", h.out.String())
	}
}

func TestLockIDIsFoundInColouredAndPlainOutput(t *testing.T) {
	for name, out := range map[string]string{
		"coloured": "\x1b[31m│\x1b[0m \x1b[0m  ID:  e067d899-0000-4000-8000-123456789abc\x1b[0m\n",
		"plain":    "│   ID:        e067d899-0000-4000-8000-123456789abc\n",
		"no box":   "  ID:        e067d899-0000-4000-8000-123456789abc\n",
	} {
		if got := lockHint(&fakeTF{}, out); !strings.Contains(got, "force-unlock e067d899-0000-4000-8000-123456789abc") || strings.Contains(got, "LOCK_ID") {
			t.Errorf("%s: %s", name, got)
		}
	}
	if got := lockHint(&fakeTF{}, "no lock info here"); !strings.Contains(got, "LOCK_ID") {
		t.Errorf("without an ID the hint must say to replace it: %s", got)
	}
}

func TestInterruptedHintIsTrueForBothKindsOfInterrupt(t *testing.T) {
	e := &deployEnv{}
	one := e.interruptedHint()
	if strings.Contains(one, "stopped cleanly") || !strings.Contains(one, "asked to stop") {
		t.Errorf("first Ctrl-C: %q", one)
	}
	f := make(chan struct{})
	e.force = f
	if got := e.interruptedHint(); got != one {
		t.Errorf("an unforced run's hint changed: %q", got)
	}
	close(f)
	two := e.interruptedHint()
	for _, want := range []string{"killed", "lock may still be held", "half-built"} {
		if !strings.Contains(two, want) {
			t.Errorf("second Ctrl-C: hint lacks %q: %q", want, two)
		}
	}
	if strings.Contains(two, "stopped cleanly") {
		t.Errorf("a killed Terraform is described as stopped cleanly: %q", two)
	}
}

// ---- the gate's reassurance ---------------------------------------------------------------

func TestPlanGateDoesNotReassureOnAMixedList(t *testing.T) {
	// A tainted NAT gateway next to a replacement the configuration asked for.
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.rejectUntilApplied()
	h.tf.planJSON = `{"resource_changes":[
		{"address":"module.vpc.module.vpc.aws_nat_gateway.this[0]","action_reason":"replace_because_tainted","change":{"actions":["delete","create"]}},
		{"address":"module.web_access[0].aws_lb.web","action_reason":"replace_because_cannot_update","change":{"actions":["delete","create"]}}]}`
	if !IsReported(h.run()) || h.tf.ran("apply") {
		t.Fatal("applied")
	}
	h.mustContain("1 of them is tainted")
	for _, bad := range []string{"is how it finishes", "every item above is tainted by it"} {
		if strings.Contains(h.out.String(), bad) {
			t.Errorf("a mixed list was called a leftover (%q):\n%s", bad, h.out.String())
		}
	}
}

func TestPlanGateDoesNotCallATaintedDatabaseAHarmlessLeftover(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markUnfinished()
	h.tf.stateOut = interruptedState
	h.rejectUntilApplied()
	h.tf.planJSON = `{"resource_changes":[
		{"address":"module.vpc.module.vpc.aws_nat_gateway.this[0]","action_reason":"replace_because_tainted","change":{"actions":["delete","create"]}},
		{"address":"module.rds.aws_db_instance.this","action_reason":"replace_because_tainted","change":{"actions":["delete","create"]}}]}`
	if !IsReported(h.run()) || h.tf.ran("apply") {
		t.Fatal("applied")
	}
	h.mustContain("holds data", "replacing it deletes what is in it", "the list holds data")
	for _, bad := range []string{"is how it finishes", "none holds data"} {
		if strings.Contains(h.out.String(), bad) {
			t.Errorf("a list with a database was reassuring (%q):\n%s", bad, h.out.String())
		}
	}
}

func TestPlanGateDoesNotReassureOnAnInstallThatIsNotAnInterruptedFirstInstall(t *testing.T) {
	// Tainted NAT and load balancer, but the state holds the access entry: a finished
	// install. Not "the interrupted first install's leftovers".
	h := newDeployHarness(t, 0)
	h.activeCluster()
	h.markFinished()
	h.tf.stateOut = finishedState
	h.tf.planJSON = interruptedPlan
	if !IsReported(h.run()) || h.tf.ran("apply") {
		t.Fatal("applied")
	}
	h.mustContain("does not show a first install interrupted before its access was made")
	if strings.Contains(h.out.String(), "is how it finishes") {
		t.Errorf("reassured on a finished install:\n%s", h.out.String())
	}
}

// ---- units ----------------------------------------------------------------------------------

func TestCreateFirstDoomed(t *testing.T) {
	for _, c := range []struct {
		name string
		c    planChange
		want bool
	}{
		{"cluster, forced", planChange{Address: clusterAddr, Replace: true, CreateFirst: true}, true},
		{"log group, forced", planChange{Address: "module.eks.module.eks.aws_cloudwatch_log_group.this[0]", Replace: true, CreateFirst: true}, true},
		{"cluster, destroy-first", planChange{Address: clusterAddr, Replace: true}, false},
		{"cluster, deleted outright", planChange{Address: clusterAddr, CreateFirst: true}, false},
		{"tainted subnet", planChange{Address: "module.vpc.module.vpc.aws_subnet.private[1]", Replace: true, CreateFirst: true, Tainted: true}, true},
		{"subnet replaced because its CIDR changed", planChange{Address: "module.vpc.module.vpc.aws_subnet.private[1]", Replace: true, CreateFirst: true}, false},
		{"tainted sg rule", planChange{Address: `module.eks.module.eks.aws_security_group_rule.node["ingress_self_all"]`, Replace: true, CreateFirst: true, Tainted: true}, true},
		{"tainted cidr reservation", planChange{Address: "module.vpc.aws_ec2_subnet_cidr_reservation.lb[0]", Replace: true, CreateFirst: true, Tainted: true}, true},
		{"unrelated tainted, create-first", planChange{Address: "module.web_access[0].aws_acm_certificate.web[0]", Replace: true, CreateFirst: true, Tainted: true}, false},
	} {
		if got := createFirstDoomed(c.c) != ""; got != c.want {
			t.Errorf("%s: doomed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHoldsData(t *testing.T) {
	for addr, want := range map[string]bool{
		"module.rds.aws_db_instance.this":                        true,
		"module.ecr.aws_ecr_repository.this[\"api\"]":            true,
		"module.eks.module.eks.aws_cloudwatch_log_group.this[0]": true,
		"module.vpc.module.vpc.aws_nat_gateway.this[0]":          false,
		"module.web_access[0].aws_lb.web":                        false,
		"module.eks.module.eks.aws_eks_cluster.this[0]":          false,
	} {
		if got := holdsData(addr); got != want {
			t.Errorf("holdsData(%s) = %v", addr, got)
		}
	}
}

func TestTaintedInstancesSkipsDeposedObjects(t *testing.T) {
	// A create-before-destroy replacement that was interrupted leaves the old object
	// "deposed", and it may carry the tainted status too: it is not the live instance.
	pull := []byte(`{"version":4,"resources":[
		{"module":"module.eks.module.eks","mode":"managed","type":"aws_eks_cluster","name":"this","instances":[
			{"index_key":0,"status":"tainted","deposed":"c0ffee00","attributes":{}},
			{"index_key":0,"attributes":{}}]},
		{"mode":"data","type":"aws_caller_identity","name":"current","instances":[{"status":"tainted","attributes":{}}]}]}`)
	got, err := taintedInstances(pull)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a deposed or data instance was read as a live tainted one: %v", got)
	}
}

// The addresses this file keys on were read from one release of the EKS module and
// from the names in modules/eks. A bump that renames either would make the state test
// in interruptedFirstInstall false for every unfinished deploy (so a resume would
// stop where it should continue) or, worse, never for a finished one.
func TestResumeAddressesMatchThePinnedEKSModule(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(terraformDir, "modules", "eks", "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	m := string(b)
	for _, want := range []string{
		`module "eks" {`,
		`source  = "terraform-aws-modules/eks/aws"`,
		`version = "` + eksModuleVersion + `"`,
		`enable_cluster_creator_admin_permissions = true`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("modules/eks/main.tf lacks %q: re-check clusterAddr and creatorAccessEntryAddr against the new module, then update eksModuleVersion", want)
		}
	}
	root, err := os.ReadFile(filepath.Join(terraformDir, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(root), "module \"eks\" {\n  source = \"./modules/eks\"") {
		t.Error("the root no longer calls the EKS wrapper as module.eks")
	}
	// The two addresses are module.eks (root) . module.eks (wrapper) . resource.
	for _, a := range []string{clusterAddr, creatorAccessEntryAddr} {
		if !strings.HasPrefix(a, "module.eks.module.eks.") {
			t.Errorf("%s is not under module.eks.module.eks", a)
		}
	}
	if !strings.Contains(creatorAccessEntryAddr, `["cluster_creator"]`) {
		t.Error("the access entry key changed")
	}
}

func TestSecondCtrlCNoteDoesNotNameACommandThatCannotRun(t *testing.T) {
	for _, bad := range []string{"force-unlock <id>", "terraform force-unlock"} {
		if strings.Contains(killedNote, bad) {
			t.Errorf("the second Ctrl-C note names %q, which does nothing outside the install's workspace and bundle", bad)
		}
	}
	if !strings.Contains(killedNote, "exact command") {
		t.Error("the note must point at the next run's exact command")
	}
}
