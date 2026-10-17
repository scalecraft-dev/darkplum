// Package routing builds and maintains the gateway's model-id -> backend
// route table. The table is assembled from one or more discovery Sources
// (vLLM upstreams, LM Studio, Bedrock, mock) and refreshed periodically by a
// Manager. Handlers read an immutable Table snapshot per request; they never
// block on or trigger a refresh themselves.
package routing

import "sort"

// Backend classes. These mirror models.backend_class in the control-plane
// schema (migrations 010 and, for ClassExternalAPI, 016) and drive both the
// collision precedence in manager.go's resolveLocked and the residency
// enforcement check in policy.ResidencyPermits: self_hosted beats
// cloud_managed beats external_api on id collision, and a residency policy
// tighter than "any" blocks routes in rank order (self_hosted_only permits
// only self_hosted; cloud_account_only additionally permits cloud_managed;
// external_api requires "any").
const (
	ClassSelfHosted   = "self_hosted"
	ClassCloudManaged = "cloud_managed"
	// ClassExternalAPI is a direct vendor API backend (Anthropic, OpenAI) —
	// the model runs entirely outside the customer's cloud account. See
	// policy.ResidencyPermits.
	ClassExternalAPI = "external_api"
)

// classRank orders backend classes for the collision-precedence rule in
// manager.go's resolveLocked: when more than one source claims the same
// model id, the lowest-ranked (most "ours") class wins — self_hosted (0)
// beats cloud_managed (1) beats external_api (2). Registration order (the
// order sources were passed to NewManager) breaks ties within the same rank;
// see resolveLocked's doc comment.
var classRank = map[string]int{
	ClassSelfHosted:   0,
	ClassCloudManaged: 1,
	ClassExternalAPI:  2,
}

// rankOfClass returns class's collision rank, or a rank worse than any known
// class if class is unrecognised. Defensive only: every Source registers
// with one of the ClassX constants above, so this fallback should never be
// exercised in practice.
func rankOfClass(class string) int {
	if r, ok := classRank[class]; ok {
		return r
	}
	return len(classRank)
}

// Route describes how a single resolved model id is served.
type Route struct {
	// ProviderKey is the key this model routes to in the caller's provider
	// registry (e.g. "vllm-default", "vllm-extra1", "lmstudio-dev", "bedrock", "mock").
	ProviderKey string
	// Kind is the backend kind written to audit records and the /v1/models
	// owned_by field ("vllm", "lmstudio", "bedrock", "mock").
	Kind string
	// Class is ClassSelfHosted or ClassCloudManaged.
	Class string
}

// Table is an immutable snapshot of model-id -> Route. A new Table is built
// and atomically swapped in by Manager on every refresh rather than mutated
// in place, so concurrent readers never observe a partially-updated table.
type Table struct {
	routes map[string]Route
	ids    []string // sorted once at construction
}

// NewTable builds a Table from a model-id -> Route map. Exported so tests
// (and Manager) can construct a Table directly without going through the
// discovery machinery.
func NewTable(routes map[string]Route) *Table {
	ids := make([]string, 0, len(routes))
	for id := range routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Copy the map so the caller cannot mutate a Table after construction.
	cp := make(map[string]Route, len(routes))
	for id, r := range routes {
		cp[id] = r
	}

	return &Table{routes: cp, ids: ids}
}

// Lookup returns the Route registered for id and whether it was found.
func (t *Table) Lookup(id string) (Route, bool) {
	r, ok := t.routes[id]
	return r, ok
}

// IDs returns every model id currently in the table, sorted, regardless of
// whether the id is enabled in policy. GET /v1/models uses this so the api's
// SyncModels sees the full discovered set.
func (t *Table) IDs() []string {
	out := make([]string, len(t.ids))
	copy(out, t.ids)
	return out
}
