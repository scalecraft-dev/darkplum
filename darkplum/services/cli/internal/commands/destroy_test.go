package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/kube"
)

const destroyPlanOK = `{"resource_changes":[
	{"address":"module.vpc.aws_vpc.this","change":{"actions":["delete"]}},
	{"address":"module.rds.aws_db_instance.this","change":{"actions":["delete"]}},
	{"address":"module.eks.aws_eks_cluster.this","change":{"actions":["delete"]}}]}`

const destroyState = "module.vpc.aws_vpc.this\nmodule.rds.aws_db_instance.this\nmodule.eks.aws_eks_cluster.this\n" + logGroupAddr + "\ndata.aws_caller_identity.current\n"

type destroyHarness struct {
	t       *testing.T
	env     *destroyEnv
	out     *strings.Builder
	tl      *timeline
	tf      *fakeTF
	k       *scriptKube
	aws     *destroyAWS
	clock   *fakeClock
	in      interface{ Dir() string }
	svcs    []objRef // LoadBalancer Services
	foreign []objRef
	pvcs    []objRef
	stuck   bool
	prompts []string // answers, in order
	asked   []string
	session checkResult
}

// destroyAWS answers what destroy asks of aws: identity, the cluster, and the read-only report lookups.
type destroyAWS struct {
	base      *fakeAWS
	tl        *timeline
	callerErr error
	extra     map[string]string // substring -> output
	extraErr  map[string]error
}

func (a *destroyAWS) run(name string, args ...string) (string, error) {
	j := strings.Join(args, " ")
	a.tl.add("aws " + j)
	if strings.Contains(j, "sts get-caller-identity") && a.callerErr != nil {
		return "", a.callerErr
	}
	if strings.Contains(j, "eks describe-cluster") || strings.Contains(j, "sts get-caller-identity") {
		return a.base.run(name, args...)
	}
	for sub, err := range a.extraErr {
		if strings.Contains(j, sub) {
			return "", err
		}
	}
	for sub, out := range a.extra {
		if strings.Contains(j, sub) {
			return out, nil
		}
	}
	if strings.Contains(j, "ecr describe-repositories") {
		return "", errors.New("RepositoryNotFoundException")
	}
	// The leftovers lookups find nothing unless a test says otherwise. They are
	// scripted, not an error: a failed lookup is no longer treated as "none".
	for _, q := range []string{"describe-network-interfaces", "describe-security-groups", "describe-volumes", "describe-load-balancers"} {
		if strings.Contains(j, q) {
			return "[]", nil
		}
	}
	return "", fmt.Errorf("unexpected aws %s", j)
}

func newDestroyHarness(t *testing.T) *destroyHarness {
	t.Helper()
	in := testInstall(t, nil)
	rec, _ := in.Load()
	_ = in.WriteFile("terraform.tfvars", []byte(pinnedTFVars+"gpu_desired_size = 0\n"))
	tfv, _ := readTFVars(in.TFVars())
	h := &destroyHarness{t: t, out: &strings.Builder{}, tl: &timeline{}, clock: &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		session: checkResult{name: "session lifetime", status: checkPass}}
	h.tf = &fakeTF{t: t, tl: h.tl, stateOut: destroyState, planJSON: destroyPlanOK, applied: true,
		outputs:   outputsJSON(0, func(v map[string]any) { v["rds_final_snapshot_identifier"] = "dp-trial-db-final-ab12" }),
		streamErr: map[string]error{}, streamText: map[string]string{}}
	h.aws = &destroyAWS{base: &fakeAWS{arn: testARN, caller: dpAccount}, tl: h.tl, extra: map[string]string{}, extraErr: map[string]error{}}
	h.k = newScriptKube(t)
	h.k.tl = h.tl
	h.k.on("delete deployments", "", nil)
	h.k.on("get configmap darkplum-install", "", notFound("configmaps", "darkplum-install"))
	h.k.onFn("get svc -A", func([]string, []byte) ([]byte, error) {
		return h.listJSON("LoadBalancer", append(append([]objRef{}, h.svcs...), h.foreign...)), nil
	})
	h.k.onFn("get pvc -A", func([]string, []byte) ([]byte, error) { return h.listJSON("", h.pvcs), nil })
	h.k.onFn("delete svc", func(args []string, _ []byte) ([]byte, error) {
		name := args[len(args)-3]
		if !h.stuck {
			h.svcs = removeRef(h.svcs, name)
		}
		return nil, nil
	})
	h.k.onFn("delete pvc", func(args []string, _ []byte) ([]byte, error) {
		name := args[len(args)-3]
		if !h.stuck {
			h.pvcs = removeRef(h.pvcs, name)
		}
		return nil, nil
	})
	h.env = &destroyEnv{out: newLockedWriter(h.out), logPath: "/ws/logs/destroy.log", in: in, rec: rec, tfv: tfv,
		now: h.clock.now, sleep: h.clock.sleep, heartbeat: time.Hour, poll: time.Second,
		interactive: true,
		prompt: func(q string) string {
			h.asked = append(h.asked, q)
			if len(h.prompts) == 0 {
				return ""
			}
			a := h.prompts[0]
			h.prompts = h.prompts[1:]
			return a
		},
		aws:       h.aws.run,
		sessionOK: func(int) checkResult { return h.session },
		openTF:    func() (tfRunner, string, error) { return h.tf, "", nil },
		cluster:   func() (clusterTarget, checkResult, error) { return installCluster(in, h.env.aws) },
		kube:      func(clusterTarget) *kube.Client { return h.k.client(platformNS) },
	}
	h.env.flags.MinSession = 30
	h.env.flags.Confirm = "dp-trial"
	return h
}

func removeRef(l []objRef, name string) []objRef {
	var out []objRef
	for _, o := range l {
		if o.Name != name {
			out = append(out, o)
		}
	}
	return out
}

func (h *destroyHarness) listJSON(typ string, refs []objRef) []byte {
	var items []any
	for _, r := range refs {
		it := map[string]any{"metadata": map[string]any{"name": r.Name, "namespace": r.NS}}
		if typ != "" {
			it["spec"] = map[string]any{"type": typ}
		}
		items = append(items, it)
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return b
}

func (h *destroyHarness) run() error { return h.env.run(context.Background()) }

// mutatingTF lists the terraform commands that change anything.
func (h *destroyHarness) mutatingTF() int {
	n := 0
	for _, c := range h.tf.cmds {
		switch {
		case c[0] == "apply", c[0] == "destroy", c[0] == "import", c[0] == "state" && len(c) > 1 && c[1] == "rm":
			n++
		}
	}
	return n
}

func (h *destroyHarness) kubeWrites() int {
	n := 0
	for _, w := range []string{"delete", "apply", "patch", "create"} {
		n += len(h.k.matching(w))
	}
	return n
}

func (h *destroyHarness) assertNothingDeleted() {
	h.t.Helper()
	if h.mutatingTF() != 0 || h.kubeWrites() != 0 {
		h.t.Errorf("something was changed:\n%s", h.tl)
	}
}

func (h *destroyHarness) contains(want ...string) {
	h.t.Helper()
	for _, w := range want {
		if !strings.Contains(h.out.String(), w) {
			h.t.Errorf("output lacks %q:\n%s", w, h.out.String())
		}
	}
}

func TestDestroyFullRunOrder(t *testing.T) {
	h := newDestroyHarness(t)
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
	h.pvcs = []objRef{{inferenceNS, "model-cache"}}
	h.aws.extra["rds describe-db-snapshots"] = `["dp-trial-db-final-ab12"]`
	h.aws.extra["logs describe-log-groups"] = "1"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	order := []string{
		"sts get-caller-identity",
		"terraform init",
		"terraform state list",
		"terraform output -json", // the state is shown to be this install's
		"eks describe-cluster",
		"terraform plan -destroy", // the sanity plan, while the database is still protected
		"terraform apply -auto-approve -input=false -var-file=/ws/terraform.tfvars -var db_deletion_protection=false -target=module.rds.aws_db_instance.this",
		"terraform state rm " + logGroupAddr,
		"delete svc darkplum-web-lb",
		"delete pvc model-cache",
		"terraform destroy",
		"rds describe-db-snapshots",
	}
	prev := -1
	for _, s := range order {
		i := h.tl.first(s)
		if i < 0 {
			t.Fatalf("%q never happened:\n%s", s, h.tl)
		}
		if i < prev {
			t.Errorf("%q happened too early:\n%s", s, h.tl)
		}
		prev = i
	}
	// Every terraform-side preparation precedes the first kubectl delete.
	firstDelete := h.tl.first("delete svc")
	for _, s := range []string{"plan -destroy", "db_deletion_protection=false", "state rm"} {
		if h.tl.first(s) > firstDelete {
			t.Errorf("%q came after Kubernetes deletion began", s)
		}
	}
	h.k.assertClean()
	h.contains("Final database snapshot dp-trial-db-final-ab12", "delete-db-snapshot", "delete-log-group", "s3://darkplum-tfstate", "Done")
	// The report only reads.
	for _, ev := range h.tl.ev {
		if strings.HasPrefix(ev, "aws ") && (strings.Contains(ev, " delete-") || strings.Contains(ev, "schedule-key-deletion") || strings.Contains(ev, " rb ")) {
			t.Errorf("the report deleted something: %s", ev)
		}
	}
}

func TestDestroyExpiredSSODeletesNothing(t *testing.T) {
	h := newDestroyHarness(t)
	h.aws.callerErr = errors.New("The SSO session associated with this profile has expired or is otherwise invalid")
	if !IsReported(h.run()) {
		t.Fatal("went on with expired credentials")
	}
	h.assertNothingDeleted()
	if h.tl.count("terraform") != 0 || h.tl.count("kubectl") != 0 {
		t.Errorf("even read-only work started before the credentials were known good:\n%s", h.tl)
	}
	h.contains("aws sso login --profile acme", "Nothing was deleted")
}

func TestDestroyShortSessionDeletesNothing(t *testing.T) {
	h := newDestroyHarness(t)
	h.session = checkResult{name: "session lifetime", status: checkFail, detail: "9 min remaining"}
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	h.assertNothingDeleted()
	if h.tl.count("terraform") != 0 {
		t.Error("terraform started")
	}
}

func TestDestroyAccountMismatchAborts(t *testing.T) {
	h := newDestroyHarness(t)
	h.aws.base.caller = "999999999999"
	if !IsReported(h.run()) {
		t.Fatal("went on in the wrong account")
	}
	h.assertNothingDeleted()
	if h.tl.count("terraform") != 0 || h.tl.count("describe-cluster") != 0 {
		t.Errorf("another account's credentials went further than the identity check:\n%s", h.tl)
	}
	h.contains("999999999999")
}

func TestDestroyAccountMismatchOnTheClusterAborts(t *testing.T) {
	// The caller is the pinned account but the cluster ARN names another one.
	h := newDestroyHarness(t)
	h.aws.base.arn = "arn:aws:eks:us-east-2:999999999999:cluster/dp-trial"
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	h.assertNothingDeleted()
}

func TestDestroyStateListAuthFailureIsNotNoDatabase(t *testing.T) {
	// The original bug: `terraform state list 2>/dev/null | grep` read an expired
	// session as "the database is not in the state" and skipped the RDS flip.
	h := newDestroyHarness(t)
	h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "Error: error loading state: operation error S3: GetObject, ExpiredToken", Err: errors.New("exit status 1")}
	if !IsReported(h.run()) {
		t.Fatal("a failed state read let the destroy continue")
	}
	h.assertNothingDeleted()
	if h.tf.ran("plan") || h.tl.count("kubectl") != 0 {
		t.Error("went past the state read")
	}
}

func TestDestroyEmptyStateIsNothingToDestroy(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "No state file was found!", Err: errors.New("exit status 1")}
	h.aws.extra["logs describe-log-groups"] = "0"
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	h.assertNothingDeleted()
	h.contains("nothing to destroy", "Left behind")
	if h.tl.count("kubectl") != 0 {
		t.Error("touched Kubernetes with an empty state")
	}
}

func TestDestroyClusterGoneSkipsKubernetesLoudly(t *testing.T) {
	h := newDestroyHarness(t)
	h.aws.base.describeErr = errors.New("ResourceNotFoundException: No cluster found for name: dp-trial")
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if h.tl.count("kubectl") != 0 {
		t.Errorf("kubectl ran against a cluster that does not exist:\n%s", h.tl)
	}
	if !h.tf.ran("destroy") {
		t.Error("terraform destroy did not run")
	}
	h.contains("SKIPPING the Kubernetes phase", "gone")
}

func TestDestroyClusterUnreachableAborts(t *testing.T) {
	h := newDestroyHarness(t)
	h.aws.base.describeErr = errors.New("Could not connect to the endpoint URL")
	if !IsReported(h.run()) {
		t.Fatal("an unreachable cluster was treated as gone")
	}
	h.assertNothingDeleted()
}

func TestDestroyConfirmation(t *testing.T) {
	t.Run("wrong --confirm", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.env.flags.Confirm = "prod-cluster"
		if !IsReported(h.run()) {
			t.Fatal("went on")
		}
		h.assertNothingDeleted()
	})
	t.Run("non-interactive without --confirm", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.env.flags.Confirm = ""
		h.env.interactive = false
		if !IsReported(h.run()) {
			t.Fatal("went on")
		}
		h.assertNothingDeleted()
		h.contains("--confirm dp-trial")
	})
	t.Run("typed name", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.env.flags.Confirm = ""
		h.prompts = []string{"dp-trial"}
		h.aws.extra["rds describe-db-snapshots"] = `[]`
		h.aws.extra["logs describe-log-groups"] = "0"
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
	})
	t.Run("typed wrong name", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.env.flags.Confirm = ""
		h.prompts = []string{"yes"}
		if !IsReported(h.run()) {
			t.Fatal("'yes' is not the cluster name")
		}
		h.assertNothingDeleted()
	})
	t.Run("nothing is deleted or flipped before the name is typed", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.env.flags.Confirm = ""
		h.prompts = []string{""}
		_ = h.run()
		h.assertNothingDeleted()
		if h.tf.ran("plan") || h.tf.ran("apply") {
			t.Error("preparation started before the confirmation")
		}
	})
}

func TestDestroyNoSnapshotNeedsItsOwnConfirmation(t *testing.T) {
	noSnap := func(v map[string]any) { v["rds_final_snapshot_identifier"] = nil }
	t.Run("--confirm does not satisfy it", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.tf.outputs = outputsJSON(0, noSnap)
		h.env.interactive = false
		if !IsReported(h.run()) {
			t.Fatal("deleted a database without a snapshot on --confirm alone")
		}
		h.assertNothingDeleted()
		h.contains("--confirm-no-snapshot dp-trial")
	})
	t.Run("the separate flag does", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.tf.outputs = outputsJSON(0, noSnap)
		h.env.flags.ConfirmNoSnapshot = "dp-trial"
		h.aws.extra["rds describe-db-snapshots"] = `[]`
		h.aws.extra["logs describe-log-groups"] = "0"
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		h.contains("NO copy")
	})
	t.Run("wrong value for the separate flag", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.tf.outputs = outputsJSON(0, noSnap)
		h.env.flags.ConfirmNoSnapshot = "other"
		if !IsReported(h.run()) {
			t.Fatal("went on")
		}
		h.assertNothingDeleted()
	})
	t.Run("interactive second prompt", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.tf.outputs = outputsJSON(0, noSnap)
		h.env.flags.Confirm = ""
		h.prompts = []string{"dp-trial", "no snapshot"}
		h.aws.extra["rds describe-db-snapshots"] = `[]`
		h.aws.extra["logs describe-log-groups"] = "0"
		if err := h.run(); err != nil {
			t.Fatal(err)
		}
		if len(h.asked) != 2 {
			t.Errorf("questions asked = %d, want 2", len(h.asked))
		}
	})
	t.Run("declined second prompt", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.tf.outputs = outputsJSON(0, noSnap)
		h.env.flags.Confirm = ""
		h.prompts = []string{"dp-trial", "dp-trial"}
		if !IsReported(h.run()) {
			t.Fatal("went on")
		}
		h.assertNothingDeleted()
	})
}

func TestDestroyOnlyFlipsProtectionWhenTheDatabaseIsInTheState(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.stateOut = "module.vpc.aws_vpc.this\nmodule.eks.aws_eks_cluster.this\n"
	h.tf.planJSON = `{"resource_changes":[{"address":"module.vpc.aws_vpc.this","change":{"actions":["delete"]}}]}`
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.tf.cmds {
		if c[0] == "apply" {
			t.Errorf("a targeted apply would plan to CREATE a database that is not there: %v", c)
		}
	}
	if h.tl.count("terraform output") != 1 { // the ownership check only
		t.Error("asked about a snapshot of a database that is not in the state")
	}
}

func TestDestroyTerraformPreparationFailuresTouchNoKubernetes(t *testing.T) {
	cases := map[string]func(h *destroyHarness){
		"deletion protection flip fails":  func(h *destroyHarness) { h.tf.streamErr["apply"] = errors.New("exit status 1") },
		"state rm of the log group fails": func(h *destroyHarness) { h.tf.rmErr = errors.New("state locked") },
		"plan -destroy fails":             func(h *destroyHarness) { h.tf.streamErr["plan"] = errors.New("exit status 1") },
		"plan creates something": func(h *destroyHarness) {
			h.tf.planJSON = `{"resource_changes":[{"address":"module.rds.aws_db_instance.this","change":{"actions":["create"]}}]}`
		},
		"plan deletes something foreign": func(h *destroyHarness) {
			h.tf.planJSON = `{"resource_changes":[{"address":"aws_s3_bucket.someone_elses","change":{"actions":["delete"]}}]}`
		},
		"snapshot setting unreadable": func(h *destroyHarness) { h.tf.outputsErr = errors.New("boom"); h.tf.outputs = "ALWAYS" },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newDestroyHarness(t)
			h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
			setup(h)
			if !IsReported(h.run()) {
				t.Fatalf("went on\n%s", h.out.String())
			}
			// The regression: "k8s gone, infra intact".
			if h.kubeWrites() != 0 {
				t.Errorf("Kubernetes was touched after a Terraform-side failure:\n%s", h.tl)
			}
			if h.tf.ran("destroy") {
				t.Error("terraform destroy ran")
			}
		})
	}
}

func TestDestroyFlipFailureSaysNothingWasChanged(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.streamErr["apply"] = errors.New("exit status 1")
	_ = h.run()
	h.contains("deletion protection")
	if h.env.protectionOff {
		t.Error("protection reported off after a failed flip")
	}
}

func TestDestroyLaterFailureWarnsThatProtectionIsOff(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.streamErr["destroy"] = errors.New("exit status 1")
	h.tf.streamText["destroy"] = "Error: ExpiredToken"
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	if !IsReported(h.run()) {
		t.Fatal("a failed destroy passed")
	}
	h.contains("deletion protection is now OFF", "aws sso login --profile acme")
	if h.tl.count("Left behind") != 0 && strings.Contains(h.out.String(), "Done") {
		t.Error("reported success")
	}
}

func TestDestroyForeignLoadBalancersAndVolumesAbortBeforeAnything(t *testing.T) {
	h := newDestroyHarness(t)
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
	h.foreign = []objRef{{"team-a", "grafana"}}
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	h.assertNothingDeleted()
	h.contains("team-a/grafana", "Nothing was deleted")
	if h.tf.ran("plan") {
		t.Error("preparation started")
	}
}

func TestDestroyForeignVolumeClaimAborts(t *testing.T) {
	h := newDestroyHarness(t)
	h.pvcs = []objRef{{"monitoring", "prometheus-data"}}
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	h.assertNothingDeleted()
	h.contains("monitoring/prometheus-data")
}

func TestDestroyStuckKubernetesObjectsAreNotForced(t *testing.T) {
	h := newDestroyHarness(t)
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
	h.stuck = true
	if !IsReported(h.run()) {
		t.Fatal("went on although the load balancer service never went away")
	}
	if h.tf.ran("destroy") {
		t.Error("terraform destroy ran with a load balancer still attached")
	}
	for _, c := range h.k.calls {
		j := c.joined()
		if strings.Contains(j, "--force") || strings.Contains(j, "--grace-period") {
			t.Errorf("forced deletion: %s", j)
		}
	}
	if h.clock.now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) < 5*time.Minute {
		t.Error("gave up before 5 minutes")
	}
	h.contains("darkplum-system/darkplum-web-lb", "Nothing is forced")
}

func TestDestroyOnlyTouchesDarkplumNamespaces(t *testing.T) {
	h := newDestroyHarness(t)
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}, {inferenceNS, "vllm-lb"}}
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.k.matching("delete") {
		j := c.joined()
		if !strings.Contains(j, "-n "+platformNS) && !strings.Contains(j, "-n "+inferenceNS) {
			t.Errorf("a delete without a Darkplum namespace: %s", j)
		}
		if strings.Contains(j, "namespace ") || strings.Contains(j, "-A") {
			t.Errorf("a broad delete: %s", j)
		}
	}
	h.k.assertClean()
}

func TestDestroyRechecksCredentialsBeforeTerraformDestroy(t *testing.T) {
	h := newDestroyHarness(t)
	calls := 0
	h.env.sessionOK = func(min int) checkResult {
		calls++
		if calls == 2 { // the re-check, after the Kubernetes phase
			return checkResult{name: "session lifetime", status: checkFail, detail: "expired"}
		}
		return checkResult{name: "session lifetime", status: checkPass}
	}
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	if h.tf.ran("destroy") {
		t.Error("terraform destroy ran with an expired session")
	}
	h.contains("Run `darkplum destroy` again")
}

func TestDestroyIdentityChangedMidRun(t *testing.T) {
	h := newDestroyHarness(t)
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
	n := 0
	orig := h.aws.run
	h.env.aws = func(name string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "sts get-caller-identity") {
			n++
			if n >= 3 { // gate, cluster validation, then the re-check
				return "", errors.New("ExpiredToken")
			}
		}
		return orig(name, args...)
	}
	if !IsReported(h.run()) || h.tf.ran("destroy") {
		t.Fatal("destroyed with credentials that no longer work")
	}
}

func TestDestroyReportFindsEverythingLeftBehind(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "No state file was found!", Err: errors.New("x")}
	h.env.rec.Backend.KMSKeyID = "arn:aws:kms:us-east-2:123456789012:key/abc"
	h.env.rec.CreatedZoneID = "Z0123456789ABCDEFGHIJ"
	h.aws.extra["rds describe-db-snapshots"] = `["dp-trial-db-final-ab12"]`
	h.aws.extra["logs describe-log-groups"] = "1"
	h.aws.extra["--repository-names darkplum-api"] = "{}"
	h.aws.extra["--repository-names dp-trial/darkplum-web"] = "{}" // should have gone with the stack
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	h.contains("dp-trial-db-final-ab12", "/aws/eks/dp-trial/cluster", "s3://darkplum-tfstate", "schedule-key-deletion",
		"Z0123456789ABCDEFGHIJ", "delete-hosted-zone", "ECR repository darkplum-api", "delete-repository")
	if strings.Contains(h.out.String(), "ECR repository darkplum-web") {
		t.Error("reported a repository that does not exist")
	}
	h.contains("ECR repository dp-trial/darkplum-web", "should have been deleted with the stack")
	h.contains("Done")
}

func TestDestroyReportSaysWhenALookupFailed(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "No state file was found!", Err: errors.New("x")}
	h.aws.extraErr["rds describe-db-snapshots"] = errors.New("AccessDenied")
	h.aws.extra["logs describe-log-groups"] = "0"
	_ = h.run()
	h.contains("could not be checked")
}

// The brief's first bug: `make destroy` ran bare kubectl against the CURRENT
// context, which was a production cluster. Here the real handle runs a fake kubectl
// binary with a decoy KUBECONFIG and current-context in place.
func TestDestroyNeverTouchesTheAmbientContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	decoy := filepath.Join(home, "prod-kubeconfig")
	decoyBody := []byte("apiVersion: v1\nkind: Config\ncurrent-context: prod-dagctl\nclusters:\n- name: prod-dagctl\n  cluster:\n    server: https://prod.example.com\n")
	if err := os.WriteFile(decoy, decoyBody, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", decoy)

	h := newDestroyHarness(t)
	// Production wiring: the install's real validated handle, kubectl found on PATH.
	h.env.kube = func(t clusterTarget) *kube.Client { return t.kube(platformNS) }
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	script := "#!/bin/sh\necho \"$KUBECONFIG|$*\" >> '" + logf + "'\ncase \"$*\" in *'get svc'*|*'get pvc'*) echo '{\"items\":[]}';; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	raw, _ := os.ReadFile(logf)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	own := h.env.in.Kubeconfig()
	if len(lines) < 2 {
		t.Fatalf("kubectl ran %d times", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, own+"|--kubeconfig "+own+" --context darkplum ") {
			t.Errorf("kubectl was not pinned to the install's cluster: %s", l)
		}
		if strings.Contains(l, decoy) || strings.Contains(l, "prod-dagctl") {
			t.Errorf("kubectl saw the ambient context: %s", l)
		}
	}
	if b, _ := os.ReadFile(decoy); string(b) != string(decoyBody) {
		t.Error("the ambient kubeconfig was modified")
	}
}

func TestDestroyNoInstallRunsNothing(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	for _, tool := range []string{"aws", "terraform", "kubectl"} {
		_ = os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho x >> '"+marker+"'\n"), 0o755)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DARKPLUM_HOME", t.TempDir())
	if err := (&DestroyCmd{Confirm: "x"}).Run(&Globals{}); !IsReported(err) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("started a program with no install")
	}
}

func TestDestroyRefusesAStateThatIsNotThisInstalls(t *testing.T) {
	cases := map[string]string{
		"another cluster": outputsJSON(0, func(v map[string]any) { v["cluster_name"] = "prod-eks" }),
		"another region":  outputsJSON(0, func(v map[string]any) { v["provider_region"] = "eu-west-1" }),
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			h := newDestroyHarness(t)
			h.tf.outputs = out
			if !IsReported(h.run()) {
				t.Fatal("destroyed a state that is not this install's")
			}
			h.assertNothingDeleted()
			if h.tf.ran("plan") || h.tl.count("kubectl") != 0 {
				t.Error("went on")
			}
		})
	}
}

func TestDestroySanityPlanComesBeforeAnythingIsChanged(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.streamErr["plan"] = errors.New("exit status 1")
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	// The regression: a failing plan used to be found only after the database's
	// deletion protection was off and the log group had left the state.
	h.assertNothingDeleted()
	if h.env.protectionOff {
		t.Error("protection was turned off before the plan was known good")
	}
}

func TestDestroyDowngradeGuard(t *testing.T) {
	h := newDestroyHarness(t)
	h.env.rec.Bundle, h.env.version = "1.4.0", "1.3.0"
	if !IsReported(h.run()) {
		t.Fatal("an older darkplum destroyed the install")
	}
	h.assertNothingDeleted()
	if h.tl.count("terraform") != 0 {
		t.Error("terraform started")
	}
	h2 := newDestroyHarness(t)
	h2.env.rec.Bundle, h2.env.version = "1.4.0", "1.3.0"
	h2.env.flags.AllowDowngrade = true
	h2.aws.extra["rds describe-db-snapshots"] = `[]`
	h2.aws.extra["logs describe-log-groups"] = "0"
	if err := h2.run(); err != nil {
		t.Fatal(err)
	}
}

func TestDestroyOnlyReportsAZoneInitCreated(t *testing.T) {
	// A zone the customer supplied (web_zone_id in the settings) is theirs.
	h := newDestroyHarness(t)
	h.tf.stateErr = &tfFailure{Cmd: "state", Stderr: "No state file was found!", Err: errors.New("x")}
	_ = h.env.in.WriteFile("terraform.tfvars", []byte(pinnedTFVars+"web_zone_id = \"Z0SUPPLIEDBYCUSTOMER\"\n"))
	h.env.tfv, _ = readTFVars(h.env.in.TFVars())
	h.aws.extra["logs describe-log-groups"] = "0"
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.out.String(), "Z0SUPPLIEDBYCUSTOMER") || strings.Contains(h.out.String(), "delete-hosted-zone") {
		t.Errorf("listed a zone Darkplum did not create:\n%s", h.out.String())
	}
}

func TestDestroyFailsWhenSomethingTaggedForTheClusterIsLeft(t *testing.T) {
	for name, tc := range map[string]struct{ key, out, want string }{
		"network interface": {"ec2 describe-network-interfaces", `["eni-0abc"]`, "network interface eni-0abc"},
		"security group":    {"ec2 describe-security-groups", `["sg-0abc"]`, "security group sg-0abc"},
		"load balancer":     {"elbv2 describe-tags", `["arn:aws:elasticloadbalancing:us-east-2:123456789012:loadbalancer/net/k8s-abc/1"]`, "load balancer arn:aws:elasticloadbalancing"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDestroyHarness(t)
			h.aws.extra["rds describe-db-snapshots"] = `[]`
			h.aws.extra["logs describe-log-groups"] = "0"
			h.aws.extra["elbv2 describe-load-balancers"] = `["arn:aws:elasticloadbalancing:us-east-2:123456789012:loadbalancer/net/k8s-abc/1"]`
			h.aws.extra[tc.key] = tc.out
			err := h.run()
			if !IsReported(err) {
				t.Fatalf("a leftover %s did not fail the destroy: %v", name, err)
			}
			h.contains(tc.want, "Still exists and should not")
			if !h.tf.ran("destroy") {
				t.Error("the infrastructure destroy itself should have run")
			}
		})
	}
	// Every lookup filters by this cluster's tag, never account-wide.
	h := newDestroyHarness(t)
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	_ = h.run()
	for _, ev := range h.tl.ev {
		if strings.Contains(ev, "describe-network-interfaces") && !strings.Contains(ev, "dp-trial") {
			t.Errorf("an unfiltered lookup: %s", ev)
		}
	}
}

func TestDestroyStopsWorkloadsBeforeDeletingVolumeClaims(t *testing.T) {
	h := newDestroyHarness(t)
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
	h.pvcs = []objRef{{inferenceNS, "model-cache"}}
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	svc, work, pvc := h.tl.first("delete svc"), h.tl.first("delete deployments,statefulsets"), h.tl.first("delete pvc")
	if !(svc >= 0 && svc < work && work < pvc) {
		t.Errorf("order must be load balancers, workloads, claims (%d %d %d):\n%s", svc, work, pvc, h.tl)
	}
	for _, c := range h.k.matching("delete deployments") {
		j := c.joined()
		if !strings.Contains(j, "-n "+platformNS) && !strings.Contains(j, "-n "+inferenceNS) {
			t.Errorf("workloads deleted outside the Darkplum namespaces: %s", j)
		}
	}
	// With no volume claims, workloads are left to the cluster's own deletion.
	h2 := newDestroyHarness(t)
	h2.aws.extra["rds describe-db-snapshots"] = `[]`
	h2.aws.extra["logs describe-log-groups"] = "0"
	if err := h2.run(); err != nil {
		t.Fatal(err)
	}
	if h2.tl.count("delete deployments") != 0 {
		t.Error("deleted workloads although there were no claims to release")
	}
}

// The semantic router has no load balancer and no volume: destroy needs no
// router-specific call, and its objects (cluster-scoped ones included) are never
// mistaken for something foreign that would stop the run.
func TestDestroyWithTheSemanticRouterOn(t *testing.T) {
	h := newDestroyHarness(t)
	h.env.rec.SemanticRouter = true
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, h.out.String(), h.tl)
	}
	for _, bad := range []string{"semantic-router", "helm", "set env", "delete"} {
		if n := len(h.k.matching(bad)); n != 0 {
			t.Errorf("destroy ran %d %q call(s) for the router: %v", n, bad, h.k.matching(bad)[0].args)
		}
	}
	if h.tl.first("terraform destroy") < 0 {
		t.Error("terraform destroy never ran")
	}
	h.contains("semantic router", "goes with the cluster", "Done")
	h.k.assertClean()
}

// An interrupted first deploy: resources, no outputs. Refused by default and
// under --confirm alone; destroyable under this cluster's exact state key and a
// second confirmation; never under a key that is not the cluster's own.
func TestDestroyUnfinishedInstall(t *testing.T) {
	unfinished := func(t *testing.T) *destroyHarness {
		h := newDestroyHarness(t)
		h.tf.outputs = "{}"
		h.aws.extra["rds describe-db-snapshots"] = `[]`
		h.aws.extra["logs describe-log-groups"] = "0"
		// No outputs means no final-snapshot setting to read: its own confirmation applies.
		h.env.flags.ConfirmNoSnapshot = "dp-trial"
		return h
	}
	t.Run("--confirm alone is refused and says how to go on", func(t *testing.T) {
		h := unfinished(t)
		h.env.interactive = false
		if !IsReported(h.run()) {
			t.Fatal("destroyed an unfinished install on --confirm alone")
		}
		h.assertNothingDeleted()
		if h.tf.ran("plan") || h.tl.count("kubectl delete") != 0 {
			t.Error("went on")
		}
		h.contains("--confirm-unfinished dp-trial", "darkplum deploy")
	})
	t.Run("--confirm-unfinished with --confirm destroys it", func(t *testing.T) {
		h := unfinished(t)
		h.env.flags.ConfirmUnfinished = "dp-trial"
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		if !h.tf.ran("destroy") {
			t.Error("terraform destroy did not run")
		}
		h.contains("never finished deploying", "--confirm-unfinished dp-trial")
		h.k.assertClean()
	})
	t.Run("--confirm-unfinished is not a --confirm", func(t *testing.T) {
		h := unfinished(t)
		h.env.flags.Confirm, h.env.flags.ConfirmUnfinished = "", "dp-trial"
		h.env.interactive = false
		if !IsReported(h.run()) {
			t.Fatal("the cluster name was never confirmed")
		}
		h.assertNothingDeleted()
	})
	t.Run("the wrong cluster name for the second confirmation", func(t *testing.T) {
		h := unfinished(t)
		h.env.flags.ConfirmUnfinished = "prod"
		if !IsReported(h.run()) {
			t.Fatal("went on")
		}
		h.assertNothingDeleted()
	})
	t.Run("typed interactively, twice", func(t *testing.T) {
		h := unfinished(t)
		h.env.flags.Confirm = ""
		h.prompts = []string{"dp-trial", "dp-trial"}
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		if len(h.asked) != 2 {
			t.Errorf("asked %d questions: %v", len(h.asked), h.asked)
		}
	})
	t.Run("typed once is not enough", func(t *testing.T) {
		h := unfinished(t)
		h.env.flags.Confirm = ""
		h.prompts = []string{"dp-trial", "yes"}
		if !IsReported(h.run()) {
			t.Fatal("went on")
		}
		h.assertNothingDeleted()
	})
	t.Run("a key that is not this cluster's own stays refused, whatever the flags", func(t *testing.T) {
		for _, key := range []string{"darkplum/aws/someone-else/terraform.tfstate", "darkplum/aws/x/dp-trial/terraform.tfstate", "darkplum/aws/terraform.tfstate"} {
			h := unfinished(t)
			h.env.rec.Backend.Key = key
			h.env.flags.ConfirmUnfinished = "dp-trial"
			if !IsReported(h.run()) {
				t.Fatalf("destroyed under key %q", key)
			}
			h.assertNothingDeleted()
			if h.tf.ran("plan") {
				t.Errorf("planned under key %q", key)
			}
		}
	})
}

func TestDestroyClusterStillBeingCreatedOrFailed(t *testing.T) {
	t.Run("CREATING: wait and re-run, nothing deleted", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.aws.base.status = "CREATING"
		if !IsReported(h.run()) {
			t.Fatal("went on")
		}
		h.assertNothingDeleted()
		h.contains("still changing state", "darkplum destroy")
	})
	t.Run("FAILED: Kubernetes skipped loudly, the infrastructure is destroyed", func(t *testing.T) {
		h := newDestroyHarness(t)
		h.aws.base.status = "FAILED"
		h.aws.extra["rds describe-db-snapshots"] = `[]`
		h.aws.extra["logs describe-log-groups"] = "0"
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		if h.tl.count("kubectl") != 0 {
			t.Errorf("kubectl ran against a FAILED cluster:\n%s", h.tl)
		}
		if !h.tf.ran("destroy") {
			t.Error("terraform destroy did not run")
		}
		h.contains("SKIPPING the Kubernetes phase", "FAILED")
	})
}

// A leftovers lookup that fails is not "nothing left": no Done line, a non-zero
// exit, and the lookup named.
func TestDestroyFailedLeftoverLookupIsNotClean(t *testing.T) {
	for _, q := range []string{"ec2 describe-network-interfaces", "ec2 describe-security-groups", "ec2 describe-volumes", "elbv2 describe-load-balancers", "elb describe-load-balancers"} {
		t.Run(q, func(t *testing.T) {
			h := newDestroyHarness(t)
			h.aws.extra["rds describe-db-snapshots"] = `[]`
			h.aws.extra["logs describe-log-groups"] = "0"
			h.aws.extraErr[q] = errors.New("An error occurred (UnauthorizedOperation): not allowed")
			err := h.run()
			if !IsReported(err) {
				t.Fatalf("err = %v\n%s", err, h.out.String())
			}
			if strings.Contains(h.out.String(), "Done") {
				t.Errorf("said Done although a lookup failed:\n%s", h.out.String())
			}
			h.contains("Could not confirm the account is clean", "UnauthorizedOperation")
			if !h.tf.ran("destroy") {
				t.Error("the destroy itself should have run")
			}
		})
	}
}

// Leftovers found AND a failed lookup: the finding fails the run and the failure is still said.
func TestDestroyLeftoverAndFailedLookupAreBothReported(t *testing.T) {
	h := newDestroyHarness(t)
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	h.aws.extra["ec2 describe-volumes"] = `["vol-0abc"]`
	h.aws.extraErr["elb describe-load-balancers"] = errors.New("Throttling")
	if !IsReported(h.run()) {
		t.Fatal("did not fail")
	}
	h.contains("EBS volume vol-0abc", "lookup(s) also failed", "Throttling")
}

// Volumes and Classic load balancers tagged for the cluster are leftovers too.
func TestDestroyEBSVolumesAndClassicLoadBalancersAreLeftovers(t *testing.T) {
	h := newDestroyHarness(t)
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	h.aws.extra["ec2 describe-volumes"] = `["vol-0abc"]`
	h.aws.extra["elb describe-load-balancers"] = `["a1b2c3"]`
	h.aws.extra["elb describe-tags"] = `["a1b2c3"]`
	if !IsReported(h.run()) {
		t.Fatal("did not fail")
	}
	h.contains("EBS volume vol-0abc", "classic load balancer a1b2c3")
	for _, ev := range h.tl.ev {
		if strings.Contains(ev, "describe-volumes") && !strings.Contains(ev, "kubernetes.io/cluster/dp-trial") {
			t.Errorf("an unfiltered volume lookup: %s", ev)
		}
		if strings.Contains(ev, "elb describe-tags") && !strings.Contains(ev, "kubernetes.io/cluster/dp-trial") {
			t.Errorf("classic tags not filtered by the cluster tag: %s", ev)
		}
	}
}

// The destroy plan can hold secrets too: owner-only from the first byte.
func TestDestroyPlanFileIsPrecreatedOwnerOnlyAndRemoved(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.planFile = true
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if len(h.tf.planBefore) != 1 {
		t.Fatalf("plan paths seen: %v", h.tf.planBefore)
	}
	for p, mode := range h.tf.planBefore {
		if mode != 0o600 {
			t.Errorf("%s held mode %o (-1 = absent) when terraform wrote it, want 0600", p, mode)
		}
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was left behind", p)
		}
	}
}

func TestPrecreatePrivateRefusesWhatIsAlreadyThere(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plan.tfplan")
	if err := precreatePrivate(p); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if err := precreatePrivate(p); err == nil {
		t.Error("overwrote an existing file")
	}
	link := filepath.Join(dir, "link.tfplan")
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	if err := precreatePrivate(link); err == nil {
		t.Error("followed a symlink")
	}
	if _, err := os.Stat(filepath.Join(dir, "elsewhere")); err == nil {
		t.Error("created the symlink's target")
	}
}

// The local record only knows what this machine applied. A newer release applied
// from elsewhere is in the cluster's darkplum-install ConfigMap, and both guards
// hold against it.
func TestDeployAndDestroyGuardAgainstTheClustersOwnRecord(t *testing.T) {
	record := func(v string) []byte { return []byte(`{"data":{"version":"` + v + `"}}`) }
	withVersion := func(k *scriptKube, v string) {
		k.handlers = append([]skHandler{{"get configmap darkplum-install", func([]string, []byte) ([]byte, error) { return record(v), nil }}}, k.handlers...)
	}
	t.Run("deploy: the cluster ran a newer release", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		liveCluster(h)
		withVersion(h.k, "1.9.0") // this binary is 1.2.3, the local record is empty
		if !IsReported(h.run()) {
			t.Fatal("deployed an older release over a newer one")
		}
		if h.tl.count("terraform") != 0 || h.writes() != 0 {
			t.Errorf("went on:\n%s", h.tl)
		}
		h.mustContain("cluster's own record", "--allow-downgrade", "Nothing was changed")
	})
	t.Run("deploy: --allow-downgrade, the same release and an older one pass", func(t *testing.T) {
		for _, tc := range []struct {
			v    string
			flag bool
		}{{"1.9.0", true}, {"1.2.3", false}, {"1.0.0", false}} {
			h := newDeployHarness(t, 0)
			liveCluster(h)
			h.env.flags.AllowDowngrade = tc.flag
			withVersion(h.k, tc.v)
			if err := h.run(); err != nil {
				t.Errorf("cluster %s, flag %v: %v\n%s", tc.v, tc.flag, err, h.out.String())
			}
		}
	})
	t.Run("deploy: an unreadable cluster record is said, not fatal", func(t *testing.T) {
		h := newDeployHarness(t, 0)
		liveCluster(h)
		h.k.handlers = append([]skHandler{{"get configmap darkplum-install", func([]string, []byte) ([]byte, error) {
			return nil, errors.New(`Error from server (Forbidden): configmaps "darkplum-install" is forbidden`)
		}}}, h.k.handlers...)
		if err := h.run(); err != nil {
			t.Fatalf("%v\n%s", err, h.out.String())
		}
		h.mustContain("release record could not be read", "local record only")
	})
	t.Run("destroy: the cluster ran a newer release", func(t *testing.T) {
		h := newDestroyHarness(t)
		withVersion(h.k, "1.9.0")
		h.env.version = "1.2.3"
		if !IsReported(h.run()) {
			t.Fatal("destroyed with an older darkplum")
		}
		h.assertNothingDeleted()
		h.contains("cluster's own record", "--allow-downgrade")
	})
}

// An interrupt during the Kubernetes deletions stops there: no further call, and
// Terraform's destroy never starts on a half-finished teardown.
func TestDestroyInterruptDuringTheKubernetesPhase(t *testing.T) {
	h := newDestroyHarness(t)
	h.svcs = []objRef{{platformNS, "darkplum-web-lb"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.k.handlers = append([]skHandler{{"delete svc", func([]string, []byte) ([]byte, error) {
		cancel()
		return nil, errors.New("kubectl: interrupted: context canceled")
	}}}, h.k.handlers...)
	err := h.env.run(ctx)
	if !IsReported(err) {
		t.Fatalf("err = %v\n%s", err, h.out.String())
	}
	h.contains("interrupted", "re-run `darkplum destroy`")
	if h.tf.ran("destroy") {
		t.Error("terraform destroy ran after the interrupt")
	}
}
