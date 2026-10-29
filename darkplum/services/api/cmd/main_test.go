package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/db"
	"github.com/scalecraft-dev/darkplum/api/internal/handler"
)

func TestParseModelSyncInterval(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty uses default", "", 60 * time.Second},
		{"valid value passes through", "30", 30 * time.Second},
		{"non-numeric falls back to default", "not-a-number", 60 * time.Second},
		{"below the floor is clamped up", "5", 10 * time.Second},
		{"zero is clamped up", "0", 10 * time.Second},
		{"negative is clamped up", "-5", 10 * time.Second},
		{"exactly the floor is accepted", "10", 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseModelSyncInterval(tc.raw); got != tc.want {
				t.Errorf("parseModelSyncInterval(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseModelAvailabilityWindow(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty uses default", "", 120 * time.Second},
		{"valid value passes through", "90", 90 * time.Second},
		{"non-numeric falls back to default", "not-a-number", 120 * time.Second},
		{"below the floor is clamped up", "10", 30 * time.Second},
		{"zero is clamped up", "0", 30 * time.Second},
		{"negative is clamped up", "-5", 30 * time.Second},
		{"exactly the floor is accepted", "30", 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseModelAvailabilityWindow(tc.raw); got != tc.want {
				t.Errorf("parseModelAvailabilityWindow(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// ---- runModelSyncTicker / ModelHandler.Kick ----

// setupSyncTestDB skips unless TEST_DATABASE_URL is set, then creates a
// fresh, uniquely-named Postgres schema, migrates it, and returns a pool
// scoped to it via search_path -- cmd's own minimal copy of
// internal/handler's testdb_test.go helper (unexported there, and this is a
// different package), needed here only so runModelSyncTicker's test can
// exercise a real ModelHandler.AnyInstalling(), which reads
// vendor_provider_settings.
func setupSyncTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB integration test")
	}

	schema := fmt.Sprintf("main_sync_test_%d_%d", os.Getpid(), time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA "%s"`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	admin.Close()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool scoped to schema %s: %v", schema, err)
	}

	if err := db.RunMigrations(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("run migrations into schema %s: %v", schema, err)
	}

	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := pool.Exec(dropCtx, fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, schema)); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
		pool.Close()
	})

	return pool
}

// newSyncCountingGateway fakes the gateway's GET /v1/models with an
// always-empty model list, sending on syncedCh (non-blocking, so a slow test
// reader never stalls the HTTP handler) every time it's hit -- lets a test
// observe exactly when SyncModels ran without racing on shared state.
func newSyncCountingGateway(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	syncedCh := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []any{},
		})
		select {
		case syncedCh <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	return srv, syncedCh
}

func waitForSync(t *testing.T, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a model sync")
	}
}

func assertNoSyncWithin(t *testing.T, ch <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected model sync occurred")
	case <-time.After(d):
	}
}

// drainAtMostOneSync tolerates one sync within d (one already in flight) and
// fails on a second.
func drainAtMostOneSync(t *testing.T, ch <-chan struct{}, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	select {
	case <-ch:
	case <-deadline:
		return
	}
	select {
	case <-ch:
		t.Fatal("more than one model sync after the cadence should have reverted")
	case <-deadline:
	}
}

// waitUntil polls cond every 20ms until it's true or timeout elapses, doing
// one final check before failing -- used for state a sync updates via its
// own DB round trip strictly after the gateway HTTP call syncedCh observes
// completes, so asserting on that state the instant a channel receive
// returns would race the sync goroutine's remaining work.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", timeout)
	}
}

// TestRunModelSyncTicker_KickTriggersImmediateSyncAndFastCadence is jared's
// item: with a long (1h) normal syncInterval, a Kick() must make the loop
// sync almost immediately instead of waiting for the next natural tick, and
// once AnyInstalling() is true the loop must keep ticking at the fast
// providerInstallSyncInterval cadence on its own (no further Kick needed),
// reverting to the slow cadence once installing_until clears.
func TestRunModelSyncTicker_KickTriggersImmediateSyncAndFastCadence(t *testing.T) {
	pool := setupSyncTestDB(t)
	gw, syncedCh := newSyncCountingGateway(t)

	modelHandler := handler.NewModelHandler(gw.URL, "svc", pool)

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		runModelSyncTicker(ctx, modelHandler, time.Hour)
		close(loopDone)
	}()

	// A Kick with a 1h normal interval configured must produce a sync almost
	// immediately -- well within jared's ~1s bar, and nowhere close to 1h.
	modelHandler.Kick()
	waitForSync(t, syncedCh, 2*time.Second)

	// No vendor is installing yet, so the loop should NOT be ticking fast:
	// nothing else arrives on its own for several seconds.
	assertNoSyncWithin(t, syncedCh, 3*time.Second)

	// Mark a vendor as installing, then Kick once more so the loop's very
	// next sync observes it and switches to the fast cadence.
	until := time.Now().Add(30 * time.Second)
	if _, err := pool.Exec(ctx, `
		INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, installing_until)
		VALUES ('anthropic', '{claude-*}', now(), $1)
	`, until); err != nil {
		t.Fatalf("seed installing vendor: %v", err)
	}
	modelHandler.Kick()
	waitForSync(t, syncedCh, 2*time.Second)
	// The sync this Kick triggered updates AnyInstalling() via its own
	// separate DB round trip AFTER the gateway HTTP call syncedCh observed
	// completes, so poll briefly rather than asserting the instant
	// waitForSync returns.
	waitUntil(t, 2*time.Second, modelHandler.AnyInstalling)

	// The fast cadence (providerInstallSyncInterval, 5s) must now be
	// running WITHOUT another Kick -- proven by TWO further un-kicked syncs
	// arriving roughly providerInstallSyncInterval apart, not just one.
	// Deliberately no database write is interleaved between these: a sync
	// this loop runs is only proven fully finished (installing_until read,
	// AnyInstalling cache updated, ticker.Reset decided) once
	// AnyInstalling() itself reflects it, NOT the instant syncedCh's HTTP-
	// hit signal fires (that fires mid-sync, before the DB round trips and
	// the cadence decision that follow it) -- writing to
	// vendor_provider_settings right after a bare waitForSync would race
	// whichever sync is still in flight.
	waitForSync(t, syncedCh, providerInstallSyncInterval+2*time.Second)
	waitForSync(t, syncedCh, providerInstallSyncInterval+2*time.Second)

	// Clear the installing hint. Some in-flight sync may or may not observe
	// this particular write first -- that race is harmless, since the next
	// assertions only depend on the clear eventually being observed, not on
	// exactly which tick catches it.
	if _, err := pool.Exec(ctx, `UPDATE vendor_provider_settings SET installing_until = NULL WHERE vendor = 'anthropic'`); err != nil {
		t.Fatalf("clear installing vendor: %v", err)
	}
	waitUntil(t, providerInstallSyncInterval+3*time.Second, func() bool { return !modelHandler.AnyInstalling() })

	// The sync that just cleared AnyInstalling() decides its post-sync
	// cadence (the ticker.Reset call) synchronously, in-memory, immediately
	// after that same check, in the same goroutine -- no further I/O
	// between them -- so once AnyInstalling() reads false here, that sync
	// has already reverted the ticker (or is about to, within nanoseconds).
	// No further sync should arrive on the fast cadence from this point:
	// proves the ticker actually reverted to the slow (1h) interval instead
	// of continuing to tick every 5s.
	//
	// Under a loaded -race run, a fast tick that was already due can still be
	// in flight when AnyInstalling() first reads false, so allow at most one
	// sync in a first window, then require a whole fast interval with none: a
	// loop still on the fast cadence would always tick inside the second.
	drainAtMostOneSync(t, syncedCh, providerInstallSyncInterval+2*time.Second)
	assertNoSyncWithin(t, syncedCh, providerInstallSyncInterval+2*time.Second)

	cancel()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("runModelSyncTicker did not return after ctx cancellation -- possible goroutine leak")
	}
}
