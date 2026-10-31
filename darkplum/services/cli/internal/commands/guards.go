package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

// ---- release ordering -------------------------------------------------------------

var (
	relVersionRe = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	rcVersionRe  = regexp.MustCompile(`^0\.0\.0-rc\.([0-9]+)\+[0-9a-f]{7}$`)
)

// versionGuard refuses to apply or destroy an install with an OLDER release than
// the one that last applied it, unless allowDowngrade. A downgrade runs old
// manifests and old Terraform against state a newer one already changed.
//
// Only like can be compared with like: two releases by version, two release
// candidates by number. A recorded version of another kind (a candidate under a
// release binary and the reverse, a development build, something unreadable) has no
// order, so it is refused too unless the flag says the operator knows.
func versionGuard(recorded, current string, allowDowngrade bool) error {
	if recorded == "" || recorded == current || allowDowngrade {
		return nil
	}
	refuse := func(why string) error {
		return fmt.Errorf("this install was last applied by %s and this darkplum is %s: %s. "+
			"Applying an older or unrelated release can undo what the newer one did; if you mean to, pass --allow-downgrade", recorded, current, why)
	}
	rm, cm := relVersionRe.FindStringSubmatch(recorded), relVersionRe.FindStringSubmatch(current)
	if rm != nil && cm != nil {
		for i := 1; i <= 3; i++ {
			r, _ := strconv.Atoi(rm[i])
			c, _ := strconv.Atoi(cm[i])
			if c != r {
				if c < r {
					return refuse("it is older")
				}
				return nil
			}
		}
		return nil
	}
	rr, cr := rcVersionRe.FindStringSubmatch(recorded), rcVersionRe.FindStringSubmatch(current)
	if rr != nil && cr != nil {
		r, _ := strconv.Atoi(rr[1])
		c, _ := strconv.Atoi(cr[1])
		if c < r {
			return refuse("it is an older release candidate")
		}
		return nil
	}
	return refuse("their order is unknown")
}

// ---- state ownership --------------------------------------------------------------

func managedCount(state map[string]bool) int {
	n := 0
	for a := range state {
		if !strings.HasPrefix(a, "data.") {
			n++
		}
	}
	return n
}

// checkStateOwnership makes sure the Terraform state this install points at is this
// install's: its outputs must name the install's cluster and region. A state whose
// outputs name another cluster is refused. A state that has resources but no
// outputs is refused when strict (destroy: it cannot be told apart from a foreign
// state, and deleting a foreign state's resources is not undoable). Deploy is not
// strict, because a first apply that was interrupted leaves resources and no
// outputs, and resuming it is the point; there the backend key, which init derives
// from the cluster name, has to name this cluster.
func checkStateOwnership(tf tfRunner, rec install.Record, state map[string]bool, strict bool) (warn string, err error) {
	if managedCount(state) == 0 {
		return "", nil
	}
	raw, err := tf.Capture("output", "-json")
	if err != nil {
		return "", fmt.Errorf("cannot read the state's outputs to check it belongs to this install: %w", err)
	}
	var all map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &all); err != nil {
		return "", errors.New("unexpected terraform output")
	}
	str := func(k string) string {
		var s string
		if v, ok := all[k]; ok {
			_ = json.Unmarshal(v.Value, &s)
		}
		return s
	}
	name, region := str("cluster_name"), str("provider_region")
	if name == "" && region == "" {
		if strict {
			return "", errors.New("the state holds resources but no outputs, so it cannot be shown to belong to this install (an unfinished apply looks like this; so does someone else's state)")
		}
		if !strings.Contains(rec.Backend.Key, "/"+rec.ClusterName+"/") {
			return "", fmt.Errorf("the state holds resources but no outputs, and its key %q does not name cluster %s", rec.Backend.Key, rec.ClusterName)
		}
		return "the state has resources but no outputs yet (an unfinished earlier apply); continuing because its key names this cluster", nil
	}
	if name != rec.ClusterName || region != rec.Region {
		return "", fmt.Errorf("the state describes cluster %q in %q, this install is %q in %q: the state backend does not belong to this install", name, region, rec.ClusterName, rec.Region)
	}
	return "", nil
}

// ---- cluster reachability -----------------------------------------------------------

// unreachable reports a kubectl failure that means "cannot connect", as opposed to
// one that means "connected and was refused or does not exist".
func unreachable(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{"unable to connect", "i/o timeout", "connection refused", "no such host", "tls handshake timeout",
		"context deadline exceeded", "net/http: request canceled", "eof", "dial tcp"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// strictSession is checkAWSSession for the re-checks made mid-run: when the
// expiry cannot be read it fails, where the preflight only warns. A re-check
// exists to catch a session about to lapse; "unknown" must not pass it.
func strictSession(profile string, minMinutes int) checkResult {
	return strictSessionWith(awsSessionExpiry, profile, minMinutes, time.Now())
}

func strictSessionWith(expiry func(string) (time.Time, error), profile string, minMinutes int, now time.Time) checkResult {
	r := sessionResult(expiry, profile, minMinutes, now)
	if r.status == checkWarn {
		r.status = checkFail
		r.detail = "cannot tell when the AWS session expires"
		login := "aws sso login"
		if profile != "" {
			login += " --profile " + profile
		}
		r.hint = "run `" + login + "` and re-run"
	}
	return r
}
