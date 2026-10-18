package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// APIError is this package's name for the shared vendor error type (see
// internal/provider/vendorerror.go). Kept as a type alias — rather than
// switching every call site and test in this package to
// provider.VendorAPIError directly — so the error-envelope/status-mapping
// extraction (STEP 0 of the direct-vendor-provider work: openai needed the
// identical type, mapping, and http.Client construction) is behaviour- and
// API-preserving for this package.
type APIError = provider.VendorAPIError

// newAPIError builds an APIError with a fully-rendered OpenAI-compatible
// body. message must already be safe to show a caller — either a static
// string this package wrote itself, or deliberately generic text; a vendor's
// raw error message is NEVER passed in here (see translateError).
func newAPIError(statusCode int, errType, code, message string, retryAfter int, cause error) *APIError {
	return provider.NewVendorAPIError("anthropic", statusCode, errType, code, message, retryAfter, cause)
}

// newInvalidRequestError builds a 400 invalid_request error whose message is
// authored by this package (normalizeMessages' own validation messages) —
// always safe to show verbatim, matching the "invalid_request" /
// "invalid_request_error" code+type pair internal/handler/chat.go already
// uses for its own 400s.
func newInvalidRequestError(message string) *APIError {
	return provider.NewVendorInvalidRequestError("anthropic", message)
}

// authFailureReporter is this package's name for the shared reporter
// interface (see provider.AuthFailureReporter) — aliased for the same
// API-preservation reason as APIError above.
type authFailureReporter = provider.AuthFailureReporter

// translateError converts an error returned by the Anthropic SDK (or by this
// package's own credential/context handling) into an *APIError per the wire
// contract: 429 -> provider_rate_limited (429, Retry-After passed through);
// 401/403 -> provider_auth_failed (502, generic text, and — via status,
// which may be nil — Report state auth_failed to the vendor status
// registry); 400 -> invalid_request (400, generic text: the vendor's own
// message is never known to be free of account/org detail, so it is never
// relayed); 5xx/529 -> upstream_error (502); a context/network timeout ->
// upstream_timeout (504); anything else -> upstream_error (502).
func translateError(err error, status authFailureReporter, name string) error {
	if err == nil {
		return nil
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		// Already translated (e.g. normalizeMessages' own invalid_request).
		return apiErr
	}

	var sdkErr *sdk.Error
	if errors.As(err, &sdkErr) {
		// Status-code mapping (429/401.../403/400/else) is shared with
		// internal/provider/openai — see
		// provider.TranslateVendorHTTPStatus. 529 Overloaded (no named
		// net/http constant) and every other 4xx/5xx fall into its default
		// branch: there is no wire-contract code more specific than
		// upstream_error for them.
		translated := provider.TranslateVendorHTTPStatus("anthropic", sdkErr.StatusCode, retryAfterSeconds(sdkErr), status, err)
		// Only the vendor's error.type tag (sanitised), never its message.
		translated.VendorErrorType = provider.SanitizeVendorErrorType([]byte(sdkErr.RawJSON()))
		translated.WorkspaceRequired = IsWorkspaceRequiredError(sdkErr)
		return translated
	}

	var idleErr *idleTimeoutError
	if errors.As(err, &idleErr) {
		return newAPIError(http.StatusGatewayTimeout, "upstream_error", "upstream_timeout",
			"the model provider timed out", 0, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return newAPIError(http.StatusGatewayTimeout, "upstream_error", "upstream_timeout",
			"the model provider timed out", 0, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newAPIError(http.StatusGatewayTimeout, "upstream_error", "upstream_timeout",
			"the model provider timed out", 0, err)
	}

	return newAPIError(http.StatusBadGateway, "upstream_error", "upstream_error",
		"the model provider returned an error", 0, err)
}

// retryAfterSeconds reads the Retry-After header off a 429's underlying HTTP
// response, or 0 if absent/unparseable.
func retryAfterSeconds(sdkErr *sdk.Error) int {
	if sdkErr.Response == nil {
		return 0
	}
	raw := sdkErr.Response.Header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds < 0 {
		return 0
	}
	return seconds
}

// vendorErrorMessage extracts the "error.message" field from an SDK error's
// raw JSON body, for INTERNAL pattern-matching only (isSamplingParamUnsupportedError) —
// it must never be written into a response body or a log line, since it may
// carry account or request detail. Returns "" if the body doesn't parse.
func vendorErrorMessage(sdkErr *sdk.Error) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(sdkErr.RawJSON()), &envelope) != nil {
		return ""
	}
	return envelope.Error.Message
}

// samplingUnsupportedPhrases are the substrings a 400's message uses to
// describe a sampling parameter (temperature/top_p) that the target model
// does not accept — deliberately phrase-based rather than a per-model name
// list, so a newly released model needs no code change here.
var samplingUnsupportedPhrases = []string{
	"not supported", "unsupported", "deprecated", "does not support", "is not compatible",
}

// isSamplingParamUnsupportedError reports whether err is a 400 whose message
// indicates temperature/top_p is unsupported/deprecated for the requested
// model — the signal Complete/Stream use to retry exactly once with BOTH
// sampling parameters (temperature, top_p) omitted together, even though the
// vendor's message may only have named one of the two: unlike
// internal/provider/openai (which has a structured "param" field and drops
// only the one named parameter), buildParams' includeSampling flag is
// all-or-nothing, so the retry's log line and this doc comment both say
// "sampling parameters (temperature, top_p)" rather than implying only the
// mentioned one is dropped.
func isSamplingParamUnsupportedError(err error) bool {
	var sdkErr *sdk.Error
	if !errors.As(err, &sdkErr) || sdkErr.StatusCode != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(vendorErrorMessage(sdkErr))
	if msg == "" {
		return false
	}
	mentionsParam := strings.Contains(msg, "temperature") || strings.Contains(msg, "top_p")
	if !mentionsParam {
		return false
	}
	for _, phrase := range samplingUnsupportedPhrases {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// workspaceHeaderMarker is the substring Anthropic's "this API key is not
// scoped to a workspace" 400 message always contains (it names the header the
// request must add).
const workspaceHeaderMarker = "anthropic-workspace-id"

// IsWorkspaceRequiredError reports whether err is (or wraps) a vendor 400
// whose parsed error.message says the request must include the
// anthropic-workspace-id header. The message is matched here and never
// forwarded anywhere.
func IsWorkspaceRequiredError(err error) bool {
	var sdkErr *sdk.Error
	if !errors.As(err, &sdkErr) || sdkErr.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(vendorErrorMessage(sdkErr)), workspaceHeaderMarker)
}

// IsAuthError reports whether err is (or wraps) a vendor 401/403 response.
// Used by routing.NewAnthropicSource to classify a DiscoverModels failure as
// auth_failed vs unreachable without the routing package needing to import
// the vendor SDK directly (mirroring how bedrocksource.go depends only on
// internal/provider/bedrock, never aws-sdk-go-v2 itself).
func IsAuthError(err error) bool {
	var sdkErr *sdk.Error
	if errors.As(err, &sdkErr) {
		return sdkErr.StatusCode == http.StatusUnauthorized || sdkErr.StatusCode == http.StatusForbidden
	}
	return false
}

// HTTPStatusOf reports the vendor HTTP status code carried by a DiscoverModels
// failure, if any. Used by routing.vendorsource.go to build a closed-vocabulary
// discovery error (vendor + state + HTTP status) without ever formatting err's
// own Error() string — *sdk.Error's Error() renders the raw response JSON
// body plus the SDK's own Request-ID/Workspace-ID, none of which may ever
// reach a log line or be returned from DiscoverModels' caller unclassified.
func HTTPStatusOf(err error) (int, bool) {
	var sdkErr *sdk.Error
	if errors.As(err, &sdkErr) {
		return sdkErr.StatusCode, true
	}
	return 0, false
}

// idleTimeoutError is the context.Cause set when Stream's idle watchdog
// fires — see Stream in anthropic.go. A distinct type (rather than a bare
// fmt.Errorf) so translateError can classify it as upstream_timeout without
// string-matching.
type idleTimeoutError struct {
	timeout string
}

func (e *idleTimeoutError) Error() string {
	return fmt.Sprintf("anthropic: no stream events received for %s", e.timeout)
}
