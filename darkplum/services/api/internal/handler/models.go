package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ModelHandler manages the models table and exposes both the chat-facing
// /v1/models endpoint (enabled models only) and the admin endpoints for listing
// and toggling models.
type ModelHandler struct {
	gatewayURL string
	serviceKey string
	pool       *pgxpool.Pool
	client     *http.Client
	gateway    *gatewayEffectiveClient

	// availabilityWindow defaults to modelAvailabilityWindow and can be
	// overridden via SetAvailabilityWindow, which cmd/main.go does from
	// MODEL_AVAILABILITY_WINDOW_SECONDS so an operator can tune it without a
	// rebuild.
	availabilityWindow time.Duration

	// effCacheMu/effCache back getSessionEffective's 5s-per-user cache of
	// the gateway's effective policy — see that method's doc comment.
	effCacheMu sync.Mutex
	effCache   map[string]effectiveCacheEntry

	// providersMu guards providers, providerFeatures, and anyInstalling --
	// everything cached from the gateway's last successful GET /v1/models
	// (service-key + capability-header call only -- see SyncModels), for
	// ListAdminModels/ProvidersHandler to pass through or gate on. providers
	// is left at its zero value (nil slice, marshals as []) until the first
	// successful sync -- "empty array if the gateway never reported one",
	// per the wire contract -- and left UNCHANGED by a failed sync (network
	// error, non-200, decode failure), so a transient gateway blip doesn't
	// flash the admin Providers panel to empty.
	providersMu      sync.Mutex
	providers        []gatewayProviderStatus
	providerFeatures []string
	// anyInstalling caches whether vendor_provider_settings has any row with
	// installing_until OR removing_until in the future, refreshed at the end
	// of every SyncModels run -- cmd/main.go's sync loop polls this (via
	// AnyInstalling) to decide whether to tick every 5s (PLAN-UI v2.1)
	// instead of MODEL_SYNC_INTERVAL_SECONDS, without adding a second query
	// to the hot ticker path.
	anyInstalling bool

	// secureCookies mirrors auth.Config.SecureCookie (SECURE_COOKIES) --
	// ListAdminModels uses it, together with the inbound request's Host, to
	// compute key_install_transport_ok the same way providers.go's
	// keyInstallTransportOK does, so the UI can show the "needs HTTPS"
	// reason before an admin ever pastes a key rather than only after PUT
	// key refuses it.
	secureCookies bool
	// kick backs Kick/KickCh -- see Kick's doc comment.
	kick chan struct{}
}

// effectiveCacheEntry is one session user's cached effective policy, used
// only to filter the chat-facing /v1/models list (never for enforcement).
// residency is the caller's OWN effective residency (org->user merge,
// tighten-only), NOT the org's alone -- see getSessionEffective and
// ListModels' use of it in place of getResidency for a session caller.
type effectiveCacheEntry struct {
	expiresAt     time.Time
	allowedModels *[]string
	defaultModel  *string
	residency     string
}

// NewModelHandler constructs a ModelHandler. gatewayURL and serviceKey are used
// by SyncModels to call the gateway's /v1/models endpoint at startup and on the
// periodic sync ticker, and by ListModels (via the shared effective-policy
// client) to filter the list for a session user.
func NewModelHandler(gatewayURL, serviceKey string, pool *pgxpool.Pool) *ModelHandler {
	return &ModelHandler{
		gatewayURL: gatewayURL,
		serviceKey: serviceKey,
		pool:       pool,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		gateway:            newGatewayEffectiveClient(gatewayURL, serviceKey),
		availabilityWindow: modelAvailabilityWindow,
		effCache:           make(map[string]effectiveCacheEntry),
		kick:               make(chan struct{}, 1),
	}
}

// Kick wakes cmd/main.go's sync loop (see KickCh) for an immediate,
// out-of-band SyncModels run, instead of waiting for the loop's next
// natural tick -- providers.go's PutKey/PutFilter/DeleteKey call this right
// after a successful write, alongside their own existing immediate (async,
// bounded) SyncModels call, so the loop notices AnyInstalling() went true
// and drops to its fast tick cadence within about a second of the write,
// rather than up to a full modelSyncInterval (default 60s) later.
//
// Non-blocking: kick is buffered to depth 1, and a Kick that arrives while
// the channel already holds an unread signal (the loop hasn't gotten around
// to draining it yet) is simply dropped -- one pending wake-up is exactly as
// good as two, since the loop always runs a fresh SyncModels when it
// drains it, and a caller must never block on this.
func (h *ModelHandler) Kick() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// KickCh returns the channel cmd/main.go's sync loop selects on to receive
// Kick() wake-ups. See Kick's doc comment.
func (h *ModelHandler) KickCh() <-chan struct{} {
	return h.kick
}

// SetAvailabilityWindow overrides the default modelAvailabilityWindow used by
// ListModels/ListAdminModels/ToggleModel. Exported so cmd/main.go (a
// different package) can apply an operator-configured
// MODEL_AVAILABILITY_WINDOW_SECONDS value after construction.
func (h *ModelHandler) SetAvailabilityWindow(w time.Duration) {
	h.availabilityWindow = w
}

// SetSecureCookies records whether the api is configured for HTTPS
// (SECURE_COOKIES=true), for ListAdminModels' key_install_transport_ok
// computation. Exported so cmd/main.go can apply authCfg.SecureCookie after
// construction, matching SetAvailabilityWindow's pattern.
func (h *ModelHandler) SetSecureCookies(v bool) {
	h.secureCookies = v
}

// modelRow is the full DB representation returned by admin endpoints.
// Available is not a column — it's derived from DiscoveredAt at response
// time via modelIsAvailable (see models_logic.go), so admin UI and toggle
// responses always reflect the current cutoff rather than a stale snapshot.
type modelRow struct {
	ID           string    `json:"id"`
	ModelID      string    `json:"model_id"`
	Provider     string    `json:"provider"`
	DisplayName  *string   `json:"display_name,omitempty"`
	Enabled      bool      `json:"enabled"`
	BackendClass string    `json:"backend_class"`
	Available    bool      `json:"available"`
	DiscoveredAt time.Time `json:"discovered_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Availability is the gateway's per-model availability object, passed
	// through verbatim (models.availability_detail); JSON null when the
	// gateway omitted it (every non-Bedrock model).
	Availability json.RawMessage `json:"availability"`
}

// routeSummary is the chat-facing (non-admin) shape of one route on a routed
// alias: name, description, and the target's backend_class (so the picker
// can say "varies by route" when a routed alias mixes self_hosted and
// cloud_managed targets) — no target model id, keywords, or availability,
// which stay admin-only (routing.go's aliasRouteResponse).
type routeSummary struct {
	RouteName    string `json:"route_name"`
	Description  string `json:"description"`
	BackendClass string `json:"backend_class"`
}

// openAIModelEntry is the wire shape for one model in the OpenAI-compatible
// /v1/models response. It doubles as the decode target for the gateway's own
// GET /v1/models response in SyncModels — the gateway sends the same fields
// (minus alias_of/alias_kind/routes, which only the api's chat-facing
// endpoint produces).
type openAIModelEntry struct {
	ID           string         `json:"id"`
	Object       string         `json:"object"`
	Created      int64          `json:"created"`
	OwnedBy      string         `json:"owned_by"`
	BackendClass string         `json:"backend_class"`
	AliasOf      *string        `json:"alias_of,omitempty"`
	AliasKind    string         `json:"alias_kind,omitempty"`
	Routes       []routeSummary `json:"routes,omitempty"`
	// Availability is the gateway's availability object for this model
	// ({"state","reason","account_mode","required_mode"}), passed through
	// verbatim; null when absent. Alias entries carry their (default) target's.
	Availability json.RawMessage `json:"availability"`
}

// openAIModelsResponse is the OpenAI-compatible envelope for the models list.
// DefaultModel is additive (top-level, not part of the OpenAI spec) so the
// webchat picker can preselect without a second round trip: it is the
// session user's effective default_model when one could be resolved, else
// the org's own default_model, else nil.
type openAIModelsResponse struct {
	Object       string             `json:"object"`
	Data         []openAIModelEntry `json:"data"`
	DefaultModel *string            `json:"default_model"`
	// PolicyFiltered is false when the caller's policy could not be applied
	// (the gateway lookup failed) and Data is the UNFILTERED list -- a model
	// shown may still be refused. The picker says so instead of offering
	// models that will 403.
	PolicyFiltered bool `json:"policy_filtered"`
}

// getSessionEffective returns the session user's effective allowed_models,
// default_model, AND residency (the org->user merge, tighten-only -- NOT
// just the org's own residency; see gatewayEffectivePolicy.Residency) for
// filtering the chat-facing /v1/models list, backed by a cache of
// modelsEffectiveCacheTTL (5s) per user so a busy chat UI re-fetching the
// model list doesn't hammer the gateway on every keystroke/tab focus. A
// cache miss/expiry calls the gateway's GET /v1/policy/effective; any
// failure (timeout, non-200, gateway down) is returned to the caller, which
// must fall back to the org-level residency and unfiltered allowlist rather
// than fail the request — see ListModels. This is a UX filter only, never
// the enforcement point.
func (h *ModelHandler) getSessionEffective(ctx context.Context, userID string) (*[]string, *string, string, error) {
	h.effCacheMu.Lock()
	if e, ok := h.effCache[userID]; ok && time.Now().Before(e.expiresAt) {
		h.effCacheMu.Unlock()
		return e.allowedModels, e.defaultModel, e.residency, nil
	}
	h.effCacheMu.Unlock()

	eff, err := h.gateway.effective(ctx, userID, nil)
	if err != nil {
		return nil, nil, "", err
	}

	h.effCacheMu.Lock()
	h.effCache[userID] = effectiveCacheEntry{
		expiresAt:     time.Now().Add(modelsEffectiveCacheTTL),
		allowedModels: eff.AllowedModels,
		defaultModel:  eff.DefaultModel,
		residency:     eff.Residency,
	}
	h.effCacheMu.Unlock()

	return eff.AllowedModels, eff.DefaultModel, eff.Residency, nil
}

// orgDefaultModel reads the org policy row's default_model, used as the
// /v1/models envelope's default_model fallback when there is no session
// user (a Bearer/API-key caller) or the gateway's effective policy couldn't
// be fetched.
func (h *ModelHandler) orgDefaultModel(ctx context.Context) (*string, error) {
	var d *string
	err := h.pool.QueryRow(ctx, `SELECT default_model FROM principal_routing_policies WHERE principal_type = 'org'`).Scan(&d)
	return d, err
}

// ListModels handles GET /v1/models — chat-facing, returns enabled models plus
// aliases whose target is enabled, in OpenAI-compatible format. Entries of a
// class residencyPermitsClass refuses (e.g. cloud_managed under
// self_hosted_only, external_api under anything but "any") are omitted
// entirely — this is a UX convenience for the picker, not the enforcement
// point (that's the gateway; see policy.Snapshot). An external_api entry is
// additionally omitted whenever the INBOUND request didn't send
// darkplumClassesHeader, regardless of residency — see
// requestPermitsExternalAPI and buildModelsList's permitExternalAPI param.
//
// The residency used is the CALLER's own effective residency (org->user
// merge, tighten-only), not just the org's: getResidency's org-level value
// is only the starting point/fallback (used when there is no session
// identity, or the session's effective policy can't be fetched — see below);
// a session user whose own row narrows residency below the org's (e.g. the
// org is "any" but this user is self_hosted_only) must not be offered a
// model their own policy will refuse. A Bearer/API-key caller doesn't need
// this override: bearerModels' underlying gateway GET /v1/models call
// already residency-filters that key's Data list itself (see the gateway's
// HandleListModels), so the allowedModels intersection below already
// reflects the caller's true residency regardless of which residency value
// this function uses for the class-level pre-filter.
//
// The list is filtered to the caller's effective allowed_models, and
// default_model is the caller's effective default, for BOTH kinds of caller:
//
//   - a session (browser) caller -- "user_id" set in the gin context, which
//     only happens via SessionRequired -- through getSessionEffective (the
//     gateway's effective endpoint with the service key, cached 5s per user);
//   - a Bearer/API-key caller through the gateway's own GET /v1/models,
//     called with the caller's credential (bearerModels), which the gateway
//     filters per user key and which now also carries that key's default.
//
// The two /v1/models (api and gateway) must give the same credential the
// same answer -- an SDK caller that lists models and then gets 403 on one
// of them reads as a broken product. Routed/static alias entries whose every
// target is disallowed are omitted. If the policy lookup fails the list
// degrades to org-level-residency-only filtering with policy_filtered=false
// (see openAIModelsResponse) — the SAME fallback already used for the
// allowlist (unrestricted rather than inventing a new failure mode);
// enforcement stays with the gateway either way.
func (h *ModelHandler) ListModels(c *gin.Context) {
	ctx := c.Request.Context()

	residency, err := h.getResidency(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "list models: read routing policy failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	permitExternalAPI := requestPermitsExternalAPI(c.GetHeader(darkplumClassesHeader))

	var allowedModels *[]string
	var defaultModel *string
	policyFiltered := false
	if userID := c.GetString("user_id"); userID != "" {
		am, dm, effResidency, effErr := h.getSessionEffective(ctx, userID)
		if effErr != nil {
			slog.WarnContext(ctx, "list models: gateway effective policy unavailable, falling back to unfiltered list", "user_id", userID, "err", effErr)
		} else {
			allowedModels = am
			defaultModel = dm
			policyFiltered = true
			// Override the org-level residency read above with the caller's
			// OWN effective residency (org->user merge) — see ListModels' doc
			// comment. effResidency is always non-empty on a successful fetch
			// (the gateway's EffectivePolicy.Residency is never the zero
			// value), but the org fallback is kept defensively rather than
			// ever showing an empty residency to buildModelsList.
			if effResidency != "" {
				residency = effResidency
			}
		}
	} else if bearer := c.GetHeader("Authorization"); strings.HasPrefix(bearer, "Bearer ") {
		// Only a Bearer credential is forwarded (apiOrSession already gates
		// the route on that prefix; checked again here so the handler is
		// safe if it is ever mounted elsewhere).
		ids, dm, gwErr := h.bearerModels(ctx, bearer)
		if gwErr != nil {
			slog.WarnContext(ctx, "list models: gateway model list unavailable for api-key caller, falling back to unfiltered list", "err", gwErr)
		} else {
			allowedModels = &ids
			defaultModel = dm
			policyFiltered = true
		}
	}
	if defaultModel == nil && policyFiltered {
		// A filtered caller with no effective default: nothing to preselect.
		// (The org default is already folded into the effective default.)
	} else if defaultModel == nil {
		if d, dErr := h.orgDefaultModel(ctx); dErr == nil {
			defaultModel = d
		} else {
			slog.WarnContext(ctx, "list models: read org default_model failed", "err", dErr)
		}
	}

	modelRows, err := h.pool.Query(ctx, `
		SELECT model_id, provider, backend_class, discovered_at, availability_detail
		FROM models
		WHERE enabled = true
		ORDER BY provider, model_id
	`)
	if err != nil {
		slog.ErrorContext(ctx, "list models query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	defer modelRows.Close()

	var models []modelDBRow
	for modelRows.Next() {
		var m modelDBRow
		if err := modelRows.Scan(&m.ModelID, &m.Provider, &m.BackendClass, &m.DiscoveredAt, &m.Availability); err != nil {
			slog.ErrorContext(ctx, "scan model row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
		models = append(models, m)
	}
	if err := modelRows.Err(); err != nil {
		slog.ErrorContext(ctx, "models rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	aliasRows, err := h.pool.Query(ctx, `
		SELECT a.alias, a.target_model_id, m.provider, m.backend_class, m.discovered_at, m.availability_detail
		FROM model_aliases a
		JOIN models m ON m.model_id = a.target_model_id
		WHERE m.enabled = true
		ORDER BY a.alias
	`)
	if err != nil {
		slog.ErrorContext(ctx, "list model aliases query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	defer aliasRows.Close()

	var aliases []aliasDBRow
	for aliasRows.Next() {
		var a aliasDBRow
		if err := aliasRows.Scan(&a.Alias, &a.TargetModelID, &a.OwnedBy, &a.BackendClass, &a.TargetDiscoveredAt, &a.Availability); err != nil {
			slog.ErrorContext(ctx, "scan model alias row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
		aliases = append(aliases, a)
	}
	if err := aliasRows.Err(); err != nil {
		slog.ErrorContext(ctx, "model aliases rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	// Routed aliases: every route for every kind='routed' alias, left-joined
	// against its (possibly missing/disabled) target model. Fetched as one
	// query and grouped in Go (by alias, ordered by r.position) rather than
	// one query per alias, avoiding an N+1 over however many routed aliases
	// exist.
	routedRows, err := h.pool.Query(ctx, `
		SELECT
			a.alias, a.default_route,
			r.route_name, r.description, r.target_model_id,
			(m.model_id IS NOT NULL) AS target_exists,
			COALESCE(m.enabled, false) AS target_enabled,
			COALESCE(m.provider, ''), COALESCE(m.backend_class, ''),
			m.discovered_at, m.availability_detail
		FROM model_aliases a
		JOIN model_alias_routes r ON r.alias = a.alias
		LEFT JOIN models m ON m.model_id = r.target_model_id
		WHERE a.kind = 'routed'
		ORDER BY a.alias, r.position
	`)
	if err != nil {
		slog.ErrorContext(ctx, "list routed model aliases query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	defer routedRows.Close()

	var routedAliases []routedAliasDBRow
	routedIndex := make(map[string]int, 4) // alias -> index into routedAliases
	for routedRows.Next() {
		var aliasName, defaultRoute string
		var rt routedAliasRouteDBRow
		var discoveredAt *time.Time
		if err := routedRows.Scan(&aliasName, &defaultRoute, &rt.RouteName, &rt.Description, &rt.TargetModelID,
			&rt.TargetExists, &rt.TargetEnabled, &rt.TargetProvider, &rt.TargetBackendClass, &discoveredAt, &rt.TargetAvailability); err != nil {
			slog.ErrorContext(ctx, "scan routed model alias row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
		if discoveredAt != nil {
			rt.TargetDiscoveredAt = *discoveredAt
		}
		idx, ok := routedIndex[aliasName]
		if !ok {
			idx = len(routedAliases)
			routedIndex[aliasName] = idx
			routedAliases = append(routedAliases, routedAliasDBRow{Alias: aliasName, DefaultRoute: defaultRoute})
		}
		routedAliases[idx].Routes = append(routedAliases[idx].Routes, rt)
	}
	if err := routedRows.Err(); err != nil {
		slog.ErrorContext(ctx, "routed model aliases rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	data := buildModelsList(models, aliases, routedAliases, residency, permitExternalAPI, time.Now(), h.availabilityWindow, allowedModels)
	c.JSON(http.StatusOK, openAIModelsResponse{Object: "list", Data: data, DefaultModel: defaultModel, PolicyFiltered: policyFiltered})
}

// bearerModels asks the gateway's GET /v1/models with the caller's own
// Authorization header. For a user API key the gateway filters that list to
// the key's effective allowlist and reports its effective default_model, so
// this is the caller's policy view without the api ever seeing which key it
// is (the api does not validate Bearer keys; the gateway does). A non-2xx
// from the gateway (a bad key, say) is returned as an error and the caller
// gets the degraded unfiltered list -- the gateway will refuse the chat
// request itself.
func (h *ModelHandler) bearerModels(ctx context.Context, authorization string) ([]string, *string, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayEffectiveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.gatewayURL+"/v1/models", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", authorization)
	// See SyncModels' identical header: this api always understands
	// external_api, so it always asks the gateway for the caller's full
	// effective list including any vendor models the caller's OWN policy
	// permits (an API-key caller whose policy allows a vendor model must see
	// it in GET /v1/models exactly as a session caller would -- the
	// models.go:194 invariant). Whether THIS api hands a vendor model back
	// to whoever called ITS /v1/models is a separate, later check
	// (permitExternalAPI in ListModels), based on what THAT caller's own
	// request carried.
	req.Header.Set(darkplumClassesHeader, backendClassExternalAPI)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("gateway /v1/models returned %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		DefaultModel *string `json:"default_model"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&body); err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		ids = append(ids, m.ID)
	}
	return ids, body.DefaultModel, nil
}

// getResidency reads the org's current residency value from
// principal_routing_policies, the authoritative table since migration 015 --
// NOT the legacy routing_policy mirror, which migration 016's trigger
// deliberately DOWNGRADES cloud_account_only to 'any' for the benefit of an
// old gateway that has no concept of the third rung (see that migration's
// comment). This api is never that old gateway: reading the mirror here
// would silently under-restrict a cloud_account_only org's own chat-facing
// model list back down to 'any'.
func (h *ModelHandler) getResidency(ctx context.Context) (string, error) {
	var dbResidency *string
	err := h.pool.QueryRow(ctx, `SELECT residency FROM principal_routing_policies WHERE principal_type = 'org'`).Scan(&dbResidency)
	if err != nil {
		return "", err
	}
	return orgResidencyString(dbResidency), nil
}

// adminModelsResponse is GET /api/v1/models' (admin) WRAPPED response shape,
// sent only when the request carries darkplumClassesHeader (see
// requestPermitsExternalAPI and ListAdminModels) -- the new web bundle
// always sends it. Without the header, the endpoint keeps returning the bare
// JSON array it always has (see ListAdminModels): changing an established
// response from an array to an object is a breaking shape change for any
// existing consumer, so the wrapper is additive and opt-in via the same
// capability handshake gating external_api visibility itself, rather than
// unconditional. Data omits external_api rows in BOTH shapes when the
// header is absent (the bare-array default can never carry a class its
// caller never asked for); Providers is passed through from the gateway's
// last successful sync regardless of the header once the wrapper is in use.
type adminModelsResponse struct {
	Data      []modelRow              `json:"data"`
	Providers []gatewayProviderStatus `json:"providers"`
	// ProviderFeatures is passed through from the gateway's last successful
	// sync (PLAN-UI): today's only defined token is "ui_key_install",
	// meaning the gateway serves POST /v1/providers/:vendor/{verify,refresh}
	// -- ProvidersHandler refuses install/filter/delete with 409
	// gateway_too_old when it's absent. Marshals as [] before the first
	// successful sync, matching Providers' own zero-value contract.
	ProviderFeatures []string `json:"provider_features"`
	// KeyInstallTransportOK reports whether THIS request could safely carry a
	// vendor key to PUT .../key or POST .../verify -- computed by the same
	// rule providers.go's keyInstallTransportOK enforces (HTTPS, via
	// secureCookies, OR a loopback Host), so the UI can show the "needs
	// HTTPS" reason up front instead of only after a paste is refused.
	KeyInstallTransportOK bool `json:"key_install_transport_ok"`
}

// AnyInstalling reports whether the last SyncModels run saw any vendor with
// a live installing_until -- see the providersMu/anyInstalling field doc
// comment. Exported so cmd/main.go's sync loop can decide its tick interval
// without importing internal/db itself.
func (h *ModelHandler) AnyInstalling() bool {
	h.providersMu.Lock()
	defer h.providersMu.Unlock()
	return h.anyInstalling
}

// cachedProviders returns the gateway's last-synced per-vendor status block
// and top-level provider_features, both copied so the caller can't mutate
// the handler's cache. Shared by ListAdminModels and ProvidersHandler (same
// package) so there is exactly one place that reads providersMu's guarded
// fields for this purpose.
func (h *ModelHandler) cachedProviders() ([]gatewayProviderStatus, []string) {
	h.providersMu.Lock()
	defer h.providersMu.Unlock()
	providers := append([]gatewayProviderStatus{}, h.providers...)
	features := append([]string{}, h.providerFeatures...)
	return providers, features
}

// providerFeature is a provider_features token. Only one is defined today.
const providerFeatureUIKeyInstall = "ui_key_install"

// hasProviderFeature reports whether features names feature.
func hasProviderFeature(features []string, feature string) bool {
	for _, f := range features {
		if f == feature {
			return true
		}
	}
	return false
}

// vendorHintWindows reads every vendor_provider_settings row's
// installing_until and removing_until, normalizing a NULL or already-past
// value to nil in each map -- "null when absent or past", per PLAN-UI's GET
// /api/v1/models contract -- so ListAdminModels never has to repeat that
// normalization at the call site. installing_until and removing_until are
// mutually exclusive in practice (PutKey clears the latter, DeleteKey clears
// the former), but this reads and normalizes both independently regardless.
func vendorHintWindows(ctx context.Context, pool *pgxpool.Pool) (installing, removing map[string]*time.Time, err error) {
	rows, err := pool.Query(ctx, `SELECT vendor, installing_until, removing_until FROM vendor_provider_settings`)
	if err != nil {
		return nil, nil, fmt.Errorf("query vendor_provider_settings: %w", err)
	}
	defer rows.Close()

	installing = make(map[string]*time.Time, 2)
	removing = make(map[string]*time.Time, 2)
	now := time.Now()
	for rows.Next() {
		var vendor string
		var installUntil, removeUntil *time.Time
		if err := rows.Scan(&vendor, &installUntil, &removeUntil); err != nil {
			return nil, nil, fmt.Errorf("scan vendor_provider_settings row: %w", err)
		}
		if installUntil != nil && installUntil.After(now) {
			installing[vendor] = installUntil
		} else {
			installing[vendor] = nil
		}
		if removeUntil != nil && removeUntil.After(now) {
			removing[vendor] = removeUntil
		} else {
			removing[vendor] = nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("vendor_provider_settings rows: %w", err)
	}
	return installing, removing, nil
}

// ListAdminModels handles GET /api/v1/models — admin view, returns all models
// with full metadata including the enabled flag.
//
// Response shape is capability-gated by darkplumClassesHeader, same as the
// chat-facing /v1/models and the gateway's own /v1/models: without the
// header, a bare JSON array (today's shape, external_api rows omitted --
// this is the ONLY case a pre-vendor-providers admin bundle ever sees, so it
// keeps working unchanged). With the header, adminModelsResponse
// ({"data":[...],"providers":[...]}), external_api rows included.
func (h *ModelHandler) ListAdminModels(c *gin.Context) {
	permitExternalAPI := requestPermitsExternalAPI(c.GetHeader(darkplumClassesHeader))

	rows, err := h.pool.Query(c.Request.Context(), `
		SELECT id, model_id, provider, display_name, enabled, backend_class, discovered_at, updated_at, availability_detail
		FROM models
		ORDER BY provider, model_id
	`)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "list admin models query failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list models failed"})
		return
	}
	defer rows.Close()

	now := time.Now()
	models := make([]modelRow, 0)
	for rows.Next() {
		var m modelRow
		if err := rows.Scan(&m.ID, &m.ModelID, &m.Provider, &m.DisplayName, &m.Enabled, &m.BackendClass, &m.DiscoveredAt, &m.UpdatedAt, &m.Availability); err != nil {
			slog.ErrorContext(c.Request.Context(), "scan admin model row failed", "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan models failed"})
			return
		}
		if m.BackendClass == backendClassExternalAPI && !permitExternalAPI {
			continue
		}
		m.Available = modelIsAvailableWithin(m.DiscoveredAt, now, h.availabilityWindow)
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(c.Request.Context(), "admin models rows error", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rows error"})
		return
	}

	if !permitExternalAPI {
		c.JSON(http.StatusOK, models)
		return
	}

	providers, features := h.cachedProviders()

	// installing_until and removing_until live only in the api's own
	// vendor_provider_settings (the gateway has no concept of either window)
	// -- merge them into each cached provider entry by vendor name. A lookup
	// failure degrades to leaving both unset rather than failing the whole
	// request: these are progress-bar hints, not enforcement, and the rest
	// of the response is still valid without them.
	installing, removing, err := vendorHintWindows(c.Request.Context(), h.pool)
	if err != nil {
		slog.WarnContext(c.Request.Context(), "list admin models: read hint windows failed", "err", err)
	}
	for i := range providers {
		if until, ok := installing[providers[i].Name]; ok {
			providers[i].InstallingUntil = until
		}
		if until, ok := removing[providers[i].Name]; ok {
			providers[i].RemovingUntil = until
		}
	}

	c.JSON(http.StatusOK, adminModelsResponse{
		Data:                  models,
		Providers:             providers,
		ProviderFeatures:      features,
		KeyInstallTransportOK: keyInstallTransportOK(h.secureCookies, c.Request.Host),
	})
}

// toggleModelRequest is the request body for PATCH /api/v1/models/:id.
// DisplayName is a tri-state field: absent/omitted leaves the stored
// display_name untouched; explicit null clears it (reverts to showing the
// raw model_id); a non-empty string (1-100 chars) sets it. SyncModels never
// writes display_name (see models.go's upsert), so an admin-set name always
// survives a resync -- this endpoint is the only writer.
//
// DisplayName is deliberately json.RawMessage, NOT *json.RawMessage: a
// *json.RawMessage field collapses "absent" and "null" to the same nil
// pointer (encoding/json sets the pointer itself to nil for a JSON null,
// same as when the key is missing entirely, before RawMessage's own
// UnmarshalJSON ever runs) -- there is no way to tell them apart afterward.
// A plain (non-pointer) json.RawMessage does NOT get that early-nil
// treatment: it stays nil (len 0) when the key is absent, but is
// unmarshaled to the literal bytes "null" when the key is explicitly null,
// so parseDisplayName can distinguish the two by inspecting the bytes.
type toggleModelRequest struct {
	Enabled     bool            `json:"enabled"`
	DisplayName json.RawMessage `json:"display_name"`
}

// displayNameMaxLen mirrors the 1-100 character bound from DESIGN4's parity
// section.
const displayNameMaxLen = 100

// parseDisplayName decodes toggleModelRequest.DisplayName's tri-state raw
// JSON into (value, changed): changed is false when the field was absent
// from the request body (nothing to update); when changed is true, a nil
// value means "clear it" (JSON null) and a non-nil value is the new name,
// already length-validated. Pure so the tri-state parsing and the 1-100
// bound are unit-testable without a database. See toggleModelRequest's doc
// comment for why raw must be a plain json.RawMessage, not a pointer to one.
func parseDisplayName(raw json.RawMessage) (value *string, changed bool, err error) {
	if raw == nil {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false, fmt.Errorf("display_name must be a string or null")
	}
	if len(s) < 1 || len(s) > displayNameMaxLen {
		return nil, false, fmt.Errorf("display_name must be 1-%d characters", displayNameMaxLen)
	}
	return &s, true, nil
}

// ToggleModel handles PATCH /api/v1/models/:id — admin only.
// Accepts {"enabled": true|false, "display_name"?: string|null} and returns
// the updated model row.
func (h *ModelHandler) ToggleModel(c *gin.Context) {
	id := c.Param("id")
	if !isValidUUID(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid model id"})
		return
	}

	var req toggleModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	displayName, changeDisplayName, err := parseDisplayName(req.DisplayName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var tag pgconn.CommandTag
	if changeDisplayName {
		tag, err = h.pool.Exec(c.Request.Context(), `
			UPDATE models SET enabled = $1, display_name = $2, updated_at = now() WHERE id = $3
		`, req.Enabled, displayName, id)
	} else {
		// UPDATE first — matching the pattern in users.go (Exec + RowsAffected).
		tag, err = h.pool.Exec(c.Request.Context(), `
			UPDATE models SET enabled = $1, updated_at = now() WHERE id = $2
		`, req.Enabled, id)
	}
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "toggle model update failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "toggle model failed"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}

	// Fetch and return the updated row so the frontend can refresh without a
	// separate GET.
	var m modelRow
	if err := h.pool.QueryRow(c.Request.Context(), `
		SELECT id, model_id, provider, display_name, enabled, backend_class, discovered_at, updated_at, availability_detail
		FROM models WHERE id = $1
	`, id).Scan(&m.ID, &m.ModelID, &m.Provider, &m.DisplayName, &m.Enabled, &m.BackendClass, &m.DiscoveredAt, &m.UpdatedAt, &m.Availability); err != nil {
		slog.ErrorContext(c.Request.Context(), "fetch updated model failed", "id", id, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fetch updated model failed"})
		return
	}
	m.Available = modelIsAvailableWithin(m.DiscoveredAt, time.Now(), h.availabilityWindow)

	c.JSON(http.StatusOK, m)
}

// gatewayProviderStatus is one entry of the gateway's GET /v1/models optional
// top-level "providers" array (service-key callers that sent
// darkplumClassesHeader only -- see the wire contract): always both vendors,
// configured or not. SyncModels captures the array verbatim; ListAdminModels
// passes it straight through, unfiltered by the capability header check that
// gates the models list itself, since it describes gateway-side plumbing an
// admin needs to see to diagnose a vendor provider regardless of whether
// their own browser bundle understands external_api models yet.
// FilterSource, Filter, and EnvFilterSetAt (PLAN-UI) are the gateway's own
// per-vendor last-writer-wins view: FilterSource is "db"|"env"|"none",
// Filter is the effective filter currently in force, and EnvFilterSetAt is
// the ConfigMap SET_AT the gateway last saw for that vendor -- the api's
// ProvidersHandler reads EnvFilterSetAt (never displays it) to compute a
// skew-proof filter_set_at on every UI write (see providers.go's
// nextFilterSetAt). InstallingUntil is NEVER set by the gateway (it has no
// concept of an install window) -- it is populated by ListAdminModels from
// vendor_provider_settings after every sync-triggered or on-demand read of
// this struct, so it round-trips through SyncModels' decode untouched (the
// gateway's JSON never contains this key). RemovingUntil is the same story
// for DELETE .../key's removal hint window (review addition to migration
// 017): also NEVER set by the gateway, populated by ListAdminModels from
// vendor_provider_settings, and deliberately NOT omitempty -- the review
// spec calls for an explicit `"removing_until": null` rather than an
// absent key when there's no removal in progress, unlike InstallingUntil's
// existing omitempty behavior (left as-is; changing an already-shipped
// field's shape is out of scope here).
type gatewayProviderStatus struct {
	Name           string     `json:"name"`
	Configured     bool       `json:"configured"`
	State          string     `json:"state"`
	ModelCount     int        `json:"model_count"`
	LastSyncAt     *time.Time `json:"last_sync_at"`
	FilterSource   string     `json:"filter_source,omitempty"`
	Filter         []string   `json:"filter,omitempty"`
	EnvFilterSetAt *time.Time `json:"env_filter_set_at,omitempty"`
	// WorkspaceID is the gateway's effective Anthropic workspace id (not a
	// secret), null when none (the gateway's "" is normalised to nil on sync).
	WorkspaceID     *string    `json:"workspace_id"`
	InstallingUntil *time.Time `json:"installing_until,omitempty"`
	RemovingUntil   *time.Time `json:"removing_until"`
}

// gatewayModelsResponse is the shape we expect from GET {gatewayURL}/v1/models.
type gatewayModelsResponse struct {
	Object           string                  `json:"object"`
	Data             []openAIModelEntry      `json:"data"`
	Providers        []gatewayProviderStatus `json:"providers,omitempty"`
	ProviderFeatures []string                `json:"provider_features,omitempty"`
}

// modelPurgeAfter documents the purge cutoff used in the DELETE below (kept
// as a literal `interval '7 days'` in the SQL itself, since pgx has no clean
// way to bind a Go time.Duration as a Postgres interval — keep the two in
// sync by hand if this changes). It is intentionally much longer than
// modelAvailabilityWindow: "available" flips off quickly (see
// models_logic.go) to hide a model that isn't currently routable, but the row
// itself — and with it the admin's enabled flag and any aliases pointing at
// it — must survive a slow backend restart (a vLLM cold start can take 15-25
// minutes) or a longer maintenance window without the admin having to
// re-toggle it or recreate aliases once it comes back.
const modelPurgeAfter = 7 * 24 * time.Hour

// SyncModels fetches the model list from the gateway and upserts each model
// into the models table. Re-discovered models do not have their enabled flag
// or display_name overwritten — only discovered_at, backend_class, provider
// and availability_state/availability_detail (gateway facts: NULL when the
// gateway omits them) are refreshed, so admin settings survive a resync while a model
// that changes backend (e.g. a Bedrock filter change) is reclassified. New
// models default to enabled = false per the schema.
//
// Called once at startup and then on a periodic ticker in main.
func (h *ModelHandler) SyncModels(ctx context.Context) {
	target := fmt.Sprintf("%s/v1/models", h.gatewayURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		slog.ErrorContext(ctx, "sync models: build request failed", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.serviceKey)
	// Order-independence by capability handshake (PLAN v3.1): the gateway
	// only includes external_api models and the providers status block in
	// its response to a caller that names the class it understands. This
	// api always understands it (that's what this migration/release ships),
	// so it always sends it here regardless of what the INBOUND request to
	// the api's own /v1/models carried -- that separate check
	// (requestPermitsExternalAPI) gates what this api hands back to ITS
	// callers, not what it asks the gateway for.
	req.Header.Set(darkplumClassesHeader, backendClassExternalAPI)

	resp, err := h.client.Do(req)
	if err != nil {
		slog.WarnContext(ctx, "sync models: gateway request failed", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "sync models: gateway returned non-200", "status", resp.StatusCode)
		return
	}

	var gwResp gatewayModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&gwResp); err != nil {
		slog.ErrorContext(ctx, "sync models: decode gateway response failed", "err", err)
		return
	}

	// Cache the providers block for ListAdminModels regardless of whether any
	// models were discovered this tick -- a successful sync with an empty
	// providers array (old gateway, pre-CU3) must still overwrite a stale
	// cached value from a hypothetical earlier newer-gateway response, since
	// "passed through from the last sync" means the LAST sync, not the last
	// non-empty one.
	h.providersMu.Lock()
	for i := range gwResp.Providers {
		if ws := gwResp.Providers[i].WorkspaceID; ws != nil && *ws == "" {
			gwResp.Providers[i].WorkspaceID = nil
		}
	}
	h.providers = gwResp.Providers
	h.providerFeatures = gwResp.ProviderFeatures
	h.providersMu.Unlock()

	// Refresh the "is any vendor mid-install-or-removal" cache cmd/main.go's
	// sync loop polls to decide its tick interval (PLAN-UI v2.1, extended by
	// review to also cover removing_until: DELETE's own "Removing" progress
	// hint deserves the same fast tick installing_until already gets) -- a
	// plain read, done once per sync rather than once per tick, and tolerant
	// of a query failure (leaves the previous cached value in place rather
	// than spuriously falling back to the slow interval on a transient DB
	// blip).
	var anyInstalling bool
	if err := h.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM vendor_provider_settings WHERE installing_until > now() OR removing_until > now())`,
	).Scan(&anyInstalling); err != nil {
		slog.WarnContext(ctx, "sync models: check installing vendors failed", "err", err)
	} else {
		h.providersMu.Lock()
		h.anyInstalling = anyInstalling
		h.providersMu.Unlock()
	}

	// The upsert runs in a transaction serialised against PutAlias (routing.go)
	// via the same fixed advisory lock, so a concurrent alias PUT can't
	// interleave with a discovery upsert of a model sharing that id and leave
	// both a model row and an alias row for it (see PutAlias's comment for the
	// other half of this). The lock is per-transaction (pg_advisory_xact_lock)
	// and the transaction holds ONLY the upsert — the alias-shadow check and the
	// purge below run after commit, outside the lock, so an admin's PutAlias
	// waits for one statement, not for the whole sync.
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "sync models: begin transaction failed", "err", err)
		return
	}
	defer tx.Rollback(ctx) // no-op once committed

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, modelRoutingAdvisoryLockKey); err != nil {
		slog.ErrorContext(ctx, "sync models: acquire routing lock failed", "err", err)
		return
	}

	// Normalize+de-dup once, up front, so both the batch path and its
	// per-row fallback see the same input (see normalizeGatewayModels).
	normalized := normalizeGatewayModels(gwResp.Data)

	// The common case (a healthy sync) is one set-based upsert instead of
	// three round trips per model inside a per-row savepoint — with a few
	// hundred Bedrock profiles, the old per-row loop held
	// modelRoutingAdvisoryLockKey long enough to stall admin PutAlias calls.
	// A batch statement failure (e.g. one row's data Postgres rejects, which
	// fails the whole INSERT) falls back to the old per-row savepoint path so
	// one bad row still can't take the rest of the sync down.
	inserted, updated, discoveredIDs, err := h.syncModelsBatch(ctx, tx, normalized)
	if err != nil {
		slog.WarnContext(ctx, "sync models: batch upsert failed, falling back to per-row upserts",
			"model_count", len(normalized), "err", err)
		var fatalErr error
		inserted, updated, discoveredIDs, fatalErr = h.syncModelsPerRow(ctx, tx, normalized)
		if fatalErr != nil {
			slog.ErrorContext(ctx, "sync models: per-row fallback failed", "err", fatalErr)
			return
		}
	} else if len(normalized) > 0 {
		slog.InfoContext(ctx, "sync models: batch upsert succeeded",
			"model_count", len(normalized), "inserted", inserted, "updated", updated)
	}

	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "sync models: commit failed", "err", err)
		return
	}

	// A discovered model id that equals an existing alias is not an error —
	// the gateway's route table lets the alias win by design — but the admin
	// needs to know, since it means that alias no longer reaches the model it
	// looks like it targets. Log one WARNING per sync naming every alias
	// shadowed this run, rather than failing the sync or silently proceeding.
	if len(discoveredIDs) > 0 {
		rows, err := h.pool.Query(ctx, `SELECT alias FROM model_aliases WHERE alias = ANY($1)`, discoveredIDs)
		if err != nil {
			slog.ErrorContext(ctx, "sync models: check alias shadowing failed", "err", err)
		} else {
			var shadowed []string
			for rows.Next() {
				var alias string
				if err := rows.Scan(&alias); err != nil {
					slog.ErrorContext(ctx, "sync models: scan shadowed alias failed", "err", err)
					continue
				}
				shadowed = append(shadowed, alias)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				slog.ErrorContext(ctx, "sync models: shadowed alias rows error", "err", err)
			}
			if len(shadowed) > 0 {
				slog.Warn("sync models: discovered model id(s) shadow existing alias(es) of the same name — the alias wins at the gateway by design",
					"aliases", shadowed)
			}
		}
	}

	// Purge rows unseen for a long time — not "not re-discovered this tick",
	// which would delete a model whose backend is mid-restart (see
	// modelPurgeAfter). Aliases pointing at a purged model are left alone;
	// they already tolerate a missing target (target_exists/target_available
	// go false, but the alias row survives so it doesn't need recreating).
	res, err := h.pool.Exec(ctx, `
		DELETE FROM models WHERE discovered_at < now() - interval '7 days'
	`)
	var purged int64
	if err != nil {
		slog.ErrorContext(ctx, "sync models: purge stale rows failed", "err", err)
	} else {
		purged = res.RowsAffected()
	}

	slog.Info("sync models: complete",
		"fetched", len(gwResp.Data),
		"inserted", inserted,
		"updated", updated,
		"purged", purged,
	)
}

// syncModelsBatch upserts every discovered model in one set-based statement,
// run inside its own savepoint of tx so a statement-level failure (e.g. one
// row's data Postgres rejects) can be rolled back without aborting tx itself
// — the caller falls back to syncModelsPerRow on any returned error.
//
// "xmax = 0" is true only for the row version created by the INSERT branch
// (xmax is unset on a freshly inserted tuple); the ON CONFLICT UPDATE path
// always sets a nonzero xmax. CommandTag can't distinguish
// INSERT-from-conflict-update because both report the same tag under
// ON CONFLICT DO UPDATE ... RETURNING, so RETURNING (xmax = 0) is the
// reliable way to tell which branch ran for each row.
func (h *ModelHandler) syncModelsBatch(ctx context.Context, tx pgx.Tx, models []normalizedModel) (inserted, updated int, discoveredIDs []string, err error) {
	if len(models) == 0 {
		return 0, 0, nil, nil
	}

	ids := make([]string, len(models))
	providers := make([]string, len(models))
	backendClasses := make([]string, len(models))
	availStates := make([]*string, len(models))
	availDetails := make([]*string, len(models))
	for i, m := range models {
		availStates[i] = m.AvailabilityState
		availDetails[i] = m.AvailabilityDetail
		ids[i] = m.ID
		providers[i] = m.Provider
		backendClasses[i] = m.BackendClass
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("open batch savepoint: %w", err)
	}
	defer sp.Rollback(ctx) // no-op once committed

	rows, err := sp.Query(ctx, `
		INSERT INTO models (model_id, provider, backend_class, availability_state, availability_detail, discovered_at, updated_at)
		SELECT id, provider, backend_class, avail_state, avail_detail::jsonb, now(), now()
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[]) AS t(id, provider, backend_class, avail_state, avail_detail)
		ON CONFLICT (model_id) DO UPDATE
			SET discovered_at = now(),
				backend_class = EXCLUDED.backend_class,
				provider = EXCLUDED.provider,
				availability_state = EXCLUDED.availability_state,
				availability_detail = EXCLUDED.availability_detail
		RETURNING model_id, (xmax = 0)
	`, ids, providers, backendClasses, availStates, availDetails)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("batch upsert: %w", err)
	}

	discoveredIDs = make([]string, 0, len(models))
	for rows.Next() {
		var id string
		var wasInsert bool
		if scanErr := rows.Scan(&id, &wasInsert); scanErr != nil {
			rows.Close()
			return 0, 0, nil, fmt.Errorf("scan batch upsert row: %w", scanErr)
		}
		discoveredIDs = append(discoveredIDs, id)
		if wasInsert {
			inserted++
		} else {
			updated++
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return 0, 0, nil, fmt.Errorf("batch upsert rows: %w", rowsErr)
	}
	rows.Close()

	if err := sp.Commit(ctx); err != nil {
		return 0, 0, nil, fmt.Errorf("commit batch savepoint: %w", err)
	}

	return inserted, updated, discoveredIDs, nil
}

// syncModelsPerRow is the fallback for syncModelsBatch: each model is
// upserted in its own savepoint, so one row Postgres rejects doesn't abort
// the rest of the sync (and the purge that follows). This is the original
// (pre-batch) upsert strategy, kept as-is for the case a discovered set
// can't go through in one statement.
//
// A savepoint-open or savepoint-commit failure (as opposed to a rejected
// upsert statement) means something is wrong with the connection/transaction
// itself, not a single row's data — that is returned as a fatal error, which
// the caller must treat as reason to abort the whole sync without
// committing, exactly as this code did before being pulled out of SyncModels.
func (h *ModelHandler) syncModelsPerRow(ctx context.Context, tx pgx.Tx, models []normalizedModel) (inserted, updated int, discoveredIDs []string, fatalErr error) {
	discoveredIDs = make([]string, 0, len(models))
	for _, m := range models {
		var wasInsert bool
		sp, err := tx.Begin(ctx)
		if err != nil {
			return inserted, updated, discoveredIDs, fmt.Errorf("savepoint failed for model_id %s: %w", m.ID, err)
		}
		err = sp.QueryRow(ctx, `
			INSERT INTO models (model_id, provider, backend_class, availability_state, availability_detail, discovered_at, updated_at)
			VALUES ($1, $2, $3, $4, $5::text::jsonb, now(), now())
			ON CONFLICT (model_id) DO UPDATE
				SET discovered_at = now(),
					backend_class = EXCLUDED.backend_class,
					provider = EXCLUDED.provider,
					availability_state = EXCLUDED.availability_state,
					availability_detail = EXCLUDED.availability_detail
			RETURNING (xmax = 0)
		`, m.ID, m.Provider, m.BackendClass, m.AvailabilityState, m.AvailabilityDetail).Scan(&wasInsert)
		if err != nil {
			slog.ErrorContext(ctx, "sync models: upsert failed", "model_id", m.ID, "err", err)
			_ = sp.Rollback(ctx)
			continue
		}
		if err := sp.Commit(ctx); err != nil {
			return inserted, updated, discoveredIDs, fmt.Errorf("release savepoint failed for model_id %s: %w", m.ID, err)
		}
		// Only ids the database accepted take part in the alias-shadow check.
		discoveredIDs = append(discoveredIDs, m.ID)

		if wasInsert {
			inserted++
		} else {
			updated++
		}
	}
	return inserted, updated, discoveredIDs, nil
}
