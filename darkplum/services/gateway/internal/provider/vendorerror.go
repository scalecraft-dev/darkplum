package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
)

var vendorErrorTypePattern = regexp.MustCompile(`^[a-z_]{1,40}$`)

// SanitizeVendorErrorType extracts error.type from a vendor's JSON error body
// (Anthropic and OpenAI share the {"error":{"type":...}} shape) and returns it
// only if it matches ^[a-z_]{1,40}$; anything else, including an unparseable
// body, yields "unknown". The message field is never read.
func SanitizeVendorErrorType(body []byte) string {
	var env struct {
		Error struct {
			Type any `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return "unknown"
	}
	t, ok := env.Error.Type.(string)
	if !ok || !vendorErrorTypePattern.MatchString(t) {
		return "unknown"
	}
	return t
}

// vendorErrorBody / vendorErrorDetail is the OpenAI-compatible wire shape
// every error a direct vendor API provider (anthropic, openai) returns is
// rendered as, matching internal/handler/chat.go's openAIError /
// openAIErrorBody exactly (field-for-field) so the gateway's error handling
// never needs to special-case a vendor provider's error body.
type vendorErrorBody struct {
	Error vendorErrorDetail `json:"error"`
}

type vendorErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	// Param names the request field a vendor 400 refers to, set only by
	// WithParam from a closed allowlist, never from vendor text.
	Param string `json:"param,omitempty"`
}

// VendorAPIError implements APIError. It is the ONLY error type a direct
// vendor API provider package (anthropic, openai) returns from
// Complete/Stream/DiscoverModels once translated — never a bare fmt.Errorf
// wrapping vendor text — so handler.handleProviderError and
// handler.writeStreamError (which type-assert the APIError interface rather
// than ever calling err.Error() on a provider failure) always get a body
// that has already been scrubbed of vendor detail, request ids, and — always
// — the API key.
//
// Originally implemented inside internal/provider/anthropic as an unexported
// APIError struct; extracted here, behaviour-preserving (anthropic keeps a
// package-local `type APIError = provider.VendorAPIError` alias so its own
// tests and call sites are unaffected), so internal/provider/openai can
// share the same wire-error rendering and status-code mapping instead of
// reimplementing it.
type VendorAPIError struct {
	// Vendor names which provider produced this error ("anthropic",
	// "openai") — used only in Error()'s own diagnostic string, never in
	// ResponseBody.
	Vendor     string
	StatusCode int
	Body       []byte
	RetryAfter int
	Cause      error
	// Code is the closed-vocabulary error code written into Body
	// (provider_auth_failed, provider_rate_limited, invalid_request,
	// upstream_error, upstream_timeout). VendorStatus is the HTTP status the
	// vendor itself returned, 0 when the failure never got a response. Both
	// exist so an operator-facing log line can say WHICH closed outcome
	// happened without carrying a byte of the vendor's own text.
	Code         string
	VendorStatus int
	// VendorErrorType is the vendor's own JSON error.type, sanitised to
	// ^[a-z_]{1,40}$ (else "unknown") by SanitizeVendorErrorType. Empty when
	// the caller did not attach one. Only the type tag is ever kept, never
	// the vendor's message.
	VendorErrorType string
	// WorkspaceRequired is set by the anthropic provider when the vendor's
	// 400 says the API key is not scoped to a workspace and the request must
	// carry the anthropic-workspace-id header. Only the boolean is kept,
	// never the vendor's message. It drives POST /v1/providers/anthropic/
	// verify's workspace_required outcome; the chat path still renders the
	// ordinary generic 400 invalid_request.
	WorkspaceRequired bool
}

// Error deliberately never includes Cause: Cause may wrap raw vendor text (an
// account id, an internal hostname), and unlike Body (always pre-scrubbed
// JSON this package built itself), Cause's own Error() string is not
// guaranteed safe to surface anywhere a caller might log or display this
// error's Error() string.
func (e *VendorAPIError) Error() string {
	return fmt.Sprintf("%s: API error %d: %s", e.Vendor, e.StatusCode, e.Body)
}

func (e *VendorAPIError) Unwrap() error { return e.Cause }

// HTTPStatus implements APIError.
func (e *VendorAPIError) HTTPStatus() int { return e.StatusCode }

// ResponseBody implements APIError.
func (e *VendorAPIError) ResponseBody() []byte { return e.Body }

// RetryAfterSeconds implements APIError.
func (e *VendorAPIError) RetryAfterSeconds() int { return e.RetryAfter }

var _ APIError = (*VendorAPIError)(nil)

// WithParam re-renders e's body to name the request field the vendor
// rejected, so a 400 on response_format or max_completion_tokens tells the
// caller which parameter this model does not accept instead of the generic
// "rejected this request". param must come from the calling package's own
// allowlist (never a vendor-supplied string); the message stays static text.
func (e *VendorAPIError) WithParam(param string) *VendorAPIError {
	body, err := json.Marshal(vendorErrorBody{Error: vendorErrorDetail{
		Message: fmt.Sprintf("the model provider does not accept parameter %q for this model", param),
		Type:    "invalid_request_error",
		Code:    "invalid_request",
		Param:   param,
	}})
	if err == nil {
		e.Body = body
	}
	return e
}

// NewVendorAPIError builds a VendorAPIError with a fully-rendered
// OpenAI-compatible body. message must already be safe to show a caller —
// either a static string the calling package wrote itself, or deliberately
// generic text; a vendor's raw error message is NEVER passed in here (see
// TranslateVendorHTTPStatus).
func NewVendorAPIError(vendor string, statusCode int, errType, code, message string, retryAfter int, cause error) *VendorAPIError {
	body, err := json.Marshal(vendorErrorBody{Error: vendorErrorDetail{Message: message, Type: errType, Code: code}})
	if err != nil {
		// json.Marshal on this fixed, all-string shape cannot fail in
		// practice; fall back to a minimal static body rather than panic.
		body = []byte(`{"error":{"message":"the model provider returned an error","type":"upstream_error","code":"upstream_error"}}`)
	}
	return &VendorAPIError{Vendor: vendor, StatusCode: statusCode, Body: body, RetryAfter: retryAfter, Code: code,
		Cause: cause}
}

// NewVendorInvalidRequestError builds a 400 invalid_request error whose
// message is authored by the calling package itself (e.g. anthropic's own
// normalizeMessages validation messages) — always safe to show verbatim,
// matching the "invalid_request" / "invalid_request_error" code+type pair
// internal/handler/chat.go already uses for its own 400s.
func NewVendorInvalidRequestError(vendor, message string) *VendorAPIError {
	return NewVendorAPIError(vendor, 400, "invalid_request_error", "invalid_request", message, 0, nil)
}

// AuthFailureReporter is the narrow slice of *StatusRegistry a direct vendor
// API provider package needs to report a request-time (not just
// discovery-time) 401/403 into the vendor status block. Defined as an
// interface so tests can substitute a fake without constructing a real
// registry.
type AuthFailureReporter interface {
	Report(name string, st ProviderStatus)
	Snapshot() []ProviderStatus
}

var _ AuthFailureReporter = (*StatusRegistry)(nil)

// ReportAuthFailed updates the vendor status registry's State to auth_failed
// on a request-time 401/403, without a full Report call clobbering
// Configured/ModelCount/LastSyncAt as set by the last discovery tick — see
// the field-preserving Snapshot lookup below. status may be nil (no
// ProviderStatus reporting, e.g. in tests that don't care about it) — the
// call is simply skipped.
func ReportAuthFailed(status AuthFailureReporter, name string) {
	if status == nil {
		return
	}
	st := ProviderStatus{Configured: true, State: ProviderStateAuthFailed}
	for _, existing := range status.Snapshot() {
		if existing.Name == name {
			st.Configured = existing.Configured
			st.ModelCount = existing.ModelCount
			st.LastSyncAt = existing.LastSyncAt
			st.FilterSource = existing.FilterSource
			st.Filter = existing.Filter
			st.EnvFilterSetAt = existing.EnvFilterSetAt
			st.WorkspaceID = existing.WorkspaceID
			break
		}
	}
	status.Report(name, st)
}

// TranslateVendorHTTPStatus converts a vendor HTTP response's status code
// into a *VendorAPIError per the wire contract's closed vocabulary: 429 ->
// provider_rate_limited (429, retryAfterSeconds passed through); 401/403 ->
// provider_auth_failed (502, generic text, and — via status, which may be
// nil — Report state auth_failed to the vendor status registry); 400 ->
// invalid_request (400, generic text: the vendor's own message is never
// known to be free of account/org detail, so it is never relayed); every
// other status (5xx, or any other 4xx) -> upstream_error (502). cause is
// retained only for Unwrap/internal diagnostics — it is NEVER surfaced in
// the response body, and must never itself carry the vendor API key.
func TranslateVendorHTTPStatus(vendor string, statusCode, retryAfterSeconds int, status AuthFailureReporter, cause error) *VendorAPIError {
	e := translateVendorHTTPStatus(vendor, statusCode, retryAfterSeconds, status, cause)
	e.VendorStatus = statusCode
	return e
}

func translateVendorHTTPStatus(vendor string, statusCode, retryAfterSeconds int, status AuthFailureReporter, cause error) *VendorAPIError {
	switch {
	case statusCode == 429:
		return NewVendorAPIError(vendor, 429, "rate_limit_error", "provider_rate_limited",
			"the model provider rate-limited this request", retryAfterSeconds, cause)
	case statusCode == 401 || statusCode == 403:
		ReportAuthFailed(status, vendor)
		return NewVendorAPIError(vendor, 502, "upstream_error", "provider_auth_failed",
			"the model provider rejected the gateway's credentials", 0, cause)
	case statusCode == 400:
		return NewVendorAPIError(vendor, 400, "invalid_request_error", "invalid_request",
			"the model provider rejected this request", 0, cause)
	default:
		return NewVendorAPIError(vendor, 502, "upstream_error", "upstream_error",
			"the model provider returned an error", 0, cause)
	}
}

// IsAuthFailedError reports whether err is (or wraps) a *VendorAPIError whose
// wire code is "provider_auth_failed" — the translated shape
// TranslateVendorHTTPStatus produces for a vendor 401/403. Used by
// internal/handler.ProvidersHandler to classify a Provider.VerifyKey failure
// into POST /v1/providers/:vendor/verify's own closed vocabulary
// ("auth_failed" vs "unreachable"): every OTHER VendorAPIError (rate limit,
// invalid_request, upstream_error/timeout) is "unreachable" instead — "can
// we verify this key" only ever answers yes/no/couldn't-reach-the-vendor, it
// does not surface the vendor's full error taxonomy.
func IsAuthFailedError(err error) bool {
	var apiErr *VendorAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	var body vendorErrorBody
	if json.Unmarshal(apiErr.Body, &body) != nil {
		return false
	}
	return body.Error.Code == "provider_auth_failed"
}

// IsTransportTimeout reports whether err is a context deadline/cancellation
// timeout or a net.Error reporting Timeout() == true — the generic
// classification a vendor provider's own http.Client.Do failure (dial
// timeout, TLS handshake timeout, response-header timeout) falls under when
// it is not itself a structured vendor HTTP response. internal/provider/openai
// (raw net/http) uses this directly; internal/provider/anthropic classifies
// the equivalent case against the SDK's own error type instead, since the SDK
// wraps most of these the same way regardless.
func IsTransportTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// VendorResponseError returns the *VendorAPIError in err's chain when the
// vendor actually answered with an HTTP response other than 401/403 (429
// included) — the "vendor_error" verify outcome. Transport failures
// (VendorStatus == 0) and auth failures return nil, false.
func VendorResponseError(err error) (*VendorAPIError, bool) {
	var apiErr *VendorAPIError
	if !errors.As(err, &apiErr) {
		return nil, false
	}
	if apiErr.VendorStatus == 0 || apiErr.VendorStatus == 401 || apiErr.VendorStatus == 403 {
		return nil, false
	}
	return apiErr, true
}
