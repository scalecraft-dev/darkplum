// Package policy loads and caches the gateway's routing-enforcement inputs —
// which models are enabled, aliases, and the org-wide residency policy — from
// Postgres. The gateway never queries these tables per-request: Store.Current
// returns the last successfully loaded Snapshot from an in-memory cache kept
// fresh by a background refresher (PgStore.Run).
//
// Fail-closed semantics: until the first successful load, Current reports
// "not loaded" and the handler must refuse every request with 503
// policy_unavailable. After a first success, a later refresh failure keeps
// serving the last-good snapshot (logged loudly) rather than failing closed —
// a transient DB blip must not take down inference traffic that policy
// already approved.
package policy

// Residency values for routing_policy.residency.
const (
	ResidencyAny            = "any"
	ResidencySelfHostedOnly = "self_hosted_only"
)

// Snapshot is the routing-enforcement policy in effect at a point in time.
type Snapshot struct {
	// Enabled maps model_id -> models.enabled. A model absent from this map
	// (not yet synced by the api) is treated as not enabled.
	Enabled map[string]bool
	// Aliases maps alias -> target_model_id. Never chained: a target is
	// looked up as-is, not re-resolved through Aliases again.
	Aliases map[string]string
	// Residency is ResidencyAny or ResidencySelfHostedOnly.
	Residency string
}

// Store exposes the current policy snapshot to request handlers.
// Implementations must be safe for concurrent use and must never perform
// per-request I/O — the handler calls Current() on every request.
type Store interface {
	// Current returns the latest successfully loaded snapshot and true, or a
	// zero Snapshot and false if no load has ever succeeded. Callers must
	// fail closed (503 policy_unavailable) when the second return value is
	// false.
	Current() (Snapshot, bool)
}
