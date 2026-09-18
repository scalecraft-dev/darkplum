package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
)

// fakeAPIError implements smithy.APIError without depending on any concrete
// AWS SDK error type, so this table test exercises the exact interface
// classifyError type-asserts against.
type fakeAPIError struct {
	code string
	msg  string
}

func (e fakeAPIError) Error() string                 { return e.msg }
func (e fakeAPIError) ErrorCode() string             { return e.code }
func (e fakeAPIError) ErrorMessage() string          { return e.msg }
func (e fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }

var _ smithy.APIError = fakeAPIError{}

// TestClassifyError covers the fixed set of discovery error shapes the
// gateway actually produces, per source:
//   - httpsource.go: dial failures and non-2xx responses arrive wrapped in
//     *url.Error (from http.Client.Do); decode failures wrap the stdlib
//     json.SyntaxError.
//   - bedrocksource.go: API-side failures wrap a smithy.APIError; credential
//     resolution failures (bedrock.New -> awsconfig.LoadDefaultConfig) are
//     plain errors with no typed shape.
func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "connection-refused dial error",
			err: fmt.Errorf("vllm-default: models request failed: %w", &url.Error{
				Op:  "Get",
				URL: "http://vllm-default.internal:8000/v1/models",
				Err: &net.OpError{
					Op:  "dial",
					Net: "tcp",
					Err: errors.New("connect: connection refused"),
				},
			}),
			want: errKindUnreachable,
		},
		{
			name: "context deadline",
			err: fmt.Errorf("vllm-default: models request failed: %w", &url.Error{
				Op:  "Get",
				URL: "http://vllm-default.internal:8000/v1/models",
				Err: context.DeadlineExceeded,
			}),
			want: errKindUnreachable,
		},
		{
			name: "HTTP 503 discovery response",
			err:  fmt.Errorf("vllm-default: models request returned %d", 503),
			want: "http_503",
		},
		{
			name: "HTTP 403 from an upstream is an authorisation problem",
			err:  fmt.Errorf("vllm-default: models request returned %d", 403),
			want: errKindAuth,
		},
		{
			name: "HTTP 401 from an upstream is an authorisation problem",
			err:  fmt.Errorf("lmstudio-dev: models request returned %d", 401),
			want: errKindAuth,
		},
		{
			name: "JSON decode error",
			err: func() error {
				var v struct{}
				decodeErr := json.Unmarshal([]byte("{not-json"), &v)
				return fmt.Errorf("vllm-default: decode models response: %w", decodeErr)
			}(),
			want: errKindBadResponse,
		},
		{
			name: "AWS AccessDeniedException",
			err:  fmt.Errorf("bedrock: list inference profiles: %w", fakeAPIError{code: "AccessDeniedException", msg: "not authorized"}),
			want: errKindAuth,
		},
		{
			name: "AWS credential-refresh error string",
			err:  errors.New("bedrock: load AWS config: failed to refresh cached credentials, no EC2 IMDS role found"),
			want: errKindAuth,
		},
		{
			name: "unknown error",
			err:  errors.New("bedrock: list inference profiles: something went sideways"),
			want: errKindError,
		},
		{
			name: "typed AWS error that is not auth-shaped",
			err:  fmt.Errorf("bedrock: list inference profiles: %w", fakeAPIError{code: "ThrottlingException", msg: "rate exceeded"}),
			want: errKindError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyError(tc.err); got != tc.want {
				t.Errorf("classifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyError_NeverReturnsRawErrorText is the /health-leak regression
// guard at the unit level: whatever kind a given error maps to, the returned
// string must be one of the fixed, generic kinds — never contain the
// original error's hostname/URL substring.
func TestClassifyError_NeverReturnsRawErrorText(t *testing.T) {
	err := fmt.Errorf("lmstudio-dev: models request failed: %w", &url.Error{
		Op:  "Get",
		URL: "http://lmstudio.internal.example.com:1234/v1/models",
		Err: errors.New("dial tcp 10.0.0.5:1234: connect: connection refused"),
	})
	got := classifyError(err)
	if got != errKindUnreachable {
		t.Fatalf("classifyError = %q, want %q", got, errKindUnreachable)
	}
	for _, needle := range []string{"lmstudio.internal.example.com", "10.0.0.5", "http://"} {
		if strings.Contains(got, needle) {
			t.Errorf("classifyError result %q must not contain raw error detail %q", got, needle)
		}
	}
}
