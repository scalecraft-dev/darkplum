package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"

	"github.com/aws/smithy-go"
)

// Discovery error kinds surfaced on the unauthenticated /health endpoint via
// SourceStatus.LastErrorKind. classifyError never returns a raw error string
// there — an unauthenticated caller must not learn upstream hostnames or
// internal error text (see classifyError doc below). The full error is still
// logged in the gateway's own log output (Manager.Refresh already does this).
const (
	// errKindUnreachable covers dial/connect/timeout/DNS failures — the
	// backend never responded at the transport level.
	errKindUnreachable = "unreachable"
	// errKindBadResponse covers a reachable backend whose response body did
	// not decode as expected.
	errKindBadResponse = "bad_response"
	// errKindAuth covers AWS credential/access failures surfaced by the
	// Bedrock source (IRSA token not yet valid, ListInferenceProfiles denied,
	// SigV4 signing failures, etc).
	errKindAuth = "auth"
	// errKindError is the catch-all for any failure that doesn't fit a more
	// specific bucket above.
	errKindError = "error"
)

// httpStatusPattern matches the trailing "<verb> returned <status>" shape
// httpsource.go's NewHTTPModelSource uses to report a non-2xx discovery
// response (e.g. "vllm-default: models request returned 503"). Anchored to
// the end of the string so it doesn't false-match a status-shaped number
// appearing elsewhere in a wrapped error chain.
var httpStatusPattern = regexp.MustCompile(`returned (\d{3})$`)

// authErrorCodes are the Bedrock/AWS API error codes classified as "auth"
// rather than the generic "error" bucket — the ones an operator would fix by
// changing the workspace's IAM policy or trust role, not by retrying.
var authErrorCodes = map[string]bool{
	"AccessDeniedException":       true,
	"UnrecognizedClientException": true,
	"ExpiredTokenException":       true,
	"InvalidSignatureException":   true,
	"UnauthorizedException":       true,
	"AuthFailure":                 true,
}

// credentialErrorSubstrings match the plain (non-smithy.APIError) errors the
// AWS SDK's credential provider chain returns when it cannot obtain or
// refresh credentials at all — e.g. an IRSA service account token that has
// not yet been projected, or a missing instance profile in local dev. These
// never reach the API and so are never a smithy.APIError.
var credentialErrorSubstrings = []string{
	"credential",
	"no ec2 imds role found",
}

// classifyError maps a Source.Discover error to one of a small, fixed set of
// kinds safe to expose on the unauthenticated /health endpoint: unreachable,
// http_<status>, bad_response, auth, or error. nil returns "".
//
// This is deliberately a pure function (no gin.Context, no logging) so the
// mapping can be table-tested directly. The raw error — which may contain
// upstream hostnames or internal detail — is never returned; callers must
// keep logging the full error themselves, as Manager.Refresh already does.
func classifyError(err error) string {
	if err == nil {
		return ""
	}

	// AWS API errors (Bedrock discovery: ListInferenceProfiles, etc). A typed
	// error code that is not one of the auth-shaped codes is still a real
	// Bedrock-side failure, not a network problem — bucket it as the generic
	// "error" kind rather than falling through to unreachable/bad_response.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if authErrorCodes[apiErr.ErrorCode()] {
			return errKindAuth
		}
		return errKindError
	}

	if isCredentialError(err) {
		return errKindAuth
	}

	// Network-level unreachability: dial failures, connection refused, DNS
	// failures, and timeouts. context.DeadlineExceeded satisfies net.Error
	// (Timeout() == true) and is what the stdlib http.Client surfaces when a
	// request exceeds its configured Timeout, so it is covered here without a
	// separate context.DeadlineExceeded check.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return errKindUnreachable
	}

	if status, ok := extractHTTPStatus(err.Error()); ok {
		return fmt.Sprintf("http_%d", status)
	}

	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return errKindBadResponse
	}

	return errKindError
}

// isCredentialError matches AWS SDK credential-provider-chain failures that
// never reach the API and so are never a smithy.APIError — they surface as
// plain errors from bedrock.New's awsconfig.LoadDefaultConfig call (e.g. "bedrock:
// load AWS config: failed to refresh cached credentials, no EC2 IMDS role found").
func isCredentialError(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, substr := range credentialErrorSubstrings {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

// extractHTTPStatus pulls the status code out of the error message shape
// NewHTTPModelSource produces for a non-2xx discovery response.
func extractHTTPStatus(msg string) (int, bool) {
	m := httpStatusPattern.FindStringSubmatch(msg)
	if m == nil {
		return 0, false
	}
	var status int
	if _, err := fmt.Sscanf(m[1], "%d", &status); err != nil {
		return 0, false
	}
	return status, true
}
