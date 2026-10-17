package routing

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// resolveVendorFilter — the pure last-writer-wins decision
// ---------------------------------------------------------------------------

func TestResolveVendorFilter_DBWinsWhenNewerOrEqual(t *testing.T) {
	env := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := env.Add(time.Second)

	got := resolveVendorFilter([]string{"claude-*"}, newer, true, []string{"gpt-*"}, env)
	if got.Source != "db" || len(got.Patterns) != 1 || got.Patterns[0] != "claude-*" {
		t.Errorf("expected db to win when strictly newer, got %+v", got)
	}

	got = resolveVendorFilter([]string{"claude-*"}, env, true, []string{"gpt-*"}, env)
	if got.Source != "db" {
		t.Errorf("expected db to win on an EQUAL timestamp (>=), got %+v", got)
	}
}

func TestResolveVendorFilter_EnvWinsWhenNewerOrNoDBRow(t *testing.T) {
	dbSetAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	envSetAt := dbSetAt.Add(time.Second)

	// Env newer than an existing DB row: last-writer-wins in BOTH
	// directions.
	got := resolveVendorFilter([]string{"claude-*"}, dbSetAt, true, []string{"gpt-*"}, envSetAt)
	if got.Source != "env" || len(got.Patterns) != 1 || got.Patterns[0] != "gpt-*" {
		t.Errorf("expected env to win when strictly newer than an existing DB row, got %+v", got)
	}

	// No DB row at all: env wins regardless of its own timestamp.
	got = resolveVendorFilter(nil, time.Time{}, false, []string{"gpt-*"}, time.Time{})
	if got.Source != "env" || len(got.Patterns) != 1 || got.Patterns[0] != "gpt-*" {
		t.Errorf("expected env to win with no DB row at all, got %+v", got)
	}
}

func TestResolveVendorFilter_NoneWhenNeitherConfigured(t *testing.T) {
	got := resolveVendorFilter(nil, time.Time{}, false, nil, time.Time{})
	if got.Source != "none" || got.Patterns != nil {
		t.Errorf("expected none/nil with nothing configured, got %+v", got)
	}
}

func TestResolveVendorFilter_MissingEnvSetAtIsZeroTime_AnyStampedDBRowWins(t *testing.T) {
	// A DB row with ANY real timestamp beats an env filter whose own SET_AT
	// is the zero time (unset/unparseable) — see config.parseVendorFilterSetAt.
	dbSetAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // old, but still > zero
	got := resolveVendorFilter([]string{"claude-*"}, dbSetAt, true, []string{"gpt-*"}, time.Time{})
	if got.Source != "db" {
		t.Errorf("expected an unstamped env filter to lose to any stamped DB row, got %+v", got)
	}
}

func TestResolveVendorFilter_EnvSetAtAlwaysReportedRegardlessOfWinner(t *testing.T) {
	envSetAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	dbSetAt := envSetAt.Add(time.Hour) // db wins
	got := resolveVendorFilter([]string{"claude-*"}, dbSetAt, true, []string{"gpt-*"}, envSetAt)
	if got.Source != "db" {
		t.Fatalf("expected db to win, got %+v", got)
	}
	if !got.EnvSetAt.Equal(envSetAt) {
		t.Errorf("expected EnvSetAt to be reported even when env didn't win, got %v want %v", got.EnvSetAt, envSetAt)
	}
}

// ---------------------------------------------------------------------------
// sameGlobSet
// ---------------------------------------------------------------------------

func TestSameGlobSet(t *testing.T) {
	if !sameGlobSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("expected order-independent equality")
	}
	if sameGlobSet([]string{"a"}, []string{"a", "b"}) {
		t.Error("expected different lengths to be unequal")
	}
	if sameGlobSet([]string{"a"}, []string{"b"}) {
		t.Error("expected different values to be unequal")
	}
	if !sameGlobSet(nil, nil) {
		t.Error("expected two nil slices to be equal")
	}
}

// ---------------------------------------------------------------------------
// NewVendorFilterFunc — the per-tick closure, including the hand-edited-
// ConfigMap warning
// ---------------------------------------------------------------------------

// fakeVendorFilterPolicySource is a vendorFilterPolicySource test double.
type fakeVendorFilterPolicySource struct {
	filter    map[string][]string
	setAt     map[string]time.Time
	ok        map[string]bool
	workspace map[string]string
}

func (f fakeVendorFilterPolicySource) VendorFilter(vendor string) ([]string, time.Time, string, bool) {
	return f.filter[vendor], f.setAt[vendor], f.workspace[vendor], f.ok[vendor]
}

func TestNewVendorFilterFunc_ReadsLiveSnapshotEveryCall(t *testing.T) {
	src := fakeVendorFilterPolicySource{
		filter: map[string][]string{"anthropic": {"claude-*"}},
		setAt:  map[string]time.Time{"anthropic": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		ok:     map[string]bool{"anthropic": true},
	}
	fn := NewVendorFilterFunc("anthropic", nil, time.Time{}, "", src)

	got := fn()
	if got.Source != "db" || len(got.Patterns) != 1 || got.Patterns[0] != "claude-*" {
		t.Fatalf("expected the DB row to win with no env filter at all, got %+v", got)
	}

	// Mutating the fake's underlying map simulates a live snapshot changing
	// between ticks — NewVendorFilterFunc must re-read on every call, not
	// cache anything from the first.
	src.filter["anthropic"] = []string{"claude-opus-*"}
	got = fn()
	if len(got.Patterns) != 1 || got.Patterns[0] != "claude-opus-*" {
		t.Errorf("expected the closure to observe the updated filter on the next call, got %+v", got)
	}
}

func TestNewVendorFilterFunc_LogsHandEditedConfigMapWarningOncePerChange(t *testing.T) {
	setAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	src := fakeVendorFilterPolicySource{
		filter: map[string][]string{"openai": {"gpt-5*"}},
		setAt:  map[string]time.Time{"openai": setAt},
		ok:     map[string]bool{"openai": true},
	}
	// Env carries the SAME timestamp but a DIFFERENT value — only possible
	// via a hand-edited ConfigMap (see the type's doc comment).
	fn := NewVendorFilterFunc("openai", []string{"o1*"}, setAt, "", src)

	buf := captureLog(t)

	fn()
	fn()
	fn()

	count := strings.Count(buf.String(), "hand-edited ConfigMap")
	if count != 1 {
		t.Fatalf("expected exactly 1 warning across 3 identical calls, got %d; log:\n%s", count, buf.String())
	}

	// A DIFFERENT mismatch (env value changes) must log again — "once per
	// CHANGE", not "once ever".
	src.filter["openai"] = []string{"gpt-4*"}
	fn()
	count = strings.Count(buf.String(), "hand-edited ConfigMap")
	if count != 2 {
		t.Fatalf("expected a second warning after the mismatch changed, got %d; log:\n%s", count, buf.String())
	}
}

func TestNewVendorFilterFunc_NoWarningWhenStampsDiffer(t *testing.T) {
	dbSetAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	envSetAt := dbSetAt.Add(time.Hour)
	src := fakeVendorFilterPolicySource{
		filter: map[string][]string{"anthropic": {"claude-*"}},
		setAt:  map[string]time.Time{"anthropic": dbSetAt},
		ok:     map[string]bool{"anthropic": true},
	}
	fn := NewVendorFilterFunc("anthropic", []string{"different-value*"}, envSetAt, "", src)

	buf := captureLog(t)
	fn()
	if strings.Contains(buf.String(), "hand-edited ConfigMap") {
		t.Errorf("expected no warning when the stamps genuinely differ (ordinary last-writer-wins), got log:\n%s", buf.String())
	}
}

func TestNewVendorFilterFunc_NoWarningWhenValuesAgree(t *testing.T) {
	setAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	src := fakeVendorFilterPolicySource{
		filter: map[string][]string{"anthropic": {"claude-*", "claude-opus-*"}},
		setAt:  map[string]time.Time{"anthropic": setAt},
		ok:     map[string]bool{"anthropic": true},
	}
	// Same values, different slice order — sameGlobSet must treat these as
	// equal, so no false-positive warning.
	fn := NewVendorFilterFunc("anthropic", []string{"claude-opus-*", "claude-*"}, setAt, "", src)

	buf := captureLog(t)
	fn()
	if strings.Contains(buf.String(), "hand-edited ConfigMap") {
		t.Errorf("expected no warning when the values agree (order-independent), got log:\n%s", buf.String())
	}
}

// Workspace id rides on the same last-writer-wins winner as the filter.
func TestNewVendorFilterFunc_WorkspaceIDFollowsFilterWinner(t *testing.T) {
	envAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := envAt.Add(time.Hour)
	older := envAt.Add(-time.Hour)

	mk := func(dbAt time.Time, dbOK bool, dbWS string) vendorFilterFunc {
		src := fakeVendorFilterPolicySource{
			filter:    map[string][]string{"anthropic": {"claude-*"}},
			setAt:     map[string]time.Time{"anthropic": dbAt},
			ok:        map[string]bool{"anthropic": dbOK},
			workspace: map[string]string{"anthropic": dbWS},
		}
		return NewVendorFilterFunc("anthropic", []string{"claude-env-*"}, envAt, "wrkspc_env", src)
	}

	if got := mk(newer, true, "wrkspc_db")(); got.Source != "db" || got.WorkspaceID != "wrkspc_db" {
		t.Errorf("newer DB row must win with its workspace: %+v", got)
	}
	// Explicitly cleared on the winning DB row: empty, NOT the env fallback.
	if got := mk(newer, true, "")(); got.Source != "db" || got.WorkspaceID != "" {
		t.Errorf("cleared DB workspace must not fall back to env: %+v", got)
	}
	if got := mk(envAt, true, "wrkspc_db")(); got.Source != "db" || got.WorkspaceID != "wrkspc_db" {
		t.Errorf("equal stamp: DB wins: %+v", got)
	}
	if got := mk(older, true, "wrkspc_db")(); got.Source != "env" || got.WorkspaceID != "wrkspc_env" {
		t.Errorf("older DB row loses to env, incl. workspace: %+v", got)
	}
	if got := mk(time.Time{}, false, "")(); got.Source != "env" || got.WorkspaceID != "wrkspc_env" {
		t.Errorf("no DB row: env workspace: %+v", got)
	}

	none := NewVendorFilterFunc("anthropic", nil, time.Time{}, "", fakeVendorFilterPolicySource{})()
	if none.Source != "none" || none.WorkspaceID != "" {
		t.Errorf("nothing configured: %+v", none)
	}
}

func TestVendorSource_ReportsWorkspaceID(t *testing.T) {
	status := &fakeStatusReporter{}
	creds := &fakeAnthropicCreds{key: "k", version: 1}
	disc := &fakeAnthropicDiscoverer{models: map[string]bool{"claude-haiku-4-5": true}}
	filter := func() vendorFilterResult {
		return vendorFilterResult{Patterns: []string{"claude-*"}, Source: "db", WorkspaceID: "wrkspc_01"}
	}
	src := NewAnthropicSource(creds, disc, filter, status)
	if _, err := src.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := status.last().WorkspaceID; got != "wrkspc_01" {
		t.Errorf("status workspace_id = %q", got)
	}
}
