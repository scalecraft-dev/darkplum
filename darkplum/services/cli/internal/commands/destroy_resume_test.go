package commands

import (
	"errors"
	"strings"
	"testing"
)

// A cluster that rejects this identity (401), met by destroy's Kubernetes survey.
// Its own change, apart from the deploy resume work: destroy.go's survey hunk.
//
// The rule (destroy.go): the entry in the state is a stop; no entry is skipped only
// for an install that never finished, or one whose audit log group has already left
// the state (a destroy that got past its Kubernetes phase); anything else stops.

func (h *destroyHarness) reject401() {
	h.k.handlers = append([]skHandler{{"get svc -A", func([]string, []byte) ([]byte, error) { return nil, errors.New(kube401) }}}, h.k.handlers...)
}

// neverFinished is an install whose first deploy was interrupted before any output
// existed: resources, no outputs, no access entry.
func neverFinished(t *testing.T) *destroyHarness {
	h := newDestroyHarness(t)
	h.tf.outputs = "{}"
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	h.env.flags.ConfirmNoSnapshot = "dp-trial"
	h.env.flags.ConfirmUnfinished = "dp-trial"
	h.reject401()
	return h
}

// halfTornDown is an install whose destroy got past its Kubernetes phase and was
// interrupted in terraform destroy: the audit log group left the state in step 4 and
// the access entry went with the first resources of the destroy, the cluster remains.
func halfTornDown(t *testing.T) *destroyHarness {
	h := newDestroyHarness(t)
	h.tf.stateOut = strings.Replace(destroyState, logGroupAddr+"\n", "", 1)
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "1"
	h.reject401()
	return h
}

func TestDestroySkipsKubernetesForAnInstallThatNeverFinished(t *testing.T) {
	h := neverFinished(t)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if !h.tf.ran("destroy") {
		t.Error("terraform destroy did not run")
	}
	if h.kubeWrites() != 0 {
		t.Errorf("Kubernetes was written to without access:\n%s", h.tl)
	}
	// The warning names what is not done, not just that something is skipped.
	h.contains("SKIPPING the Kubernetes phase", "holds no access entry for it", "outside the Darkplum namespaces", "release record", "volume claims", "runs at the end, and also if the destroy fails")
}

func TestDestroySkipsKubernetesForADestroyThatWasInterruptedAfterItsKubernetesPhase(t *testing.T) {
	h := halfTornDown(t)
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if !h.tf.ran("destroy") || h.kubeWrites() != 0 {
		t.Errorf("destroy ran = %v, kube writes = %d\n%s", h.tf.ran("destroy"), h.kubeWrites(), h.tl)
	}
	h.contains("SKIPPING the Kubernetes phase")
}

func TestDestroyStopsOnA401ForAnInstallThatLooksFinished(t *testing.T) {
	// The default fixture: outputs, a database, the audit log group in the state, no
	// access entry. A finished install that lost its entry looks like this, and so does
	// a first deploy interrupted after its outputs: either way a destroy must not go on
	// without Kubernetes access, and the way out is `darkplum deploy`.
	h := newDestroyHarness(t)
	h.reject401()
	if !IsReported(h.run()) {
		t.Fatal("went on without Kubernetes access")
	}
	h.assertNothingDeleted()
	if h.tf.ran("plan") {
		t.Errorf("planned after a refusal:\n%s", h.tl)
	}
	h.contains("rejected this identity's credentials", "outputs", "`darkplum deploy` finishes it", "Nothing was deleted")
}

func TestDestroyStillStopsOnA401WhenTheStateHoldsTheAccessEntry(t *testing.T) {
	// The identity should have access and does not, in whatever shape the rest is.
	for name, h := range map[string]*destroyHarness{"never finished": neverFinished(t), "half torn down": halfTornDown(t), "looks finished": func() *destroyHarness {
		h := newDestroyHarness(t)
		h.reject401()
		return h
	}()} {
		h.tf.stateOut += creatorAccessEntryAddr + "\n"
		if !IsReported(h.run()) {
			t.Fatalf("%s: went on although the identity has lost its access", name)
		}
		h.assertNothingDeleted()
		h.contains("holds this install's access entry", "Nothing was deleted")
	}
}

func TestDestroyDoesNotSkipOnOtherSurveyErrors(t *testing.T) {
	h := neverFinished(t)
	h.k.handlers = append([]skHandler{{"get svc -A", func([]string, []byte) ([]byte, error) {
		return nil, errors.New(`kubectl: Error from server (Forbidden): services is forbidden`)
	}}}, h.k.handlers...)
	if !IsReported(h.run()) {
		t.Fatal("a Forbidden survey was skipped")
	}
	h.assertNothingDeleted()
}

func TestDestroyLooksForLeftoversWhenItSkippedKubernetesAndTheDestroyFails(t *testing.T) {
	// A load balancer the cluster's controllers made is the usual reason a VPC will not
	// delete, and the report that names it normally only runs after a successful destroy.
	h := neverFinished(t)
	h.tf.streamErr["destroy"] = errors.New("exit status 1")
	h.aws.extra["ec2 describe-network-interfaces"] = `["eni-0abc"]`
	if !IsReported(h.run()) {
		t.Fatal("a failed destroy was reported as success")
	}
	h.contains("exit status 1", "the Kubernetes phase was skipped", "eni-0abc", "usual reason a VPC will not delete")
}

func TestDestroyDoesNotAddTheLeftoverReportToAFailureWhenKubernetesWasNotSkipped(t *testing.T) {
	h := newDestroyHarness(t)
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	h.tf.streamErr["destroy"] = errors.New("exit status 1")
	if !IsReported(h.run()) {
		t.Fatal("a failed destroy was reported as success")
	}
	if strings.Contains(h.out.String(), "the Kubernetes phase was skipped") {
		t.Errorf("the skipped-phase report appeared although Kubernetes was cleaned up first:\n%s", h.out.String())
	}
}
