package routing

import (
	"context"
	"fmt"
	"log"
	"sort"
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

// defaultSourceTimeout bounds how long a single source's Discover call may
// run during a Refresh when no more specific timeout has been configured via
// Run. Refresh runs every source concurrently (see Refresh), so one wedged
// source cannot serialise behind the others — but it still must not be
// allowed to run forever and delay the merge indefinitely.
const defaultSourceTimeout = 30 * time.Second

// SourceStatus is a point-in-time diagnostic view of one source, for the
// /health endpoint. It deliberately carries a model COUNT, never model ids —
// /health is unauthenticated.
type SourceStatus struct {
	// Models is the number of ids this source currently contributes to the
	// route table (0 if it has never succeeded, or if its last-good list has
	// expired per lastGoodTTL).
	Models int
	// LastSuccessSecondsAgo is how long ago this source's last successful
	// discovery completed, or nil if it has never succeeded.
	LastSuccessSecondsAgo *int64
	// LastErrorKind classifies the most recent failed discovery into one of a
	// small fixed set safe to expose on the unauthenticated /health endpoint
	// (see classifyError), or "" if the last attempt succeeded (or none has
	// run yet). The raw error — which may contain upstream hostnames or
	// internal detail — is never exposed here; it is only ever logged, by
	// Manager.Refresh.
	LastErrorKind string
}

// Status is the aggregate routing health snapshot returned by
// Manager.Status.
type Status struct {
	// Models is the total number of routable model ids in the current table.
	Models int
	// Sources maps source name -> SourceStatus.
	Sources map[string]SourceStatus
}

// Manager owns periodic discovery across all configured Sources and exposes
// the current merged Table via Snapshot. Safe for concurrent use.
type Manager struct {
	sources []Source

	// lastGoodTTL caps how long a source's last-good model list stays
	// routable after discovery starts failing for it. Zero disables the cap
	// (last-good is retained forever, the original behaviour) — used by
	// tests that don't exercise TTL expiry and don't advance time.
	lastGoodTTL time.Duration

	// sourceTimeout bounds each individual source's Discover call within a
	// single Refresh. Defaults to defaultSourceTimeout; Run derives it from
	// the discovery interval.
	sourceTimeout time.Duration

	// now is an injectable clock for tests. Defaults to time.Now via clock()
	// when nil.
	now func() time.Time

	table atomic.Pointer[Table]

	// refreshing guards against overlapping Refresh calls: if the discovery
	// interval ticks again before the previous Refresh (bounded, per source,
	// by sourceTimeout, but still not instantaneous) has finished, the new
	// tick is skipped rather than piling up concurrent discovery storms.
	refreshing atomic.Bool

	mu          sync.Mutex
	lastGood    map[string][]string  // source name -> last successful id list
	lastSuccess map[string]time.Time // source name -> time of last successful discovery
	lastErr     map[string]error     // source name -> most recent discovery error (nil once it succeeds)
	expired     map[string]bool      // source name -> currently past lastGoodTTL with no recovery
	prevWinner  map[string]string    // model id -> winning source name as of the last resolve, for collision-change and route-change logging
}

// NewManager builds a Manager for the given sources. Registration order in
// sources is significant: it is the tie-break among self-hosted sources that
// claim the same model id. lastGoodTTL is the ROUTE_LAST_GOOD_TTL_SECONDS cap
// (0 disables it). The returned Manager's Table is empty until the first call
// to Refresh or Run.
func NewManager(sources []Source, lastGoodTTL time.Duration) *Manager {
	m := &Manager{
		sources:       sources,
		lastGoodTTL:   lastGoodTTL,
		sourceTimeout: defaultSourceTimeout,
		lastGood:      make(map[string][]string),
		lastSuccess:   make(map[string]time.Time),
		lastErr:       make(map[string]error),
		expired:       make(map[string]bool),
		prevWinner:    make(map[string]string),
	}
	m.table.Store(NewTable(nil))
	return m
}

// clock returns the current time via the injectable now field, or time.Now
// if unset.
func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// Snapshot returns the current route table. Safe to call from any goroutine;
// never blocks on a refresh in progress.
func (m *Manager) Snapshot() *Table {
	return m.table.Load()
}

// Status returns a diagnostic snapshot of routing health for /health. Safe
// for concurrent use.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	sources := make(map[string]SourceStatus, len(m.sources))
	for _, src := range m.sources {
		models := 0
		if !m.expired[src.Name] {
			models = len(m.lastGood[src.Name])
		}

		var lastSuccessAgo *int64
		if t, ok := m.lastSuccess[src.Name]; ok && !t.IsZero() {
			secs := int64(now.Sub(t).Seconds())
			lastSuccessAgo = &secs
		}

		sources[src.Name] = SourceStatus{
			Models:                models,
			LastSuccessSecondsAgo: lastSuccessAgo,
			LastErrorKind:         classifyError(m.lastErr[src.Name]),
		}
	}

	return Status{
		Models:  len(m.table.Load().IDs()),
		Sources: sources,
	}
}

// discoverResult carries one source's Discover outcome back to the collecting
// goroutine, tagged with its registration-order index so results can be
// reassembled in order regardless of completion order.
type discoverResult struct {
	idx int
	ids []string
	err error
}

// Refresh calls Discover on every source CONCURRENTLY (each bounded by
// sourceTimeout derived from ctx), retains each source's last-good list on
// failure (until lastGoodTTL, if set, expires it), resolves collisions, and
// atomically swaps in the new Table. If a Refresh is already in progress, a
// concurrent call is skipped (logged) rather than piling up.
func (m *Manager) Refresh(ctx context.Context) {
	if !m.refreshing.CompareAndSwap(false, true) {
		log.Printf("routing: refresh already in progress, skipping this tick")
		return
	}
	defer m.refreshing.Store(false)

	results := m.discoverAllConcurrently(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock()
	for i, src := range m.sources {
		r := results[i]
		if r.err != nil {
			m.lastErr[src.Name] = r.err
			log.Printf("routing: %s: discovery failed, keeping last-good (%d models): %v",
				src.Name, len(m.lastGood[src.Name]), r.err)
			continue
		}
		m.lastErr[src.Name] = nil
		m.lastGood[src.Name] = r.ids
		m.lastSuccess[src.Name] = now
	}

	m.updateExpiryLocked(now)

	routes, winners := m.resolveLocked()
	m.logRouteChangesLocked(routes, winners)
	m.prevWinner = winners
	m.table.Store(NewTable(routes))
}

// discoverAllConcurrently runs Discover on every source in its own goroutine,
// each bounded by a context derived from ctx with sourceTimeout, and returns
// results indexed by registration order (m.sources[i] <-> results[i]).
func (m *Manager) discoverAllConcurrently(ctx context.Context) []discoverResult {
	resultsCh := make(chan discoverResult, len(m.sources))

	for i, src := range m.sources {
		go func(i int, src Source) {
			srcCtx, cancel := context.WithTimeout(ctx, m.sourceTimeout)
			defer cancel()
			ids, err := src.Discover(srcCtx)
			resultsCh <- discoverResult{idx: i, ids: ids, err: err}
		}(i, src)
	}

	results := make([]discoverResult, len(m.sources))
	for range m.sources {
		r := <-resultsCh
		results[r.idx] = r
	}
	return results
}

// updateExpiryLocked recomputes, for every source, whether its last-good list
// has exceeded lastGoodTTL with no successful discovery since, and logs a
// WARNING once on entering that state and once on recovering from it. A
// source that has never succeeded is never considered expired (it already
// contributes nothing). Must be called with m.mu held, after lastGood/
// lastSuccess/lastErr have been updated for this tick.
func (m *Manager) updateExpiryLocked(now time.Time) {
	if m.lastGoodTTL <= 0 {
		return
	}
	for _, src := range m.sources {
		lastOK, everSucceeded := m.lastSuccess[src.Name]
		isExpired := everSucceeded && !lastOK.IsZero() && now.Sub(lastOK) > m.lastGoodTTL
		wasExpired := m.expired[src.Name]

		if isExpired && !wasExpired {
			log.Printf("routing: WARNING %s: no successful discovery for over %s (last success %s ago) — its %d last-good models are leaving the route table",
				src.Name, m.lastGoodTTL, now.Sub(lastOK), len(m.lastGood[src.Name]))
		} else if !isExpired && wasExpired {
			log.Printf("routing: %s: recovered — %d models restored to the route table",
				src.Name, len(m.lastGood[src.Name]))
		}
		m.expired[src.Name] = isExpired
	}
}

// resolveLocked rebuilds the route table from the current last-good lists,
// applying the collision rule: self_hosted beats cloud_managed; among
// self-hosted sources, earlier registration order (index into m.sources)
// wins. Sources currently past lastGoodTTL (m.expired) contribute nothing. It
// also logs a WARNING naming every source involved whenever the winner for a
// contested model id changes from the previous refresh. Must be called with
// m.mu held.
func (m *Manager) resolveLocked() (map[string]Route, map[string]string) {
	// claimants[id] holds the registration-order indices of every source
	// currently claiming id, built by iterating m.sources in order so the
	// slice is already sorted by registration order.
	claimants := make(map[string][]int)
	for i, src := range m.sources {
		if m.expired[src.Name] {
			continue
		}
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

// logRouteChangesLocked compares the newly resolved winners against
// m.prevWinner (the previous refresh's winners, not yet overwritten by the
// caller) and logs one line naming every model id added to or removed from
// the routable set, with the source that served/serves it. Nothing is logged
// when the routable set is unchanged. Must be called with m.mu held, before
// m.prevWinner is reassigned.
func (m *Manager) logRouteChangesLocked(_ map[string]Route, winners map[string]string) {
	var added, removed []string

	for id, src := range winners {
		if _, existed := m.prevWinner[id]; !existed {
			added = append(added, fmt.Sprintf("%s(%s)", id, src))
		}
	}
	for id, src := range m.prevWinner {
		if _, stillExists := winners[id]; !stillExists {
			removed = append(removed, fmt.Sprintf("%s(%s)", id, src))
		}
	}

	if len(added) == 0 && len(removed) == 0 {
		return
	}
	sort.Strings(added)
	sort.Strings(removed)
	log.Printf("routing: route table changed: added=[%s] removed=[%s]",
		strings.Join(added, ", "), strings.Join(removed, ", "))
}

// Run performs an initial synchronous Refresh so the first Table reflects
// startup state, then refreshes every interval in a goroutine tied to ctx
// until ctx is done (e.g. on SIGTERM). Call once at startup. The per-source
// discovery timeout is derived from interval (capped at defaultSourceTimeout)
// so a single wedged source cannot run past the next scheduled tick.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval > 0 && interval < m.sourceTimeout {
		m.sourceTimeout = interval
	}

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
