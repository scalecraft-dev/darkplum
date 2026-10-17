// Vendor API key install/rotate/remove from the admin UI (PLAN-UI). Four
// admin-session-only routes:
//
//	POST   /api/v1/providers/:vendor/verify  -- check a key (or the
//	       installed one) against the vendor through the gateway, without
//	       storing anything.
//	PUT    /api/v1/providers/:vendor/key     -- install or rotate the
//	       vendor's key and choose which of its models are catalogued.
//	PUT    /api/v1/providers/:vendor/filter  -- change which models are
//	       catalogued for an already-installed key ("Adopt" is this route
//	       called with the env-reported filter).
//	DELETE /api/v1/providers/:vendor/key     -- remove the vendor's key.
//
// The key itself is NEVER stored in Postgres: PutKey merge-PATCHes it
// straight into the cluster Secret darkplum-provider-keys through
// internal/kube, and every other piece of state this file owns
// (vendor_provider_settings, migration 017) holds only the model_filter,
// its last-writer-wins timestamp, and the 5-minute "installing" hint
// window -- never the credential.
//
// Every gateway-reported error MESSAGE is discarded and replaced by a fixed,
// closed-vocabulary translation keyed only on the gateway's error CODE (see
// translateVerifyError) -- a buggy or malicious gateway that echoes the
// submitted key back in an error string can never make it into this api's
// own response body, logs, or activity_events row.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scalecraft/darkplum/api/internal/activity"
	"github.com/scalecraft/darkplum/api/internal/kube"
)

const (
	vendorAnthropic = "anthropic"
	vendorOpenAI    = "openai"

	// installingWindow is how long PutKey's installing_until hint stays in
	// the future after a successful install/rotate -- PLAN-UI v2.1 (5
	// minutes, not the original plan's 3): drives both the UI's
	// "Installing" progress state and cmd/main.go's faster model-sync
	// ticking while any vendor is mid-install.
	installingWindow = 5 * time.Minute

	// removingWindow is DeleteKey's equivalent hint window (review addition
	// to migration 017): how long removing_until stays in the future after
	// a successful key removal, driving the UI's "Removing" progress state
	// and the same fast model-sync ticking installingWindow does.
	removingWindow = 5 * time.Minute

	// gatewayProviderCallTimeout bounds one call to the gateway's
	// POST /v1/providers/:vendor/{verify,refresh} -- longer than
	// gatewayEffectiveTimeout (2s) because verify makes a real round trip to
	// the vendor API on the gateway's side, not just a Postgres read.
	gatewayProviderCallTimeout = 20 * time.Second
)

// validVendor mirrors vendor_provider_settings.vendor's CHECK constraint
// (migration 017) and the gateway's own :vendor route set.
func validVendor(v string) bool {
	return v == vendorAnthropic || v == vendorOpenAI
}

// secretFileName is the data key this vendor's key is stored under in the
// darkplum-provider-keys Secret -- matches the Makefile's PK_FILE for the
// same vendor (Makefile:599,609) so the UI and CLI paths are interchangeable.
func secretFileName(vendor string) string {
	switch vendor {
	case vendorAnthropic:
		return "anthropic-api-key"
	case vendorOpenAI:
		return "openai-api-key"
	default:
		return ""
	}
}

// configFilterKey and configFilterSetAtKey are the darkplum-provider-config
// ConfigMap data keys for a vendor's filter -- matches the Makefile's
// PK_FILTER for the same vendor (Makefile:600,610).
func configFilterKey(vendor string) string      { return strings.ToUpper(vendor) + "_MODEL_FILTER" }
func configFilterSetAtKey(vendor string) string { return configFilterKey(vendor) + "_SET_AT" }

// configWorkspaceIDKey is the ConfigMap key carrying the Anthropic workspace
// id (anthropic only). It is written together with the filter and SET_AT, so
// one stamp governs all three; an empty string clears it.
const configWorkspaceIDKey = "ANTHROPIC_WORKSPACE_ID"

// workspaceIDRe bounds a submitted Anthropic workspace id; it ends up in an
// HTTP header on the gateway, so the alphabet is deliberately narrow.
var workspaceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkWorkspaceID validates the optional workspace_id of a request. nil means
// the field was absent (keep the current value). Writes the 400 and reports
// ok=false on a non-anthropic vendor (the field is meaningless there) or a
// malformed id; "" is valid and means "clear".
func checkWorkspaceID(c *gin.Context, vendor string, ws *string) bool {
	if ws == nil {
		return true
	}
	if vendor != vendorAnthropic {
		errJSON(c, http.StatusBadRequest, "invalid_request", "workspace_id is only supported for anthropic")
		return false
	}
	if *ws != "" && !workspaceIDRe.MatchString(*ws) {
		errJSON(c, http.StatusBadRequest, "invalid_workspace_id", "workspace_id must be 1-64 characters of letters, digits, underscore, or hyphen (or empty to clear it)")
		return false
	}
	return true
}

// workspaceIDFor reads the gateway's reported effective workspace id for
// vendor from the cached providers block ("" if none, "" and found=false if
// the vendor is not in the block).
func workspaceIDFor(providers []gatewayProviderStatus, vendor string) (id string, found bool) {
	for _, p := range providers {
		if p.Name == vendor {
			if p.WorkspaceID != nil {
				return *p.WorkspaceID, true
			}
			return "", true
		}
	}
	return "", false
}

func filterSourceFor(providers []gatewayProviderStatus, vendor string) string {
	for _, p := range providers {
		if p.Name == vendor {
			return p.FilterSource
		}
	}
	return ""
}

// currentWorkspaceID is the workspace id in force right now for an
// "absent = keep current" write: the stored row's value, except when the
// gateway reports the env pair as winning (a newer `make set-*-key` write) or
// there is no stored row, where it is the gateway-reported effective value.
// Always "" for openai.
func currentWorkspaceID(vendor string, rowExists bool, rowWorkspaceID *string, providers []gatewayProviderStatus) string {
	if vendor != vendorAnthropic {
		return ""
	}
	if rowExists && filterSourceFor(providers, vendor) != "env" {
		if rowWorkspaceID != nil {
			return *rowWorkspaceID
		}
		return ""
	}
	if id, found := workspaceIDFor(providers, vendor); found {
		return id
	}
	if rowWorkspaceID != nil {
		return *rowWorkspaceID
	}
	return ""
}

// finalWorkspaceID applies a request's optional workspace_id (nil = keep) on
// top of the current one.
func finalWorkspaceID(requested *string, current string) string {
	if requested != nil {
		return *requested
	}
	return current
}

// workspaceAuditDetail adds workspace_id_before/after to an audit detail map,
// omitted when both are empty. The id is not a secret.
func workspaceAuditDetail(detail map[string]any, before, after string) {
	if before == "" && after == "" {
		return
	}
	detail["workspace_id_before"] = before
	detail["workspace_id_after"] = after
}

func vendorDisplayName(vendor string) string {
	switch vendor {
	case vendorAnthropic:
		return "Anthropic"
	case vendorOpenAI:
		return "OpenAI"
	default:
		return vendor
	}
}

// vendorExampleFilter mirrors the Makefile's PK_EXAMPLE for the same vendor
// (Makefile:604,614), used only in error copy.
func vendorExampleFilter(vendor string) string {
	switch vendor {
	case vendorAnthropic:
		return "claude-*"
	case vendorOpenAI:
		return "gpt-*"
	default:
		return "*"
	}
}

// keyFormatRe bounds a submitted vendor key BEFORE any network call: 20-512
// characters of letters, digits, underscore, or hyphen -- every real
// Anthropic/OpenAI key format fits comfortably inside this, and it rejects
// whitespace/control characters/quotes a copy-paste mistake might carry in.
var keyFormatRe = regexp.MustCompile(`^[A-Za-z0-9_-]{20,512}$`)

func validKeyFormat(k string) bool { return keyFormatRe.MatchString(k) }

// bareWildcardPattern reports whether p, after removing every '*' and '?'
// character, is empty -- i.e. it admits everything the key can reach,
// mirroring the Makefile's set-*-key guard (`[[ -z "${PAT//[*?]/}" ]]`,
// Makefile:625).
func bareWildcardPattern(p string) bool {
	return strings.NewReplacer("*", "", "?", "").Replace(p) == ""
}

// firstBareWildcard returns the first bare-wildcard pattern in patterns, if
// any.
func firstBareWildcard(patterns []string) (string, bool) {
	for _, p := range patterns {
		if bareWildcardPattern(p) {
			return p, true
		}
	}
	return "", false
}

// bareWildcardMessage mirrors the Makefile's set-*-key wording (Makefile:
// 626-628) so an admin sees the same explanation whether they hit this from
// the UI or the CLI.
func bareWildcardMessage(vendor, pattern string) string {
	return fmt.Sprintf(
		"%q admits every model this key can reach, including ones %s has not released yet. "+
			"Nothing below the gateway bounds a vendor key, so name a family, e.g. %s. "+
			"(allow_all overrides. Nothing was changed.)",
		pattern, vendorDisplayName(vendor), vendorExampleFilter(vendor),
	)
}

// nextFilterSetAt implements PLAN-UI v2.1's skew-proof last-writer-wins
// stamp: GREATEST(now, envFilterSetAt+1s, previousFilterSetAt+1s). A UI
// write always outranks whatever env ConfigMap stamp the gateway last
// reported for this vendor -- even one from a laptop clock set in the
// future -- and always outranks this vendor's own previous DB stamp, so
// successive UI writes are strictly increasing even when the wall clock
// hasn't visibly advanced between them.
func nextFilterSetAt(now time.Time, envFilterSetAt, previousFilterSetAt *time.Time) time.Time {
	result := now
	if envFilterSetAt != nil {
		if c := envFilterSetAt.Add(time.Second); c.After(result) {
			result = c
		}
	}
	if previousFilterSetAt != nil {
		if c := previousFilterSetAt.Add(time.Second); c.After(result) {
			result = c
		}
	}
	return result
}

// retryOnce calls fn, and on failure calls it exactly once more, returning
// the second attempt's result -- the "every later step is idempotent and
// retried once" rule from PutKey's failure story, shared by every step after
// the Secret PATCH across PutKey/PutFilter/DeleteKey.
func retryOnce(fn func() error) error {
	if err := fn(); err != nil {
		return fn()
	}
	return nil
}

// lastN returns the last n characters of s, or all of s if it's shorter --
// used only for the audit log's key_last4, computed from the SUBMITTED
// body. Safe here because every caller has already enforced
// len(s) >= 20 via validKeyFormat.
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// apiErrorBody is the wire shape of EVERY error response these four routes
// return, at any status code: {"error": {"code": "...", "message": "..."}}
// -- confirmed with the web engineer, whose UI reads error.message verbatim,
// so every message here must be admin-readable, closed text (never a raw
// err.Error() that could carry internal detail).
type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// VendorHTTPStatus / VendorErrorType are set only on the vendor_error
	// code: the HTTP status the vendor answered the key check with and its
	// sanitised error.type tag. Both are re-validated here, never relayed raw.
	VendorHTTPStatus int    `json:"vendor_http_status,omitempty"`
	VendorErrorType  string `json:"vendor_error_type,omitempty"`
}

func errJSON(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": apiErrorBody{Code: code, Message: message}})
}

// invalidJSONMessage is the fixed 400 message every ShouldBindJSON failure
// on these four routes returns, instead of err.Error(). A JSON syntax error
// from encoding/json echoes the offending byte/character back in its own
// message (e.g. "invalid character 's' looking for beginning of value") --
// for a route whose body IS a bare pasted vendor key on a malformed
// request, that would put one character of the key into the response body.
// The static message loses nothing an admin needs (their JSON is malformed;
// which byte doesn't matter) and can never carry a fragment of the key.
const invalidJSONMessage = "request body is not valid JSON"

// isLoopbackHost reports whether host -- an http.Request.Host value,
// optionally with a port -- names the loopback interface: localhost,
// 127.0.0.1, or ::1, bracketed or not. Used only by keyInstallTransportOK.
func isLoopbackHost(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// keyInstallTransportOK reports whether a vendor key may safely cross THIS
// connection: either the api is itself served over HTTPS (secureCookies,
// i.e. SECURE_COOKIES=true) or the request arrived over loopback
// (localhost/127.0.0.1/[::1], with or without a port -- a local kind dev
// setup where the browser and the api are the same machine and the
// connection never actually traverses a network even though it's plain
// HTTP). Anything else means the key would cross a real network
// unencrypted between the browser and this api. Shared by
// ProvidersHandler (PutKey, and PostVerify when a key is present) and
// ModelHandler.ListAdminModels (key_install_transport_ok), so both compute
// the exact same answer for the exact same request.
func keyInstallTransportOK(secureCookies bool, host string) bool {
	return secureCookies || isLoopbackHost(host)
}

// insecureTransportMessage is PUT key's and POST verify's (key-present)
// exact 403 message when keyInstallTransportOK is false -- confirmed with
// the web engineer and carla's runbook.
const insecureTransportMessage = "Installing a key from here needs HTTPS, because the key would cross the network unencrypted. Serve Darkplum over HTTPS (make expose-web CERT_ARN=…), or use make set-<vendor>-key."

// requireSecureTransport refuses 403 insecure_transport when
// keyInstallTransportOK is false for this request. Callers: PutKey always;
// PostVerify only when a key was submitted (verify-with-installed-key never
// puts a credential on the wire from the browser to begin with).
func (h *ProvidersHandler) requireSecureTransport(c *gin.Context) bool {
	if !keyInstallTransportOK(h.secureCookies, c.Request.Host) {
		errJSON(c, http.StatusForbidden, "insecure_transport", insecureTransportMessage)
		return false
	}
	return true
}

// errVendorBusy is returned by acquireVendorLock when another request
// already holds this vendor's in-process lock -- see that function's doc
// comment.
var errVendorBusy = errors.New("another change to this vendor is already in progress")

// vendorBusyJSON writes the 409 busy response for errVendorBusy.
func vendorBusyJSON(c *gin.Context) {
	errJSON(c, http.StatusConflict, "busy", "Another change to this vendor is in progress. Try again in a moment.")
}

// acquireVendorLock claims vendor's in-process mutex (from h.vendorLocks,
// one per validVendor entry, built once in NewProvidersHandler) for the
// entire read-previous -> verify -> PATCH -> upsert -> mirror sequence
// PutKey/PutFilter/DeleteKey each run, so two admins racing the SAME vendor
// can't interleave their gateway verify + Secret PATCH + settings upsert and
// leave the Secret and the settings row reflecting two different requests.
// anthropic and openai each get their own *sync.Mutex, so they never contend
// with each other.
//
// This replaced a Postgres pg_try_advisory_xact_lock held for a whole
// transaction's duration, which meant a pooled connection sat idle-in-
// transaction across gatewayVerify (up to gatewayProviderCallTimeout, 20s)
// and the kube PATCH (up to kube's own requestTimeout, 10s) -- up to ~30s of
// one connection held per in-flight install, enough to exhaust the pool
// under a handful of concurrent requests. An in-process sync.Mutex has no
// connection to hold, so callers acquire it before any of those calls and
// release it after every database write is done, and NO pooled connection
// is ever held across an external call. See withShortTx's doc comment for
// the database side of this: PutKey/PutFilter/DeleteKey's actual Postgres
// work now happens in short, independent transactions scoped only around
// the reads and the writes, never around gatewayVerify or a kube call.
//
// Consequence of being in-process rather than database-backed: this only
// serializes requests within ONE api pod. That is why the api deployment is
// documented replicas: 1 (see providerProcessRL's identical note in
// cmd/main.go and k8s/base/api/deployment.yaml) -- N replicas would each get
// their own independent per-vendor mutex, so the same vendor could be
// installed/rotated/removed concurrently from two different pods without
// either one ever seeing errVendorBusy. The day the api ever scales beyond
// one replica, this needs to become a database-backed lock or lease --
// acquired and released the same way, around the short transactions only,
// never re-widened to span the external calls the way the original
// pg_try_advisory_xact_lock version did.
//
// Non-blocking (sync.Mutex.TryLock): if another request already holds this
// vendor's lock, ok is false and the caller must write the 409 busy response
// itself and return without calling fn. release must be invoked exactly
// once on every path once acquired, including a panic -- every caller does
// this via `defer release()` immediately after a successful acquire, so the
// lock is held for that request's entire remaining body.
func (h *ProvidersHandler) acquireVendorLock(vendor string) (release func(), ok bool) {
	mu := h.vendorLocks[vendor]
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// withShortTx runs fn inside its own short-lived transaction -- begin, fn,
// then commit on success or rollback on any error (including fn's own
// error). Every database statement PutKey/PutFilter/DeleteKey run is scoped
// inside one of these, and NEVER wrapped around a gatewayVerify or kube
// call -- see acquireVendorLock's doc comment for why that separation is the
// point. A failure of fn OR of the Commit itself is returned identically to
// the caller, so a caller classifying "did the write actually take" cannot
// mistake a failed commit for success (see PutKey's partiallyInstalled
// handling, which relies on this).
func (h *ProvidersHandler) withShortTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// savepointRetryOnce runs a database statement inside tx exactly like
// retryOnce, using a SAVEPOINT so a failed attempt can be rolled back
// without aborting the surrounding transaction -- a plain failed tx.Exec
// would otherwise leave the whole transaction in Postgres' aborted state,
// unable to run anything else, including the retry itself or a second,
// independent statement sharing that same short transaction (DeleteKey's
// disable-models-then-clear-hint-windows pair, run inside one withShortTx
// call, is the remaining caller of this today).
func savepointRetryOnce(ctx context.Context, tx pgx.Tx, name string, fn func() error) error {
	attempt := func() error {
		if _, err := tx.Exec(ctx, "SAVEPOINT "+name); err != nil {
			return err
		}
		if err := fn(); err != nil {
			_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+name)
			return err
		}
		_, _ = tx.Exec(ctx, "RELEASE SAVEPOINT "+name)
		return nil
	}
	if err := attempt(); err != nil {
		return attempt()
	}
	return nil
}

// verifyErrorMessages is the closed vocabulary translateVerifyError draws
// from -- see this file's package doc comment for why the gateway's own
// error message text is never used.
var verifyErrorMessages = map[string]string{
	"auth_failed":            "the vendor rejected this key",
	"unreachable":            "could not reach the vendor to verify this key",
	"key_required":           "no key is installed for this vendor yet",
	"filter_matched_nothing": "the selected models are not reachable with this key",
	"workspace_required":     "This key is not scoped to an Anthropic workspace. Enter the workspace ID to use with it, or use a key created inside a workspace.",
}

// translateVerifyError maps a gateway-reported verify error CODE to a fixed
// api message, defaulting to auth_failed for any code this api doesn't
// recognize -- fail closed rather than forward an unknown, possibly
// attacker-influenced string.
var vendorErrorTypePattern = regexp.MustCompile(`^[a-z_]{1,40}$`)

// translateVendorError builds the vendor_error body from the gateway's two
// closed fields, re-validating both (a status outside 100-599 or a type that
// is not ^[a-z_]{1,40}$ is replaced, never forwarded). The message is authored
// here from the vendor's display name; no gateway text is used.
func translateVendorError(vendor string, status int, errType string) apiErrorBody {
	if status < 100 || status > 599 {
		status = http.StatusBadGateway
	}
	if !vendorErrorTypePattern.MatchString(errType) {
		errType = "unknown"
	}
	return apiErrorBody{
		Code: "vendor_error",
		Message: fmt.Sprintf("%s answered HTTP %d (%s) to the key check. Nothing was stored.",
			vendorDisplayName(vendor), status, errType),
		VendorHTTPStatus: status,
		VendorErrorType:  errType,
	}
}

func translateVerifyError(code string) apiErrorBody {
	if msg, ok := verifyErrorMessages[code]; ok {
		return apiErrorBody{Code: code, Message: msg}
	}
	return apiErrorBody{Code: "auth_failed", Message: verifyErrorMessages["auth_failed"]}
}

// modelsEnabledError is DELETE .../key's 409 body when the vendor has
// enabled models and the caller didn't ask to disable them: the standard
// {code, message} shape plus `models`, the enabled model ids.
type modelsEnabledError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Models  []string `json:"models"`
}

// ProvidersHandler serves the vendor key install/rotate/remove admin
// endpoints.
type ProvidersHandler struct {
	db         *pgxpool.Pool
	activity   *activity.Recorder
	gatewayURL string
	serviceKey string
	client     *http.Client
	kube       kube.ProviderObjects
	// models backs the gateway_too_old / providers cache reads (cachedProviders)
	// and is used to kick an immediate SyncModels after a successful write.
	models *ModelHandler
	// secureCookies mirrors auth.Config.SecureCookie (SECURE_COOKIES) -- see
	// keyInstallTransportOK's doc comment for how PutKey/PostVerify use it.
	secureCookies bool
	// vendorLocks holds one *sync.Mutex per validVendor entry, built once
	// here and never mutated afterward (so concurrent map reads from
	// acquireVendorLock need no additional guard) -- see that method's doc
	// comment.
	vendorLocks map[string]*sync.Mutex
}

// NewProvidersHandler constructs a ProvidersHandler. providerObjects is
// typically *kube.Writer in production and a fake in tests. secureCookies
// mirrors auth.Config.SecureCookie (SECURE_COOKIES) -- see
// keyInstallTransportOK's doc comment.
func NewProvidersHandler(pool *pgxpool.Pool, rec *activity.Recorder, gatewayURL, serviceKey string, providerObjects kube.ProviderObjects, models *ModelHandler, secureCookies bool) *ProvidersHandler {
	return &ProvidersHandler{
		db:            pool,
		activity:      rec,
		gatewayURL:    gatewayURL,
		serviceKey:    serviceKey,
		client:        &http.Client{Timeout: gatewayProviderCallTimeout},
		kube:          providerObjects,
		models:        models,
		secureCookies: secureCookies,
		vendorLocks: map[string]*sync.Mutex{
			vendorAnthropic: {},
			vendorOpenAI:    {},
		},
	}
}

// vendorParam extracts and validates :vendor, writing a 404 and reporting
// ok=false on an unrecognized value.
func (h *ProvidersHandler) vendorParam(c *gin.Context) (vendor string, ok bool) {
	v := c.Param("vendor")
	if !validVendor(v) {
		errJSON(c, http.StatusNotFound, "unknown_vendor", "unknown vendor")
		return "", false
	}
	return v, true
}

// requireUIKeyInstall refuses with 409 gateway_too_old when the gateway's
// last-synced provider_features block doesn't name "ui_key_install" --
// PLAN-UI v2.1's gate on install/filter/delete (not verify, which is
// harmless and read-only).
func (h *ProvidersHandler) requireUIKeyInstall(c *gin.Context) bool {
	_, features := h.models.cachedProviders()
	if !hasProviderFeature(features, providerFeatureUIKeyInstall) {
		errJSON(c, http.StatusConflict, "gateway_too_old",
			"the gateway does not support installing vendor keys from the UI yet; redeploy the gateway to the current version, or use make set-<vendor>-key")
		return false
	}
	return true
}

// requireKubeAvailable refuses with the same 503 kubeErrorJSON gives
// kube.ErrNotInCluster when this deployment cannot write the cluster
// objects this flow needs at all -- checked before any gateway call so a
// doomed request never spends a vendor API call it can't act on the result
// of.
func (h *ProvidersHandler) requireKubeAvailable(c *gin.Context) bool {
	if !h.kube.Available() {
		kubeErrorJSON(c, kube.ErrNotInCluster)
		return false
	}
	return true
}

// kubeNotInClusterMessage, kubeObjectMissingMessage, and
// kubeForbiddenMessage are the exact, runbook-quoted 503 messages for
// internal/kube's three named failure modes -- confirmed with the web
// engineer and carla's runbook; do not reword without updating both.
const (
	kubeNotInClusterMessage  = "Installing a key from here needs Darkplum to be running in Kubernetes. Use make set-<vendor>-key instead."
	kubeObjectMissingMessage = "This cluster was deployed before keys could be installed from here. Run make deploy-platform once, then try again."
	kubeForbiddenMessage     = "Darkplum is not permitted to write the vendor-key Secret on this cluster. Run make deploy-platform to restore its role, then try again."
	kubeUnavailableMessage   = "Could not reach the Kubernetes API server; try again."
)

// kubeErrorJSON maps one of internal/kube's four errors to a distinct,
// plain-language 503 telling the admin what to do -- never the underlying
// request or response body (see internal/kube's own doc comment on why).
// Classification is by errors.Is on the error's TYPE only, never its
// Error() text, so a hypothetical future ProviderObjects implementation
// that embeds request content in its error string still can't leak it here.
func kubeErrorJSON(c *gin.Context, err error) {
	switch {
	case errors.Is(err, kube.ErrNotInCluster):
		errJSON(c, http.StatusServiceUnavailable, "not_in_cluster", kubeNotInClusterMessage)
	case errors.Is(err, kube.ErrObjectMissing):
		errJSON(c, http.StatusServiceUnavailable, "object_missing", kubeObjectMissingMessage)
	case errors.Is(err, kube.ErrForbidden):
		errJSON(c, http.StatusServiceUnavailable, "forbidden", kubeForbiddenMessage)
	default:
		errJSON(c, http.StatusServiceUnavailable, "unavailable", kubeUnavailableMessage)
	}
}

// gatewayVerifyResult is the 200 shape of the gateway's
// POST /v1/providers/:vendor/verify.
type gatewayVerifyResult struct {
	Reachable []string `json:"reachable"`
	Admitted  []string `json:"admitted"`
}

// gatewayVerifyErrorEnvelope is the 422 shape. Message is decoded but never
// used -- see translateVerifyError and this file's package doc comment.
type gatewayVerifyErrorEnvelope struct {
	Error struct {
		Code             string `json:"code"`
		Message          string `json:"message"`
		VendorHTTPStatus int    `json:"vendor_http_status"`
		VendorErrorType  string `json:"vendor_error_type"`
	} `json:"error"`
}

// gatewayVerify calls the gateway's POST /v1/providers/:vendor/verify. Its
// returned *apiErrorBody is always built by translateVerifyError from the
// gateway's error CODE alone -- never from the gateway's own message text,
// so a gateway response that echoes the submitted key back in an error
// string can never reach this api's own response. A non-nil error return
// means the gateway could not be reached or returned something this api
// doesn't understand; callers treat that as "verification failed, nothing
// was stored".
//
// workspaceID, when non-nil, is sent as the gateway's workspace_id (a pointer
// so an explicit "" -- check with no workspace -- survives); nil omits it.
func (h *ProvidersHandler) gatewayVerify(ctx context.Context, vendor, key string, filter []string, workspaceID *string) (*gatewayVerifyResult, *apiErrorBody, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayProviderCallTimeout)
	defer cancel()

	body, err := json.Marshal(struct {
		Key         string   `json:"key,omitempty"`
		ModelFilter []string `json:"model_filter"`
		WorkspaceID *string  `json:"workspace_id,omitempty"`
	}{Key: key, ModelFilter: filter, WorkspaceID: workspaceID})
	if err != nil {
		return nil, nil, fmt.Errorf("encode verify request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/providers/%s/verify", h.gatewayURL, vendor), bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build verify request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.serviceKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("call gateway verify: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out gatewayVerifyResult
		if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
			return nil, nil, fmt.Errorf("decode gateway verify response: %w", err)
		}
		return &out, nil, nil
	case http.StatusUnprocessableEntity:
		var out gatewayVerifyErrorEnvelope
		if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(&out); err != nil {
			return nil, nil, fmt.Errorf("decode gateway verify error: %w", err)
		}
		if out.Error.Code == "vendor_error" {
			translated := translateVendorError(vendor, out.Error.VendorHTTPStatus, out.Error.VendorErrorType)
			return nil, &translated, nil
		}
		translated := translateVerifyError(out.Error.Code)
		return nil, &translated, nil
	default:
		return nil, nil, fmt.Errorf("gateway verify returned %d", resp.StatusCode)
	}
}

// gatewayRefresh calls the gateway's POST /v1/providers/:vendor/refresh,
// which starts (or extends) a short hint window on the gateway side. A
// non-202 or transport failure is non-fatal to the caller -- see
// PutKey/PutFilter/DeleteKey's warnings handling.
func (h *ProvidersHandler) gatewayRefresh(ctx context.Context, vendor string) error {
	ctx, cancel := context.WithTimeout(ctx, gatewayProviderCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/providers/%s/refresh", h.gatewayURL, vendor), bytes.NewReader([]byte("{}")))
	if err != nil {
		return fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.serviceKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("call gateway refresh: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("gateway refresh returned %d", resp.StatusCode)
	}
	return nil
}

// syncModelsBoundedTimeout bounds the detached SyncModels call
// PutKey/PutFilter/DeleteKey fire after a successful write -- long enough
// for a real gateway round trip, short enough that a hung gateway can't pile
// these goroutines up indefinitely across a busy admin session.
const syncModelsBoundedTimeout = 30 * time.Second

// syncModelsBounded runs h.models.SyncModels with syncModelsBoundedTimeout,
// for use in the detached `go h.syncModelsBounded()` calls PutKey, PutFilter,
// and DeleteKey each make after a successful write -- an unbounded
// context.Background() call here would never be canceled if the gateway
// hangs, leaving the goroutine (and its outstanding HTTP request) alive for
// the lifetime of the process.
func (h *ProvidersHandler) syncModelsBounded() {
	ctx, cancel := context.WithTimeout(context.Background(), syncModelsBoundedTimeout)
	defer cancel()
	h.models.SyncModels(ctx)
}

// mirrorConfigMap writes this vendor's filter and its SET_AT stamp into the
// darkplum-provider-config ConfigMap, matching the Makefile's own
// <VENDOR>_MODEL_FILTER[_SET_AT] keys exactly.
//
// For anthropic the workspace id is written in the same patch (ANTHROPIC_WORKSPACE_ID,
// empty string clears), so filter, stamp and workspace id always land together.
func (h *ProvidersHandler) mirrorConfigMap(ctx context.Context, vendor string, filter []string, filterSetAt time.Time, workspaceID string) error {
	kv := map[string]string{
		configFilterKey(vendor):      strings.Join(filter, ","),
		configFilterSetAtKey(vendor): filterSetAt.UTC().Format(time.RFC3339),
	}
	if vendor == vendorAnthropic {
		kv[configWorkspaceIDKey] = workspaceID
	}
	return h.kube.SetConfig(ctx, kv)
}

// envFilterSetAtFor and configuredFor read the gateway's last-synced
// per-vendor status for vendor out of the cached providers block --
// providersBefore must come from h.models.cachedProviders(), read ONCE per
// request before any write, so a handler's "was this vendor configured
// before this action" check and its skew-proof stamp computation agree on
// the same snapshot.
func envFilterSetAtFor(providers []gatewayProviderStatus, vendor string) *time.Time {
	for _, p := range providers {
		if p.Name == vendor {
			return p.EnvFilterSetAt
		}
	}
	return nil
}

func configuredFor(providers []gatewayProviderStatus, vendor string) bool {
	for _, p := range providers {
		if p.Name == vendor {
			return p.Configured
		}
	}
	return false
}

// effectiveFilterFor reads the gateway's own reported effective Filter for
// vendor out of the cached providers block -- the filter actually in force
// right now, as opposed to model_filter_after (what was just SUBMITTED,
// which never got saved when this is used -- see partiallyInstalledMessage).
func effectiveFilterFor(providers []gatewayProviderStatus, vendor string) []string {
	for _, p := range providers {
		if p.Name == vendor {
			return p.Filter
		}
	}
	return nil
}

// partiallyInstalledMessage is PutKey's 500 partially_installed message: the
// key IS installed and verified (the Secret PATCH already succeeded), but
// the settings upsert failed even after a retry, so the submitted
// model_filter was never saved. It names whichever filter is actually in
// force right now -- providersBefore's gateway-reported Filter for vendor,
// the snapshot taken before this request touched anything -- rather than
// the filter this request tried and failed to save, so the admin knows
// exactly what Darkplum is still offering while they retry.
func partiallyInstalledMessage(vendor string, providersBefore []gatewayProviderStatus) string {
	filter := effectiveFilterFor(providersBefore, vendor)
	inForce := "no models are offered"
	if len(filter) > 0 {
		inForce = strings.Join(filter, ", ")
	}
	return fmt.Sprintf(
		"The key was stored, but your model selection was not saved. Until it is, Darkplum keeps offering: %s. Open Change models to try again.",
		inForce,
	)
}

// verifyRequest is POST /api/v1/providers/:vendor/verify's body. Key empty
// means "verify with the already-installed credential" (gateway
// DiscoverModels path); ModelFilter empty means "just list what's
// reachable".
type verifyRequest struct {
	Key         string   `json:"key"`
	ModelFilter []string `json:"model_filter"`
	// WorkspaceID (anthropic only): scopes a submitted key's check to that
	// workspace; with no key, overrides the installed workspace id for the
	// check. Absent = the gateway's own effective value; "" = no workspace.
	WorkspaceID *string `json:"workspace_id"`
}

type verifyResponseBody struct {
	Reachable []string `json:"reachable"`
	Admitted  []string `json:"admitted"`
}

// PostVerify handles POST /api/v1/providers/:vendor/verify.
func (h *ProvidersHandler) PostVerify(c *gin.Context) {
	vendor, ok := h.vendorParam(c)
	if !ok {
		return
	}

	var req verifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "invalid_request", invalidJSONMessage)
		return
	}
	if req.Key != "" && !validKeyFormat(req.Key) {
		errJSON(c, http.StatusBadRequest, "invalid_key_format", "key must be 20-512 characters of letters, digits, underscore, or hyphen")
		return
	}
	if !checkWorkspaceID(c, vendor, req.WorkspaceID) {
		return
	}
	// Only when a key was actually submitted -- verifying with the
	// already-installed credential (req.Key == "") never puts a credential
	// on the wire from the browser, so there is nothing this connection's
	// transport could expose.
	if req.Key != "" && !h.requireSecureTransport(c) {
		return
	}

	ctx := c.Request.Context()
	result, apiErr, err := h.gatewayVerify(ctx, vendor, req.Key, req.ModelFilter, req.WorkspaceID)
	if err != nil {
		slog.WarnContext(ctx, "provider verify: gateway call failed", "vendor", vendor, "err", err)
		errJSON(c, http.StatusBadGateway, "gateway_unreachable", "could not reach the gateway to verify this key")
		return
	}
	if apiErr != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": apiErr})
		return
	}
	if len(req.ModelFilter) > 0 && len(result.Admitted) == 0 {
		errJSON(c, http.StatusUnprocessableEntity, "filter_matched_nothing", verifyErrorMessages["filter_matched_nothing"])
		return
	}

	c.JSON(http.StatusOK, verifyResponseBody{Reachable: result.Reachable, Admitted: result.Admitted})
}

// putKeyRequest is PUT /api/v1/providers/:vendor/key's body.
type putKeyRequest struct {
	Key         string   `json:"key"`
	ModelFilter []string `json:"model_filter"`
	// WorkspaceID (anthropic only): absent/null keeps the current workspace
	// id, "" clears it, a value sets it.
	WorkspaceID *string `json:"workspace_id"`
	AllowAll    bool    `json:"allow_all"`
}

type putKeyResponseBody struct {
	Admitted        []string  `json:"admitted"`
	InstallingUntil time.Time `json:"installing_until"`
	Warnings        []string  `json:"warnings,omitempty"`
}

// PutKey handles PUT /api/v1/providers/:vendor/key -- install or rotate.
//
// Order: validate format -> validate model_filter -> insecure_transport
// (403) -> gateway_too_old (409) -> kube.Available() (503) -> acquire this
// vendor's in-process lock (409 busy if another request holds it) ->
// read-previous (short tx) -> gateway verify -> PATCH Secret -> upsert
// vendor_provider_settings, clearing removing_until (short tx) -> mirror
// ConfigMap -> release the lock (deferred) -> audit -> gateway refresh ->
// immediate (async, bounded) SyncModels. No pooled database connection is
// held across gatewayVerify or the kube PATCH -- see acquireVendorLock's and
// withShortTx's doc comments.
//
// Failure story: nothing is written before the Secret PATCH succeeds --
// every check above it, including a failed gateway verify, changes nothing.
// After the PATCH, the settings upsert (and its Commit) is retried once; if
// either still fails, the response is 500 partially_installed naming what
// filter is still actually in force (partiallyInstalledMessage) -- the key
// IS installed and verified, and re-submitting is safe (the upsert is
// idempotent). Critically, a failed Commit after a successful upsert
// statement is indistinguishable here from a failed statement: withShortTx
// returns the Commit error the same way, so this never falls through to a
// generic 500 claiming nothing changed when the Secret PATCH already
// landed. A mirror or refresh failure past that point is logged and
// returned in a non-fatal `warnings` array -- never a failed install.
func (h *ProvidersHandler) PutKey(c *gin.Context) {
	vendor, ok := h.vendorParam(c)
	if !ok {
		return
	}

	var req putKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "invalid_request", invalidJSONMessage)
		return
	}
	if !validKeyFormat(req.Key) {
		errJSON(c, http.StatusBadRequest, "invalid_key_format", "key must be 20-512 characters of letters, digits, underscore, or hyphen")
		return
	}
	if len(req.ModelFilter) == 0 {
		errJSON(c, http.StatusBadRequest, "filter_required", "model_filter must name at least one model or family")
		return
	}
	if !checkWorkspaceID(c, vendor, req.WorkspaceID) {
		return
	}
	if pattern, bad := firstBareWildcard(req.ModelFilter); bad && !req.AllowAll {
		errJSON(c, http.StatusUnprocessableEntity, "filter_too_broad", bareWildcardMessage(vendor, pattern))
		return
	}
	if !h.requireSecureTransport(c) {
		return
	}
	if !h.requireUIKeyInstall(c) {
		return
	}
	if !h.requireKubeAvailable(c) {
		return
	}

	ctx := c.Request.Context()

	// One snapshot of the gateway's last-synced status for this vendor,
	// taken before any write -- see envFilterSetAtFor/configuredFor's doc
	// comment.
	providersBefore, _ := h.models.cachedProviders()
	wasConfigured := configuredFor(providersBefore, vendor)
	envFilterSetAt := envFilterSetAtFor(providersBefore, vendor)

	var actorID *string
	if uid := c.GetString("user_id"); uid != "" {
		actorID = &uid
	}

	release, ok := h.acquireVendorLock(vendor)
	if !ok {
		vendorBusyJSON(c)
		return
	}
	defer release()

	var previousFilter []string
	var previousFilterSetAt *time.Time
	var previousWorkspaceID *string
	previousRowExists := false
	if err := h.withShortTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT model_filter, filter_set_at, workspace_id FROM vendor_provider_settings WHERE vendor = $1`, vendor,
		).Scan(&previousFilter, &previousFilterSetAt, &previousWorkspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		previousRowExists = err == nil
		return err
	}); err != nil {
		slog.ErrorContext(ctx, "put provider key: read previous settings failed", "vendor", vendor, "err", err)
	}

	currentWorkspace := currentWorkspaceID(vendor, previousRowExists, previousWorkspaceID, providersBefore)
	workspaceID := finalWorkspaceID(req.WorkspaceID, currentWorkspace)
	var verifyWorkspace *string
	if vendor == vendorAnthropic {
		verifyWorkspace = &workspaceID
	}

	result, apiErr, vErr := h.gatewayVerify(ctx, vendor, req.Key, req.ModelFilter, verifyWorkspace)
	if vErr != nil {
		slog.WarnContext(ctx, "put provider key: gateway verify failed", "vendor", vendor, "err", vErr)
		errJSON(c, http.StatusBadGateway, "gateway_unreachable", "could not reach the gateway to verify this key; nothing was stored")
		return
	}
	if apiErr != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": apiErr})
		return
	}
	if len(result.Admitted) == 0 {
		errJSON(c, http.StatusUnprocessableEntity, "filter_matched_nothing", verifyErrorMessages["filter_matched_nothing"])
		return
	}

	// Point of no return: everything above this line has written nothing,
	// so a failure anywhere above changes nothing (see PutKey's doc
	// comment).
	if err := h.kube.SetSecretKey(ctx, secretFileName(vendor), []byte(req.Key)); err != nil {
		kubeErrorJSON(c, err)
		return
	}

	now := time.Now().UTC()
	filterSetAt := nextFilterSetAt(now, envFilterSetAt, previousFilterSetAt)
	installingUntil := now.Add(installingWindow)

	upsertOnce := func() error {
		return h.withShortTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, installing_until, removing_until, updated_at, updated_by, workspace_id)
				VALUES ($1, $2, $3, $4, NULL, now(), $5, NULLIF($6, ''))
				ON CONFLICT (vendor) DO UPDATE
					SET model_filter = EXCLUDED.model_filter,
						filter_set_at = EXCLUDED.filter_set_at,
						installing_until = EXCLUDED.installing_until,
						removing_until = NULL,
						updated_at = now(),
						updated_by = EXCLUDED.updated_by,
						workspace_id = EXCLUDED.workspace_id
			`, vendor, req.ModelFilter, filterSetAt, installingUntil, actorID, workspaceID)
			return err
		})
	}
	upsertFailed := false
	if err := retryOnce(upsertOnce); err != nil {
		// The key IS installed already; there is nothing left to roll back.
		// Report partially_installed below -- covers both a failed
		// statement and a failed Commit, since withShortTx returns either
		// the same way (see this function's doc comment).
		slog.ErrorContext(ctx, "put provider key: settings upsert failed after retry", "vendor", vendor, "err", err)
		upsertFailed = true
	}

	var warnings []string
	if !upsertFailed {
		if err := retryOnce(func() error { return h.mirrorConfigMap(ctx, vendor, req.ModelFilter, filterSetAt, workspaceID) }); err != nil {
			slog.ErrorContext(ctx, "put provider key: configmap mirror failed", "vendor", vendor, "err", err)
			warnings = append(warnings, "the model filter was saved but has not yet reached the gateway's configuration; it will apply on the gateway's next resync")
		}
	}

	if upsertFailed {
		errJSON(c, http.StatusInternalServerError, "partially_installed", partiallyInstalledMessage(vendor, providersBefore))
		return
	}

	eventType := activity.ProviderKeyInstalled
	if wasConfigured {
		eventType = activity.ProviderKeyRotated
	}
	evt := activity.FromContext(c, eventType)
	// TargetID is left unset: activity_events.target_id is UUID-typed
	// (migration 005), and vendor is a plain string ("anthropic"/"openai")
	// -- not a UUID -- so it is carried in Detail only, matching
	// routing.go's DeleteAlias (its alias is likewise a non-UUID name).
	evt.TargetType = activity.StrPtr("vendor_provider")
	evt.Detail = map[string]any{
		"vendor":              vendor,
		"key_last4":           lastN(req.Key, 4),
		"model_filter_before": previousFilter,
		"model_filter_after":  req.ModelFilter,
	}
	workspaceAuditDetail(evt.Detail, currentWorkspace, workspaceID)
	h.activity.Record(ctx, evt)

	if err := retryOnce(func() error { return h.gatewayRefresh(ctx, vendor) }); err != nil {
		slog.WarnContext(ctx, "put provider key: gateway refresh hint failed", "vendor", vendor, "err", err)
		warnings = append(warnings, "the gateway will pick up this change on its normal schedule instead of immediately")
	}

	h.models.Kick()
	go h.syncModelsBounded()

	c.JSON(http.StatusOK, putKeyResponseBody{Admitted: result.Admitted, InstallingUntil: installingUntil, Warnings: warnings})
}

// putFilterRequest is PUT /api/v1/providers/:vendor/filter's body.
type putFilterRequest struct {
	ModelFilter []string `json:"model_filter"`
	// WorkspaceID (anthropic only): absent/null keeps the current workspace
	// id, "" clears it, a value sets it.
	WorkspaceID *string `json:"workspace_id"`
	AllowAll    bool    `json:"allow_all"`
}

type putFilterResponseBody struct {
	Admitted []string `json:"admitted"`
	Warnings []string `json:"warnings,omitempty"`
}

// PutFilter handles PUT /api/v1/providers/:vendor/filter -- change which
// models are catalogued for an already-installed key ("Adopt" is this route
// called with the gateway's own env-reported filter). Verified with the
// INSTALLED key (key omitted from the gateway verify call); does not touch
// installing_until or removing_until (this is not an install or removal
// action, so neither progress state should appear).
//
// Like PutKey, the read-previous -> verify -> upsert -> mirror sequence runs
// inside this vendor's in-process lock (409 busy if PutKey/DeleteKey/another
// PutFilter for the same vendor already holds it) -- see acquireVendorLock's
// doc comment. As with PutKey, no pooled database connection is held across
// gatewayVerify: the upsert runs in its own short transaction (withShortTx)
// after verify returns, not inside one spanning the whole request.
func (h *ProvidersHandler) PutFilter(c *gin.Context) {
	vendor, ok := h.vendorParam(c)
	if !ok {
		return
	}

	var req putFilterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "invalid_request", invalidJSONMessage)
		return
	}
	if len(req.ModelFilter) == 0 {
		errJSON(c, http.StatusBadRequest, "filter_required", "model_filter must name at least one model or family")
		return
	}
	if !checkWorkspaceID(c, vendor, req.WorkspaceID) {
		return
	}
	if pattern, bad := firstBareWildcard(req.ModelFilter); bad && !req.AllowAll {
		errJSON(c, http.StatusUnprocessableEntity, "filter_too_broad", bareWildcardMessage(vendor, pattern))
		return
	}
	if !h.requireUIKeyInstall(c) {
		return
	}
	if !h.requireKubeAvailable(c) {
		return
	}

	ctx := c.Request.Context()

	providersBefore, _ := h.models.cachedProviders()
	envFilterSetAt := envFilterSetAtFor(providersBefore, vendor)

	var actorID *string
	if uid := c.GetString("user_id"); uid != "" {
		actorID = &uid
	}

	release, ok := h.acquireVendorLock(vendor)
	if !ok {
		vendorBusyJSON(c)
		return
	}
	defer release()

	var previousFilter []string
	var previousFilterSetAt *time.Time
	var previousWorkspaceID *string
	previousRowExists := false
	if err := h.withShortTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT model_filter, filter_set_at, workspace_id FROM vendor_provider_settings WHERE vendor = $1`, vendor,
		).Scan(&previousFilter, &previousFilterSetAt, &previousWorkspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		previousRowExists = err == nil
		return err
	}); err != nil {
		slog.ErrorContext(ctx, "put provider filter: read previous settings failed", "vendor", vendor, "err", err)
	}

	currentWorkspace := currentWorkspaceID(vendor, previousRowExists, previousWorkspaceID, providersBefore)
	workspaceID := finalWorkspaceID(req.WorkspaceID, currentWorkspace)
	var verifyWorkspace *string
	if vendor == vendorAnthropic {
		verifyWorkspace = &workspaceID
	}

	result, apiErr, vErr := h.gatewayVerify(ctx, vendor, "", req.ModelFilter, verifyWorkspace)
	if vErr != nil {
		slog.WarnContext(ctx, "put provider filter: gateway verify failed", "vendor", vendor, "err", vErr)
		errJSON(c, http.StatusBadGateway, "gateway_unreachable", "could not reach the gateway to verify this filter; nothing was changed")
		return
	}
	if apiErr != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": apiErr})
		return
	}
	if len(result.Admitted) == 0 {
		errJSON(c, http.StatusUnprocessableEntity, "filter_matched_nothing", verifyErrorMessages["filter_matched_nothing"])
		return
	}

	now := time.Now().UTC()
	filterSetAt := nextFilterSetAt(now, envFilterSetAt, previousFilterSetAt)

	// installing_until and removing_until are deliberately absent from both
	// the INSERT column list and the UPDATE SET clause: a fresh row (Adopt,
	// or a key installed only via `make set-*-key`) gets NULL for both (no
	// install/removal in progress), and an existing row's current values
	// are left untouched either way.
	upsertOnce := func() error {
		return h.withShortTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO vendor_provider_settings (vendor, model_filter, filter_set_at, updated_at, updated_by, workspace_id)
				VALUES ($1, $2, $3, now(), $4, NULLIF($5, ''))
				ON CONFLICT (vendor) DO UPDATE
					SET model_filter = EXCLUDED.model_filter,
						filter_set_at = EXCLUDED.filter_set_at,
						updated_at = now(),
						updated_by = EXCLUDED.updated_by,
						workspace_id = EXCLUDED.workspace_id
			`, vendor, req.ModelFilter, filterSetAt, actorID, workspaceID)
			return err
		})
	}
	if err := retryOnce(upsertOnce); err != nil {
		slog.ErrorContext(ctx, "put provider filter: settings upsert failed after retry", "vendor", vendor, "err", err)
		errJSON(c, http.StatusInternalServerError, "internal_error", "the filter could not be saved; nothing was changed")
		return
	}

	var warnings []string
	if err := retryOnce(func() error { return h.mirrorConfigMap(ctx, vendor, req.ModelFilter, filterSetAt, workspaceID) }); err != nil {
		slog.ErrorContext(ctx, "put provider filter: configmap mirror failed", "vendor", vendor, "err", err)
		warnings = append(warnings, "the model filter was saved but has not yet reached the gateway's configuration; it will apply on the gateway's next resync")
	}

	evt := activity.FromContext(c, activity.ProviderFilterChanged)
	// TargetID is left unset: activity_events.target_id is UUID-typed
	// (migration 005), and vendor is a plain string ("anthropic"/"openai")
	// -- not a UUID -- so it is carried in Detail only, matching
	// routing.go's DeleteAlias (its alias is likewise a non-UUID name).
	evt.TargetType = activity.StrPtr("vendor_provider")
	evt.Detail = map[string]any{
		"vendor":              vendor,
		"model_filter_before": previousFilter,
		"model_filter_after":  req.ModelFilter,
	}
	workspaceAuditDetail(evt.Detail, currentWorkspace, workspaceID)
	h.activity.Record(ctx, evt)

	if err := retryOnce(func() error { return h.gatewayRefresh(ctx, vendor) }); err != nil {
		slog.WarnContext(ctx, "put provider filter: gateway refresh hint failed", "vendor", vendor, "err", err)
		warnings = append(warnings, "the gateway will pick up this change on its normal schedule instead of immediately")
	}

	h.models.Kick()
	go h.syncModelsBounded()

	c.JSON(http.StatusOK, putFilterResponseBody{Admitted: result.Admitted, Warnings: warnings})
}

// deleteKeyRequest is DELETE /api/v1/providers/:vendor/key's body. Confirm
// must equal the vendor name (typed confirmation stays on removal only --
// PLAN-UI v2.1 dropped it from install/rotate, since a rotated key is
// verified before anything is stored).
type deleteKeyRequest struct {
	Confirm       string `json:"confirm"`
	DisableModels bool   `json:"disable_models"`
}

type deleteKeyResponseBody struct {
	DisabledModels []string `json:"disabled_models,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// DeleteKey handles DELETE /api/v1/providers/:vendor/key.
//
// Order (the Secret PATCH is the point of no return -- see below): validate
// confirm -> gateway_too_old (409) -> kube.Available() (503) -> acquire this
// vendor's in-process lock -> check enabled models (short tx), 409
// models_enabled (NO writes yet) unless disable_models -> PATCH the key out
// -> THEN, in one more short tx, disable those models and update the
// settings row (keep model_filter -- richard: "Your model filter is kept"
// -- clear installing_until, set removing_until) -> release the lock
// (deferred) -> audit -> gateway refresh -> immediate (async, bounded)
// SyncModels. The ConfigMap mirror is left unchanged. As with PutKey/
// PutFilter, no pooled database connection is held across the kube call --
// see acquireVendorLock's doc comment.
//
// The 409 check happens before ANY write, so a request refused for enabled
// models changes nothing. Once the Secret PATCH succeeds, disabling the
// now-orphaned models is retried once (via a SAVEPOINT within the
// disable-and-clear short tx, so one statement's failure doesn't abort the
// other); if it still fails, DeleteKey does NOT fail the removal -- the key
// is gone either way -- it reports a `warnings` entry naming which models
// are still enabled (and may now fail at request time) instead.
func (h *ProvidersHandler) DeleteKey(c *gin.Context) {
	vendor, ok := h.vendorParam(c)
	if !ok {
		return
	}

	var req deleteKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errJSON(c, http.StatusBadRequest, "invalid_request", invalidJSONMessage)
		return
	}
	if req.Confirm != vendor {
		errJSON(c, http.StatusBadRequest, "confirm_mismatch", fmt.Sprintf("confirm must equal %q", vendor))
		return
	}
	if !h.requireUIKeyInstall(c) {
		return
	}
	if !h.requireKubeAvailable(c) {
		return
	}

	ctx := c.Request.Context()

	var actorID *string
	if uid := c.GetString("user_id"); uid != "" {
		actorID = &uid
	}

	release, ok := h.acquireVendorLock(vendor)
	if !ok {
		vendorBusyJSON(c)
		return
	}
	defer release()

	var enabledModels []string
	if err := h.withShortTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT model_id FROM models WHERE provider = $1 AND enabled = true ORDER BY model_id`, vendor)
		if err != nil {
			return fmt.Errorf("check enabled models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("scan enabled models: %w", err)
			}
			enabledModels = append(enabledModels, id)
		}
		return rows.Err()
	}); err != nil {
		slog.ErrorContext(ctx, "delete provider key: check enabled models failed", "vendor", vendor, "err", err)
		errJSON(c, http.StatusInternalServerError, "internal_error", "the key could not be removed; nothing was changed")
		return
	}

	if len(enabledModels) > 0 && !req.DisableModels {
		c.JSON(http.StatusConflict, gin.H{"error": modelsEnabledError{
			Code:    "models_enabled",
			Message: fmt.Sprintf("%s has %d enabled model(s); turn them off first, or resubmit with disable_models: true", vendorDisplayName(vendor), len(enabledModels)),
			Models:  enabledModels,
		}})
		return
	}

	// Point of no return: nothing above this line has written anything --
	// see this handler's doc comment. Every enabled model is exactly as it
	// was if the PATCH below fails.
	if err := h.kube.RemoveSecretKey(ctx, secretFileName(vendor)); err != nil {
		kubeErrorJSON(c, err)
		return
	}

	var disabledModels []string
	var previousFilter []string
	var warnings []string
	if err := h.withShortTx(ctx, func(tx pgx.Tx) error {
		if len(enabledModels) > 0 {
			disable := func() error {
				_, err := tx.Exec(ctx,
					`UPDATE models SET enabled = false, updated_at = now() WHERE provider = $1 AND enabled = true`, vendor,
				)
				return err
			}
			if err := savepointRetryOnce(ctx, tx, "delete_key_disable_models", disable); err != nil {
				// Never a failed removal -- the key is already gone. Name
				// which models are still enabled instead.
				slog.ErrorContext(ctx, "delete provider key: disable models failed after retry", "vendor", vendor, "err", err)
				warnings = append(warnings, fmt.Sprintf(
					"the key was removed, but these models are still enabled and may fail until you turn them off from Models: %s",
					strings.Join(enabledModels, ", "),
				))
			} else {
				disabledModels = enabledModels
			}
		}

		removingUntil := time.Now().UTC().Add(removingWindow)
		clear := func() error {
			return tx.QueryRow(ctx, `
				UPDATE vendor_provider_settings
				SET installing_until = NULL, removing_until = $2, updated_at = now(), updated_by = $3
				WHERE vendor = $1
				RETURNING model_filter
			`, vendor, removingUntil, actorID).Scan(&previousFilter)
		}
		if err := savepointRetryOnce(ctx, tx, "delete_key_clear", clear); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// The key is already removed from the cluster at this point -- a
			// failure here is a non-fatal bookkeeping miss (a still-open
			// installing_until, if any, simply expires on its own once the
			// window closes; the UI's timeout state handles that already).
			slog.ErrorContext(ctx, "delete provider key: clear/set hint windows failed", "vendor", vendor, "err", err)
		}
		return nil
	}); err != nil {
		// Only a Commit failure lands here -- the two statements above
		// always return nil from this closure, logging or warning instead
		// of failing it. The key is already removed from the cluster either
		// way; this mirrors the pre-existing message for that rare case.
		slog.ErrorContext(ctx, "delete provider key: settings transaction commit failed", "vendor", vendor, "err", err)
		errJSON(c, http.StatusInternalServerError, "internal_error", "the key could not be removed; nothing was changed")
		return
	}

	evt := activity.FromContext(c, activity.ProviderKeyRemoved)
	// TargetID is left unset: activity_events.target_id is UUID-typed
	// (migration 005), and vendor is a plain string ("anthropic"/"openai")
	// -- not a UUID -- so it is carried in Detail only, matching
	// routing.go's DeleteAlias (its alias is likewise a non-UUID name).
	evt.TargetType = activity.StrPtr("vendor_provider")
	detail := map[string]any{
		"vendor":              vendor,
		"model_filter_before": previousFilter,
		"model_filter_after":  previousFilter,
	}
	// disabled_models is omitted entirely (not even an empty array) when
	// nothing was disabled -- confirmed with the web engineer, who reads
	// its presence as "some models were turned off by this removal". Uses
	// disabledModels (what was ACTUALLY disabled), not enabledModels (what
	// was found enabled going in): the two differ when disabling failed and
	// warnings named the still-enabled set instead.
	if len(disabledModels) > 0 {
		detail["disabled_models"] = disabledModels
	}
	evt.Detail = detail
	h.activity.Record(ctx, evt)

	if err := retryOnce(func() error { return h.gatewayRefresh(ctx, vendor) }); err != nil {
		slog.WarnContext(ctx, "delete provider key: gateway refresh hint failed", "vendor", vendor, "err", err)
		warnings = append(warnings, "the gateway will notice the key is gone on its normal schedule instead of immediately")
	}

	go h.syncModelsBounded()

	c.JSON(http.StatusOK, deleteKeyResponseBody{DisabledModels: disabledModels, Warnings: warnings})
}
