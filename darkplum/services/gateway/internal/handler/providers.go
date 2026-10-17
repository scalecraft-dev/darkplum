package handler

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/scalecraft/darkplum/gateway/internal/middleware"
	"github.com/scalecraft/darkplum/gateway/internal/provider"
	"github.com/scalecraft/darkplum/gateway/internal/provider/credsource"
	"github.com/scalecraft/darkplum/gateway/internal/routing"
)

// refreshHintWindow is how long a POST /v1/providers/:vendor/refresh call
// keeps ticking RefreshSource for that vendor — 5 MINUTES per the v2.1
// ruling (not the original plan's 3 minutes), matching
// vendor_provider_settings.installing_until and the "Taking longer than
// expected" UI state's deadline. A second refresh call for the same vendor
// while a hint is already running EXTENDS this window rather than starting a
// second one — see (*ProvidersHandler).triggerRefreshHint.
const refreshHintWindow = 5 * time.Minute

// refreshHintTickInterval is how often the hint loop calls
// vendorRefreshManager.RefreshSource for the vendor while a hint is active.
const refreshHintTickInterval = 5 * time.Second

// refreshHintCredsRecheckInterval is the temporary credsource recheck
// interval applied for the duration of a refresh hint (see
// credsource.Source.SetTemporaryRecheckInterval) — short enough that every
// refreshHintTickInterval tick is guaranteed to see a freshly re-read key
// file rather than a cached value from just before the Secret landed.
const refreshHintCredsRecheckInterval = time.Second

// vendorKeyVerifier is the subset of *anthropic.Provider / *openai.Provider
// ProvidersHandler needs. Defined as an interface so tests can substitute a
// fake instead of a real vendor client.
type vendorKeyVerifier interface {
	// VerifyKey checks key directly against the vendor, returning the raw
	// reachable model ids or a closed-vocabulary *provider.VendorAPIError
	// (see provider.IsAuthFailedError).
	// workspaceID is the submitted Anthropic workspace id ("" for none);
	// the openai provider ignores it.
	VerifyKey(ctx context.Context, key, workspaceID string) ([]string, error)
	// DiscoverModels is the installed-credential path used when no key is
	// submitted (see HandleVerify) — the SAME method routing.Manager's
	// discovery tick calls, so "verify the installed key" always agrees with
	// what is actually catalogued.
	DiscoverModels(ctx context.Context) (map[string]bool, error)
}

// vendorCredentialSource is the subset of *credsource.Source ProvidersHandler
// needs to detect "no key installed" (key_required) before falling back to
// the installed-credential discovery path.
type vendorCredentialSource interface {
	Key() (key string, version uint64, err error)
}

// vendorRecheckSetter is the subset of *credsource.Source ProvidersHandler
// needs to shorten a vendor's credential recheck interval for the duration
// of an active refresh hint. May be nil in a ProviderVendorConfig that
// doesn't need it (tests).
type vendorRecheckSetter interface {
	SetTemporaryRecheckInterval(interval, duration time.Duration)
}

// vendorRefreshManager is the subset of *routing.Manager ProvidersHandler
// needs to trigger an on-demand, single-source refresh.
type vendorRefreshManager interface {
	RefreshSource(ctx context.Context, name string)
}

var (
	_ vendorCredentialSource = (*credsource.Source)(nil)
	_ vendorRecheckSetter    = (*credsource.Source)(nil)
	_ vendorRefreshManager   = (*routing.Manager)(nil)
)

// ProviderVendorConfig is everything ProvidersHandler needs for one vendor
// ("anthropic" or "openai"). cmd/main.go builds one of these per vendor from
// the SAME Provider/credsource.Source instances wired into the vendor's
// routing.Source, so "verify" and "what routing.Manager actually catalogues"
// can never quietly diverge.
type ProviderVendorConfig struct {
	// Verifier is the vendor's Provider (VerifyKey + the installed-credential
	// DiscoverModels path).
	Verifier vendorKeyVerifier
	// Creds is the vendor's credsource.Source, used only to distinguish
	// "no key installed" (key_required) from an installed key that fails
	// discovery, BEFORE calling Verifier.DiscoverModels.
	Creds vendorCredentialSource
	// Recheck shortens Creds' recheck interval for the duration of an active
	// refresh hint (see triggerRefreshHint). May be nil.
	Recheck vendorRecheckSetter
	// EnvVar names the vendor's model-filter env var, passed through to
	// routing.ApplyGlobFilter so a malformed submitted glob's error names a
	// familiar knob.
	EnvVar string
	// IsAuthError classifies a RAW (untranslated) DiscoverModels failure as
	// auth vs unreachable — the installed-credential path's counterpart to
	// provider.IsAuthFailedError (which classifies VerifyKey's ALREADY
	// translated error instead). anthropic.IsAuthError / openai.IsAuthError.
	IsAuthError func(error) bool
	// IsWorkspaceRequired classifies a RAW DiscoverModels failure on the
	// installed-key path as Anthropic's "key not scoped to a workspace" 400
	// (workspace_required). nil for openai.
	IsWorkspaceRequired func(error) bool
	// PostFilter is applied after the submitted/glob-filtered id list,
	// exactly mirroring routing.vendorSourceConfig.postFilter — nil for
	// anthropic; openai.FilterChatModels for openai.
	PostFilter func([]string) []string
}

// ProvidersHandler implements the two machine-only routes the api calls
// (with the service key) to let an admin install/rotate/verify a vendor key
// from Admin > Models without a terminal:
//
//	POST /v1/providers/:vendor/verify  — check a candidate (or the installed)
//	                                      key and preview which ids a filter
//	                                      would admit, WITHOUT installing
//	                                      anything.
//	POST /v1/providers/:vendor/refresh — hint routing.Manager to re-discover
//	                                      one vendor's models sooner than the
//	                                      next periodic tick, after the api
//	                                      has just written a new key/filter.
//
// :vendor must be a key of vendors (anthropic|openai) — anything else 404s.
type ProvidersHandler struct {
	// ctx is the gateway's root/server-lifetime context: hint loops are
	// children of it, so they stop cleanly on shutdown rather than leaking.
	ctx     context.Context
	vendors map[string]ProviderVendorConfig
	routes  vendorRefreshManager

	hintMu       sync.Mutex
	hintDeadline map[string]time.Time
	hintRunning  map[string]bool
}

// NewProvidersHandler builds a ProvidersHandler. ctx should be the gateway's
// root context (canceled on SIGTERM) — every hint loop it ever starts is
// tied to it.
func NewProvidersHandler(ctx context.Context, routes vendorRefreshManager, vendors map[string]ProviderVendorConfig) *ProvidersHandler {
	return &ProvidersHandler{
		ctx:          ctx,
		vendors:      vendors,
		routes:       routes,
		hintDeadline: make(map[string]time.Time),
		hintRunning:  make(map[string]bool),
	}
}

// resolveVendor looks up :vendor and writes the 404 unknown-endpoint
// envelope if it isn't one of the configured vendors (anthropic|openai).
func (h *ProvidersHandler) resolveVendor(c *gin.Context) (string, ProviderVendorConfig, bool) {
	vendor := c.Param("vendor")
	cfg, ok := h.vendors[vendor]
	if !ok {
		c.JSON(http.StatusNotFound, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("unknown vendor %q", vendor),
				Type:    "invalid_request_error",
				Code:    "unknown_endpoint",
			},
		})
		return "", ProviderVendorConfig{}, false
	}
	return vendor, cfg, true
}

// requireServiceKey refuses (403) any caller other than the shared service
// key, mirroring the existing gate on GET/POST /v1/policy/effective and POST
// /v1/policy/impact — the api always holds the service key and is the only
// caller of either route in this handler.
func requireServiceKey(c *gin.Context, route string) bool {
	if c.GetString(middleware.ContextKeyType) == middleware.KeyTypeService {
		return true
	}
	c.JSON(http.StatusForbidden, openAIError{
		Error: openAIErrorBody{
			Message: fmt.Sprintf("%s is only available to the service key", route),
			Type:    "permission_error",
			Code:    "service_key_required",
		},
	})
	return false
}

// verifyRequest is the body of POST /v1/providers/:vendor/verify. Key is
// never logged — see this file's package doc and middleware/recording.go
// (the Recording middleware is mounted only on the recorded group, which
// this route is deliberately NOT part of — see cmd/main.go's buildRouter).
type verifyRequest struct {
	Key         string   `json:"key"`
	ModelFilter []string `json:"model_filter"`
	// WorkspaceID scopes a SUBMITTED key's check to an Anthropic workspace
	// (anthropic-workspace-id header); with key omitted it overrides the
	// effective workspace id for the installed-key check. Ignored for openai.
	// A pointer so "absent" (installed path: use the effective workspace id)
	// differs from "" (installed path: check with NO workspace id).
	WorkspaceID *string `json:"workspace_id"`
}

type verifyResponse struct {
	Reachable []string `json:"reachable"`
	Admitted  []string `json:"admitted"`
}

// vendorVerifyErrorMessages is the closed-vocabulary text for each
// vendorVerifyError code POST /v1/providers/:vendor/verify may return. Never
// vendor-supplied text (a vendor's raw 401 body can echo back part of the
// submitted key) — always one of these three static strings.
var vendorVerifyErrorMessages = map[string]string{
	"auth_failed":        "the vendor rejected this key",
	"vendor_error":       "the vendor answered the key check with an error",
	"unreachable":        "the vendor could not be reached to verify this key",
	"workspace_required": "This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace.",
	"key_required":       "no key was submitted and no vendor key is currently installed",
}

// respondVendorError writes the 422 vendor_error outcome: the vendor ANSWERED
// (any HTTP status but 401/403). Carries only the numeric status and the
// sanitised error.type tag, never vendor text.
func respondVendorError(c *gin.Context, status int, errType string) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error": gin.H{
			"code":               "vendor_error",
			"message":            vendorVerifyErrorMessages["vendor_error"],
			"vendor_http_status": status,
			"vendor_error_type":  errType,
		},
	})
}

func respondVendorVerifyError(c *gin.Context, code string) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error": gin.H{
			"code":    code,
			"message": vendorVerifyErrorMessages[code],
		},
	})
}

// respondInvalidModelFilter writes a plain 400 for a model_filter entry that
// is not a well-formed glob — distinct from the three-code 422 closed
// vocabulary above (which is strictly about REACHING/AUTHENTICATING to the
// vendor), since this is a malformed-request-shape problem the api's own
// PUT validation is expected to catch before it ever reaches here.
func respondInvalidModelFilter(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, openAIError{
		Error: openAIErrorBody{
			Message: err.Error(),
			Type:    "invalid_request_error",
			Code:    "invalid_request",
		},
	})
}

// decodeJSONBody decodes body into v, tolerating a completely empty body as
// the zero value (POST .../refresh's wire contract is `{}`, and an admin
// checking the installed key sends no `key` field at all).
func decodeJSONBody(body io.ReadCloser, v any) error {
	if body == nil {
		return nil
	}
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// HandleVerify handles POST /v1/providers/:vendor/verify. See the wire
// contract in DELIVER item 5 of the implementation plan: key omitted means
// "verify the vendor's installed credential" (422 key_required if none is
// installed); an empty model_filter means "list everything reachable, after
// the vendor's own non-chat hygiene denylist" — a non-empty filter applies
// globs first, then that same denylist, exactly what routing.Manager's own
// discovery tick would catalogue for this vendor.
func (h *ProvidersHandler) HandleVerify(c *gin.Context) {
	if !requireServiceKey(c, "POST /v1/providers/:vendor/verify") {
		return
	}
	vendor, cfg, ok := h.resolveVendor(c)
	if !ok {
		return
	}

	var req verifyRequest
	if err := decodeJSONBody(c.Request.Body, &req); err != nil {
		c.JSON(http.StatusBadRequest, openAIError{
			Error: openAIErrorBody{
				Message: fmt.Sprintf("invalid request body: %s", err.Error()),
				Type:    "invalid_request_error",
				Code:    "invalid_request",
			},
		})
		return
	}

	workspaceID := ""
	if req.WorkspaceID != nil {
		workspaceID = *req.WorkspaceID
		if workspaceID != "" && !provider.ValidWorkspaceID(workspaceID) {
			c.JSON(http.StatusBadRequest, openAIError{
				Error: openAIErrorBody{
					Message: "workspace_id must match ^[A-Za-z0-9_-]{1,64}$",
					Type:    "invalid_request_error",
					Code:    "invalid_request",
				},
			})
			return
		}
	}

	var reachable []string
	if req.Key != "" {
		ids, err := cfg.Verifier.VerifyKey(c.Request.Context(), req.Key, workspaceID)
		if err != nil {
			var wsErr *provider.VendorAPIError
			if errors.As(err, &wsErr) && wsErr.WorkspaceRequired {
				log.Printf("providers: verify %s with a submitted key: outcome=workspace_required %s", vendor, verifyDiagnostic(err))
				respondVendorVerifyError(c, "workspace_required")
				return
			}
			if vendorErr, ok := provider.VendorResponseError(err); ok {
				errType := vendorErr.VendorErrorType
				if errType == "" {
					errType = "unknown"
				}
				log.Printf("providers: verify %s with a submitted key: outcome=vendor_error %s", vendor, verifyDiagnostic(err))
				respondVendorError(c, vendorErr.VendorStatus, errType)
				return
			}
			code := "unreachable"
			if provider.IsAuthFailedError(err) {
				code = "auth_failed"
			}
			log.Printf("providers: verify %s with a submitted key: outcome=%s %s", vendor, code, verifyDiagnostic(err))
			respondVendorVerifyError(c, code)
			return
		}
		reachable = ids
	} else {
		if _, _, err := cfg.Creds.Key(); err != nil {
			if errors.Is(err, credsource.ErrNotConfigured) {
				respondVendorVerifyError(c, "key_required")
				return
			}
			// The key file exists but is unreadable/empty — the same
			// "there is a key problem" case routing.vendorSourceState.discover
			// reports as auth_failed rather than not_configured.
			respondVendorVerifyError(c, "auth_failed")
			return
		}
		discoverCtx := c.Request.Context()
		if req.WorkspaceID != nil && vendor == "anthropic" {
			discoverCtx = provider.WithWorkspaceOverride(discoverCtx, workspaceID)
		}
		discovered, err := cfg.Verifier.DiscoverModels(discoverCtx)
		if err != nil {
			if cfg.IsWorkspaceRequired != nil && cfg.IsWorkspaceRequired(err) {
				log.Printf("providers: verify %s with the installed key: outcome=workspace_required code=invalid_request vendor_http_status=400", vendor)
				respondVendorVerifyError(c, "workspace_required")
				return
			}
			code := "unreachable"
			if cfg.IsAuthError != nil && cfg.IsAuthError(err) {
				code = "auth_failed"
			}
			respondVendorVerifyError(c, code)
			return
		}
		reachable = make([]string, 0, len(discovered))
		for id := range discovered {
			reachable = append(reachable, id)
		}
	}
	sort.Strings(reachable)

	admitted := reachable
	if len(req.ModelFilter) > 0 {
		filtered, err := routing.ApplyGlobFilter(reachable, req.ModelFilter, cfg.EnvVar)
		if err != nil {
			respondInvalidModelFilter(c, err)
			return
		}
		admitted = filtered
	}
	if cfg.PostFilter != nil {
		admitted = cfg.PostFilter(admitted)
	}
	if admitted == nil {
		admitted = []string{}
	}
	if reachable == nil {
		reachable = []string{}
	}

	c.JSON(http.StatusOK, verifyResponse{Reachable: reachable, Admitted: admitted})
}

// HandleRefresh handles POST /v1/providers/:vendor/refresh: starts (or
// extends, deduped per vendor) a refreshHintWindow hint that shortens the
// vendor's credsource recheck interval and re-discovers just that one
// source every refreshHintTickInterval, so a key/filter the api just wrote
// is picked up well before the next periodic discovery tick. Always 202 —
// this never blocks on discovery actually completing.
func (h *ProvidersHandler) HandleRefresh(c *gin.Context) {
	if !requireServiceKey(c, "POST /v1/providers/:vendor/refresh") {
		return
	}
	vendor, cfg, ok := h.resolveVendor(c)
	if !ok {
		return
	}
	// The body ({}) carries nothing; drained (not decoded) so a caller that
	// sends a body doesn't get an error, but nothing about it is ever logged
	// or retained.
	if c.Request.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(c.Request.Body, 1<<20))
	}

	if cfg.Recheck != nil {
		cfg.Recheck.SetTemporaryRecheckInterval(refreshHintCredsRecheckInterval, refreshHintWindow)
	}
	h.triggerRefreshHint(vendor)

	c.Status(http.StatusAccepted)
}

// triggerRefreshHint (re)arms vendor's refreshHintWindow deadline. If a hint
// loop for vendor is already running, this only extends hintDeadline — the
// running loop reads the extended value on its next iteration; no second
// loop is started ("deduped per vendor").
func (h *ProvidersHandler) triggerRefreshHint(vendor string) {
	h.hintMu.Lock()
	h.hintDeadline[vendor] = time.Now().Add(refreshHintWindow)
	alreadyRunning := h.hintRunning[vendor]
	if !alreadyRunning {
		h.hintRunning[vendor] = true
	}
	h.hintMu.Unlock()

	if !alreadyRunning {
		go h.runRefreshHintLoop(vendor)
	}
}

// runRefreshHintLoop calls routes.RefreshSource(vendor) immediately, then
// every refreshHintTickInterval, until hintDeadline[vendor] has passed or
// h.ctx is done (server shutdown) — cleanly, with no goroutine left behind
// either way.
func (h *ProvidersHandler) runRefreshHintLoop(vendor string) {
	defer func() {
		h.hintMu.Lock()
		h.hintRunning[vendor] = false
		h.hintMu.Unlock()
	}()

	ticker := time.NewTicker(refreshHintTickInterval)
	defer ticker.Stop()

	for {
		h.routes.RefreshSource(h.ctx, vendor)

		h.hintMu.Lock()
		deadline := h.hintDeadline[vendor]
		h.hintMu.Unlock()
		if !time.Now().Before(deadline) {
			return
		}

		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// verifyDiagnostic names, in a closed vocabulary, why a vendor-key check
// failed, for the gateway log: the translated error code and the vendor's own
// HTTP status when there was a response, or the transport failure class when
// there was not. It never includes the vendor's text, the request, or the key —
// "unreachable" on its own has already cost an operator a trip through the
// runbook with nothing to go on.
func verifyDiagnostic(err error) string {
	var vendorErr *provider.VendorAPIError
	if errors.As(err, &vendorErr) {
		if vendorErr.VendorStatus != 0 {
			t := vendorErr.VendorErrorType
			if t == "" {
				t = "unknown"
			}
			return fmt.Sprintf("code=%s vendor_http_status=%d vendor_error_type=%s", vendorErr.Code, vendorErr.VendorStatus, t)
		}
		return "code=" + vendorErr.Code + " " + transportClass(vendorErr.Cause)
	}
	return transportClass(err)
}

func transportClass(err error) string {
	switch {
	case err == nil:
		return "transport=none"
	case errors.Is(err, context.DeadlineExceeded):
		return "transport=timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "transport=dns"
	}
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var certErr x509.CertificateInvalidError
	if errors.As(err, &unknownCA) || errors.As(err, &hostErr) || errors.As(err, &certErr) {
		return "transport=tls"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "transport=timeout"
		}
		return "transport=network"
	}
	return "transport=other"
}
