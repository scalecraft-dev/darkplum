package commands

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{
		300 * time.Millisecond: "under 1s",
		41 * time.Second:       "41s",
		133 * time.Second:      "2m13s",
		62 * time.Minute:       "1h02m00s",
		3733 * time.Second:     "1h02m13s",
	} {
		if got := elapsed(d); got != want {
			t.Errorf("elapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestDeployPrintsEachStepsTimeAndTheTotal(t *testing.T) {
	h := newDeployHarness(t, 0)
	// Terraform "takes" 90s: the fake clock only moves when something sleeps.
	prev := h.tf.onApply
	h.tf.onApply = func() { prev(); _ = h.clock.sleep(t.Context(), 90*time.Second) }
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	out := h.out.String()
	if n := strings.Count(out, "step took "); n != deploySteps {
		t.Errorf("want a time for each of %d steps, got %d:\n%s", deploySteps, n, out)
	}
	if !strings.Contains(out, "step took 1m30s") {
		t.Errorf("the 90s apply is not shown on its step:\n%s", out)
	}
	total := strings.Index(out, "Total time: ")
	if total < 0 || total > strings.Index(out, "Darkplum is deployed") {
		t.Errorf("the total must be printed once, before the verdict:\n%s", out)
	}
	if strings.Count(out, "Total time: ") != 1 {
		t.Errorf("total printed more than once:\n%s", out)
	}
}

func TestDeployFailureStillPrintsTheTotal(t *testing.T) {
	h := newDeployHarness(t, 0)
	h.resolveErr = errors.New("signature mismatch")
	if err := h.run(); err == nil {
		t.Fatal("expected the verification failure")
	}
	if !strings.Contains(h.out.String(), "Total time: ") {
		t.Errorf("a failed run must say how long it ran:\n%s", h.out.String())
	}
}

func TestDeployClockSurvivesAnInterruptedRunAndRestartsAfterACompleteOne(t *testing.T) {
	h := newDeployHarness(t, 0)
	start := h.clock.now()
	h.resolveErr = errors.New("registry down")
	_ = h.run()
	c, ok := loadDeployClock(h.in)
	if !ok || !c.Started.Equal(start) || !c.Finished.IsZero() {
		t.Fatalf("a failed deploy records its start and no finish: %+v %v", c, ok)
	}
	// An hour later the retry succeeds; the clock still counts from the first try.
	h.clock.sleep(t.Context(), time.Hour)
	h.resolveErr = nil
	h.env.timing = stepClock{}
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	c, _ = loadDeployClock(h.in)
	if !c.Started.Equal(start) || c.Finished.Sub(c.Started) < time.Hour {
		t.Errorf("the retry must keep the first start and set a finish: %+v", c)
	}
	// A deploy after a completed one starts a new clock.
	h.clock.sleep(t.Context(), time.Hour)
	later := h.clock.now()
	h.env.timing = stepClock{}
	if err := h.run(); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	c, _ = loadDeployClock(h.in)
	if !c.Started.Equal(later) {
		t.Errorf("a new deploy after a finished one starts a new clock: %+v want %s", c, later)
	}
}

func TestConfigureReportsTimeToFirstChatOnce(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{bedrockModel("us.anthropic.claude-sonnet", false)}
	h := newHarness(t, api, 1)
	start := h.clock
	var saved deployClock
	h.clockStore = &saved
	saved = deployClock{Started: start.Add(-17 * time.Minute), Finished: start.Add(-3 * time.Minute)}
	if err := h.run(&ConfigureCmd{}, "Jane.Doe@acme.com\n\n\n", testPassword, testPassword); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	h.contains("time to first chat: 17m", "from the start of the deploy to this chat (deploy took 14m00s)", "configure took ")
	if saved.FirstChat.IsZero() {
		t.Error("the first chat was not recorded")
	}
}

// The number would only grow if it were repeated on a later configure.
func TestConfigureReportsFirstChatOnlyOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	saved := deployClock{Started: now.Add(-20 * time.Minute)}
	var out bytes.Buffer
	r := &configureRun{configureEnv: &configureEnv{out: &out, now: func() time.Time { return now },
		loadClock: func() (deployClock, bool) { return saved, true },
		saveClock: func(c deployClock) error { saved = c; return nil }}}
	r.firstChat()
	if !strings.Contains(out.String(), "time to first chat: 20m00s") {
		t.Fatalf("first report missing:\n%s", out.String())
	}
	out.Reset()
	now = now.Add(time.Hour)
	r.firstChat()
	if out.Len() != 0 {
		t.Errorf("reported again:\n%s", out.String())
	}
}

func TestConfigureWithoutADeployRecordSaysNothingAboutIt(t *testing.T) {
	api := newFakeAPI()
	api.models = []map[string]any{bedrockModel("us.anthropic.claude-sonnet", false)}
	h := newHarness(t, api, 1)
	h.clockStore = &deployClock{} // zero Started: no deploy record
	if err := h.run(&ConfigureCmd{}, "Jane.Doe@acme.com\n\n\n", testPassword, testPassword); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if strings.Contains(h.out.String(), "time to first chat") {
		t.Errorf("no deploy record, so no time to report:\n%s", h.out.String())
	}
}
