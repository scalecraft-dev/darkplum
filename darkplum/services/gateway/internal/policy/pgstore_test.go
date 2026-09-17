package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
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
