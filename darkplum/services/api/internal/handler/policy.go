// Per-principal routing policy: org -> user -> api_key (migration
// 015_principal_policies.sql). Same JSON shape at every level:
//
//	{"residency": null|"self_hosted_only", "allowed_models": null|[...], "default_model": null|string}
//
// null always means "inherit the level above"; org's null residency means
// "any" (there is nothing above org). allowed_models is a tri-state via
// *[]string: nil (absent or JSON null) = inherit, a non-nil (possibly empty)
// slice = an explicit allowlist. An explicit empty allowlist is only ever
// meaningful as an intermediate value on the way to a 409 lock-out check
// below -- it is never silently accepted as "deny everything" without that
// check running first.
//
// "Effective" (the merged, enforced view) has exactly ONE implementation, in
// the gateway (internal/policy) -- see gatewayEffectiveClient. Every "GET
// .../effective" view proxies GET {gateway}/v1/policy/effective; every write
// (PUT's lock-out check) and preview (POST .../preview) proxies POST
// {gateway}/v1/policy/effective with a hypothetical override instead --
// there is deliberately no second, locally-reimplemented merge/intersection
// anywhere in this file (an earlier version of this file had one --
// mergeAllowedModels -- specifically so writes did not depend on the gateway
// being reachable; that traded a second source of truth for a marginal
// availability win, which DESIGN4's "gateway is the only place policy is
// merged" rule forbids. A PUT or preview now 502s if the gateway is down,
// same as the "effective" reads always have).
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
)

const (
	principalTypeOrg    = "org"
	principalTypeUser   = "user"
	principalTypeAPIKey = "api_key"

	// principalsListMaxLimit caps GET /api/v1/routing-policy/principals'
	// page size (and hence the number of per-principal gateway calls it
	// makes -- see PolicyHandler.ListPrincipalPolicies) at 200, per contract.
	principalsListMaxLimit     = 200
	principalsListDefaultLimit = 50

	// gatewayEffectiveTimeout bounds one call to the gateway's
	// GET /v1/policy/effective. Kept short (well under the 10s
	// RoutingHandler/ModelHandler client timeouts used elsewhere) because
	// this endpoint backs interactive admin views and the chat model
	// picker -- a slow gateway must fail fast, not hang the request.
	gatewayEffectiveTimeout = 2 * time.Second

	// modelsEffectiveCacheTTL bounds how often ListModels re-fetches a given
	// session user's effective policy from the gateway; see
	// ModelHandler.getSessionEffective.
	modelsEffectiveCacheTTL = 5 * time.Second
)

// policyObject is the wire shape for a principal's own stored policy row
// (user/api_key), used by GET/PUT .../routing-policy. Residency and
// AllowedModels are tri-state pointers: nil means "inherit"; AllowedModels
// additionally distinguishes an explicit empty list ([]              , not
// nil) from "inherit" (nil).
type policyObject struct {
	Residency     *string   `json:"residency"`
	AllowedModels *[]string `json:"allowed_models"`
	DefaultModel  *string   `json:"default_model"`
}

// principalPolicyResponse is GET/PUT .../routing-policy's response shape for
// a user or api_key principal.
type principalPolicyResponse struct {
	Residency     *string    `json:"residency"`
	AllowedModels *[]string  `json:"allowed_models"`
	DefaultModel  *string    `json:"default_model"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// validPrincipalResidency mirrors principal_routing_policies.residency's
// CHECK constraint: nil (inherit) or exactly "self_hosted_only". Unlike the
// org level, a principal can never explicitly set "any" -- there is nothing
// to override "any" with, since inheriting IS "any" in the absence of a
// stricter ancestor.
func validPrincipalResidency(r *string) bool {
	return r == nil || *r == residencySelfHostedOnly
}

// modelAllowed reports whether id is permitted by an (already-merged)
// effective allowlist: nil means unrestricted (always true); otherwise id
// must be a member.
func modelAllowed(effective *[]string, id string) bool {
	if effective == nil {
		return true
	}
	for _, m := range *effective {
		if m == id {
			return true
		}
	}
	return false
}

// aliasLookup is the result of resolving a candidate default_model id
// against models/model_aliases.
type aliasLookup struct {
	isModel      bool
	isAlias      bool
	aliasTargets []string // for an alias: its target (static) or every route's target (routed)
}

// lookupModelOrAlias classifies id as a concrete model, an alias, or
// unknown, and for an alias resolves its target(s) -- used by both allowlist
// validation (aliases are rejected there) and default_model validation
// (aliases are allowed there, checked against their targets).
func lookupModelOrAlias(ctx context.Context, db *pgxpool.Pool, id string) (aliasLookup, error) {
	var lk aliasLookup
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM models WHERE model_id = $1)`, id).Scan(&lk.isModel); err != nil {
		return lk, fmt.Errorf("check model existence: %w", err)
	}

	var kind *string
	var target *string
	err := db.QueryRow(ctx, `SELECT kind, target_model_id FROM model_aliases WHERE alias = $1`, id).Scan(&kind, &target)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return lk, fmt.Errorf("check alias existence: %w", err)
	}
	if err == nil {
		lk.isAlias = true
		if kind != nil && *kind == aliasKindStatic && target != nil {
			lk.aliasTargets = []string{*target}
		} else {
			rows, rErr := db.Query(ctx, `SELECT target_model_id FROM model_alias_routes WHERE alias = $1`, id)
			if rErr != nil {
				return lk, fmt.Errorf("load alias routes: %w", rErr)
			}
			defer rows.Close()
			for rows.Next() {
				var t string
				if sErr := rows.Scan(&t); sErr != nil {
					return lk, fmt.Errorf("scan alias route target: %w", sErr)
				}
				lk.aliasTargets = append(lk.aliasTargets, t)
			}
			if rErr := rows.Err(); rErr != nil {
				return lk, fmt.Errorf("alias route rows: %w", rErr)
			}
		}
	}
	return lk, nil
}

// validateAllowedModelIDs enforces the allowlist contract: every id must be
// a real, currently-known model id -- never an alias (aliases resolve to
// zero, one, or many models depending on routing/classification, so an
// allowlist entry naming one would be ambiguous policy). Returns a single
// error describing the first problem found; used for the 400 response body.
func validateAllowedModelIDs(ctx context.Context, db *pgxpool.Pool, ids []string) error {
	for _, id := range ids {
		lk, err := lookupModelOrAlias(ctx, db, id)
		if err != nil {
			return err
		}
		if lk.isAlias {
			return fmt.Errorf("allowlists take model ids; add the alias's targets instead: %s", id)
		}
		if !lk.isModel {
			return fmt.Errorf("unknown model id: %s", id)
		}
	}
	return nil
}

// validateDefaultModel checks that a candidate default_model is a real
// model or alias id, and — when effective is a restricted (non-nil)
// allowlist — that it is actually reachable under it: a concrete model id
// must be a member; an alias must have at least one target that is (its
// other targets are simply never selectable, exactly as a route to a
// disallowed target is refused per-request rather than silently swapped).
func validateDefaultModel(ctx context.Context, db *pgxpool.Pool, id string, effective *[]string) error {
	lk, err := lookupModelOrAlias(ctx, db, id)
	if err != nil {
		return err
	}
	if !lk.isModel && !lk.isAlias {
		return fmt.Errorf("unknown model or alias id: %s", id)
	}
	if effective == nil {
		return nil
	}
	if lk.isModel && !lk.isAlias {
		if !modelAllowed(effective, id) {
			return fmt.Errorf("default_model %q is not in the effective allowlist", id)
		}
		return nil
	}
	for _, t := range lk.aliasTargets {
		if modelAllowed(effective, t) {
			return nil
		}
	}
	return fmt.Errorf("default_model %q (alias) has no target in the effective allowlist", id)
}

// gatewayEffectivePolicy is the wire shape of the gateway's GET/POST
// /v1/policy/effective (service-key only) -- see DESIGN4's Gateway section.
// AllowedModels is nil for "every model" (no restriction). Keys is only ever
// populated on a POST response (see effectiveWithOverride) -- GET never sets
// it.
type gatewayEffectivePolicy struct {
	AllowedModels   *[]string             `json:"allowed_models"`
	Residency       string                `json:"residency"`
	DefaultModel    *string               `json:"default_model"`
	Sources         map[string]string     `json:"sources"`
	ReachableModels []string              `json:"reachable_models"`
	Keys            []gatewayKeyEffective `json:"keys,omitempty"`
}

// gatewayKeyEffective is one entry of gatewayEffectivePolicy.Keys -- an
// api_key owned by the user_id under preview/write, and what ITS effective
// policy would be under the same hypothetical override. The gateway names it
// by id only (it has no api_keys.name column to read); callers that need a
// name (409 messages, keys_affected) look it up locally via keyNamesByID.
type gatewayKeyEffective struct {
	APIKeyID  string                 `json:"api_key_id"`
	Effective gatewayEffectivePolicy `json:"effective"`
}

// gatewayPolicyOverride is the wire shape of one POST /v1/policy/effective
// override: the chain level to substitute, and what to substitute it with.
// Policy nil means "inherit" -- remove whatever row this level currently has,
// exactly like DELETE .../routing-policy. policyObject already has the exact
// field shape the gateway expects for Policy, so it is reused directly rather
// than duplicated.
type gatewayPolicyOverride struct {
	Level  string        `json:"level"`
	Policy *policyObject `json:"policy"`
}

// gatewayEffectiveOverrideRequest is the body of POST /v1/policy/effective.
type gatewayEffectiveOverrideRequest struct {
	UserID   string                 `json:"user_id"`
	APIKeyID string                 `json:"api_key_id,omitempty"`
	Override *gatewayPolicyOverride `json:"override,omitempty"`
}

// gatewayEffectiveClient calls the gateway's GET /v1/policy/effective, the
// single implementation of the org->user->key merge. Every "effective" view
// in the api (admin, /me, previews' shown-to-user fields, /v1/models
// filtering) proxies through this rather than re-deriving the merge, so
// those views can never disagree with what the gateway will actually
// enforce on the next request.
type gatewayEffectiveClient struct {
	gatewayURL string
	serviceKey string
	client     *http.Client
}

func newGatewayEffectiveClient(gatewayURL, serviceKey string) *gatewayEffectiveClient {
	return &gatewayEffectiveClient{
		gatewayURL: gatewayURL,
		serviceKey: serviceKey,
		client:     &http.Client{Timeout: gatewayEffectiveTimeout},
	}
}

// effective calls GET {gatewayURL}/v1/policy/effective?user_id=...[&api_key_id=...].
// A non-nil error means the caller should treat the gateway as unreachable
// or misbehaving -- callers decide per endpoint whether that becomes a 502
// (admin effective/preview endpoints) or a warned-and-degraded fallback
// (ListModels).
func (g *gatewayEffectiveClient) effective(ctx context.Context, userID string, apiKeyID *string) (*gatewayEffectivePolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayEffectiveTimeout)
	defer cancel()

	target := fmt.Sprintf("%s/v1/policy/effective?user_id=%s", g.gatewayURL, url.QueryEscape(userID))
	if apiKeyID != nil {
		target += "&api_key_id=" + url.QueryEscape(*apiKeyID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build effective policy request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.serviceKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call gateway effective policy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway effective policy returned %d", resp.StatusCode)
	}

	var out gatewayEffectivePolicy
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode gateway effective policy: %w", err)
	}
	return &out, nil
}

// effectiveWithOverride calls POST {gatewayURL}/v1/policy/effective -- the
// gateway's impact-preview surface (DESIGN4's Gateway "POST
// /v1/policy/effective"). override nil reproduces the current, un-overridden
// effective policy (identical to effective/GET, except Keys IS populated
// when apiKeyID is empty -- see the gateway's HandleEffectivePolicyOverride
// doc comment); non-nil hypothetically substitutes one chain level without
// writing anything. This is the ONLY way PUT's lock-out check and the
// preview endpoints ask "what would this look like" -- see this file's
// package doc comment for why there is no local reimplementation of the
// merge to fall back on.
func (g *gatewayEffectiveClient) effectiveWithOverride(ctx context.Context, userID, apiKeyID string, override *gatewayPolicyOverride) (*gatewayEffectivePolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayEffectiveTimeout)
	defer cancel()

	body, err := json.Marshal(gatewayEffectiveOverrideRequest{UserID: userID, APIKeyID: apiKeyID, Override: override})
	if err != nil {
		return nil, fmt.Errorf("encode override policy request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.gatewayURL+"/v1/policy/effective", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build override policy request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.serviceKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call gateway override policy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway override policy returned %d", resp.StatusCode)
	}

	var out gatewayEffectivePolicy
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode gateway override policy: %w", err)
	}
	return &out, nil
}

// gatewayPolicyImpactRequest is the body of POST /v1/policy/impact: an org
// level override only (the gateway rejects any other level).
type gatewayPolicyImpactRequest struct {
	Override gatewayPolicyOverride `json:"override"`
}

// gatewayPolicyImpactPrincipal is one locked-out principal from POST
// /v1/policy/impact -- a user (APIKeyID empty) or a key (both set).
type gatewayPolicyImpactPrincipal struct {
	UserID   string `json:"user_id"`
	APIKeyID string `json:"api_key_id"`
}

// gatewayPolicyImpact is the wire shape of the gateway's POST
// /v1/policy/impact response. OrgLockedOut is the "empty org allowlist" case
// (everyone is locked out, so LockedOut is not enumerated).
type gatewayPolicyImpact struct {
	OrgLockedOut bool                           `json:"org_locked_out"`
	LockedOut    []gatewayPolicyImpactPrincipal `json:"locked_out"`
	Evaluated    int                            `json:"evaluated"`
}

// impact calls POST {gatewayURL}/v1/policy/impact with an org-level
// override: which users and keys would have an EMPTY effective allowlist if
// the org row became policy. This is PUT /api/v1/routing-policy's lock-out
// guard -- the org-wide counterpart of effectiveWithOverride, so the org PUT
// refuses lock-outs with the same 409 the user/key PUTs do, decided by the
// same gateway merge, in one call rather than one per principal.
func (g *gatewayEffectiveClient) impact(ctx context.Context, orgPolicy policyObject) (*gatewayPolicyImpact, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayEffectiveTimeout)
	defer cancel()

	body, err := json.Marshal(gatewayPolicyImpactRequest{Override: gatewayPolicyOverride{Level: principalTypeOrg, Policy: &orgPolicy}})
	if err != nil {
		return nil, fmt.Errorf("encode policy impact request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.gatewayURL+"/v1/policy/impact", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build policy impact request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.serviceKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call gateway policy impact: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway policy impact returned %d", resp.StatusCode)
	}

	var out gatewayPolicyImpact
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode gateway policy impact: %w", err)
	}
	return &out, nil
}

// PolicyHandler serves the per-principal routing-policy endpoints: GET/PUT/
// DELETE on a user's or api key's own policy row, the admin/self "effective"
// proxies, the principals overview list, and impact previews. Org-level GET/
// PUT stay on RoutingHandler (routing.go) alongside the alias/residency logic
// they already share.
type PolicyHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
	gateway  *gatewayEffectiveClient
}

// NewPolicyHandler returns a PolicyHandler backed by db, calling the
// gateway's effective-policy endpoint with the service credential.
func NewPolicyHandler(db *pgxpool.Pool, rec *activity.Recorder, gatewayURL, serviceKey string) *PolicyHandler {
	return &PolicyHandler{
		db:       db,
		activity: rec,
		gateway:  newGatewayEffectiveClient(gatewayURL, serviceKey),
	}
}

// getPrincipalPolicy reads one user or api_key's own stored policy row (nil
// fields if no row exists — "inherit everything").
func getPrincipalPolicy(ctx context.Context, db *pgxpool.Pool, principalType, id string) (*policyObject, *time.Time, error) {
	col := "user_id"
	if principalType == principalTypeAPIKey {
		col = "api_key_id"
	}
	var p policyObject
	var updatedAt time.Time
	err := db.QueryRow(ctx, fmt.Sprintf(`
		SELECT residency, allowed_models, default_model, updated_at
		FROM principal_routing_policies WHERE principal_type = $1 AND %s = $2
	`, col), principalType, id).Scan(&p.Residency, &p.AllowedModels, &p.DefaultModel, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read principal policy: %w", err)
	}
	return &p, &updatedAt, nil
}

// userExists/apiKeyOwner are small existence/ownership lookups shared by the
// principal-policy handlers below.
func userExists(ctx context.Context, db *pgxpool.Pool, id string) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

// apiKeyInfo is what the policy handlers need about an api_keys row: its
// owner, name (for lock-out messages), and whether it is revoked (revoked
// keys keep their policy row for audit but never count toward a lock-out —
// they can't make requests either way).
type apiKeyInfo struct {
	UserID    string
	Name      string
	RevokedAt *time.Time
}

func lookupAPIKey(ctx context.Context, db *pgxpool.Pool, id string) (*apiKeyInfo, error) {
	var info apiKeyInfo
	err := db.QueryRow(ctx, `SELECT user_id, name, revoked_at FROM api_keys WHERE id = $1`, id).
		Scan(&info.UserID, &info.Name, &info.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// lockedOutKey names one api_key that would have (or has) an empty effective
// allowlist, for the 409 message and the preview response's would_lock_out.
type lockedOutKey struct {
	ID   string
	Name string
}

// errGatewayUnavailable wraps a gateway call failure inside
// validateAndPreviewWrite so callers (PUT/preview handlers) can tell it apart
// from a genuine validation error: the former is a 502 (the write/preview
// simply cannot be evaluated right now), the latter is a 400 (the request
// itself is invalid regardless of gateway reachability).
var errGatewayUnavailable = errors.New("gateway unavailable")

// keyNamesByID batches api_keys.name lookups for a set of ids -- used to
// attach a human name to the gateway's Keys list (ids only; the gateway has
// no api_keys.name column to read -- see gatewayKeyEffective) for 409
// messages and keys_affected.
func keyNamesByID(ctx context.Context, db *pgxpool.Pool, ids []string) (map[string]string, error) {
	names := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	rows, err := db.Query(ctx, `SELECT id::text, name FROM api_keys WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("lookup key names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan key name: %w", err)
		}
		names[id] = name
	}
	return names, rows.Err()
}

// validateAndPreviewWrite runs every format/existence check shared by PUT and
// preview for a user or api_key policy, then asks the gateway (the ONE merge
// implementation -- see this file's package doc comment) what req's effective
// policy would be, as a POST /v1/policy/effective override. It never touches
// principal_routing_policies for writes -- callers do that afterward (PUT
// only, once this returns no error and no lock-out).
//
// principalType/targetID/ownerUserID identify the override: for a user
// write, targetID is the user id and ownerUserID is ignored; for an api_key
// write, targetID is the key id and ownerUserID is its owner (from
// lookupAPIKey). The returned gatewayEffectivePolicy's Keys field is
// populated for a user-principal write (every non-revoked key that user
// owns, under the same override) and empty for an api_key-principal write
// (only the key itself is affected) -- exactly DESIGN4's "keys" contract.
func (h *PolicyHandler) validateAndPreviewWrite(ctx context.Context, principalType, targetID, ownerUserID string, req policyObject) (*gatewayEffectivePolicy, error) {
	if !validPrincipalResidency(req.Residency) {
		return nil, errors.New("residency must be null or 'self_hosted_only'")
	}
	if req.AllowedModels != nil {
		if err := validateAllowedModelIDs(ctx, h.db, *req.AllowedModels); err != nil {
			return nil, err
		}
	}

	userID, apiKeyID := ownerUserID, ""
	if principalType == principalTypeAPIKey {
		apiKeyID = targetID
	} else {
		userID = targetID
	}
	eff, err := h.gateway.effectiveWithOverride(ctx, userID, apiKeyID, &gatewayPolicyOverride{Level: principalType, Policy: &req})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errGatewayUnavailable, err)
	}

	if req.DefaultModel != nil {
		if err := validateDefaultModel(ctx, h.db, *req.DefaultModel, eff.AllowedModels); err != nil {
			return nil, err
		}
	}

	return eff, nil
}

// lockedOutFromEffective collects every locked-out principal from a
// validateAndPreviewWrite result: the target itself (named selfName -- "
// (user)" for a user write, the key's own name for an api_key write) if its
// own allowed_models is a non-nil empty slice, plus every entry in eff.Keys
// (only ever non-empty for a user-principal write) that is likewise empty.
func lockedOutFromEffective(ctx context.Context, db *pgxpool.Pool, eff *gatewayEffectivePolicy, selfID, selfName string) ([]lockedOutKey, error) {
	var out []lockedOutKey
	if eff.AllowedModels != nil && len(*eff.AllowedModels) == 0 {
		out = append(out, lockedOutKey{ID: selfID, Name: selfName})
	}
	if len(eff.Keys) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(eff.Keys))
	for _, k := range eff.Keys {
		if k.Effective.AllowedModels != nil && len(*k.Effective.AllowedModels) == 0 {
			ids = append(ids, k.APIKeyID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	names, err := keyNamesByID(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out = append(out, lockedOutKey{ID: id, Name: names[id]})
	}
	return out, nil
}

// GetUserPolicy handles GET /api/v1/users/:id/routing-policy.
func (h *PolicyHandler) GetUserPolicy(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	ok, err := userExists(c.Request.Context(), h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check user failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	h.respondPolicy(c, principalTypeUser, id)
}

// GetAPIKeyPolicy handles GET /api/v1/api-keys/:id/routing-policy.
func (h *PolicyHandler) GetAPIKeyPolicy(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api key id"})
		return
	}
	info, err := lookupAPIKey(c.Request.Context(), h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check api key failed"})
		return
	}
	if info == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}
	h.respondPolicy(c, principalTypeAPIKey, id)
}

func (h *PolicyHandler) respondPolicy(c *gin.Context, principalType, id string) {
	p, updatedAt, err := getPrincipalPolicy(c.Request.Context(), h.db, principalType, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get policy failed"})
		return
	}
	resp := principalPolicyResponse{UpdatedAt: updatedAt}
	if p != nil {
		resp.Residency = p.Residency
		resp.AllowedModels = p.AllowedModels
		resp.DefaultModel = p.DefaultModel
	}
	c.JSON(http.StatusOK, resp)
}

// PutUserPolicy handles PUT /api/v1/users/:id/routing-policy.
func (h *PolicyHandler) PutUserPolicy(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	ctx := c.Request.Context()
	ok, err := userExists(ctx, h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check user failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	var req policyObject
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	eff, err := h.validateAndPreviewWrite(ctx, principalTypeUser, id, "", req)
	if err != nil {
		if errors.Is(err, errGatewayUnavailable) {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}

	lockedOut, err := lockedOutFromEffective(ctx, h.db, eff, id, "(user)")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check owned keys failed"})
		return
	}
	if len(lockedOut) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":      "this policy would leave the user (or a key it owns) with no reachable models",
			"locked_out": lockedOutNames(lockedOut),
		})
		return
	}

	before, _, err := getPrincipalPolicy(ctx, h.db, principalTypeUser, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read current policy failed"})
		return
	}

	resp, err := h.upsertPolicy(ctx, c, principalTypeUser, id, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update policy failed"})
		return
	}

	h.recordPrincipalChanged(c, principalTypeUser, id, before, &req)
	c.JSON(http.StatusOK, resp)
}

// PutAPIKeyPolicy handles PUT /api/v1/api-keys/:id/routing-policy.
func (h *PolicyHandler) PutAPIKeyPolicy(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api key id"})
		return
	}
	ctx := c.Request.Context()
	info, err := lookupAPIKey(ctx, h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check api key failed"})
		return
	}
	if info == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}

	var req policyObject
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	eff, err := h.validateAndPreviewWrite(ctx, principalTypeAPIKey, id, info.UserID, req)
	if err != nil {
		if errors.Is(err, errGatewayUnavailable) {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	if eff.AllowedModels != nil && len(*eff.AllowedModels) == 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":      "this policy would lock this key out of every model",
			"locked_out": []string{info.Name},
		})
		return
	}

	before, _, err := getPrincipalPolicy(ctx, h.db, principalTypeAPIKey, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read current policy failed"})
		return
	}

	resp, err := h.upsertPolicy(ctx, c, principalTypeAPIKey, id, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update policy failed"})
		return
	}

	h.recordPrincipalChanged(c, principalTypeAPIKey, id, before, &req)
	c.JSON(http.StatusOK, resp)
}

func lockedOutNames(keys []lockedOutKey) []string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.Name
	}
	return names
}

// upsertPolicy writes req as principalType/id's stored policy row via the
// partial-unique-index upsert matching migration 015, and returns the
// resulting principalPolicyResponse.
func (h *PolicyHandler) upsertPolicy(ctx context.Context, c *gin.Context, principalType, id string, req policyObject) (principalPolicyResponse, error) {
	var updatedBy *string
	if uid := c.GetString("user_id"); uid != "" {
		updatedBy = &uid
	}

	col := "user_id"
	if principalType == principalTypeAPIKey {
		col = "api_key_id"
	}

	var resp principalPolicyResponse
	err := h.db.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO principal_routing_policies (principal_type, %s, residency, allowed_models, default_model, updated_at, updated_by)
		VALUES ($1, $2, $3, $4, $5, now(), $6)
		ON CONFLICT (%s) WHERE %s IS NOT NULL DO UPDATE
			SET residency = EXCLUDED.residency, allowed_models = EXCLUDED.allowed_models,
				default_model = EXCLUDED.default_model, updated_at = now(), updated_by = EXCLUDED.updated_by
		RETURNING residency, allowed_models, default_model, updated_at
	`, col, col, col), principalType, id, req.Residency, req.AllowedModels, req.DefaultModel, updatedBy).
		Scan(&resp.Residency, &resp.AllowedModels, &resp.DefaultModel, &resp.UpdatedAt)
	return resp, err
}

func (h *PolicyHandler) recordPrincipalChanged(c *gin.Context, principalType, id string, before *policyObject, after *policyObject) {
	evt := activity.FromContext(c, activity.RoutingPolicyPrincipalChanged)
	evt.TargetType = activity.StrPtr(principalType)
	evt.TargetID = activity.StrPtr(id)
	evt.Detail = map[string]any{
		"principal_type": principalType,
		"before":         before,
		"after":          after,
	}
	h.activity.Record(c.Request.Context(), evt)
}

// DeleteUserPolicy handles DELETE /api/v1/users/:id/routing-policy -- reset
// to fully inherit (removes the stored row; 204 whether or not one existed).
func (h *PolicyHandler) DeleteUserPolicy(c *gin.Context) {
	h.deletePolicy(c, principalTypeUser, "user_id", func(ctx context.Context, id string) (bool, error) {
		return userExists(ctx, h.db, id)
	})
}

// DeleteAPIKeyPolicy handles DELETE /api/v1/api-keys/:id/routing-policy.
func (h *PolicyHandler) DeleteAPIKeyPolicy(c *gin.Context) {
	h.deletePolicy(c, principalTypeAPIKey, "api_key_id", func(ctx context.Context, id string) (bool, error) {
		info, err := lookupAPIKey(ctx, h.db, id)
		return info != nil, err
	})
}

func (h *PolicyHandler) deletePolicy(c *gin.Context, principalType, col string, exists func(context.Context, string) (bool, error)) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	ctx := c.Request.Context()
	ok, err := exists(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check principal failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "principal not found"})
		return
	}

	before, _, err := getPrincipalPolicy(ctx, h.db, principalType, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read current policy failed"})
		return
	}

	if _, err := h.db.Exec(ctx, fmt.Sprintf(
		`DELETE FROM principal_routing_policies WHERE principal_type = $1 AND %s = $2`, col,
	), principalType, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete policy failed"})
		return
	}

	if before != nil {
		h.recordPrincipalChanged(c, principalType, id, before, nil)
	}
	c.Status(http.StatusNoContent)
}

// GetUserPolicyEffective handles GET /api/v1/users/:id/routing-policy/effective (admin).
func (h *PolicyHandler) GetUserPolicyEffective(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	ok, err := userExists(c.Request.Context(), h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check user failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	h.respondEffective(c, principalTypeUser, id, id, nil)
}

// GetAPIKeyPolicyEffective handles GET /api/v1/api-keys/:id/routing-policy/effective (admin).
func (h *PolicyHandler) GetAPIKeyPolicyEffective(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api key id"})
		return
	}
	info, err := lookupAPIKey(c.Request.Context(), h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check api key failed"})
		return
	}
	if info == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}
	h.respondEffective(c, principalTypeAPIKey, id, info.UserID, &id)
}

// GetMyPolicyEffective handles GET /api/v1/me/routing-policy/effective (any
// session user, for themselves only -- no "stored" field, since a
// non-admin has no reason to see the raw row versus the merged view).
func (h *PolicyHandler) GetMyPolicyEffective(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
		return
	}
	eff, err := h.gateway.effective(c.Request.Context(), userID, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway effective policy unavailable: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, eff)
}

func (h *PolicyHandler) respondEffective(c *gin.Context, principalType, id, userID string, apiKeyID *string) {
	eff, err := h.gateway.effective(c.Request.Context(), userID, apiKeyID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway effective policy unavailable: " + err.Error()})
		return
	}
	stored, _, err := getPrincipalPolicy(c.Request.Context(), h.db, principalType, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read stored policy failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"allowed_models":   eff.AllowedModels,
		"residency":        eff.Residency,
		"default_model":    eff.DefaultModel,
		"sources":          eff.Sources,
		"reachable_models": eff.ReachableModels,
		"stored":           stored,
	})
}

// previewResponse is POST .../routing-policy/preview's response shape.
// Effective is the REAL gateway shape (sources, reachable_models included) --
// a live POST /v1/policy/effective override call, not a local approximation.
// See this file's package doc comment: there is exactly one merge
// implementation, and it is the gateway's, for a write preview exactly as
// much as for a write or a read.
type previewResponse struct {
	Effective    previewEffective   `json:"effective"`
	KeysAffected []previewKeyImpact `json:"keys_affected"`
	WouldLockOut []string           `json:"would_lock_out"`
}

// previewEffective mirrors gatewayEffectivePolicy but omits Keys: the
// gateway's raw per-key entries feed KeysAffected (with names attached
// locally) instead of round-tripping onto the wire a second time under
// "effective".
type previewEffective struct {
	AllowedModels   *[]string         `json:"allowed_models"`
	Residency       string            `json:"residency"`
	DefaultModel    *string           `json:"default_model"`
	Sources         map[string]string `json:"sources"`
	ReachableModels []string          `json:"reachable_models"`
}

func newPreviewEffective(eff *gatewayEffectivePolicy) previewEffective {
	return previewEffective{
		AllowedModels:   eff.AllowedModels,
		Residency:       eff.Residency,
		DefaultModel:    eff.DefaultModel,
		Sources:         eff.Sources,
		ReachableModels: eff.ReachableModels,
	}
}

type previewKeyImpact struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Before *[]string `json:"before"`
	After  *[]string `json:"after"`
}

// PreviewUserPolicy handles POST /api/v1/users/:id/routing-policy/preview --
// same body and validation as PutUserPolicy, but never writes; lock-outs are
// reported in would_lock_out instead of a 409. "before" comes from a second
// gateway call with no override (still returns the user's keys, since
// api_key_id is empty -- see the gateway's HandleEffectivePolicyOverride doc
// comment), "after" from the proposed policy as an override, per DESIGN4.
func (h *PolicyHandler) PreviewUserPolicy(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		return
	}
	ctx := c.Request.Context()
	ok, err := userExists(ctx, h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check user failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	var req policyObject
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.AllowedModels != nil {
		if err := validateAllowedModelIDs(ctx, h.db, *req.AllowedModels); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if !validPrincipalResidency(req.Residency) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "residency must be null or 'self_hosted_only'"})
		return
	}

	before, err := h.gateway.effectiveWithOverride(ctx, id, "", nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway effective policy unavailable: " + err.Error()})
		return
	}
	after, err := h.gateway.effectiveWithOverride(ctx, id, "", &gatewayPolicyOverride{Level: principalTypeUser, Policy: &req})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway effective policy unavailable: " + err.Error()})
		return
	}
	if req.DefaultModel != nil {
		if err := validateDefaultModel(ctx, h.db, *req.DefaultModel, after.AllowedModels); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	beforeByKey := make(map[string]*[]string, len(before.Keys))
	for _, k := range before.Keys {
		beforeByKey[k.APIKeyID] = k.Effective.AllowedModels
	}
	ids := make([]string, 0, len(after.Keys))
	for _, k := range after.Keys {
		ids = append(ids, k.APIKeyID)
	}
	names, err := keyNamesByID(ctx, h.db, ids)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup key names failed"})
		return
	}

	resp := previewResponse{
		Effective:    newPreviewEffective(after),
		KeysAffected: make([]previewKeyImpact, 0, len(after.Keys)),
		WouldLockOut: []string{},
	}
	if after.AllowedModels != nil && len(*after.AllowedModels) == 0 {
		resp.WouldLockOut = append(resp.WouldLockOut, "(user)")
	}
	for _, k := range after.Keys {
		name := names[k.APIKeyID]
		resp.KeysAffected = append(resp.KeysAffected, previewKeyImpact{
			ID: k.APIKeyID, Name: name, Before: beforeByKey[k.APIKeyID], After: k.Effective.AllowedModels,
		})
		if k.Effective.AllowedModels != nil && len(*k.Effective.AllowedModels) == 0 {
			resp.WouldLockOut = append(resp.WouldLockOut, name)
		}
	}

	c.JSON(http.StatusOK, resp)
}

// PreviewAPIKeyPolicy handles POST /api/v1/api-keys/:id/routing-policy/preview.
func (h *PolicyHandler) PreviewAPIKeyPolicy(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api key id"})
		return
	}
	ctx := c.Request.Context()
	info, err := lookupAPIKey(ctx, h.db, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check api key failed"})
		return
	}
	if info == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		return
	}

	var req policyObject
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.AllowedModels != nil {
		if err := validateAllowedModelIDs(ctx, h.db, *req.AllowedModels); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if !validPrincipalResidency(req.Residency) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "residency must be null or 'self_hosted_only'"})
		return
	}

	before, err := h.gateway.effectiveWithOverride(ctx, info.UserID, id, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway effective policy unavailable: " + err.Error()})
		return
	}
	after, err := h.gateway.effectiveWithOverride(ctx, info.UserID, id, &gatewayPolicyOverride{Level: principalTypeAPIKey, Policy: &req})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway effective policy unavailable: " + err.Error()})
		return
	}
	if req.DefaultModel != nil {
		if err := validateDefaultModel(ctx, h.db, *req.DefaultModel, after.AllowedModels); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	resp := previewResponse{
		Effective:    newPreviewEffective(after),
		KeysAffected: []previewKeyImpact{{ID: id, Name: info.Name, Before: before.AllowedModels, After: after.AllowedModels}},
		WouldLockOut: []string{},
	}
	if after.AllowedModels != nil && len(*after.AllowedModels) == 0 {
		resp.WouldLockOut = append(resp.WouldLockOut, info.Name)
	}
	c.JSON(http.StatusOK, resp)
}

// principalListEntry is one row in GET /api/v1/routing-policy/principals.
type principalListEntry struct {
	PrincipalType   string       `json:"principal_type"`
	PrincipalID     string       `json:"principal_id"`
	Display         string       `json:"display"`
	Policy          policyObject `json:"policy"`
	UpdatedAt       time.Time    `json:"updated_at"`
	ReachableModels []string     `json:"reachable_models,omitempty"`
	EffectiveError  string       `json:"effective_error,omitempty"`
}

// ListPrincipalPolicies handles GET /api/v1/routing-policy/principals
// (admin): every explicit user/key policy row, paginated (default 50, max
// 200 per page). For each row on the page it calls the gateway's effective
// endpoint to attach reachable_models -- a per-principal call, capped at 200
// per page by construction, per DESIGN4's contract. A single principal's
// gateway call failing degrades that row (effective_error set,
// reachable_models omitted) rather than failing the whole page, since an
// admin overview listing up to 200 rows should not 502 entirely over one
// slow/unreachable lookup.
func (h *PolicyHandler) ListPrincipalPolicies(c *gin.Context) {
	limit := principalsListDefaultLimit
	if v := c.Query("limit"); v != "" {
		if n, err := parsePositiveInt(v); err == nil {
			limit = n
		}
	}
	if limit > principalsListMaxLimit {
		limit = principalsListMaxLimit
	}
	if limit <= 0 {
		limit = principalsListDefaultLimit
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := parsePositiveInt(v); err == nil {
			offset = n
		}
	}

	ctx := c.Request.Context()

	var total int
	if err := h.db.QueryRow(ctx, `SELECT COUNT(*) FROM principal_routing_policies WHERE principal_type IN ('user','api_key')`).Scan(&total); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "count principals failed"})
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT p.principal_type, COALESCE(p.user_id::text, p.api_key_id::text),
			COALESCE(u.email, k.name || ' (' || ku.email || ')'),
			p.residency, p.allowed_models, p.default_model, p.updated_at,
			p.user_id, p.api_key_id
		FROM principal_routing_policies p
		LEFT JOIN users u ON u.id = p.user_id
		LEFT JOIN api_keys k ON k.id = p.api_key_id
		LEFT JOIN users ku ON ku.id = k.user_id
		WHERE p.principal_type IN ('user','api_key')
		ORDER BY p.updated_at DESC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list principals failed"})
		return
	}

	type rawRow struct {
		entry    principalListEntry
		userID   *string
		apiKeyID *string
	}
	var raw []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.entry.PrincipalType, &r.entry.PrincipalID, &r.entry.Display,
			&r.entry.Policy.Residency, &r.entry.Policy.AllowedModels, &r.entry.Policy.DefaultModel, &r.entry.UpdatedAt,
			&r.userID, &r.apiKeyID); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan principal failed"})
			return
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "principal rows error"})
		return
	}
	rows.Close()

	entries := make([]principalListEntry, 0, len(raw))
	for _, r := range raw {
		var userID string
		var apiKeyID *string
		if r.userID != nil {
			userID = *r.userID
		} else if r.apiKeyID != nil {
			apiKeyID = r.apiKeyID
			// Look up the owning user id for the gateway call — the
			// effective endpoint always keys off user_id, with api_key_id
			// narrowing further.
			info, err := lookupAPIKey(ctx, h.db, *r.apiKeyID)
			if err != nil || info == nil {
				r.entry.EffectiveError = "owner lookup failed"
				entries = append(entries, r.entry)
				continue
			}
			userID = info.UserID
		}
		eff, err := h.gateway.effective(ctx, userID, apiKeyID)
		if err != nil {
			r.entry.EffectiveError = err.Error()
		} else {
			r.entry.ReachableModels = eff.ReachableModels
		}
		entries = append(entries, r.entry)
	}

	c.JSON(http.StatusOK, gin.H{
		"principals": entries,
		"total":      total,
		"limit":      limit,
		"offset":     offset,
	})
}

func parsePositiveInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
