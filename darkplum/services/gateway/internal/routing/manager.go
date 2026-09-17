package routing

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Source discovers the model ids currently available from one backend
// instance (a single vLLM upstream, LM Studio, Bedrock, or the mock
// provider). Manager.Refresh calls Discover on every registered Source each
// tick and merges the results according to the collision rule in resolve.
//
// A Source is deliberately a plain struct rather than an interface: the
// per-backend behaviour (HTTP probe, Bedrock SDK call, static list) lives
// entirely inside the Discover closure, which keeps fakes for tests trivial
// (no network, no DB — just a function literal).
type Source struct {
	// Name uniquely identifies this source for logging and last-good
	// tracking (e.g. "vllm-default", "vllm-extra1", "lmstudio-dev", "bedrock", "mock").
	Name string
	// Kind is the backend kind written to audit records and /v1/models
	// owned_by ("vllm", "lmstudio", "bedrock", "mock").
	Kind string
	// Class is ClassSelfHosted or ClassCloudManaged.
	Class string
	// ProviderKey is the key this source's models route to in the handler's
	// provider registry.
	ProviderKey string
	// Discover returns the current set of model ids served by this source.
	// An error is non-fatal to the table refresh: Manager keeps the source's
	// last-good list and logs loudly. A source that has never returned a
	// successful result contributes nothing to the table.
	Discover func(ctx context.Context) ([]string, error)
}

// Manager owns periodic discovery across all configured Sources and exposes
// the current merged Table via Snapshot. Safe for concurrent use.
type Manager struct {
	sources []Source

	table atomic.Pointer[Table]

	mu         sync.Mutex
	lastGood   map[string][]string // source name -> last successful id list
	prevWinner map[string]string   // model id -> winning source name as of the last resolve, for collision-change logging
}

// NewManager builds a Manager for the given sources. Registration order in
// sources is significant: it is the tie-break among self-hosted sources that
// claim the same model id. The returned Manager's Table is empty until the
// first call to Refresh or Run.
func NewManager(sources []Source) *Manager {
	m := &Manager{
		sources:    sources,
		lastGood:   make(map[string][]string),
		prevWinner: make(map[string]string),
	}
	m.table.Store(NewTable(nil))
	return m
}

// Snapshot returns the current route table. Safe to call from any goroutine;
// never blocks on a refresh in progress.
func (m *Manager) Snapshot() *Table {
	return m.table.Load()
}

// Refresh calls Discover on every source, retains each source's last-good
// list on failure, resolves collisions, and atomically swaps in the new
// Table. Safe to call concurrently, though normal use is single-goroutine
// (see Run).
func (m *Manager) Refresh(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, src := range m.sources {
		ids, err := src.Discover(ctx)
		if err != nil {
			log.Printf("routing: %s: discovery failed, keeping last-good (%d models): %v",
				src.Name, len(m.lastGood[src.Name]), err)
			continue
		}
		m.lastGood[src.Name] = ids
	}

	routes, winners := m.resolveLocked()
	m.prevWinner = winners
	m.table.Store(NewTable(routes))
}

// resolveLocked rebuilds the route table from the current last-good lists,
// applying the collision rule: self_hosted beats cloud_managed; among
// self-hosted sources, earlier registration order (index into m.sources)
// wins. It also logs a WARNING naming every source involved whenever the
// winner for a contested model id changes from the previous refresh. Must be
// called with m.mu held.
func (m *Manager) resolveLocked() (map[string]Route, map[string]string) {
	// claimants[id] holds the registration-order indices of every source
	// currently claiming id, built by iterating m.sources in order so the
	// slice is already sorted by registration order.
	claimants := make(map[string][]int)
	for i, src := range m.sources {
		for _, id := range m.lastGood[src.Name] {
			claimants[id] = append(claimants[id], i)
		}
	}

	routes := make(map[string]Route, len(claimants))
	winners := make(map[string]string, len(claimants))

	for id, idxs := range claimants {
		winner := idxs[0]
		for _, idx := range idxs {
			if m.sources[idx].Class == ClassSelfHosted {
				winner = idx
				break
			}
		}

		src := m.sources[winner]
		routes[id] = Route{ProviderKey: src.ProviderKey, Kind: src.Kind, Class: src.Class}
		winners[id] = src.Name

		if len(idxs) > 1 {
			if prev, existed := m.prevWinner[id]; !existed || prev != src.Name {
				var claimantNames []string
				for _, idx := range idxs {
					claimantNames = append(claimantNames, m.sources[idx].Name)
				}
				log.Printf("routing: WARNING model id %q claimed by multiple sources (%s); %q wins",
					id, strings.Join(claimantNames, ", "), src.Name)
			}
		}
	}

	return routes, winners
}

// Run performs an initial synchronous Refresh so the first Table reflects
// startup state, then refreshes every interval in a goroutine tied to ctx
// until ctx is done (e.g. on SIGTERM). Call once at startup.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	m.Refresh(ctx)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.Refresh(ctx)
			}
		}
	}()
}
