package billing

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	asked chan time.Duration
	fire  chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, asked: make(chan time.Duration, 16), fire: make(chan time.Time)}
}
func (c *fakeClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) set(t time.Time) { c.mu.Lock(); c.now = t; c.mu.Unlock() }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.asked <- d
	return c.fire
}

type stubVendor struct {
	pulls atomic.Int32
	keys  chan string
	rep   *Report
	err   error
}

func (s *stubVendor) Verify(context.Context, string, time.Time) (Sample, error) {
	return Sample{}, s.err
}
func (s *stubVendor) Pull(_ context.Context, key string, _, _ time.Time) (*Report, error) {
	s.pulls.Add(1)
	select {
	case s.keys <- key:
	default:
	}
	return s.rep, s.err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func statusOf(t *testing.T, s *Store, vendor string) Status {
	t.Helper()
	sts, err := s.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range sts {
		if st.Vendor == vendor {
			return st
		}
	}
	t.Fatalf("no status for %s", vendor)
	return Status{}
}

func TestUpsertIdempotent(t *testing.T) {
	pool := setupDB(t)
	st := &Store{DB: pool}
	ctx := context.Background()
	usage := []UsageRow{{Day: "2026-10-01", Model: "m", UncachedInput: 5, OutputToken: 2}}
	costs := []CostRow{{Day: "2026-10-01", Description: "d", Cents: mustRat("1.5")}}
	for i := 0; i < 3; i++ {
		usage[0].OutputToken = int64(2 + i) // later pull wins
		if err := st.Upsert(ctx, Anthropic, usage, costs); err != nil {
			t.Fatal(err)
		}
	}
	var n, out int
	if err := pool.QueryRow(ctx, `SELECT count(*), max(output) FROM vendor_usage_reports`).Scan(&n, &out); err != nil || n != 1 || out != 4 {
		t.Fatalf("usage rows=%d out=%d %v", n, out, err)
	}
	var c string
	if err := pool.QueryRow(ctx, `SELECT count(*)::text || '/' || max(cost_cents)::text FROM vendor_cost_reports`).Scan(&c); err != nil || c != "1/1.5000" {
		t.Fatalf("cost rows %s %v", c, err)
	}
}

func writeKey(t *testing.T, dir, vendor, key string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, KeyFile(vendor)), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStatusTransitions(t *testing.T) {
	pool := setupDB(t)
	logs := captureLogs(t)
	dir := t.TempDir()
	store := &Store{DB: pool}
	rep := &Report{Usage: []UsageRow{{Day: "2026-10-01", Model: "m"}}, Costs: []CostRow{{Day: "2026-10-01", Description: "d", Cents: mustRat("2")}}}
	av := &stubVendor{rep: rep, keys: make(chan string, 4)}
	ov := &stubVendor{err: &Error{Kind: KindWrongKeyType, HTTPStatus: 403}}
	svc := NewService(store, KeyReader{Dir: dir}, map[string]Vendor{Anthropic: av, OpenAI: ov}, nil, nil)
	ctx := context.Background()

	// no key -> not_configured, no vendor call
	svc.PullAll(ctx)
	if st := statusOf(t, store, Anthropic); st.Configured || st.State != StateNotConfigure || av.pulls.Load() != 0 {
		t.Fatalf("%+v", st)
	}
	if st := statusOf(t, store, Bedrock); st.Configured || st.State != StateNotConfigure {
		t.Fatalf("bedrock without a source must be not_configured: %+v", st)
	}

	// key present -> ok, last4 set, rows stored
	writeKey(t, dir, Anthropic, sentinel)
	writeKey(t, dir, OpenAI, "sk-proj-"+strings.Repeat("z", 30)+"WXYZ")
	svc.PullAll(ctx)
	st := statusOf(t, store, Anthropic)
	if !st.Configured || st.State != StateOK || st.LastPulledAt == nil || st.KeyLast4 == nil || *st.KeyLast4 != sentinel[len(sentinel)-4:] {
		t.Fatalf("%+v", st)
	}
	// OpenAI failure is classified and does not block/undo anthropic
	ost := statusOf(t, store, OpenAI)
	if ost.State != KindWrongKeyType || !ost.Configured || ost.LastErrorKind == nil || *ost.LastErrorKind != KindWrongKeyType || ost.LastPulledAt != nil {
		t.Fatalf("%+v", ost)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM vendor_cost_reports WHERE vendor='anthropic'`).Scan(&n)
	if n != 1 {
		t.Fatalf("anthropic rows not stored: %d", n)
	}

	// cost report unavailable -> state = kind but usage still stored
	av.rep = &Report{Usage: rep.Usage, CostsErr: &Error{Kind: KindUnreachable, HTTPStatus: 404}}
	svc.PullAll(ctx)
	if st := statusOf(t, store, Anthropic); st.State != KindUnreachable {
		t.Fatalf("%+v", st)
	}

	// removal via API overrides a lingering mounted file
	svc.MarkRemoved(Anthropic)
	before := av.pulls.Load()
	svc.PullAll(ctx)
	if st := statusOf(t, store, Anthropic); st.Configured || st.State != StateNotConfigure || av.pulls.Load() != before {
		t.Fatalf("%+v", st)
	}
	// override key (install time, file not yet propagated) is used verbatim
	svc.MarkInstalled(Anthropic)
	_ = os.Remove(filepath.Join(dir, KeyFile(Anthropic)))
	av.rep = rep
	svc.PullVendor(ctx, Anthropic, "sk-ant-admin01-OVERRIDEKEY00000000")
	if got := <-av.keys; got == "" {
		t.Fatal("no key")
	}
	if st := statusOf(t, store, Anthropic); st.State != StateOK {
		t.Fatalf("%+v", st)
	}

	// panic in a vendor client never escapes
	svc.Vendors[Anthropic] = panicVendor{}
	svc.PullVendor(ctx, Anthropic, "sk-ant-admin01-OVERRIDEKEY00000000")

	if l := logs.String(); strings.Contains(l, "SENTINEL") || strings.Contains(l, "OVERRIDEKEY") || strings.Contains(l, "zzzz") {
		t.Fatalf("key leaked into logs: %s", l)
	}
	if !strings.Contains(logs.String(), "kind=wrong_key_type") {
		t.Fatalf("closed kind not logged: %s", logs.String())
	}
}

type panicVendor struct{}

func (panicVendor) Verify(context.Context, string, time.Time) (Sample, error) { return Sample{}, nil }
func (panicVendor) Pull(context.Context, string, time.Time, time.Time) (*Report, error) {
	panic("boom")
}

func TestPollerSchedule(t *testing.T) {
	pool := setupDB(t)
	dir := t.TempDir()
	writeKey(t, dir, Anthropic, sentinel)
	av := &stubVendor{rep: &Report{}, keys: make(chan string, 8)}
	clk := newFakeClock(time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC))
	svc := NewService(&Store{DB: pool}, KeyReader{Dir: dir}, map[string]Vendor{Anthropic: av, OpenAI: &stubVendor{rep: &Report{}}}, nil, clk)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// startup pull, then waits until the next 03:00 UTC (17h away)
	if d := <-clk.asked; d != 17*time.Hour {
		t.Fatalf("first wait = %v", d)
	}
	if av.pulls.Load() != 1 {
		t.Fatalf("startup pulls = %d", av.pulls.Load())
	}
	// a Kick pulls immediately and re-arms
	svc.Kick()
	if d := <-clk.asked; d != 17*time.Hour {
		t.Fatalf("after kick wait = %v", d)
	}
	waitFor(t, "kick pull", func() bool { return av.pulls.Load() == 2 })
	// timer fires at 03:00 -> pull, next wait is 24h
	clk.set(time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC))
	clk.fire <- clk.Now()
	if d := <-clk.asked; d != 24*time.Hour {
		t.Fatalf("after daily wait = %v", d)
	}
	waitFor(t, "daily pull", func() bool { return av.pulls.Load() == 3 })
}

func TestPullUnknownVendorIsNoop(t *testing.T) {
	svc := NewService(&Store{}, KeyReader{}, nil, nil, nil)
	svc.PullVendor(context.Background(), "azure", "")
}

func mustRat(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }
