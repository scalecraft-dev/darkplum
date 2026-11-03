package routing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
	"github.com/scalecraft-dev/darkplum/gateway/internal/provider/credsource"
)

// vendorCredentialChecker is the subset of *credsource.Source every direct
// vendor API Source (anthropic, openai) depends on. Named per-vendor
// interfaces (anthropicCredentialChecker, openaiCredentialChecker) alias this
// shape in their own files for doc clarity; Go's structural typing lets a
// concrete *credsource.Source (or a test fake) satisfy either without any
// explicit conversion.
type vendorCredentialChecker interface {
	Key() (key string, version uint64, err error)
}

var _ vendorCredentialChecker = (*credsource.Source)(nil)

// vendorDiscoverer is the subset of a vendor provider (anthropic.Provider,
// openai.Provider) every vendor Source depends on.
type vendorDiscoverer interface {
	provider.Provider
	DiscoverModels(ctx context.Context) (map[string]bool, error)
}

// vendorStatusReporter is the subset of *provider.StatusRegistry a vendor
// Source needs — just Report, unlike provider.AuthFailureReporter (which
// also reads Snapshot to preserve fields across a request-time report at the
// provider package level).
type vendorStatusReporter interface {
	Report(name string, st provider.ProviderStatus)
}

var _ vendorStatusReporter = (*provider.StatusRegistry)(nil)

// vendorFilterPolicySource is the minimal read NewVendorFilterFunc needs from
// the live policy snapshot's DB-mirrored vendor_provider_settings row,
// decoupled from the policy package itself: internal/policy already imports
// this package (for residency/backend-class checks — see effective.go), so
// importing policy here would create a cycle. cmd/main.go adapts
// *policy.PgStore to this interface with a small wrapper reading
// Snapshot.VendorFilters.
type vendorFilterPolicySource interface {
	// VendorFilter returns vendor's DB-stored filter row (filter patterns and
	// the timestamp they were set at) and whether a live, loaded snapshot has
	// one — false when the snapshot hasn't loaded, the table doesn't exist
	// yet, or the vendor has never been installed from the UI.
	// workspaceID is the row's Anthropic workspace id ("" when none); it is
	// read in the same call so it can never come from a different snapshot
	// than the filter and stamp it rides with.
	VendorFilter(vendor string) (filter []string, setAt time.Time, workspaceID string, ok bool)
}

// vendorFilterResult is what a vendorFilterFunc resolves to on one discovery
// tick: which glob patterns are currently in effect, where they came from,
// and (regardless of which source won) the env pair's own SET_AT — reported
// on provider.ProviderStatus as filter/filter_source/env_filter_set_at so
// the admin UI can compare a DB filter's timestamp against the env's even
// when the DB filter isn't the one currently winning.
type vendorFilterResult struct {
	// Patterns is the winning filter's glob patterns — nil when Source is
	// "none".
	Patterns []string
	// Source is "db" (a vendor_provider_settings row won), "env" (the
	// <VENDOR>_MODEL_FILTER/_SET_AT pair won, including when it is the only
	// one configured at all), or "none" (neither is configured).
	Source string
	// EnvSetAt is the env <VENDOR>_MODEL_FILTER_SET_AT value, parsed at
	// startup, or the zero time if it was unset/unparseable — see
	// config.Load's parseVendorFilterSetAt.
	EnvSetAt time.Time
	// WorkspaceID is the effective Anthropic workspace id ("" for none). It
	// follows the SAME winner as Patterns: the DB row's value when Source is
	// "db" (empty there means explicitly cleared), the env value otherwise.
	WorkspaceID string
}

// vendorFilterFunc resolves one vendor's EFFECTIVE model filter, evaluated
// fresh on every discovery tick (see vendorSourceState.discover) rather than
// fixed at startup, so a filter change made from the UI (or via
// vendor_provider_settings/the ConfigMap directly) takes effect on the next
// tick with no gateway restart.
type vendorFilterFunc func() vendorFilterResult

// resolveVendorFilter implements the [RULING] last-writer-wins rule between
// a DB-stored vendor filter and the env-configured pair: the DB row wins iff
// it exists AND its filter_set_at is >= envSetAt (a missing/unparseable env
// SET_AT is the zero time, so ANY stamped DB row beats an unstamped env
// value); otherwise the env filter wins — which also covers "no DB row at
// all" and "the DB row is now older than a later env write" (last-writer-wins
// in BOTH directions, per the plan). "none" is reported only when neither
// side is configured at all.
func resolveVendorFilter(dbFilter []string, dbSetAt time.Time, dbOK bool, envFilter []string, envSetAt time.Time) vendorFilterResult {
	if dbOK && !dbSetAt.Before(envSetAt) {
		return vendorFilterResult{Patterns: dbFilter, Source: "db", EnvSetAt: envSetAt}
	}
	if !dbOK && len(envFilter) == 0 {
		return vendorFilterResult{Source: "none", EnvSetAt: envSetAt}
	}
	return vendorFilterResult{Patterns: envFilter, Source: "env", EnvSetAt: envSetAt}
}

// sameGlobSet reports whether a and b contain the same glob patterns,
// ignoring order — used only to detect the hand-edited-ConfigMap warning
// case in NewVendorFilterFunc (identical timestamps, different values).
func sameGlobSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// NewVendorFilterFunc builds the vendorFilterFunc cmd/main.go wires into
// NewAnthropicSource/NewOpenAISource. vendor is "anthropic" or "openai" —
// both the policy.Snapshot.VendorFilters key and the ProviderStatus name.
// envFilter/envSetAt are read once at startup (an env var never changes
// without a restart); polStore supplies the live snapshot on every call, so
// a UI-driven DB write is picked up on the very next discovery tick.
//
// Logs one WARNING per distinct (timestamp, db value, env value) combination
// when the DB and env stamps are EXACTLY equal but the filter values differ
// — this can only happen if a ConfigMap was hand-edited to copy a
// filter_set_at without copying the filter itself, since every real writer
// (the UI's PUT, and `darkplum keys set`) stamps the pair it wrote together.
func NewVendorFilterFunc(vendor string, envFilter []string, envSetAt time.Time, envWorkspaceID string, polStore vendorFilterPolicySource) vendorFilterFunc {
	var warnMu sync.Mutex
	var lastWarnKey string

	return func() vendorFilterResult {
		dbFilter, dbSetAt, dbWorkspaceID, dbOK := polStore.VendorFilter(vendor)

		if dbOK && dbSetAt.Equal(envSetAt) && !sameGlobSet(dbFilter, envFilter) {
			key := fmt.Sprintf("%s|%v|%v", dbSetAt.Format(time.RFC3339Nano), dbFilter, envFilter)
			warnMu.Lock()
			alreadyWarned := lastWarnKey == key
			lastWarnKey = key
			warnMu.Unlock()
			if !alreadyWarned {
				log.Printf("routing: WARNING %s: DB and env model filters carry the identical timestamp %s but different values (db=%v env=%v) — a hand-edited ConfigMap; the DB value wins",
					vendor, dbSetAt.Format(time.RFC3339), dbFilter, envFilter)
			}
		}

		res := resolveVendorFilter(dbFilter, dbSetAt, dbOK, envFilter, envSetAt)
		if res.Source == "db" {
			res.WorkspaceID = dbWorkspaceID
		} else {
			res.WorkspaceID = envWorkspaceID
		}
		return res
	}
}

// vendorSourceConfig holds everything the shared six-state discovery
// skeleton (vendorSourceState.discover) needs for one vendor.
type vendorSourceConfig struct {
	// name is both the routing.Source.Name and the ProviderKey (the
	// provider.Registry key cmd/main.go registers the vendor's Provider
	// under) — every direct vendor API provider uses the same string for
	// both, matching NewAnthropicSource/NewOpenAISource's existing contract.
	name string
	kind string
	// envVar names the model-filter env var (ANTHROPIC_MODEL_FILTER,
	// OPENAI_MODEL_FILTER), passed through to ApplyGlobFilter so a bad glob's
	// error names the knob an operator would actually fix.
	envVar      string
	creds       vendorCredentialChecker
	prov        vendorDiscoverer
	filters     vendorFilterFunc
	status      vendorStatusReporter
	isAuthError func(error) bool
	// httpStatusOf extracts the vendor HTTP status code (if any) carried by a
	// DiscoverModels failure, WITHOUT ever formatting the underlying error's
	// own Error() string (anthropic.HTTPStatusOf / openai.HTTPStatusOf) — used
	// by discover to build a closed-vocabulary error safe to log or return
	// anywhere on the discovery path. nil (a test fake that doesn't set it)
	// means no status is ever reported, which is safe — newVendorDiscoveryError
	// simply omits it.
	httpStatusOf func(error) (int, bool)
	// postFilter, if non-nil, is applied to the glob-filtered id list AFTER
	// ApplyGlobFilter — e.g. openai's chat-completions-model denylist
	// hygiene (see internal/provider/openai.FilterChatModels), which must
	// run after the operator's own filter so a broad "*" OPENAI_MODEL_FILTER
	// alone can never put a non-chat model in the catalog. nil means no
	// additional filtering (anthropic).
	postFilter func([]string) []string
}

// vendorSourceState is the six-state discovery skeleton shared by every
// direct vendor API Source: every tick determines and reports exactly one of
// not_configured, filter_missing, auth_failed, filter_matched_nothing,
// unreachable, ok to status, then returns the ids (if any) that belong in
// the route table for that tick.
//
// Factored out of what was originally an anthropic-only implementation
// (anthropicSourceState) once internal/provider/openai needed the identical
// state machine — see NewAnthropicSource and NewOpenAISource, both now thin
// wrappers around newVendorSource.
type vendorSourceState struct {
	cfg vendorSourceConfig
}

// newVendorSource builds a routing Source around the shared six-state
// skeleton. cfg.creds and cfg.prov should be the SAME instances the caller
// passed to the vendor provider's own constructor, so both share one cached
// credential read per tick. cfg.status may be nil (no ProviderStatus
// reporting, e.g. in tests that don't care about it) — Report is simply
// skipped.
func newVendorSource(cfg vendorSourceConfig) Source {
	state := &vendorSourceState{cfg: cfg}
	return Source{
		Name:        cfg.name,
		Kind:        cfg.kind,
		Class:       ClassExternalAPI,
		ProviderKey: cfg.name,
		Discover:    state.discover,
	}
}

func (s *vendorSourceState) discover(ctx context.Context) ([]string, error) {
	// fr is resolved once at the top of the tick, before the credential
	// check, so every report call below — including not_configured/
	// auth_failed, which never reach ApplyGlobFilter — carries the SAME
	// filter/filter_source/env_filter_set_at snapshot an admin would see for
	// this tick, regardless of which state discover ultimately reports.
	fr := s.cfg.filters()

	_, _, err := s.cfg.creds.Key()
	if err != nil {
		if errors.Is(err, credsource.ErrNotConfigured) {
			// Expected, quiet state: the operator hasn't set up this vendor
			// yet. Not an error — a vendor provider must never poison
			// another source's tick just because it isn't configured.
			s.report(provider.ProviderStateNotConfigured, false, 0, false, fr)
			return nil, nil
		}
		// Key file present but unreadable/empty: a real misconfiguration.
		// Reported as auth_failed (NOT not_configured) — an admin needs to
		// see "there is a key problem", not "nothing is set up", and the
		// admin UI's auth_failed copy already covers this. There is no
		// separate "degraded" state in the wire contract's closed
		// vocabulary. Manager.Refresh still logs the returned error loudly —
		// as a closed-vocabulary vendorDiscoveryError, NEVER %w/%v of err
		// itself: a credsource error is low-risk (a file path) today, but
		// this path must stay safe by construction regardless of what a
		// future credential source implementation's error might carry.
		s.report(provider.ProviderStateAuthFailed, true, 0, false, fr)
		return nil, newVendorDiscoveryError(s.cfg.name, provider.ProviderStateAuthFailed, 0, false, err)
	}

	if len(fr.Patterns) == 0 {
		// [RULING] "no filter => no vendor models" is deliberate, unlike
		// ApplyGlobFilter's normal empty-patterns-means-everything
		// semantics used by self-hosted/cloud-managed sources.
		s.report(provider.ProviderStateFilterMissing, true, 0, false, fr)
		return nil, nil
	}

	discovered, err := s.cfg.prov.DiscoverModels(ctx)
	if err != nil {
		// Classify FIRST, using the raw err — which may be an OpenAI
		// discoveryError (a 401 body can echo back part of the submitted
		// key) or an Anthropic *sdk.Error (Error() renders the raw response
		// JSON plus Request-ID/Workspace-ID) — entirely internally to this
		// function. Once classified, err itself is discarded: only the
		// vendor name, the reported state, and (if available) the vendor's
		// HTTP status code are ever returned or logged from here on. Neither
		// %w nor %v of err (or its Cause) may be introduced anywhere below
		// this point, or on any other discovery path in this package.
		state := provider.ProviderStateUnreachable
		if s.cfg.isAuthError(err) {
			state = provider.ProviderStateAuthFailed
		}
		s.report(state, true, 0, true, fr)

		status, hasStatus := 0, false
		if s.cfg.httpStatusOf != nil {
			status, hasStatus = s.cfg.httpStatusOf(err)
		}
		return nil, newVendorDiscoveryError(s.cfg.name, state, status, hasStatus, err)
	}

	ids := make([]string, 0, len(discovered))
	for id := range discovered {
		ids = append(ids, id)
	}

	filtered, err := ApplyGlobFilter(ids, fr.Patterns, s.cfg.envVar)
	if err != nil {
		// config.Load already validates every glob at startup; this only
		// fires if that guard is bypassed. Treat like filter_missing rather
		// than poisoning the table with a hard failure.
		s.report(provider.ProviderStateFilterMissing, true, 0, false, fr)
		return nil, err
	}

	if s.cfg.postFilter != nil {
		filtered = s.cfg.postFilter(filtered)
	}

	if len(filtered) == 0 {
		s.report(provider.ProviderStateFilterMatchedNothing, true, 0, true, fr)
		return nil, nil
	}

	s.report(provider.ProviderStateOK, true, len(filtered), true, fr)
	return filtered, nil
}

// report updates the vendor status registry. synced controls whether
// LastSyncAt is set: only when an actual vendor API call was attempted this
// tick (auth_failed, unreachable, filter_matched_nothing, ok) — not for
// not_configured/filter_missing, where discovery never left the gateway. fr
// is this tick's resolved filter (see discover) — always reported, even for
// a state that never reached ApplyGlobFilter, so filter_source/
// env_filter_set_at are visible before a vendor is even fully configured.
func (s *vendorSourceState) report(state string, configured bool, modelCount int, synced bool, fr vendorFilterResult) {
	if s.cfg.status == nil {
		return
	}
	st := provider.ProviderStatus{
		Configured:   configured,
		State:        state,
		ModelCount:   modelCount,
		FilterSource: fr.Source,
		Filter:       fr.Patterns,
		WorkspaceID:  fr.WorkspaceID,
	}
	if !fr.EnvSetAt.IsZero() {
		envSetAt := fr.EnvSetAt
		st.EnvFilterSetAt = &envSetAt
	}
	if synced {
		now := time.Now()
		st.LastSyncAt = &now
	}
	s.cfg.status.Report(s.cfg.name, st)
}

// vendorDiscoveryError is the ONLY error a vendorSourceState.discover call
// ever returns on failure — a closed-vocabulary rendering of vendor name +
// reported state + (if known) the vendor's HTTP status code, e.g.
// "openai: discovery failed (auth_failed, HTTP 401)". It exists so that
// Manager.Refresh's %v log line (and any other caller further up the
// discovery path — classifyError, /health's SourceStatus) can never end up
// formatting a raw vendor error's own Error() string, which for a direct
// vendor API provider may embed a response body (a 401's body can echo back
// part of the submitted API key), an SDK-generated Request-ID/Workspace-ID,
// or an account identifier.
//
// cause is retained via Unwrap ONLY so classifyError's existing
// errors.As/errors.Is-based classification (smithy.APIError, net.Error, json
// errors — none of which format the error's text, only inspect its type)
// keeps working unchanged for a vendor source exactly as it already does for
// a self-hosted/Bedrock source. Nothing in this package may ever call
// cause.Error(), or format cause with %v/%w/%s, directly.
type vendorDiscoveryError struct {
	vendor     string
	state      string
	statusCode int // 0 when hasStatus is false: the failure never got an HTTP response
	hasStatus  bool
	cause      error
}

// newVendorDiscoveryError builds a vendorDiscoveryError. cause is retained
// only for Unwrap (see the type's doc comment) — its own Error() text is
// never read by this function or by Error() below.
func newVendorDiscoveryError(vendor, state string, statusCode int, hasStatus bool, cause error) *vendorDiscoveryError {
	return &vendorDiscoveryError{vendor: vendor, state: state, statusCode: statusCode, hasStatus: hasStatus, cause: cause}
}

func (e *vendorDiscoveryError) Error() string {
	if e.hasStatus {
		return fmt.Sprintf("%s: discovery failed (%s, HTTP %d)", e.vendor, e.state, e.statusCode)
	}
	return fmt.Sprintf("%s: discovery failed (%s)", e.vendor, e.state)
}

func (e *vendorDiscoveryError) Unwrap() error { return e.cause }

// EffectiveWorkspaceID returns the workspace id carried by f's current
// resolution ("" for none). cmd/main.go hands the anthropic provider a closure
// over this so every request reads the live last-writer-wins value.
func EffectiveWorkspaceID(f vendorFilterFunc) string { return f().WorkspaceID }
