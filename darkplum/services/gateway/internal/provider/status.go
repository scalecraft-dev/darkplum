package provider

import (
	"sort"
	"sync"
	"time"
)

// Provider status states. Closed vocabulary — see the wire contract in
// DARKPLUM_IMPLEMENTATION.md's "Gateway GET /v1/models" section. Every vendor
// provider (anthropic, openai) reports exactly one of these at all times.
const (
	// ProviderStateNotConfigured is the state a vendor reports until its own
	// key/filter configuration exists AND its Source has completed a first
	// discovery attempt. This is the only state a gateway build with no
	// anthropic/openai package wired in yet can ever report — see
	// NewStatusRegistry.
	ProviderStateNotConfigured = "not_configured"
	// ProviderStateOK means the vendor's most recent discovery succeeded and
	// returned at least one routable model.
	ProviderStateOK = "ok"
	// ProviderStateFilterMissing means credentials are present but no
	// ANTHROPIC_MODEL_FILTER/OPENAI_MODEL_FILTER was set — "no filter => no
	// vendor models" is deliberate (see the plan's failure-isolation ruling),
	// not a fatal error.
	ProviderStateFilterMissing = "filter_missing"
	// ProviderStateFilterMatchedNothing means a filter was set but matched no
	// model the vendor's API currently reports.
	ProviderStateFilterMatchedNothing = "filter_matched_nothing"
	// ProviderStateAuthFailed means the vendor's API rejected the configured
	// key (invalid, revoked, or insufficiently scoped).
	ProviderStateAuthFailed = "auth_failed"
	// ProviderStateUnreachable means discovery could not reach the vendor's
	// API at the transport level (DNS/dial/timeout).
	ProviderStateUnreachable = "unreachable"
)

// ProviderStatus is one vendor's discovery/configuration status, reported on
// GET /v1/models (service-key + X-Darkplum-Classes: external_api callers
// only) and passed through by the api's admin models list unchanged. See the
// wire contract's `providers` array shape.
//
// Filter/FilterSource/EnvFilterSetAt exist so the admin UI's "install a
// vendor key" flow can show where the currently-effective model filter came
// from (a UI-installed DB row vs the <VENDOR>_MODEL_FILTER/_SET_AT env pair)
// and offer "Adopt filter" when only the env side is set — see
// routing.vendorFilterFunc. These three fields are ADMIN-ONLY detail: GET
// /health reports a separate, reduced DTO (see cmd/main.go's
// healthProviderStatus) that never includes them, since /health is
// unauthenticated.
type ProviderStatus struct {
	Name       string     `json:"name"`
	Configured bool       `json:"configured"`
	State      string     `json:"state"`
	ModelCount int        `json:"model_count"`
	LastSyncAt *time.Time `json:"last_sync_at"`
	// FilterSource is "db" (a vendor_provider_settings row currently wins),
	// "env" (the <VENDOR>_MODEL_FILTER/_SET_AT pair wins, including when it
	// is the only one configured), or "none" (neither is configured).
	FilterSource string `json:"filter_source"`
	// Filter is the winning filter's glob patterns — nil when FilterSource
	// is "none".
	Filter []string `json:"filter"`
	// EnvFilterSetAt is the parsed <VENDOR>_MODEL_FILTER_SET_AT env value,
	// regardless of which source is currently winning, or nil if that env
	// var is unset/unparseable. Lets the UI offer "Adopt filter" (a DB write
	// of the env's own value) even while FilterSource is "env".
	EnvFilterSetAt *time.Time `json:"env_filter_set_at"`
	// WorkspaceID is the effective Anthropic workspace id (sent as the
	// anthropic-workspace-id header), "" when none. It rides on the same
	// last-writer-wins source as Filter (FilterSource covers both). Not a
	// secret; ADMIN-ONLY like the filter fields (never on /health).
	WorkspaceID string `json:"workspace_id"`
}

// StatusRegistry is a concurrency-safe store of the current ProviderStatus
// for every vendor provider. A vendor's own discovery Source is the only
// writer (via Report, called once per discovery tick, successful or not);
// GET /v1/models is the reader (via Snapshot). This lives in internal/provider
// rather than internal/routing because it is a provider-configuration/health
// concern (parallel to Regional/APIError above), not a routing-table concern
// — a vendor can be "not_configured" or "auth_failed" while contributing zero
// routes, which routing.Manager's Source/Status types have no room to say.
type StatusRegistry struct {
	mu       sync.RWMutex
	statuses map[string]ProviderStatus
}

// NewStatusRegistry builds a registry seeded with names, each starting at
// ProviderStateNotConfigured/Configured=false/ModelCount=0 — the state every
// vendor reports until its own Source's discovery loop calls Report for the
// first time. cmd/main.go seeds this with "anthropic" and "openai" at
// startup, before either provider package exists, so GET /v1/models always
// lists both vendors (the wire contract's "Always lists both vendors") even
// on a build that has wired in neither yet.
func NewStatusRegistry(names ...string) *StatusRegistry {
	statuses := make(map[string]ProviderStatus, len(names))
	for _, n := range names {
		statuses[n] = ProviderStatus{Name: n, State: ProviderStateNotConfigured}
	}
	return &StatusRegistry{statuses: statuses}
}

// Report records name's current status, overwriting whatever was reported
// before. name is forced onto the stored value regardless of what st.Name
// was set to, so a caller can never mis-attribute a status by typo. Safe for
// concurrent use; called by a vendor Source's discovery closure on every
// tick.
func (r *StatusRegistry) Report(name string, st ProviderStatus) {
	st.Name = name
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses[name] = st
}

// Snapshot returns every registered provider's current status, sorted by
// name for a stable wire response. Never nil (an empty registry returns a
// non-nil empty slice), so it marshals as `[]`, not `null`.
func (r *StatusRegistry) Snapshot() []ProviderStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderStatus, 0, len(r.statuses))
	for _, st := range r.statuses {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
