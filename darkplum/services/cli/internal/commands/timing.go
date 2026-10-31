package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

// elapsed renders a duration the way an operator reads a wait: "41s", "2m13s",
// "1h02m13s". Sub-second times are "under 1s" rather than a misleading "0s".
func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Second:
		return "under 1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm%02ds", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
}

// deployClock is what the workspace remembers of when a deploy began, so
// `darkplum configure` can say how long the whole path took. It is a workspace
// file of its own (not the install record): an older darkplum must still read
// the record, and nothing here is a setting of the install.
//
// An interrupted deploy keeps its original Started, so re-running it does not
// hide the time already spent; the first deploy that completes sets Finished,
// and the next deploy after that starts a new clock.
type deployClock struct {
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitzero"`
	FirstChat time.Time `json:"first_chat,omitzero"`
}

const deployClockFile = "deploy-clock.json"

func loadDeployClock(in install.Install) (deployClock, bool) {
	b, err := os.ReadFile(filepath.Join(in.Dir(), deployClockFile))
	if err != nil {
		return deployClock{}, false
	}
	var c deployClock
	if json.Unmarshal(b, &c) != nil || c.Started.IsZero() {
		return deployClock{}, false
	}
	return c, true
}

func saveDeployClock(in install.Install, c deployClock) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return in.WriteFile(deployClockFile, append(b, '\n'))
}

// stepClock times the numbered steps of a run and its total.
type stepClock struct {
	now       func() time.Time
	start     time.Time
	stepStart time.Time
	stepOpen  bool
	done      bool
}

func (s *stepClock) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}
