package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPgStore_FailsClosedBeforeFirstLoad(t *testing.T) {
	s := &PgStore{}
	if _, ok := s.Current(); ok {
		t.Error("expected Current to report not-loaded before any Refresh")
	}
}

func TestPgStore_RefreshSuccessPopulatesSnapshot(t *testing.T) {
	s := &PgStore{
		loadFn: func(_ context.Context) (Snapshot, error) {
			return Snapshot{
				Enabled:   map[string]bool{"llama-3-8b": true},
				Aliases:   map[string]string{"default": "llama-3-8b"},
				Residency: ResidencyAny,
			}, nil
		},
	}

	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snap, ok := s.Current()
	if !ok {
		t.Fatal("expected Current to report loaded after a successful Refresh")
	}
	if !snap.Enabled["llama-3-8b"] {
		t.Error("expected llama-3-8b to be enabled")
	}
	if snap.Aliases["default"] != "llama-3-8b" {
		t.Error("expected alias default -> llama-3-8b")
	}
	if snap.Residency != ResidencyAny {
		t.Errorf("expected residency %q, got %q", ResidencyAny, snap.Residency)
	}
}

// TestPgStore_RefreshFailureBeforeFirstSuccessStaysUnloaded models a fresh
// customer database where the api has not run migration 010 yet: the table
// literally does not exist. That must be a load failure, not a panic, and the
// store must stay fail-closed until a later tick succeeds.
func TestPgStore_RefreshFailureBeforeFirstSuccessStaysUnloaded(t *testing.T) {
	s := &PgStore{
		loadFn: func(_ context.Context) (Snapshot, error) {
			return Snapshot{}, errors.New(`relation "model_aliases" does not exist`)
		},
	}

	err := s.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected an error from Refresh")
	}
	if _, ok := s.Current(); ok {
		t.Error("expected Current to remain not-loaded after a failed first Refresh")
	}
}

// TestPgStore_RefreshFailureAfterSuccessKeepsLastGood is the core fail-open
// (after first success) contract: a later refresh failure must not erase a
// previously loaded snapshot.
func TestPgStore_RefreshFailureAfterSuccessKeepsLastGood(t *testing.T) {
	attempt := 0
	s := &PgStore{
		loadFn: func(_ context.Context) (Snapshot, error) {
			attempt++
			if attempt == 1 {
				return Snapshot{Enabled: map[string]bool{"llama-3-8b": true}, Residency: ResidencyAny}, nil
			}
			return Snapshot{}, errors.New("connection reset by peer")
		},
	}

	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error on first refresh: %v", err)
	}
	first, ok := s.Current()
	if !ok {
		t.Fatal("expected loaded snapshot after first refresh")
	}

	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}

	second, ok := s.Current()
	if !ok {
		t.Fatal("expected Current to still report loaded (last-good) after a later refresh failure")
	}
	if second.Residency != first.Residency || !second.Enabled["llama-3-8b"] {
		t.Errorf("expected last-good snapshot to be unchanged, got %+v", second)
	}
	if attempt != 2 {
		t.Fatalf("expected exactly 2 load attempts, got %d", attempt)
	}
}

// ---------------------------------------------------------------------------
// Staleness cap (POLICY_MAX_STALENESS_SECONDS) — item 2
// ---------------------------------------------------------------------------

// fakeClock is a simple injectable clock for staleness tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestPgStore_FreshSnapshotWithinCapIsAvailable(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	s := &PgStore{
		maxStaleness: 30 * time.Second,
		now:          clock.now,
		loadFn: func(_ context.Context) (Snapshot, error) {
			return Snapshot{Residency: ResidencyAny, Enabled: map[string]bool{}}, nil
		},
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clock.advance(29 * time.Second)
	if _, ok := s.Current(); !ok {
		t.Error("expected Current to report loaded within the staleness cap")
	}
}

// TestPgStore_StaleSnapshotBehavesUnavailable is the core item-2 regression
// guard: once the last successful load is older than maxStaleness, Current
// must report "not loaded" (-> 503 policy_unavailable) even though a
// last-good snapshot is technically still cached.
func TestPgStore_StaleSnapshotBehavesUnavailable(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	refreshShouldFail := false
	s := &PgStore{
		maxStaleness: 30 * time.Second,
		now:          clock.now,
		loadFn: func(_ context.Context) (Snapshot, error) {
			if refreshShouldFail {
				return Snapshot{}, errors.New("db unreachable")
			}
			return Snapshot{Residency: ResidencyAny, Enabled: map[string]bool{}}, nil
		},
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error on initial load: %v", err)
	}

	// Simulate an extended outage: every later refresh fails, and enough
	// time passes to exceed the cap.
	refreshShouldFail = true
	clock.advance(31 * time.Second)
	_ = s.Refresh(context.Background())

	if _, ok := s.Current(); ok {
		t.Error("expected Current to report not-loaded once the snapshot exceeds the staleness cap")
	}
}

func TestPgStore_ZeroMaxStalenessDisablesCap(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	s := &PgStore{
		// maxStaleness left at zero value: disabled.
		now: clock.now,
		loadFn: func(_ context.Context) (Snapshot, error) {
			return Snapshot{Residency: ResidencyAny, Enabled: map[string]bool{}}, nil
		},
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clock.advance(10 * 365 * 24 * time.Hour) // 10 years
	if _, ok := s.Current(); !ok {
		t.Error("expected a disabled cap (maxStaleness=0) to never report stale")
	}
}

// TestPgStore_RecoveryAfterStalenessRestoresAvailability confirms that a
// successful refresh after a stale period clears the stale state.
func TestPgStore_RecoveryAfterStalenessRestoresAvailability(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	refreshShouldFail := false
	s := &PgStore{
		maxStaleness: 30 * time.Second,
		now:          clock.now,
		loadFn: func(_ context.Context) (Snapshot, error) {
			if refreshShouldFail {
				return Snapshot{}, errors.New("db unreachable")
			}
			return Snapshot{Residency: ResidencyAny, Enabled: map[string]bool{"m": true}}, nil
		},
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	refreshShouldFail = true
	clock.advance(31 * time.Second)
	_ = s.Refresh(context.Background())
	if _, ok := s.Current(); ok {
		t.Fatal("expected stale snapshot to be unavailable")
	}

	refreshShouldFail = false
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error on recovery refresh: %v", err)
	}
	snap, ok := s.Current()
	if !ok {
		t.Fatal("expected Current to report loaded again after a successful refresh clears staleness")
	}
	if !snap.Enabled["m"] {
		t.Error("expected the recovered snapshot's data")
	}
}

func TestPgStore_ConcurrentRefreshAndCurrent(t *testing.T) {
	s := &PgStore{
		loadFn: func(_ context.Context) (Snapshot, error) {
			return Snapshot{Residency: ResidencyAny, Enabled: map[string]bool{"m": true}}, nil
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.Refresh(context.Background())
		}()
		go func() {
			defer wg.Done()
			_, _ = s.Current()
		}()
	}
	wg.Wait()
}
