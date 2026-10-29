package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

// APIError is this package's name for the shared vendor error type (see
// internal/provider/vendorerror.go, extracted from internal/provider/anthropic
// as STEP 0 of the direct-vendor-provider work) — an alias, matching
// internal/provider/anthropic's own APIError alias, so both vendor packages'
// tests use the identical `var apiErr *APIError` shape.
type APIError = provider.VendorAPIError

// authFailureReporter is this package's name for the shared reporter
// interface (see provider.AuthFailureReporter).
type authFailureReporter = provider.AuthFailureReporter

// translateCredentialError wraps a credsource.Source.Key() failure that
// surfaced at request time (rather than at a routing discovery tick, where
// it is handled directly) into a generic scrubbed upstream error. This
// should not normally be reachable in production: a model only appears in
// the route table once discovery has already proven a key is present and
// working, but a key that is rotated OUT from under a still-routable model
// (before the next discovery tick expires it) can hit this path. Mirrors
// internal/provider/anthropic's translateCredentialError exactly.
func translateCredentialError(err error) error {
	return provider.NewVendorAPIError("openai", http.StatusBadGateway, "upstream_error", "upstream_error",
		"the model provider is not currently configured", 0, err)
}

// translateTransportError converts an http.Client.Do failure (dial timeout,
// TLS handshake timeout, response-header timeout, connection refused, a
// cancelled/expired context) into a *APIError: a timeout-shaped failure maps
// to upstream_timeout (504); anything else maps to the generic
// upstream_error (502) — there is no way to tell a rate limit or an auth
// failure from a transport-level failure that never got a vendor HTTP
// response at all.
func translateTransportError(err error) error {
	if provider.IsTransportTimeout(err) {
		return provider.NewVendorAPIError("openai", http.StatusGatewayTimeout, "upstream_error", "upstream_timeout",
			"the model provider timed out", 0, err)
	}
	return provider.NewVendorAPIError("openai", http.StatusBadGateway, "upstream_error", "upstream_error",
		"the model provider returned an error", 0, err)
}

// translateVendorError converts a vendor HTTP response's status code into a
// *APIError per the wire contract (see provider.TranslateVendorHTTPStatus):
// 429 -> provider_rate_limited (Retry-After passed through, read from
// header if non-nil); 401/403 -> provider_auth_failed (502, and Report
// auth_failed into status if non-nil); 400 -> invalid_request (400, generic
// scrubbed text); everything else -> upstream_error (502). The vendor's own
// response body is deliberately never included in the returned error's
// message or Cause — only the status code is used for classification.
func translateVendorError(statusCode int, header http.Header, status authFailureReporter) error {
	retryAfter := 0
	if header != nil {
		retryAfter = retryAfterSeconds(header)
	}
	cause := fmt.Errorf("openai: vendor returned status %d", statusCode)
	return provider.TranslateVendorHTTPStatus("openai", statusCode, retryAfter, status, cause)
}

// retryAfterSeconds reads the Retry-After header off a vendor response, or 0
// if absent/unparseable/negative — seconds only, matching every vendor's
// actual behaviour (mirrors internal/provider/anthropic's identical helper,
// which reads the same header off the SDK-wrapped response instead).
// translateVendorErrorBody is translateVendorError plus the vendor's
// sanitised error.type, extracted from body (never the message).
func translateVendorErrorBody(statusCode int, header http.Header, status authFailureReporter, body []byte) error {
	err := translateVendorError(statusCode, header, status)
	if apiErr, ok := err.(*APIError); ok {
		apiErr.VendorErrorType = provider.SanitizeVendorErrorType(body)
	}
	return err
}

// rejectableParams are the request fields a vendor 400 is allowed to be
// attributed to by name: the ones this package sends that a given model can
// legitimately refuse (see translateVendor400). Matched against the vendor's
// structured error.param, never its message, and never relayed verbatim.
var rejectableParams = map[string]bool{
	"response_format":       true,
	"max_completion_tokens": true,
}

// translateVendor400 is translateVendorErrorBody for a 400, additionally
// naming the offending field when the vendor's structured error.param is one
// of rejectableParams (e.g. response_format on a model with no JSON mode).
func translateVendor400(header http.Header, status authFailureReporter, body []byte) error {
	err := translateVendorErrorBody(http.StatusBadRequest, header, status, body)
	apiErr, ok := err.(*APIError)
	if !ok {
		return err
	}
	var env vendorErrorEnvelope
	if json.Unmarshal(body, &env) == nil {
		if param := strings.ToLower(strings.TrimSpace(env.Error.Param)); rejectableParams[param] {
			apiErr.WithParam(param)
		}
	}
	return apiErr
}

func retryAfterSeconds(h http.Header) int {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return 0
	}
	return seconds
}

// discoveryError carries ONLY a raw vendor HTTP status code from
// DiscoverModels — deliberately never the response body (which for a 401 may
// echo back part of the submitted key, and for other statuses may carry
// account/org detail) — classified directly by IsAuthError below rather than
// through translateVendorError's client-facing JSON rendering —
// DiscoverModels never reaches an HTTP caller directly (see its own doc
// comment), so its returned error stays untranslated, mirroring
// internal/provider/anthropic's DiscoverModels (which returns the SDK's own
// *sdk.Error untranslated for the identical reason — see that package's
// HTTPStatusOf for why the SDK's error needs the same status-only treatment).
//
// Error() intentionally renders ONLY the status code: routing.vendorsource.go
// classifies discovery failures into a closed-vocabulary message (vendor
// name, state, HTTP status) before this error's Unwrap()-able cause ever
// reaches a log line, but Error() itself must independently never be able to
// leak the vendor body even if some future caller formats this error
// directly with %v/%w instead of going through that classification.
type discoveryError struct {
	statusCode int
}

func (e *discoveryError) Error() string {
	return fmt.Sprintf("openai: discovery request failed with status %d", e.statusCode)
}

// IsAuthError reports whether err is (or wraps) a vendor 401/403 response
// from DiscoverModels. Used by routing.NewOpenAISource to classify a
// discovery failure as auth_failed vs unreachable, mirroring
// internal/provider/anthropic.IsAuthError.
func IsAuthError(err error) bool {
	var de *discoveryError
	if errors.As(err, &de) {
		return de.statusCode == http.StatusUnauthorized || de.statusCode == http.StatusForbidden
	}
	return false
}

// HTTPStatusOf reports the vendor HTTP status code carried by a DiscoverModels
// failure, if any. Used by routing.vendorsource.go to build a closed-vocabulary
// discovery error (vendor + state + HTTP status) without ever formatting err's
// own Error() string — see discoveryError's doc comment.
func HTTPStatusOf(err error) (int, bool) {
	var de *discoveryError
	if errors.As(err, &de) {
		return de.statusCode, true
	}
	return 0, false
}
