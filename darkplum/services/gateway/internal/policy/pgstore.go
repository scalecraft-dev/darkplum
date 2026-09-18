package policy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgStore loads Snapshots from Postgres and caches the last-good result.
// Refresh performs exactly 5 SELECTs (models, model_aliases,
// model_alias_routes, routing_policy, principal_routing_policies joined to
// api_keys).
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

	// routeRows and aliasRows are scanned into plain structs (rawRouteRow /
	// rawAliasRow) and handed to buildAliases, which is pure — no pgx, no
	// network — so the assembly/drop-if-inconsistent logic (the part with
	// real branching to get right) is unit-testable with fakes instead of a
	// live model_alias_routes migration.
	var routeRows []rawRouteRow
	rrows, err := s.pool.Query(ctx, `
		SELECT alias, route_name, description, target_model_id, keywords
		  FROM model_alias_routes
		 ORDER BY alias, position`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("policy: query model_alias_routes: %w", err)
	}
	for rrows.Next() {
		var r rawRouteRow
		if err := rrows.Scan(&r.Alias, &r.Name, &r.Description, &r.Target, &r.Keywords); err != nil {
			rrows.Close()
			return Snapshot{}, fmt.Errorf("policy: scan model_alias_routes row: %w", err)
		}
		routeRows = append(routeRows, r)
	}
	if err := rrows.Err(); err != nil {
		rrows.Close()
		return Snapshot{}, fmt.Errorf("policy: read model_alias_routes rows: %w", err)
	}
	rrows.Close()

	var aliasRows []rawAliasRow
	arows, err := s.pool.Query(ctx, `
		SELECT alias, kind, target_model_id, default_route, classifier_kind
		  FROM model_aliases`)
	if err != nil {
		return Snapshot{}, fmt.Errorf("policy: query model_aliases: %w", err)
	}
	for arows.Next() {
		var a rawAliasRow
		if err := arows.Scan(&a.Alias, &a.Kind, &a.Target, &a.DefaultRoute, &a.ClassifierKind); err != nil {
			arows.Close()
			return Snapshot{}, fmt.Errorf("policy: scan model_aliases row: %w", err)
		}
		aliasRows = append(aliasRows, a)
	}
	if err := arows.Err(); err != nil {
		arows.Close()
		return Snapshot{}, fmt.Errorf("policy: read model_aliases rows: %w", err)
	}
	arows.Close()

	var residency string
	if err := s.pool.QueryRow(ctx, `SELECT residency FROM routing_policy WHERE id = true`).Scan(&residency); err != nil {
		return Snapshot{}, fmt.Errorf("policy: query routing_policy: %w", err)
	}

	// principal_routing_policies (migration 015) may not exist yet on a
	// customer database mid-rollout, exactly like model_aliases/routing_policy
	// before migration 010 landed — see loadPrincipalPolicyRows.
	principalRows, err := s.loadPrincipalPolicyRows(ctx)
	if err != nil {
		if !isUndefinedTableError(err) {
			return Snapshot{}, fmt.Errorf("policy: query principal_routing_policies: %w", err)
		}
		log.Printf("policy: WARNING principal_routing_policies does not exist yet (migration 015 not applied) — treating as zero explicit principal policies for this snapshot")
		principalRows = nil
	}
	built := buildPrincipalPolicies(principalRows, residency)

	// api_keys ownership (ALL non-revoked keys, not just the ones with an
	// explicit principal_routing_policies row) -- feeds Snapshot.KeyOwner/
	// UserKeys, which the POST /v1/policy/effective override path uses to
	// enumerate "every key owned by this user" for its "keys" list. A
	// separate query from loadPrincipalPolicyRows' LEFT JOIN above, which only
	// resolves ownership for keys that already have a policy row.
	keyOwner, userKeys, err := s.loadAPIKeyOwners(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("policy: query api_keys: %w", err)
	}

	return Snapshot{
		Enabled:         enabled,
		Aliases:         buildAliases(aliasRows, routeRows),
		Residency:       residency,
		OrgPolicy:       built.OrgPolicy,
		UserPolicies:    built.UserPolicies,
		KeyPolicies:     built.KeyPolicies,
		PrincipalCounts: built.Counts,
		OrgRow:          built.OrgRow,
		UserRows:        built.UserRows,
		KeyRows:         built.KeyRows,
		KeyOwner:        keyOwner,
		UserKeys:        userKeys,
	}, nil
}

// loadAPIKeyOwners queries every non-revoked api_keys row for its owning
// user_id and returns both the key_id -> user_id map and its inverse,
// user_id -> []key_id. Unlike loadPrincipalPolicyRows' LEFT JOIN (which only
// resolves ownership for a key that already has a principal_routing_policies
// row), this covers every api_key that exists -- api_keys has existed since
// migration 001, so unlike principal_routing_policies there is no rollout
// tolerance needed here.
func (s *PgStore) loadAPIKeyOwners(ctx context.Context) (keyOwner map[string]string, userKeys map[string][]string, err error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, user_id::text FROM api_keys WHERE revoked_at IS NULL`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	keyOwner = make(map[string]string)
	userKeys = make(map[string][]string)
	for rows.Next() {
		var keyID, userID string
		if err := rows.Scan(&keyID, &userID); err != nil {
			return nil, nil, fmt.Errorf("scan api_keys row: %w", err)
		}
		keyOwner[keyID] = userID
		userKeys[userID] = append(userKeys[userID], keyID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read api_keys rows: %w", err)
	}
	return keyOwner, userKeys, nil
}

// undefinedTableErrorCode is the Postgres SQLSTATE for "undefined_table". See
// https://www.postgresql.org/docs/current/errcodes-appendix.html.
const undefinedTableErrorCode = "42P01"

// isUndefinedTableError reports whether err is a Postgres "undefined_table"
// error (SQLSTATE 42P01) — the signature of principal_routing_policies not
// existing yet on a customer database that hasn't run migration 015.
func isUndefinedTableError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == undefinedTableErrorCode
	}
	return false
}

// rawPrincipalPolicyRow is one scanned row of the principal_routing_policies
// / api_keys join in loadPrincipalPolicyRows. KeyOwnerUserID is api_keys.user_id
// for an api_key-type row (nil for org/user rows, where the LEFT JOIN finds
// nothing to match); buildPrincipalPolicies uses it to locate that key's
// owning user's OWN row (if any) so a key's chain is org -> user -> api_key
// even though principal_routing_policies itself never stores a key's owning
// user_id (an api_key row's user_id column is NULL, enforced by the
// principal_type CHECK constraint — the owner is looked up via api_keys
// instead).
type rawPrincipalPolicyRow struct {
	PrincipalType  string
	UserID         *string
	APIKeyID       *string
	Residency      *string
	AllowedModels  *[]string
	DefaultModel   *string
	KeyOwnerUserID *string
}

// loadPrincipalPolicyRows runs the principal_routing_policies / api_keys join
// and scans every row. It performs I/O only — see buildPrincipalPolicies for
// the pure assembly logic this feeds.
func (s *PgStore) loadPrincipalPolicyRows(ctx context.Context) ([]rawPrincipalPolicyRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT prp.principal_type,
		       prp.user_id::text,
		       prp.api_key_id::text,
		       prp.residency,
		       prp.allowed_models,
		       prp.default_model,
		       ak.user_id::text
		  FROM principal_routing_policies prp
		  LEFT JOIN api_keys ak ON ak.id = prp.api_key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []rawPrincipalPolicyRow
	for rows.Next() {
		var r rawPrincipalPolicyRow
		var allowedModels []string
		if err := rows.Scan(&r.PrincipalType, &r.UserID, &r.APIKeyID, &r.Residency, &allowedModels, &r.DefaultModel, &r.KeyOwnerUserID); err != nil {
			return nil, fmt.Errorf("scan principal_routing_policies row: %w", err)
		}
		r.AllowedModels = nullableSlicePtr(allowedModels)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read principal_routing_policies rows: %w", err)
	}
	return out, nil
}

// nullableSlicePtr returns nil for a nil slice (SQL NULL) and a pointer to s
// otherwise, preserving the NULL vs empty-array distinction pgx's array
// codec already gives a plain []string scan target: NULL leaves it nil, '{}'
// produces a non-nil zero-length slice.
func nullableSlicePtr(s []string) *[]string {
	if s == nil {
		return nil
	}
	return &s
}

// buildPrincipalPolicies assembles a Snapshot's OrgPolicy/UserPolicies/
// KeyPolicies/PrincipalCounts from the raw scanned rows of
// loadPrincipalPolicyRows. Pure (no I/O) so it is unit-testable with fakes,
// exactly like buildAliases.
//
// orgResidencyFallback is the org residency already read from
// routing_policy.residency (loadFromDB's separate, always-present query) —
// used as the org row's residency whenever no principal_type='org' row is
// found, which covers both the 42P01-tolerance case (rows is nil/empty
// because the table doesn't exist yet) and the — should be unreachable given
// prp_org_singleton plus migration 015's seed INSERT, but not trusted to hold
// forever — case where the table exists yet somehow has no org row.
// principalPolicies is buildPrincipalPolicies' return value: the precomputed
// EffectivePolicy per level (used by Snapshot.EffectiveFor at request time)
// alongside the raw, unmerged PolicyRow per level (used by
// Snapshot.ChainFor/PolicyOverride, the POST /v1/policy/effective preview
// path, to reconstruct a principal's chain and substitute exactly one level
// without a second query or an un-merge).
type principalPolicies struct {
	OrgPolicy    *EffectivePolicy
	UserPolicies map[string]*EffectivePolicy
	KeyPolicies  map[string]*EffectivePolicy
	Counts       PrincipalCounts

	OrgRow   PolicyRow
	UserRows map[string]PolicyRow
	KeyRows  map[string]PolicyRow
}

func buildPrincipalPolicies(rows []rawPrincipalPolicyRow, orgResidencyFallback string) principalPolicies {
	orgRow := PolicyRow{Level: PrincipalTypeOrg}
	foundOrgRow := false

	userRows := make(map[string]PolicyRow)

	type keyRowWithOwner struct {
		row   PolicyRow
		owner string
	}
	keyRows := make(map[string]keyRowWithOwner)

	for _, r := range rows {
		switch r.PrincipalType {
		case PrincipalTypeOrg:
			orgRow = PolicyRow{Level: PrincipalTypeOrg, Residency: r.Residency, AllowedModels: r.AllowedModels, DefaultModel: r.DefaultModel}
			foundOrgRow = true
		case PrincipalTypeUser:
			if r.UserID == nil {
				// Violates the principal_type CHECK constraint; should be
				// unreachable, but a user-type row with no user_id is
				// unusable — drop it rather than key a map entry on "".
				log.Printf("policy: WARNING principal_routing_policies row has principal_type='user' but a NULL user_id, dropping")
				continue
			}
			userRows[*r.UserID] = PolicyRow{Level: PrincipalTypeUser, Residency: r.Residency, AllowedModels: r.AllowedModels, DefaultModel: r.DefaultModel}
		case PrincipalTypeAPIKey:
			if r.APIKeyID == nil {
				log.Printf("policy: WARNING principal_routing_policies row has principal_type='api_key' but a NULL api_key_id, dropping")
				continue
			}
			owner := ""
			if r.KeyOwnerUserID != nil {
				owner = *r.KeyOwnerUserID
			}
			keyRows[*r.APIKeyID] = keyRowWithOwner{
				row:   PolicyRow{Level: PrincipalTypeAPIKey, Residency: r.Residency, AllowedModels: r.AllowedModels, DefaultModel: r.DefaultModel},
				owner: owner,
			}
		default:
			log.Printf("policy: WARNING principal_routing_policies row has unrecognised principal_type %q, ignoring", r.PrincipalType)
		}
	}

	if !foundOrgRow && orgResidencyFallback == ResidencySelfHostedOnly {
		v := ResidencySelfHostedOnly
		orgRow.Residency = &v
	}

	orgEff := Effective([]PolicyRow{orgRow})
	orgPolicy := &orgEff

	userPolicies := make(map[string]*EffectivePolicy, len(userRows))
	for id, row := range userRows {
		eff := Effective([]PolicyRow{orgRow, row})
		userPolicies[id] = &eff
	}

	keyPolicies := make(map[string]*EffectivePolicy, len(keyRows))
	keyRowsPlain := make(map[string]PolicyRow, len(keyRows))
	for id, kr := range keyRows {
		chain := []PolicyRow{orgRow}
		if kr.owner != "" {
			if ur, ok := userRows[kr.owner]; ok {
				chain = append(chain, ur)
			}
		}
		chain = append(chain, kr.row)
		eff := Effective(chain)
		keyPolicies[id] = &eff
		keyRowsPlain[id] = kr.row
	}

	return principalPolicies{
		OrgPolicy:    orgPolicy,
		UserPolicies: userPolicies,
		KeyPolicies:  keyPolicies,
		Counts:       PrincipalCounts{Users: len(userRows), APIKeys: len(keyRows)},
		OrgRow:       orgRow,
		UserRows:     userRows,
		KeyRows:      keyRowsPlain,
	}
}

// rawRouteRow is one scanned model_alias_routes row (migration 013), in
// model_alias_routes.position order per the ORDER BY in loadFromDB.
type rawRouteRow struct {
	Alias       string
	Name        string
	Description string
	Target      string
	Keywords    []string
}

// rawAliasRow is one scanned model_aliases row (migration 013, narrowed by
// 014 — classifier_model_id is no longer selected, see buildAliases). Target,
// DefaultRoute, and ClassifierKind are nullable per the alias_kind_shape
// CHECK constraint (NULL for the fields that don't apply to this row's Kind).
type rawAliasRow struct {
	Alias          string
	Kind           string
	Target         *string
	DefaultRoute   *string
	ClassifierKind *string
}

// buildAliases assembles the Aliases map of a Snapshot from the raw scanned
// rows of model_aliases and model_alias_routes. Pure (no I/O) so the
// drop-if-inconsistent rule below can be unit-tested with fakes ahead of (and
// independently of) migration 013 landing against a real database.
//
// A routed alias whose rows are inconsistent — zero routes, or a
// default_route that does not name one of them — is dropped from the
// snapshot with one WARN log line; it never fails the whole load. The api
// enforces both invariants at PUT time inside the same transaction that
// writes model_alias_routes, but the gateway does not trust that to hold for
// every possible write path (a partially-applied migration, a direct DB
// edit, or simply two independently-deployed services never being perfectly
// in sync) — resolving a request to an empty target would be worse than
// dropping the alias and refusing it with 404 model_not_found instead.
//
// classifier_kind = 'model' (the in-house classifier prompt protocol,
// removed by migration 014) is a similar tolerance case: the api's
// migration coerces every existing row to 'rules', but a gateway pod
// started before that migration lands against a freshly onboarded (or
// not-yet-migrated) customer database may still load a row carrying the old
// value. Rather than drop the alias, it is treated as classifier_kind
// 'rules' — RulesClassifier is always a safe, I/O-free degrade — with one
// WARN logged for the whole snapshot load, not per alias or per request.
func buildAliases(aliasRows []rawAliasRow, routeRows []rawRouteRow) map[string]AliasDef {
	// routesByAlias holds every candidate route for a routed alias, still in
	// model_alias_routes.position order. A static alias never has an entry.
	routesByAlias := make(map[string][]RouteDef)
	for _, r := range routeRows {
		rd := RouteDef{Name: r.Name, Description: r.Description, Target: r.Target}
		// Case-fold defensively: the api case-folds keywords on write, but
		// the gateway must not depend on that holding for every write path
		// that reaches this table, since keyword matching itself is done
		// against already-lower-cased text (see internal/router).
		rd.Keywords = make([]string, len(r.Keywords))
		for i, kw := range r.Keywords {
			rd.Keywords[i] = strings.ToLower(kw)
		}
		routesByAlias[r.Alias] = append(routesByAlias[r.Alias], rd)
	}

	aliases := make(map[string]AliasDef, len(aliasRows))
	sawLegacyModelClassifierKind := false
	for _, a := range aliasRows {
		if a.Kind == AliasKindStatic {
			if a.Target == nil {
				// Violates alias_kind_shape; should be unreachable, but a
				// static alias with no target is unusable — drop it rather
				// than resolve every caller of it to the empty string.
				log.Printf("policy: WARNING static alias %q has no target_model_id, dropping from snapshot", a.Alias)
				continue
			}
			aliases[a.Alias] = AliasDef{Kind: AliasKindStatic, Target: *a.Target}
			continue
		}

		// a.Kind == AliasKindRouted (the only other value alias_kind_shape
		// permits — a value outside {static, routed} would itself violate
		// the kind CHECK constraint and could not have been written).
		def := AliasDef{Kind: AliasKindRouted, Routes: routesByAlias[a.Alias]}
		if a.DefaultRoute != nil {
			def.DefaultRoute = *a.DefaultRoute
		}
		if a.ClassifierKind != nil {
			def.ClassifierKind = *a.ClassifierKind
		}
		if def.ClassifierKind == legacyClassifierKindModel {
			// See buildAliases's doc comment: the model classifier is gone,
			// but a not-yet-migrated (or pre-migration-014) row may still
			// carry this value. Coerce to rules rather than drop the alias.
			def.ClassifierKind = ClassifierKindRules
			sawLegacyModelClassifierKind = true
		}

		if len(def.Routes) == 0 {
			log.Printf("policy: WARNING routed alias %q has zero routes, dropping from snapshot", a.Alias)
			continue
		}
		if _, ok := def.Route(def.DefaultRoute); !ok {
			log.Printf("policy: WARNING routed alias %q default_route %q is not among its routes, dropping from snapshot", a.Alias, def.DefaultRoute)
			continue
		}
		aliases[a.Alias] = def
	}
	if sawLegacyModelClassifierKind {
		log.Printf("policy: WARNING one or more routed aliases had classifier_kind=%q (removed) — treating as %q for this snapshot; run migration 014 to persist the change", legacyClassifierKindModel, ClassifierKindRules)
	}
	return aliases
}

// legacyClassifierKindModel is the removed "model" classifier_kind value
// (the in-house Arch-Router prompt protocol classifier, deleted alongside
// internal/router's ModelClassifier). Not exported as a policy.ClassifierKind*
// constant — it is not a value the gateway ever selects a classifier by,
// only one it tolerates on load. See buildAliases.
const legacyClassifierKindModel = "model"

// compile-time interface check.
var _ Store = (*PgStore)(nil)
