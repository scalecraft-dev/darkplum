package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/runner"
)

// deadlineTF records the context deadline of each Stream and blocks until released.
type deadlineTF struct {
	*fakeTF
	mu       sync.Mutex
	deadline map[string]bool
}

func (d *deadlineTF) Stream(ctx context.Context, w io.Writer, o runner.StreamOptions, args ...string) error {
	_, has := ctx.Deadline()
	d.mu.Lock()
	d.deadline[args[0]] = has
	d.mu.Unlock()
	return d.fakeTF.Stream(ctx, w, o, args...)
}

// Terraform's apply and destroy are not cut off by a timer: the heartbeat, Ctrl-C
// and the post-interrupt backstop bound them. A read-only plan is bounded.
func TestOnlyTheReadOnlyPlanHasATimeLimit(t *testing.T) {
	d := newDestroyHarness(t)
	rec := &deadlineTF{fakeTF: d.tf, deadline: map[string]bool{}}
	d.env.openTF = func() (tfRunner, string, error) { return rec, "", nil }
	d.aws.extra["rds describe-db-snapshots"] = `[]`
	d.aws.extra["logs describe-log-groups"] = "0"
	if err := d.run(); err != nil {
		t.Fatalf("%v\n%s", err, d.out.String())
	}
	if rec.deadline["destroy"] {
		t.Error("terraform destroy has a deadline")
	}
	if rec.deadline["apply"] {
		t.Error("the protection apply has a deadline")
	}
	if !rec.deadline["plan"] {
		t.Error("plan -destroy has no time limit")
	}

	h := newDeployHarness(t, 0)
	dr := &deadlineTF{fakeTF: h.tf, deadline: map[string]bool{}}
	h.env.openTF = func() (tfRunner, string, error) { return dr, "", nil }
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if dr.deadline["apply"] || dr.deadline["plan"] {
		t.Errorf("deploy's Terraform has a deadline: %v", dr.deadline)
	}
}

func TestStreamTerraformLimitStopsOnlyWhenSet(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.tf.streamDelay = time.Hour
	var out strings.Builder
	start := time.Now()
	tl, ctxErr, err := streamTerraform(context.Background(), h.tf, &out, time.Now, time.Hour, "t", nil, tfStreamOpts{Limit: 30 * time.Millisecond}, "plan")
	if err == nil || !errors.Is(ctxErr, context.DeadlineExceeded) || time.Since(start) > 5*time.Second || tl == nil {
		t.Fatalf("limit not enforced: %v %v", err, ctxErr)
	}
	// no limit: only the caller's context ends it
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, ctxErr, err = streamTerraform(ctx, h.tf, &out, time.Now, time.Hour, "t", nil, tfStreamOpts{}, "plan")
	if err == nil || !errors.Is(ctxErr, context.Canceled) {
		t.Errorf("without a limit it ended for %v (%v)", ctxErr, err)
	}
}

// plan -destroy shows a heartbeat, not Terraform's scroll, and says why it was
// stopped when the limit is hit.
func TestDestroyPlanHasAHeartbeatAndKeepsTheScrollOffTheTerminal(t *testing.T) {
	h := newDestroyHarness(t)
	h.env.heartbeat = 10 * time.Millisecond
	h.tf.streamDelay = 120 * time.Millisecond
	h.tf.streamText["plan"] = "aws_vpc.this: Refreshing state... [id=vpc-0abc]"
	h.aws.extra["rds describe-db-snapshots"] = `[]`
	h.aws.extra["logs describe-log-groups"] = "0"
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	out := h.out.String()
	if !strings.Contains(out, "terraform plan -destroy 0m00s") {
		t.Errorf("no heartbeat while plan -destroy was quiet:\n%s", out)
	}
	if strings.Contains(out, "Refreshing state") {
		t.Errorf("the plan's scroll is on the terminal:\n%s", out)
	}
}

func TestDestroyPlanFailureShowsTheSessionHint(t *testing.T) {
	h := newDestroyHarness(t)
	h.tf.streamErr["plan"] = errors.New("exit status 1")
	h.tf.streamText["plan"] = "Error: ExpiredToken: The security token included in the request is expired"
	if !IsReported(h.run()) {
		t.Fatal("went on")
	}
	h.assertNothingDeleted()
	h.contains("plan -destroy", "aws sso login")
}
