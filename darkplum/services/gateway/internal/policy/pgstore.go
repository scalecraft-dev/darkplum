package policy

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore loads Snapshots from Postgres and caches the last-good result.
// Refresh performs exactly 3 SELECTs (models, model_aliases, routing_policy).
//
// A freshly onboarded customer database may not have run the api's migration
// 010 yet when the gateway starts — model_aliases/routing_policy simply don't
// exist. That is treated as an ordinary load failure (fail closed, retried on
// the next tick), never a crash.
//
// Staleness cap: if maxStaleness is positive and the last successful load is
// older than it (e.g. Postgres has been unreachable for a long outage and
// every refresh has failed since), Current reports "not loaded" even though a
// last-good snapshot technically still exists in memory — fail closed rather
// than keep enforcing a routing policy that may be arbitrarily out of date
// forever. maxStaleness <= 0 disables the cap (only ever the case for a
// PgStore built directly in tests; NewPgStore's caller (cmd/main.go) always
// passes a positive value, enforced to be >= 30s at config-load time).
type PgStore struct {
	pool *pgxpool.Pool

	// loadFn performs the actual load. Defaults to loadFromDB; overridable in
	// tests (same package) so refresh/last-good behaviour can be exercised
	// without a real database.
	loadFn func(ctx context.Context) (Snapshot, error)

	// maxStaleness is the staleness cap described above. Zero disables it.
	maxStaleness time.Duration

	// now is an injectable clock for tests. Defaults to time.Now via clock()
	// when nil, so zero-value PgStore literals (as used by existing tests)
	// keep working unchanged.
	now func() time.Time

	snap     atomic.Pointer[Snapshot]
	loadedAt atomic.Pointer[time.Time]

	// stale tracks whether the store is currently in the "stale, refusing as
	// unavailable" state, so Current logs loudly exactly once on each
	// transition rather than on every request.
	stale atomic.Bool
}

// NewPgStore constructs a PgStore backed by pool. maxStaleness is the
// POLICY_MAX_STALENESS_SECONDS cap (0 disables it — callers should not pass 0
// in production; config.Load enforces a minimum of 30s). The store is
// unloaded (fail-closed) until Refresh or Run is called.
func NewPgStore(pool *pgxpool.Pool, maxStaleness time.Duration) *PgStore {
	s := &PgStore{pool: pool, maxStaleness: maxStaleness}
	s.loadFn = s.loadFromDB
	return s
}

// clock returns the current time via the injectable now field, or time.Now
// if unset.
func (s *PgStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Current implements Store. It fails closed (returns false) both when no
// snapshot has ever loaded successfully AND when the last successful load is
// older than maxStaleness.
func (s *PgStore) Current() (Snapshot, bool) {
	p := s.snap.Load()
	if p == nil {
		return Snapshot{}, false
	}

	if s.maxStaleness > 0 {
		loadedAt := s.loadedAt.Load()
		if loadedAt == nil {
			// Should not happen (snap and loadedAt are always set together in
			// Refresh) but fail closed rather than assume freshness.
			return Snapshot{}, false
		}
		age := s.clock().Sub(*loadedAt)
		if age > s.maxStaleness {
			if !s.stale.Swap(true) {
				log.Printf("policy: WARNING snapshot age %s exceeds POLICY_MAX_STALENESS_SECONDS (%s) — refusing chat completions with 503 policy_unavailable until a refresh succeeds", age, s.maxStaleness)
			}
			return Snapshot{}, false
		}
	}

	if s.stale.Swap(false) {
		log.Printf("policy: snapshot is fresh again — resuming normal service")
	}
	return *p, true
}

// Refresh loads a fresh snapshot and swaps it in on success. On failure it
// logs loudly and leaves the previous snapshot in place — or the unloaded
// fail-closed state, if this is the first attempt.
func (s *PgStore) Refresh(ctx context.Context) error {
	snap, err := s.loadFn(ctx)
	if err != nil {
		// Check the raw snapshot pointer, not Current() — a snapshot that has
		// gone stale (see Current) was still, historically, successfully
		// loaded, and the log message below should say so rather than
		// implying no load has ever succeeded.
		if s.snap.Load() != nil {
			log.Printf("policy: WARNING refresh failed, keeping last-good snapshot: %v", err)
		} else {
			log.Printf("policy: WARNING no policy snapshot loaded yet — requests will 503 policy_unavailable until this succeeds: %v", err)
		}
		return err
	}
	s.snap.Store(&snap)
	now := s.clock()
	s.loadedAt.Store(&now)
	return nil
}

// Status is a point-in-time diagnostic snapshot of the store's freshness, for
// the /health endpoint. Unlike Current, it never fails closed — it reports
// what IS true, so operators can see "loaded but stale" as distinct from
// "never loaded", rather than both collapsing to the same 503 behaviour.
type Status struct {
	// Loaded is true once any Refresh has ever succeeded, regardless of
	// current staleness.
	Loaded bool
	// AgeSeconds is the age of the last successful load, or nil if Loaded is
	// false.
	AgeSeconds *int64
	// Stale is true when AgeSeconds exceeds maxStaleness (always false when
	// the cap is disabled or the store has never loaded).
	Stale bool
}

// Status reports the store's current freshness for /health.
func (s *PgStore) Status() Status {
	if s.snap.Load() == nil {
		return Status{Loaded: false}
	}
	loadedAt := s.loadedAt.Load()
	if loadedAt == nil {
		return Status{Loaded: true}
	}
	age := int64(s.clock().Sub(*loadedAt).Seconds())
	return Status{
		Loaded:     true,
		AgeSeconds: &age,
		Stale:      s.maxStaleness > 0 && time.Duration(age)*time.Second > s.maxStaleness,
	}
}

// Run performs an initial synchronous Refresh, then refreshes every interval
// in a goroutine tied to ctx until ctx is done.
func (s *PgStore) Run(ctx context.Context, interval time.Duration) {
	_ = s.Refresh(ctx)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.Refresh(ctx)
			}
		}
	}()
}

// loadFromDB runs the 3 SELECTs and assembles a Snapshot.
func (s *PgStore) loadFromDB(ctx context.Context) (Snapshot, error) {
	enabled := make(map[string]bool)
	rows, err := s.pool.Query(ctx, `SELECT model_id, enabled FROM models`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("policy: query models: %w", err)
	}
	for rows.Next() {
		var id string
		var en bool
		if err := rows.Scan(&id, &en); err != nil {
			rows.Close()
			return Snapshot{}, fmt.Errorf("policy: scan models row: %w", err)
		}
		enabled[id] = en
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Snapshot{}, fmt.Errorf("policy: read models rows: %w", err)
	}
	rows.Close()

	aliases := make(map[string]string)
	aliasRows, err := s.pool.Query(ctx, `SELECT alias, target_model_id FROM model_aliases`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("policy: query model_aliases: %w", err)
	}
	for aliasRows.Next() {
		var alias, target string
		if err := aliasRows.Scan(&alias, &target); err != nil {
			aliasRows.Close()
			return Snapshot{}, fmt.Errorf("policy: scan model_aliases row: %w", err)
		}
		aliases[alias] = target
	}
	if err := aliasRows.Err(); err != nil {
		aliasRows.Close()
		return Snapshot{}, fmt.Errorf("policy: read model_aliases rows: %w", err)
	}
	aliasRows.Close()

	var residency string
	if err := s.pool.QueryRow(ctx, `SELECT residency FROM routing_policy WHERE id = true`).Scan(&residency); err != nil {
		return Snapshot{}, fmt.Errorf("policy: query routing_policy: %w", err)
	}

	return Snapshot{Enabled: enabled, Aliases: aliases, Residency: residency}, nil
}

// compile-time interface check.
var _ Store = (*PgStore)(nil)
