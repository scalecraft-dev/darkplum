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
type PgStore struct {
	pool *pgxpool.Pool

	// loadFn performs the actual load. Defaults to loadFromDB; overridable in
	// tests (same package) so refresh/last-good behaviour can be exercised
	// without a real database.
	loadFn func(ctx context.Context) (Snapshot, error)

	snap atomic.Pointer[Snapshot]
}

// NewPgStore constructs a PgStore backed by pool. The store is unloaded
// (fail-closed) until Refresh or Run is called.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	s := &PgStore{pool: pool}
	s.loadFn = s.loadFromDB
	return s
}

// Current implements Store.
func (s *PgStore) Current() (Snapshot, bool) {
	p := s.snap.Load()
	if p == nil {
		return Snapshot{}, false
	}
	return *p, true
}

// Refresh loads a fresh snapshot and swaps it in on success. On failure it
// logs loudly and leaves the previous snapshot in place — or the unloaded
// fail-closed state, if this is the first attempt.
func (s *PgStore) Refresh(ctx context.Context) error {
	snap, err := s.loadFn(ctx)
	if err != nil {
		if _, ok := s.Current(); ok {
			log.Printf("policy: WARNING refresh failed, keeping last-good snapshot: %v", err)
		} else {
			log.Printf("policy: WARNING no policy snapshot loaded yet — requests will 503 policy_unavailable until this succeeds: %v", err)
		}
		return err
	}
	s.snap.Store(&snap)
	return nil
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
