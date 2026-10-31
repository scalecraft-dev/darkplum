package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft-dev/darkplum/api/internal/activity"
)

// aliasRe mirrors the CHECK constraint on model_aliases.alias in migration
// 010_model_routing.sql: lowercase alphanumeric, '.', '_', '-', starting with
// an alphanumeric character, 1-63 characters total.
var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// routeNameRe mirrors the CHECK constraint on model_alias_routes.route_name
// in migration 013_routed_aliases.sql: lowercase alphanumeric, '_', '-',
// starting with an alphanumeric character, 1-32 characters total.
var routeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

const (
	routeDescriptionMaxLen = 500 // mirrors model_alias_routes.description's CHECK
	routeKeywordMaxLen     = 64  // mirrors migration 013's "each keyword <= 64 chars"
	routeKeywordsMaxCount  = 32  // mirrors model_alias_routes.keywords' cardinality CHECK
	// routesMaxCount bounds a routed alias: every route is embedded in every
	// /v1/models response and listed in the chat model picker, and no
	// classifier distinguishes dozens of intents usefully.
	routesMaxCount = 32

	aliasTestMaxTextBytes = 16 * 1024 // 16 KiB, per DESIGN3's /model-aliases/:alias/test contract

	// classifierKindSemanticRouter mirrors model_aliases.classifier_kind's
	// 'semantic_router' value (migration 010, narrowed to two values by 014).
	// It is the only classifier kind that triggers a gateway health probe for
	// the "Semantic Router is not installed" warning — see
	// semanticRouterNeedsWarning.
	classifierKindSemanticRouter = "semantic_router"
)

// validAliasFormat is a pure predicate so alias validation can be unit tested
// without a database; the same shape is also enforced by the DB CHECK
// constraint as a second, authoritative line of defense.
func validAliasFormat(alias string) bool {
	return aliasRe.MatchString(alias)
}

// validResidency is a pure predicate mirroring the org row's residency
// domain (principal_routing_policies.residency, migration 016's widened
// CHECK): the full three-rung ladder, since the org level is the one place
// "any" is an explicit, settable value rather than "inherit".
func validResidency(v string) bool {
	return v == residencyAny || v == residencyCloudAccountOnly || v == residencySelfHostedOnly
}

// validRouteName is a pure predicate mirroring model_alias_routes.route_name's
// CHECK constraint (migration 013).
func validRouteName(s string) bool {
	return routeNameRe.MatchString(s)
}

// validRouteDescription mirrors model_alias_routes.description's CHECK
// constraint (char_length BETWEEN 1 AND 500). Uses rune count, not byte
// length, to match Postgres's char_length semantics.
func validRouteDescription(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= routeDescriptionMaxLen
}

// normalizeKeyword trims and case-folds one route keyword per migration
// 013's "keyword items: each <=64 chars, non-empty after trim, case-folded on
// write" rule, and reports whether the result is valid. It is pure so
// keyword normalization is unit-testable without a database.
func normalizeKeyword(kw string) (string, bool) {
	trimmed := strings.TrimSpace(kw)
	if trimmed == "" {
		return "", false
	}
	if utf8.RuneCountInString(trimmed) > routeKeywordMaxLen {
		return "", false
	}
	return strings.ToLower(trimmed), true
}

// semanticRouterHealthCacheTTL bounds how often PutAlias/ListAliases probe
// the gateway's own unauthenticated GET /health for Semantic Router status,
// mirroring the gateway's own SemanticRouterClassifier.HealthStatus cache —
// a cheap cached probe, not a per-request round trip.
const semanticRouterHealthCacheTTL = 30 * time.Second

// semanticRouterProbeTimeout bounds one health probe call.
const semanticRouterProbeTimeout = 2 * time.Second

// semanticRouterNotConfiguredWarning is the exact warning text surfaced on a
// routed alias using classifier_kind='semantic_router' when the gateway's
// own /health reports routing.classifier.semantic_router.status != "configured".
const semanticRouterNotConfiguredWarning = "Semantic Router is not installed on the gateway — turn the semantic router on with 'darkplum init', then run 'darkplum deploy'. Until then this alias always uses its default route."

// RoutingHandler serves the model-alias and routing-policy admin endpoints.
type RoutingHandler struct {
	db       *pgxpool.Pool
	activity *activity.Recorder
	// availabilityWindow must match ModelHandler's so an alias's
	// target_available flips at the same moment as the model's available.
	availabilityWindow time.Duration

	// gatewayURL and serviceKey are used by TestAlias to proxy an admin's
	// "test a prompt" request to the gateway's routing dryrun endpoint,
	// mirroring ChatHandler/ModelHandler's own gateway-calling fields.
	// gatewayURL is also used, unauthenticated, by semanticRouterNeedsWarning.
	gatewayURL string
	serviceKey string
	httpClient *http.Client
	// gateway is PutRoutingPolicy's lock-out guard: POST /v1/policy/impact,
	// the org-wide counterpart of the POST /v1/policy/effective call the
	// user/key PUTs in policy.go make before writing.
	gateway *gatewayEffectiveClient

	// srProbeMu guards the cached result of the last gateway /health probe
	// (see semanticRouterNeedsWarning): srProbeWarn is valid until
	// srProbeExpiresAt. A probe failure (gateway unreachable, bad response)
	// never fails the calling request — it caches "no warning" for the same
	// TTL and logs once, rather than retrying on every alias in the response.
	srProbeMu        sync.Mutex
	srProbeWarn      bool
	srProbeExpiresAt time.Time
}

// NewRoutingHandler returns a RoutingHandler backed by db. gatewayURL and
// serviceKey are used by TestAlias to call the gateway's routing dryrun
// endpoint with the service credential.
// kickGateway asks the gateway to reload policy (aliases included) now; see
// kickGatewayPolicy.
func (h *RoutingHandler) kickGateway(ctx context.Context) {
	kickGatewayPolicy(ctx, &http.Client{Timeout: gatewayRefreshTimeout}, h.gatewayURL, h.serviceKey)
}

func NewRoutingHandler(db *pgxpool.Pool, rec *activity.Recorder, gatewayURL, serviceKey string) *RoutingHandler {
	return &RoutingHandler{
		db:                 db,
		activity:           rec,
		availabilityWindow: modelAvailabilityWindow,
		gatewayURL:         gatewayURL,
		serviceKey:         serviceKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		gateway: newGatewayEffectiveClient(gatewayURL, serviceKey),
	}
}

// SetAvailabilityWindow applies the operator-configured
// MODEL_AVAILABILITY_WINDOW_SECONDS; see ModelHandler.SetAvailabilityWindow.
func (h *RoutingHandler) SetAvailabilityWindow(w time.Duration) {
	h.availabilityWindow = w
}

// aliasClassifierResponse is the wire shape of a routed alias's classifier.
// ModelID no longer exists (migration 014 dropped model_aliases.
// classifier_model_id along with the classifier_kind='model' value it
// belonged to) -- Kind is always 'rules' or 'semantic_router' now.
type aliasClassifierResponse struct {
	Kind string `json:"kind"`
}

// aliasRouteResponse is the wire shape of one route within a routed alias's
// GET/PUT response.
type aliasRouteResponse struct {
	RouteName          string   `json:"route_name"`
	Description        string   `json:"description"`
	TargetModelID      string   `json:"target_model_id"`
	Keywords           []string `json:"keywords"`
	TargetExists       bool     `json:"target_exists"`
	TargetEnabled      bool     `json:"target_enabled"`
	TargetAvailable    bool     `json:"target_available"`
	TargetBackendClass *string  `json:"target_backend_class"`
}

// aliasResponse is the wire shape for one row in the model-aliases endpoints.
// TargetModelID/TargetExists/TargetEnabled/TargetBackendClass/TargetAvailable
// keep their pre-routed-alias meaning and JSON shape for kind='static'
// (TargetAvailable is false whenever TargetExists is false, or the target row
// exists but hasn't been re-discovered within modelAvailabilityWindow — see
// models_logic.go); for kind='routed' they carry their zero value (no single
// target exists at the alias level) and DefaultRoute/Classifier/Routes are
// populated instead.
type aliasResponse struct {
	Alias              string                   `json:"alias"`
	Kind               string                   `json:"kind"`
	TargetModelID      string                   `json:"target_model_id"`
	TargetExists       bool                     `json:"target_exists"`
	TargetEnabled      bool                     `json:"target_enabled"`
	TargetBackendClass *string                  `json:"target_backend_class"`
	TargetAvailable    bool                     `json:"target_available"`
	DefaultRoute       string                   `json:"default_route,omitempty"`
	OnRouteUnavailable string                   `json:"on_route_unavailable,omitempty"`
	Classifier         *aliasClassifierResponse `json:"classifier,omitempty"`
	Routes             []aliasRouteResponse     `json:"routes,omitempty"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	// Warnings is populated only for a routed alias whose classifier is
	// semantic_router, and only when the gateway's own /health reports its
	// Semantic Router as not "configured" — see
	// RoutingHandler.semanticRouterWarnings. Never populated for a static
	// alias or a rules-classified routed alias.
	Warnings []string `json:"warnings,omitempty"`
}

// rowsQuerier is satisfied by both *pgxpool.Pool and pgx.Tx, mirroring
// rowQuerier (conversations.go) but for multi-row Query calls, so
// loadAliasRoutes can run either as a standalone lookup (ListAliases) or
// inside PutAlias's already-open advisory-lock transaction, reading back the
// routes it just wrote.
type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadAliasRoutes fetches every route for the given routed aliases (ordered
// by position within each alias), joined against models for the same
// target_exists/target_enabled/target_available/target_backend_class shape
// ListAliases and PutAlias have always exposed for a static alias's single
// target. Returns an empty, non-nil map for an empty aliases slice without a
// query round trip.
func loadAliasRoutes(ctx context.Context, q rowsQuerier, aliases []string, now time.Time, window time.Duration) (map[string][]aliasRouteResponse, error) {
	result := make(map[string][]aliasRouteResponse)
	if len(aliases) == 0 {
		return result, nil
	}

	rows, err := q.Query(ctx, `
		SELECT
			r.alias, r.route_name, r.description, r.target_model_id, r.keywords,
			(m.model_id IS NOT NULL) AS target_exists,
			COALESCE(m.enabled, false) AS target_enabled,
			m.backend_class,
			m.discovered_at
		FROM model_alias_routes r
		LEFT JOIN models m ON m.model_id = r.target_model_id
		WHERE r.alias = ANY($1)
		ORDER BY r.alias, r.position
	`, aliases)
	if err != nil {
		return nil, fmt.Errorf("query alias routes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var alias string
		var rr aliasRouteResponse
		var discoveredAt *time.Time
		if err := rows.Scan(&alias, &rr.RouteName, &rr.Description, &rr.TargetModelID, &rr.Keywords,
			&rr.TargetExists, &rr.TargetEnabled, &rr.TargetBackendClass, &discoveredAt); err != nil {
			return nil, fmt.Errorf("scan alias route: %w", err)
		}
		rr.TargetAvailable = discoveredAt != nil && modelIsAvailableWithin(*discoveredAt, now, window)
		result[alias] = append(result[alias], rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alias routes rows: %w", err)
	}
	return result, nil
}

// ListAliases handles GET /api/v1/model-aliases.
func (h *RoutingHandler) ListAliases(c *gin.Context) {
	ctx := c.Request.Context()

	rows, err := h.db.Query(ctx, `
		SELECT
			a.alias,
			a.kind,
			a.target_model_id,
			a.default_route,
			a.classifier_kind,
			a.on_route_unavailable,
			(m.model_id IS NOT NULL) AS target_exists,
			COALESCE(m.enabled, false) AS target_enabled,
			m.backend_class,
			m.discovered_at,
			a.created_at,
			a.updated_at
		FROM model_aliases a
		LEFT JOIN models m ON m.model_id = a.target_model_id
		ORDER BY a.alias
	`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list aliases failed"})
		return
	}
	defer rows.Close()

	now := time.Now()
	aliases := make([]aliasResponse, 0)
	var routedNames []string
	var semanticRouterAliasIdx []int
	for rows.Next() {
		var a aliasResponse
		var targetModelID *string
		var defaultRoute *string
		var classifierKind *string
		var onRouteUnavailable string
		var targetDiscoveredAt *time.Time
		if err := rows.Scan(
			&a.Alias, &a.Kind, &targetModelID, &defaultRoute, &classifierKind, &onRouteUnavailable,
			&a.TargetExists, &a.TargetEnabled, &a.TargetBackendClass, &targetDiscoveredAt, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan alias failed"})
			return
		}
		if targetModelID != nil {
			a.TargetModelID = *targetModelID
		}
		a.TargetAvailable = targetDiscoveredAt != nil && modelIsAvailableWithin(*targetDiscoveredAt, now, h.availabilityWindow)
		if a.Kind == aliasKindRouted {
			if defaultRoute != nil {
				a.DefaultRoute = *defaultRoute
			}
			a.OnRouteUnavailable = onRouteUnavailable
			if classifierKind != nil {
				a.Classifier = &aliasClassifierResponse{Kind: *classifierKind}
				if *classifierKind == classifierKindSemanticRouter {
					semanticRouterAliasIdx = append(semanticRouterAliasIdx, len(aliases))
				}
			}
			routedNames = append(routedNames, a.Alias)
		}
		aliases = append(aliases, a)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	routesByAlias, err := loadAliasRoutes(ctx, h.db, routedNames, now, h.availabilityWindow)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load alias routes failed"})
		return
	}
	for i := range aliases {
		if aliases[i].Kind == aliasKindRouted {
			aliases[i].Routes = routesByAlias[aliases[i].Alias]
		}
	}

	if len(semanticRouterAliasIdx) > 0 && h.semanticRouterNeedsWarning(ctx) {
		for _, idx := range semanticRouterAliasIdx {
			aliases[idx].Warnings = []string{semanticRouterNotConfiguredWarning}
		}
	}

	c.JSON(http.StatusOK, aliases)
}

// putAliasRouteInput is one route as given in a PUT /model-aliases/:alias
// body for a routed alias.
type putAliasRouteInput struct {
	RouteName     string   `json:"route_name"`
	Description   string   `json:"description"`
	TargetModelID string   `json:"target_model_id"`
	Keywords      []string `json:"keywords"`
}

// putAliasClassifierInput is the "classifier" object in a routed PUT body.
type putAliasClassifierInput struct {
	Kind    string  `json:"kind"`
	ModelID *string `json:"model_id,omitempty"`
}

// putAliasRequest is the union of the static and routed PUT bodies: a static
// body is exactly {"target_model_id": "..."} (kind omitted or "static"); a
// routed body is {"kind":"routed","default_route":...,"classifier":{...},
// "routes":[...]}.
type putAliasRequest struct {
	Kind          string `json:"kind,omitempty"`
	TargetModelID string `json:"target_model_id,omitempty"`
	DefaultRoute  string `json:"default_route,omitempty"`
	// OnRouteUnavailable is optional: omitted keeps the alias's current
	// value on replace and takes the column default on create, so older
	// clients that never send it neither fail nor silently change it.
	OnRouteUnavailable *string                  `json:"on_route_unavailable,omitempty"`
	Classifier         *putAliasClassifierInput `json:"classifier,omitempty"`
	Routes             []putAliasRouteInput     `json:"routes,omitempty"`
}

// onRouteUnavailable values; they mirror model_aliases'
// on_route_unavailable CHECK (migration 027) and the gateway's
// policy.OnRouteUnavailable* constants.
const (
	onRouteUnavailableRefuse         = "refuse"
	onRouteUnavailableSameOrStricter = "fallback_same_or_stricter"
	onRouteUnavailableAnyPermitted   = "fallback_any_permitted"
)

func validOnRouteUnavailable(v string) bool {
	switch v {
	case onRouteUnavailableRefuse, onRouteUnavailableSameOrStricter, onRouteUnavailableAnyPermitted:
		return true
	}
	return false
}

// validateRoutedAliasShape checks every format-level 400 rule for a routed
// alias's PUT body that does not require a database round trip: route count,
// route name format, description length, unique route names, default_route
// membership, classifier kind/model_id shape, and keyword bounds. It also
// normalizes each route's keywords (trim + case-fold per migration 013).
// DB-dependent checks (target existence, alias chaining, classifier model
// backend class, residency) happen separately in PutAlias, inside the
// advisory-lock transaction. Pure so every rule is unit-testable without a
// database.
func validateRoutedAliasShape(req putAliasRequest) ([]putAliasRouteInput, error) {
	if len(req.Routes) == 0 {
		return nil, errors.New("at least one route is required")
	}
	if len(req.Routes) > routesMaxCount {
		return nil, fmt.Errorf("at most %d routes are allowed", routesMaxCount)
	}

	seenNames := make(map[string]bool, len(req.Routes))
	normalized := make([]putAliasRouteInput, len(req.Routes))
	for i, rt := range req.Routes {
		if !validRouteName(rt.RouteName) {
			return nil, fmt.Errorf("routes[%d]: invalid route_name %q", i, rt.RouteName)
		}
		if seenNames[rt.RouteName] {
			return nil, fmt.Errorf("duplicate route name: %s", rt.RouteName)
		}
		seenNames[rt.RouteName] = true

		if !validRouteDescription(rt.Description) {
			return nil, fmt.Errorf("routes[%d]: description must be 1-%d characters", i, routeDescriptionMaxLen)
		}
		if rt.TargetModelID == "" {
			return nil, fmt.Errorf("routes[%d]: target_model_id is required", i)
		}
		if len(rt.Keywords) > routeKeywordsMaxCount {
			return nil, fmt.Errorf("routes[%d]: at most %d keywords are allowed", i, routeKeywordsMaxCount)
		}

		normKeywords := make([]string, 0, len(rt.Keywords))
		seenKeywords := make(map[string]bool, len(rt.Keywords))
		for _, kw := range rt.Keywords {
			nk, ok := normalizeKeyword(kw)
			if !ok {
				return nil, fmt.Errorf("routes[%d]: keyword %q must be non-empty and at most %d characters", i, kw, routeKeywordMaxLen)
			}
			// "dup", "DUP" and " dup " all normalise to one keyword.
			if seenKeywords[nk] {
				continue
			}
			seenKeywords[nk] = true
			normKeywords = append(normKeywords, nk)
		}

		normalized[i] = putAliasRouteInput{
			RouteName:     rt.RouteName,
			Description:   rt.Description,
			TargetModelID: rt.TargetModelID,
			Keywords:      normKeywords,
		}
	}

	if req.DefaultRoute == "" {
		return nil, errors.New("default_route is required")
	}
	if !seenNames[req.DefaultRoute] {
		return nil, fmt.Errorf("default_route %q must reference one of the routes", req.DefaultRoute)
	}

	if req.OnRouteUnavailable != nil && !validOnRouteUnavailable(*req.OnRouteUnavailable) {
		return nil, fmt.Errorf("on_route_unavailable must be one of %s, %s, %s", onRouteUnavailableRefuse, onRouteUnavailableSameOrStricter, onRouteUnavailableAnyPermitted)
	}

	if req.Classifier == nil {
		return nil, errors.New("classifier is required")
	}
	switch req.Classifier.Kind {
	case "rules", "semantic_router":
		if req.Classifier.ModelID != nil && *req.Classifier.ModelID != "" {
			return nil, fmt.Errorf("classifier.model_id must not be set when classifier.kind is %q", req.Classifier.Kind)
		}
	case "model":
		// Owner decision, ratified by product: the in-house "model" classifier
		// (an Arch-Router prompt-protocol re-implementation) is removed.
		return nil, errors.New("classifier kind 'model' is no longer supported; use 'rules' or 'semantic_router'")
	default:
		return nil, errors.New("classifier.kind must be 'rules' or 'semantic_router'")
	}

	return normalized, nil
}

// PutAlias handles PUT /api/v1/model-aliases/:alias — create or replace an
// alias, either static (a single target model) or routed (an ordered set of
// named routes selected per request by a classifier; see
// validateRoutedAliasShape and DESIGN3's API contract).
//
// The whole handler runs in one transaction that opens by taking
// modelRoutingAdvisoryLockKey (see models_logic.go), the same lock
// ModelHandler.SyncModels takes around its upsert transaction. Without this,
// the model-id conflict check (below) and the alias upsert are separate
// statements a concurrent SyncModels run can interleave with: SyncModels
// could insert a model row for this exact id in the gap between the conflict
// check and the upsert, leaving both a model row and an alias row for the
// same id. The lock serialises the two transactions so one of them always
// completes (and the other observes its result) before proceeding. For a
// routed alias, default_route's membership in routes and every route's
// write happen inside the same transaction, so a reader never observes an
// alias with a default_route that doesn't exist yet (or a partial route set).
func (h *RoutingHandler) PutAlias(c *gin.Context) {
	alias := c.Param("alias")
	if !validAliasFormat(alias) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid alias format"})
		return
	}

	var req putAliasRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	kind := aliasKindStatic
	switch req.Kind {
	case "", aliasKindStatic:
		kind = aliasKindStatic
	case aliasKindRouted:
		kind = aliasKindRouted
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be 'static' or 'routed'"})
		return
	}

	var normalizedRoutes []putAliasRouteInput
	if kind == aliasKindStatic {
		if req.TargetModelID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target_model_id is required"})
			return
		}
		if req.OnRouteUnavailable != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "on_route_unavailable applies only to routed aliases (kind \"routed\"); a static alias has no routes to fall back between"})
			return
		}
	} else {
		var verr error
		normalizedRoutes, verr = validateRoutedAliasShape(req)
		if verr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": verr.Error()})
			return
		}
	}

	ctx := c.Request.Context()

	tx, err := h.db.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "begin transaction failed"})
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, modelRoutingAdvisoryLockKey); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "acquire routing lock failed"})
		return
	}

	// 409: the alias name would shadow a real model id. The gateway resolves
	// alias-vs-model collisions by letting the alias win (admin intent is
	// authoritative), but allowing that ambiguity to be created here would
	// just be confusing, so it's rejected outright.
	var modelIDConflict bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM models WHERE model_id = $1)`, alias,
	).Scan(&modelIDConflict); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check alias conflict failed"})
		return
	}
	if modelIDConflict {
		c.JSON(http.StatusConflict, gin.H{"error": "alias conflicts with an existing model id"})
		return
	}

	if kind == aliasKindRouted {
		h.putRoutedAlias(c, tx, alias, req, normalizedRoutes)
		return
	}
	h.putStaticAlias(c, tx, alias, req)
}

// putStaticAlias is the kind='static' half of PutAlias: unchanged behaviour
// from before routed aliases existed, except the upsert now also resets
// default_route/classifier_kind to NULL and clears any leftover
// model_alias_routes rows, so retargeting a previously-routed alias back to
// static leaves no orphaned routing state behind.
func (h *RoutingHandler) putStaticAlias(c *gin.Context, tx pgx.Tx, alias string, req putAliasRequest) {
	ctx := c.Request.Context()

	// 400: no alias chains — the target must not itself be an alias.
	var targetIsAlias bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM model_aliases WHERE alias = $1)`, req.TargetModelID,
	).Scan(&targetIsAlias); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check alias target failed"})
		return
	}
	if targetIsAlias {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target is itself an alias"})
		return
	}

	// 404: the target must be a currently known model.
	var targetEnabled bool
	var targetBackendClass string
	var targetDiscoveredAt time.Time
	err := tx.QueryRow(ctx,
		`SELECT enabled, backend_class, discovered_at FROM models WHERE model_id = $1`, req.TargetModelID,
	).Scan(&targetEnabled, &targetBackendClass, &targetDiscoveredAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "target model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check target model failed"})
		return
	}

	// 409: the same residency rule the routed path applies -- an alias to
	// a model the org's residency refuses would be accepted here and then
	// refused on every chat. Read from the authoritative org row, never the
	// legacy mirror (see putRoutedAlias for why).
	var dbResidency *string
	if err := tx.QueryRow(ctx, `SELECT residency FROM principal_routing_policies WHERE principal_type = 'org'`).Scan(&dbResidency); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check routing policy failed"})
		return
	}
	if residency := orgResidencyString(dbResidency); !residencyPermitsClass(residency, targetBackendClass) {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("residency policy is %s; alias targets a model of class %s, %q", residency, targetBackendClass, req.TargetModelID),
		})
		return
	}

	if _, err := tx.Exec(ctx, `DELETE FROM model_alias_routes WHERE alias = $1`, alias); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "clear stale routes failed"})
		return
	}

	var row aliasResponse
	var created bool
	err = tx.QueryRow(ctx, `
		INSERT INTO model_aliases (alias, kind, target_model_id, default_route, classifier_kind, created_at, updated_at)
		VALUES ($1, 'static', $2, NULL, NULL, now(), now())
		ON CONFLICT (alias) DO UPDATE
			SET kind = 'static', target_model_id = EXCLUDED.target_model_id,
				default_route = NULL, classifier_kind = NULL,
				updated_at = now()
		RETURNING alias, target_model_id, created_at, updated_at, (xmax = 0)
	`, alias, req.TargetModelID).Scan(&row.Alias, &row.TargetModelID, &row.CreatedAt, &row.UpdatedAt, &created)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upsert alias failed"})
		return
	}
	row.Kind = aliasKindStatic
	row.TargetExists = true
	row.TargetEnabled = targetEnabled
	row.TargetBackendClass = &targetBackendClass
	row.TargetAvailable = modelIsAvailableWithin(targetDiscoveredAt, time.Now(), h.availabilityWindow)

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "commit alias failed"})
		return
	}

	h.kickGateway(ctx)
	evtType := activity.ModelAliasUpdated
	if created {
		evtType = activity.ModelAliasCreated
	}
	evt := activity.FromContext(c, evtType)
	evt.TargetType = activity.StrPtr("model_alias")
	// activity_events.target_id is a UUID column; a model alias is an
	// arbitrary TEXT identifier (e.g. "default"), not a UUID, so it cannot go
	// in TargetID (the insert would fail on a type mismatch and the write
	// would be silently dropped — Recorder.Record only logs errors). Carry it
	// in Detail instead, alongside the target model id.
	evt.Detail = map[string]any{"alias": alias, "target_model_id": req.TargetModelID, "kind": aliasKindStatic, "route_count": 0}
	h.activity.Record(ctx, evt)

	c.JSON(http.StatusOK, row)
}

// putRoutedAlias is the kind='routed' half of PutAlias. All DB-dependent
// validation (target existence, alias chaining, classifier model backend
// class, residency) runs inside tx, so a concurrent SyncModels/PutAlias run
// serialised by modelRoutingAdvisoryLockKey always sees a consistent view.
func (h *RoutingHandler) putRoutedAlias(c *gin.Context, tx pgx.Tx, alias string, req putAliasRequest, routes []putAliasRouteInput) {
	ctx := c.Request.Context()

	targetIDs := make([]string, len(routes))
	for i, rt := range routes {
		targetIDs[i] = rt.TargetModelID
	}

	type targetInfo struct {
		backendClass string
	}
	targetsByID := make(map[string]targetInfo, len(targetIDs))
	rows, err := tx.Query(ctx, `SELECT model_id, backend_class FROM models WHERE model_id = ANY($1)`, targetIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check route targets failed"})
		return
	}
	for rows.Next() {
		var id, bc string
		if err := rows.Scan(&id, &bc); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan route target failed"})
			return
		}
		targetsByID[id] = targetInfo{backendClass: bc}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "route targets rows error"})
		return
	}
	rows.Close()

	// 400: no alias chains — a route's target must not itself be an alias.
	aliasTargets := make(map[string]bool)
	aliasRows, err := tx.Query(ctx, `SELECT alias FROM model_aliases WHERE alias = ANY($1)`, targetIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check route target aliasing failed"})
		return
	}
	for aliasRows.Next() {
		var a string
		if err := aliasRows.Scan(&a); err != nil {
			aliasRows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan route target alias failed"})
			return
		}
		aliasTargets[a] = true
	}
	if err := aliasRows.Err(); err != nil {
		aliasRows.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "route target alias rows error"})
		return
	}
	aliasRows.Close()

	// Alias-chaining is checked before existence, matching putStaticAlias's
	// precedent: a route target that names an alias is never present in
	// models (aliases and models are disjoint id spaces once an alias
	// exists), so checking existence first would misreport a chained target
	// as "not found" instead of "itself an alias".
	for _, rt := range routes {
		if aliasTargets[rt.TargetModelID] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target is itself an alias: " + rt.TargetModelID})
			return
		}
		if _, ok := targetsByID[rt.TargetModelID]; !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "target model not found: " + rt.TargetModelID})
			return
		}
	}

	// 409: one or more routes target a model of a class the CURRENT org
	// residency refuses. The per-request check in the gateway still enforces
	// later flips (an admin loosening residency back doesn't need to touch
	// this alias again).
	//
	// Reads principal_routing_policies directly, NOT the legacy routing_policy
	// mirror: since migration 016, that mirror DOWNGRADES cloud_account_only
	// to 'any' for an old gateway's benefit (see that migration's comment),
	// so reading it here would silently let a cloud_account_only org create a
	// routed alias with an external_api route it will never actually be able
	// to reach.
	var dbResidency *string
	if err := tx.QueryRow(ctx, `SELECT residency FROM principal_routing_policies WHERE principal_type = 'org'`).Scan(&dbResidency); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check routing policy failed"})
		return
	}
	residency := orgResidencyString(dbResidency)
	var conflicts []string
	for _, rt := range routes {
		class := targetsByID[rt.TargetModelID].backendClass
		if !residencyPermitsClass(residency, class) {
			conflicts = append(conflicts, fmt.Sprintf("route %q targets a %s model %q", rt.RouteName, class, rt.TargetModelID))
		}
	}
	if len(conflicts) > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("residency policy is %s; %s", residency, strings.Join(conflicts, "; ")),
		})
		return
	}

	// The value being replaced, for the audit event: "" when the alias is new.
	// Read inside the transaction, under the routing advisory lock, so it is
	// the value this upsert actually overwrites.
	var previousOnRouteUnavailable string
	if err := tx.QueryRow(ctx, `SELECT on_route_unavailable FROM model_aliases WHERE alias = $1`, alias).Scan(&previousOnRouteUnavailable); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read current alias setting failed"})
		return
	}

	var createdAt, updatedAt time.Time
	var created bool
	var onRouteUnavailable string
	err = tx.QueryRow(ctx, `
		INSERT INTO model_aliases (alias, kind, target_model_id, default_route, classifier_kind, on_route_unavailable, created_at, updated_at)
		VALUES ($1, 'routed', NULL, $2, $3, COALESCE($4::text, 'fallback_same_or_stricter'), now(), now())
		ON CONFLICT (alias) DO UPDATE
			SET kind = 'routed', target_model_id = NULL, default_route = EXCLUDED.default_route,
				classifier_kind = EXCLUDED.classifier_kind,
				on_route_unavailable = COALESCE($4::text, model_aliases.on_route_unavailable),
				updated_at = now()
		RETURNING created_at, updated_at, (xmax = 0), on_route_unavailable
	`, alias, req.DefaultRoute, req.Classifier.Kind, req.OnRouteUnavailable).Scan(&createdAt, &updatedAt, &created, &onRouteUnavailable)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upsert alias failed"})
		return
	}

	// Routes are replaced wholesale: delete then re-insert the full set in
	// the same transaction as the alias upsert, so a reader never observes a
	// routed alias with a stale or partial route list.
	if _, err := tx.Exec(ctx, `DELETE FROM model_alias_routes WHERE alias = $1`, alias); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "clear old routes failed"})
		return
	}
	for i, rt := range routes {
		if _, err := tx.Exec(ctx, `
			INSERT INTO model_alias_routes (alias, route_name, description, target_model_id, position, keywords)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, alias, rt.RouteName, rt.Description, rt.TargetModelID, i, rt.Keywords); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "insert route failed"})
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "commit alias failed"})
		return
	}

	now := time.Now()
	routesByAlias, err := loadAliasRoutes(ctx, h.db, []string{alias}, now, h.availabilityWindow)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load alias routes failed"})
		return
	}

	resp := aliasResponse{
		Alias:        alias,
		Kind:         aliasKindRouted,
		DefaultRoute: req.DefaultRoute,
		Classifier:   &aliasClassifierResponse{Kind: req.Classifier.Kind},
		Routes:       routesByAlias[alias],
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,

		OnRouteUnavailable: onRouteUnavailable,
	}
	if req.Classifier.Kind == classifierKindSemanticRouter && h.semanticRouterNeedsWarning(ctx) {
		resp.Warnings = []string{semanticRouterNotConfiguredWarning}
	}

	h.kickGateway(ctx)
	evtType := activity.ModelAliasUpdated
	if created {
		evtType = activity.ModelAliasCreated
	}
	evt := activity.FromContext(c, evtType)
	evt.TargetType = activity.StrPtr("model_alias")
	evt.Detail = map[string]any{"alias": alias, "kind": aliasKindRouted, "route_count": len(routes),
		"on_route_unavailable": onRouteUnavailable, "previous_on_route_unavailable": previousOnRouteUnavailable}
	h.activity.Record(ctx, evt)

	c.JSON(http.StatusOK, resp)
}

// DeleteAlias handles DELETE /api/v1/model-aliases/:alias. Routes are
// removed automatically via model_alias_routes' ON DELETE CASCADE (migration
// 013); kind and route_count are read before the delete purely to enrich the
// activity event, not because the cascade needs them.
func (h *RoutingHandler) DeleteAlias(c *gin.Context) {
	alias := c.Param("alias")
	ctx := c.Request.Context()

	var kind string
	var routeCount int
	err := h.db.QueryRow(ctx, `
		SELECT a.kind, (SELECT COUNT(*) FROM model_alias_routes r WHERE r.alias = a.alias)
		FROM model_aliases a WHERE a.alias = $1
	`, alias).Scan(&kind, &routeCount)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check alias failed"})
		return
	}

	// 409: an alias that is somebody's default_model cannot simply vanish --
	// /v1/models would preselect a name that is not in its own list and the
	// next org/principal save would fail on "unknown model or alias id".
	var referencedBy int
	if err := h.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM principal_routing_policies WHERE default_model = $1`, alias,
	).Scan(&referencedBy); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check alias references failed"})
		return
	}
	if referencedBy > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("alias %q is the default model of %d routing polic%s; change those first", alias, referencedBy, map[bool]string{true: "y", false: "ies"}[referencedBy == 1]),
		})
		return
	}

	tag, err := h.db.Exec(ctx, `DELETE FROM model_aliases WHERE alias = $1`, alias)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete alias failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "alias not found"})
		return
	}
	h.kickGateway(ctx)

	evt := activity.FromContext(c, activity.ModelAliasDeleted)
	evt.TargetType = activity.StrPtr("model_alias")
	// See PutAlias: target_id is UUID-typed, alias is not — carry it in Detail.
	evt.Detail = map[string]any{"alias": alias, "kind": kind, "route_count": routeCount}
	h.activity.Record(ctx, evt)

	c.Status(http.StatusNoContent)
}

// testAliasRequest is the body of POST /api/v1/model-aliases/:alias/test.
type testAliasRequest struct {
	Text string `json:"text"`
}

// TestAlias handles POST /api/v1/model-aliases/:alias/test (admin-only) —
// proxies an admin's "test a prompt" request to the gateway's routing dryrun
// endpoint (POST {GATEWAY_URL}/v1/routing/dryrun), which classifies the text
// against the alias's routes and reports the outcome without ever calling a
// target model (see DESIGN3's Audit section). The alias's own existence is
// checked here first — a nonexistent alias 404s exactly like the other
// alias endpoints, before ever making a network call — but the response
// otherwise passes the gateway's status code and body through byte-for-byte,
// including a 400 if the gateway's own (possibly staler) snapshot considers
// the alias not routed.
func (h *RoutingHandler) TestAlias(c *gin.Context) {
	alias := c.Param("alias")

	var req testAliasRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Text == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text is required"})
		return
	}
	if len(req.Text) > aliasTestMaxTextBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("text exceeds %d bytes", aliasTestMaxTextBytes)})
		return
	}

	ctx := c.Request.Context()

	var exists bool
	if err := h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_aliases WHERE alias = $1)`, alias).Scan(&exists); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check alias failed"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "alias not found"})
		return
	}

	body, err := json.Marshal(map[string]string{"alias": alias, "text": req.Text})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encode dryrun request failed"})
		return
	}

	target := fmt.Sprintf("%s/v1/routing/dryrun", h.gatewayURL)
	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "build dryrun request failed"})
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("Authorization", "Bearer "+h.serviceKey)
	proxyReq.Header.Set("X-Darkplum-User-Id", c.GetString("user_id"))
	proxyReq.Header.Set("X-Request-Id", c.GetString("request_id"))

	resp, err := h.httpClient.Do(proxyReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway unavailable"})
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "read gateway response failed"})
		return
	}

	c.Data(resp.StatusCode, "application/json", respBody)
}

// routingPolicyResponse is the wire shape for GET/PUT /api/v1/routing-policy
// (the org-level policy row; migration 015_principal_policies.sql).
// AllowedModels/DefaultModel were added alongside per-principal policy --
// Residency stays a plain (non-nullable) string here, unlike the
// user/api_key shape, since "any" IS a value an admin can explicitly choose
// at the org level (there's nothing above org to inherit from). Warnings is
// populated only by PutRoutingPolicy, and only when the new residency is not
// 'any' and at least one routed alias has a route targeting a model of a
// class that residency refuses (residencyRouteWarnings) — that route will be
// refused per-request from now on, even though the PUT itself still succeeds
// with 200. AffectedPrincipals and
// OrgNarrowed are set only when the PUT tightens the org policy (see
// orgPolicyTightened) and the gateway could evaluate it: AffectedPrincipals
// is the number of users and keys WITH A POLICY OF THEIR OWN whose reachable
// set actually shrank (not "has a row", the earlier approximation), and
// OrgNarrowed says whether the organisation's own reachable set shrank --
// i.e. whether everyone who simply inherits now reaches fewer models. Both
// come from the gateway's POST /v1/policy/impact, the same merge that
// enforces.
type routingPolicyResponse struct {
	Residency     string    `json:"residency"`
	AllowedModels *[]string `json:"allowed_models"`
	DefaultModel  *string   `json:"default_model"`
	// ParamsMode is "lenient" (a request parameter a backend cannot honour is
	// dropped and recorded) or "strict" (it is refused). NULL stores as
	// lenient. Org level only.
	ParamsMode string    `json:"params_mode"`
	UpdatedAt  time.Time `json:"updated_at"`
	Warnings   []string  `json:"warnings,omitempty"`
	// AlreadyLockedOut names principals with nothing reachable both before
	// and after this save -- not caused by it (so not a 409), but shown.
	AlreadyLockedOut   []string `json:"already_locked_out,omitempty"`
	AffectedPrincipals *int     `json:"affected_principals,omitempty"`
	OrgNarrowed        *bool    `json:"org_narrowed,omitempty"`
	// UnresolvedModels lists enabled, allowed models the gateway has not yet
	// seen a backend for, when the new residency is self_hosted_only: their
	// class is unknown, so they are neither counted as lost nor as kept. Any
	// that turn out cloud-managed will be refused. Omitted when empty.
	UnresolvedModels []string `json:"unresolved_models,omitempty"`
}

// orgResidencyString converts the org row's nullable residency column
// (NULL = any) to the wire value.
func orgResidencyString(dbResidency *string) string {
	if dbResidency == nil {
		return residencyAny
	}
	return *dbResidency
}

// params_mode values (principal_routing_policies.params_mode, migration 029).
const (
	paramsModeLenient = "lenient"
	paramsModeStrict  = "strict"
)

// validParamsMode mirrors the column's CHECK.
func validParamsMode(v string) bool { return v == paramsModeLenient || v == paramsModeStrict }

// paramsModeString converts the nullable column to the wire value (NULL is
// lenient).
func paramsModeString(db *string) string {
	if db == nil {
		return paramsModeLenient
	}
	return *db
}

// GetRoutingPolicy handles GET /api/v1/routing-policy.
func (h *RoutingHandler) GetRoutingPolicy(c *gin.Context) {
	var dbResidency, dbParamsMode *string
	var resp routingPolicyResponse
	err := h.db.QueryRow(c.Request.Context(),
		`SELECT residency, allowed_models, default_model, params_mode, updated_at FROM principal_routing_policies WHERE principal_type = 'org'`,
	).Scan(&dbResidency, &resp.AllowedModels, &resp.DefaultModel, &dbParamsMode, &resp.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get routing policy failed"})
		return
	}
	resp.Residency = orgResidencyString(dbResidency)
	resp.ParamsMode = paramsModeString(dbParamsMode)
	c.JSON(http.StatusOK, resp)
}

type putRoutingPolicyRequest struct {
	Residency     string    `json:"residency"`
	AllowedModels *[]string `json:"allowed_models"`
	DefaultModel  *string   `json:"default_model"`
	// ParamsMode is optional: omitted keeps the current value, so older
	// clients that never send it neither fail nor change it.
	ParamsMode *string `json:"params_mode"`
}

// orgPolicyTightened reports whether newReq narrows what oldResidency/oldAM
// already permitted -- residency moving UP the any/cloud_account_only/
// self_hosted_only ladder (see residencyRank), or allowed_models going from
// unrestricted to restricted, or losing at least one previously-allowed
// model id. Used to decide whether PutRoutingPolicy asks the gateway for
// impact at all (a loosening or no-op change can't shrink anyone's effective
// set, so it isn't worth a call).
func orgPolicyTightened(oldResidency, newResidency string, oldAM, newAM *[]string) bool {
	if residencyRank(newResidency) > residencyRank(oldResidency) {
		return true
	}
	return allowlistNarrowed(oldAM, newAM)
}

// allowlistNarrowed reports whether newAM can take a model away from someone
// oldAM allowed: unrestricted -> restricted, or a restricted list that drops
// at least one id. A widening or unchanged list is never a narrowing, and
// neither is a change that leaves the list unrestricted.
func allowlistNarrowed(oldAM, newAM *[]string) bool {
	if newAM == nil {
		return false
	}
	if oldAM == nil {
		return true
	}
	allowed := make(map[string]bool, len(*newAM))
	for _, id := range *newAM {
		allowed[id] = true
	}
	for _, id := range *oldAM {
		if !allowed[id] {
			return true
		}
	}
	return false
}

// orgLockOutNames turns POST /v1/policy/impact's id-only locked_out list into
// the names PutRoutingPolicy's 409 reports: a user's email, or "<key name>
// (<owner email>)" for a key. Ids the database no longer knows (a key
// revoked between the gateway's snapshot and now) fall back to the id.
func orgLockOutNames(ctx context.Context, db *pgxpool.Pool, locked []gatewayPolicyImpactPrincipal) ([]string, error) {
	userIDs := make([]string, 0, len(locked))
	keyIDs := make([]string, 0, len(locked))
	for _, p := range locked {
		userIDs = append(userIDs, p.UserID)
		if p.APIKeyID != "" {
			keyIDs = append(keyIDs, p.APIKeyID)
		}
	}
	emails := make(map[string]string, len(userIDs))
	rows, err := db.Query(ctx, `SELECT id::text, email FROM users WHERE id = ANY($1)`, userIDs)
	if err != nil {
		return nil, fmt.Errorf("lookup user emails: %w", err)
	}
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan user email: %w", err)
		}
		emails[id] = email
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keyNames, err := keyNamesByID(ctx, db, keyIDs)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(locked))
	for _, p := range locked {
		owner := emails[p.UserID]
		if owner == "" {
			owner = p.UserID
		}
		if p.APIKeyID == "" {
			names = append(names, owner)
			continue
		}
		name := keyNames[p.APIKeyID]
		if name == "" {
			name = p.APIKeyID
		}
		names = append(names, fmt.Sprintf("%s (%s)", name, owner))
	}
	return names, nil
}

// PutRoutingPolicy handles PUT /api/v1/routing-policy (org level).
func (h *RoutingHandler) PutRoutingPolicy(c *gin.Context) {
	var req putRoutingPolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validResidency(req.Residency) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "residency must be 'any', 'cloud_account_only', or 'self_hosted_only'"})
		return
	}
	if req.ParamsMode != nil && !validParamsMode(*req.ParamsMode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "params_mode must be 'lenient' or 'strict'"})
		return
	}

	ctx := c.Request.Context()

	if req.AllowedModels != nil {
		deduped := dedupeStrings(*req.AllowedModels)
		req.AllowedModels = &deduped
		if err := validateAllowedModelIDs(ctx, h.db, *req.AllowedModels); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if req.DefaultModel != nil {
		if err := validateDefaultModel(ctx, h.db, *req.DefaultModel, req.AllowedModels); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	var oldDBResidency, oldDBParamsMode *string
	var oldAM *[]string
	if err := h.db.QueryRow(ctx,
		`SELECT residency, allowed_models, params_mode FROM principal_routing_policies WHERE principal_type = 'org'`,
	).Scan(&oldDBResidency, &oldAM, &oldDBParamsMode); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read current routing policy failed"})
		return
	}
	oldResidency := orgResidencyString(oldDBResidency)

	// dbResidency mirrors orgResidencyString's inverse: NULL stores "any" (the
	// org level's "nothing above to inherit from" base case); any other
	// ladder value is stored literally.
	var dbResidency *string
	if req.Residency != residencyAny {
		dbResidency = &req.Residency
	}

	// Lock-out guard, the same rule the user/key PUTs enforce (policy.go):
	// never write a policy that leaves a principal with no allowed model.
	// An empty org allowlist locks out everyone and needs no gateway call;
	// any other tightening is evaluated org-wide by the gateway's merge via
	// POST /v1/policy/impact, which also says who actually reaches less
	// (affected_principals / org_narrowed below). A widening can neither lock
	// anyone out nor narrow anyone, so it skips the call entirely. If the
	// gateway is unreachable: an allowlist narrowing cannot be checked for
	// lock-outs, so it is refused (502); a residency-only tightening cannot
	// lock anyone out, so it proceeds without the counts.
	if req.AllowedModels != nil && len(*req.AllowedModels) == 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":      "an empty organisation allowlist would lock every user and key out of every model",
			"locked_out": []string{"(organisation)"},
		})
		return
	}
	var impact *gatewayPolicyImpact
	if orgPolicyTightened(oldResidency, req.Residency, oldAM, req.AllowedModels) {
		var err error
		impact, err = h.gateway.impact(ctx, policyObject{Residency: dbResidency, AllowedModels: req.AllowedModels, DefaultModel: req.DefaultModel})
		if err != nil {
			if allowlistNarrowed(oldAM, req.AllowedModels) {
				c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("%v: %v", errGatewayUnavailable, err)})
				return
			}
			slog.WarnContext(ctx, "put routing policy: gateway impact unavailable; residency change proceeds without affected counts", "err", err)
			impact = nil
		}
	}
	if impact != nil {
		if impact.OrgLockedOut {
			c.JSON(http.StatusConflict, gin.H{
				"error":      "this policy would lock every user and key out of every model",
				"locked_out": []string{"(organisation)"},
			})
			return
		}
		if len(impact.LockedOut) > 0 {
			names, err := orgLockOutNames(ctx, h.db, impact.LockedOut)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "resolve locked-out principals failed"})
				return
			}
			c.JSON(http.StatusConflict, gin.H{
				"error":      fmt.Sprintf("this policy would leave %d user(s) or key(s) with no reachable model; widen it, or change their own policies first", len(names)),
				"locked_out": names,
			})
			return
		}
	}

	var updatedBy *string
	if uid := c.GetString("user_id"); uid != "" {
		updatedBy = &uid
	}

	var resp routingPolicyResponse
	var newDBResidency, newDBParamsMode *string
	err := h.db.QueryRow(ctx, `
		INSERT INTO principal_routing_policies (principal_type, residency, allowed_models, default_model, params_mode, updated_at, updated_by)
		VALUES ('org', $1, $2, $3, $5, now(), $4)
		ON CONFLICT (principal_type) WHERE principal_type = 'org' DO UPDATE
			SET residency = EXCLUDED.residency, allowed_models = EXCLUDED.allowed_models,
				default_model = EXCLUDED.default_model,
				params_mode = COALESCE($5, principal_routing_policies.params_mode),
				updated_at = now(), updated_by = EXCLUDED.updated_by
		RETURNING residency, allowed_models, default_model, params_mode, updated_at
	`, dbResidency, req.AllowedModels, req.DefaultModel, updatedBy, req.ParamsMode).
		Scan(&newDBResidency, &resp.AllowedModels, &resp.DefaultModel, &newDBParamsMode, &resp.UpdatedAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update routing policy failed"})
		return
	}
	resp.Residency = orgResidencyString(newDBResidency)
	resp.ParamsMode = paramsModeString(newDBParamsMode)

	if resp.Residency != residencyAny {
		warnings, err := h.residencyRouteWarnings(ctx, resp.Residency)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "check routed alias residency conflicts failed"})
			return
		}
		resp.Warnings = warnings
	}

	if impact != nil {
		if len(impact.AlreadyLockedOut) > 0 {
			if names, err := orgLockOutNames(ctx, h.db, impact.AlreadyLockedOut); err == nil {
				resp.AlreadyLockedOut = names
			}
		}
		count := len(impact.Narrowed)
		resp.AffectedPrincipals = &count
		narrowed := impact.OrgNarrowed
		resp.OrgNarrowed = &narrowed
		resp.UnresolvedModels = impact.UnresolvedModels
	}

	h.kickGateway(ctx)
	evt := activity.FromContext(c, activity.RoutingPolicyOrgChanged)
	evt.TargetType = activity.StrPtr("routing_policy")
	evt.Detail = map[string]any{
		"residency":           resp.Residency,
		"allowed_models":      resp.AllowedModels,
		"default_model":       resp.DefaultModel,
		"affected_principals": resp.AffectedPrincipals,
		"org_narrowed":        resp.OrgNarrowed,
		"unresolved_models":   resp.UnresolvedModels,
	}
	h.activity.Record(ctx, evt)

	// params_mode decides whether a request the gateway used to accept
	// (dropping a parameter) is now refused, so a change is its own event
	// with both values.
	if prev := paramsModeString(oldDBParamsMode); prev != resp.ParamsMode {
		pe := activity.FromContext(c, activity.RoutingPolicyParamsModeChanged)
		pe.TargetType = activity.StrPtr("routing_policy")
		pe.Detail = map[string]any{"previous": prev, "new": resp.ParamsMode}
		h.activity.Record(ctx, pe)
	}

	c.JSON(http.StatusOK, resp)
}

// residencyRouteWarnings lists, in "alias X: route 'y' → <model> (<class>)
// will be refused" form, every routed alias route whose target's
// backend_class the given residency refuses (residencyPermitsClass) — used
// by PutRoutingPolicy whenever the new org residency is anything but "any",
// so the admin sees, at write time, exactly which already-configured routes
// are about to start being refused per request. Generalized from the
// two-class, self_hosted_only-only version of this check: under
// cloud_account_only this reports external_api routes; under
// self_hosted_only it reports both cloud_managed and external_api routes.
func (h *RoutingHandler) residencyRouteWarnings(ctx context.Context, residency string) ([]string, error) {
	rows, err := h.db.Query(ctx, `
		SELECT r.alias, r.route_name, r.target_model_id, m.backend_class
		FROM model_alias_routes r
		JOIN models m ON m.model_id = r.target_model_id
		ORDER BY r.alias, r.position
	`)
	if err != nil {
		return nil, fmt.Errorf("query routes: %w", err)
	}
	defer rows.Close()

	var warnings []string
	for rows.Next() {
		var aliasName, routeName, targetModelID, backendClass string
		if err := rows.Scan(&aliasName, &routeName, &targetModelID, &backendClass); err != nil {
			return nil, fmt.Errorf("scan route: %w", err)
		}
		if residencyPermitsClass(residency, backendClass) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("alias %s: route '%s' → %s (%s) will be refused", aliasName, routeName, targetModelID, backendClass))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("routes rows: %w", err)
	}
	return warnings, nil
}

// semanticRouterNeedsWarning reports whether a routed alias using
// classifier_kind='semantic_router' should carry
// semanticRouterNotConfiguredWarning right now. It probes the gateway's own
// unauthenticated GET {gatewayURL}/health at most once per
// semanticRouterHealthCacheTTL (shared across every call from PutAlias and
// ListAliases, not per alias) and caches the result for that window.
//
// A probe failure (gateway unreachable, non-200, unparseable body) never
// fails the caller's request and never surfaces a warning for that window —
// "no signal" is treated as "assume configured" rather than alarming an
// admin over a transient network blip — but it is logged once per window via
// slog.Warn so an operator can still notice a gateway that is down for an
// extended period.
func (h *RoutingHandler) semanticRouterNeedsWarning(ctx context.Context) bool {
	h.srProbeMu.Lock()
	if time.Now().Before(h.srProbeExpiresAt) {
		warn := h.srProbeWarn
		h.srProbeMu.Unlock()
		return warn
	}
	h.srProbeMu.Unlock()

	warn, err := h.probeSemanticRouterConfigured(ctx)
	if err != nil {
		slog.Warn("routing: gateway health probe for semantic router status failed", "err", err)
		warn = false
	}

	h.srProbeMu.Lock()
	h.srProbeWarn = warn
	h.srProbeExpiresAt = time.Now().Add(semanticRouterHealthCacheTTL)
	h.srProbeMu.Unlock()
	return warn
}

// gatewayHealthResponse is the subset of the gateway's GET /health body
// semanticRouterNeedsWarning needs. See gateway cmd/main.go's
// newHealthHandler for the full shape.
type gatewayHealthResponse struct {
	Routing struct {
		Classifier struct {
			SemanticRouter struct {
				Status string `json:"status"`
			} `json:"semantic_router"`
		} `json:"classifier"`
	} `json:"routing"`
}

// probeSemanticRouterConfigured calls the gateway's unauthenticated /health
// and reports whether routing.classifier.semantic_router.status is anything
// other than "configured" (i.e. a warning is warranted). Returns a non-nil
// error for any failure to obtain that answer (network, non-200, bad JSON) —
// the caller decides how to degrade.
func (h *RoutingHandler) probeSemanticRouterConfigured(ctx context.Context) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, semanticRouterProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, h.gatewayURL+"/health", nil)
	if err != nil {
		return false, fmt.Errorf("build gateway health request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("call gateway health: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("gateway health returned %d", resp.StatusCode)
	}

	var body gatewayHealthResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body); err != nil {
		return false, fmt.Errorf("decode gateway health: %w", err)
	}

	return body.Routing.Classifier.SemanticRouter.Status != "configured", nil
}
